package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"macclean/internal/devdeps"
	"macclean/internal/fsutil"
)

// Selecting entries must surface a visible call-to-action, and the trash
// key must work — including uppercase — plus c to clear.
func TestSelectionShowsActionAndKeysWork(t *testing.T) {
	m := newTestModel()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m.deps = &devdeps.Report{
		Entries: []devdeps.Entry{
			{Path: "/u/stale/node_modules", Size: 100, LastActivity: 1000, Stale: true, Reinstall: "npm ci"},
			{Path: "/u/live/node_modules", Size: 500, LastActivity: 9999999999, Reinstall: "yarn install"},
		},
		StaleDays: 90,
	}
	m.depsChecked = map[string]bool{}
	m.depsByAge = true
	m.screen = scrDeps

	m.Update(spaceKey()) // select first row
	view := m.View()
	if !strings.Contains(view, "press d to move") {
		t.Fatalf("selection must show the trash call-to-action:\n%s", view)
	}

	// Uppercase D also trashes (opens confirm), and the modal warns about
	// in-use entries in the selection.
	m.depsChecked["/u/live/node_modules"] = true
	m.Update(key("D"))
	if m.modal == nil || m.modal.kind != modalConfirm {
		t.Fatal("D should open the confirm modal")
	}
	joined := strings.Join(m.modal.lines, " ")
	if !strings.Contains(joined, "IN USE") {
		t.Fatalf("modal should warn about in-use entries: %v", m.modal.lines)
	}
	m.modal = nil

	// c clears the selection and the CTA disappears.
	m.Update(key("c"))
	if len(m.depsChecked) != 0 {
		t.Fatal("c should clear the selection")
	}
	if strings.Contains(m.View(), "press d to move") {
		t.Fatal("CTA should disappear after clearing")
	}
}

// The same CTA applies to the plain file list.
func TestFileListSelectionShowsAction(t *testing.T) {
	m := newTestModel()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m.flTitle = "Large files"
	m.flFiles = []fsutil.FileInfo{{Path: "/u/a.bin", Size: 10}, {Path: "/u/b.bin", Size: 20}}
	m.flChecked = map[string]bool{"/u/a.bin": true}
	m.screen = scrFileList

	if !strings.Contains(m.View(), "press d to move") {
		t.Fatal("file list selection must show the trash call-to-action")
	}
}
