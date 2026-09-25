package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"macclean/internal/analysis"
	"macclean/internal/dashboard"
	"macclean/internal/devcache"
	"macclean/internal/devdeps"
	"macclean/internal/duplicates"
	"macclean/internal/fsutil"
	"macclean/internal/scanner"
	"macclean/internal/sysinfo"
)

// Esc inside a drilled-down folder must return to the main menu, and
// Backspace must still climb one level.
func TestBrowserEscGoesToMenu(t *testing.T) {
	m := newTestModel()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	root := node("/u/root", node("/u/root/a", node("/u/root/a/b")))
	m.tree = root
	m.cur = root.Children[0].Children[0] // inside a/b
	m.buildBrowserRows()
	m.screen = scrBrowser

	m.Update(escKey())
	if m.screen != scrMenu {
		t.Fatalf("esc in folder: screen = %v, want menu", m.screen)
	}

	// Backspace climbs: a/b → a → a's parent resets cursor.
	m.screen = scrBrowser
	m.cur = root.Children[0].Children[0]
	m.buildBrowserRows()
	m.Update(backspaceKey())
	if m.cur.Path != "/u/root/a" {
		t.Fatalf("backspace should climb to /u/root/a, got %s", m.cur.Path)
	}
}

// Esc from a detail screen opened via another screen returns to that
// screen, not the menu.
func TestEscReturnsToOrigin(t *testing.T) {
	m := newTestModel()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	// downloads → category file list → esc → downloads
	m.dl = analysis.AnalyzeDownloads("/u/DL", []fsutil.FileInfo{
		{Path: "/u/DL/a.dmg", Size: 10},
	}, 1<<60, 180)
	m.screen = scrDownloads
	m.Update(enterKey())
	if m.screen != scrFileList {
		t.Fatalf("screen = %v", m.screen)
	}
	m.Update(escKey())
	if m.screen != scrDownloads {
		t.Fatalf("esc from downloads category: screen = %v, want downloads", m.screen)
	}

	// dashboard → duplicates → esc → dashboard
	m.dash = &dashboard.Report{Volume: sysinfo.Volume{}, Dup: &duplicates.Result{}, Deps: &devdeps.Report{},
		Categories: []dashboard.Category{{Name: "Duplicates", Kind: "duplicates", Actual: 1}}}
	m.screen = scrDashboard
	m.dashCursor = 0
	m.Update(enterKey())
	if m.screen != scrDuplicates {
		t.Fatalf("screen = %v", m.screen)
	}
	m.Update(escKey())
	if m.screen != scrDashboard {
		t.Fatalf("esc from dashboard detail: screen = %v, want dashboard", m.screen)
	}

	// Backspace behaves like esc on detail screens.
	m.screen = scrDashboard
	m.Update(enterKey())
	m.Update(backspaceKey())
	if m.screen != scrDashboard {
		t.Fatalf("backspace from dashboard detail: screen = %v, want dashboard", m.screen)
	}
}

// Detail screens opened straight from the menu still Esc to the menu.
func TestEscFromMenuOpenedScreen(t *testing.T) {
	m := newTestModel()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m.deps = &devdeps.Report{Entries: []devdeps.Entry{{Path: "/u/x/node_modules"}}}
	m.depsChecked = map[string]bool{}
	m.screen = scrDeps
	m.Update(escKey())
	if m.screen != scrMenu {
		t.Fatalf("screen = %v, want menu", m.screen)
	}
	m.cleanList = devRows([]devcache.Cache{{Name: "npm", Path: "/x", Exists: true}})
	m.screen = scrCleanup
	m.Update(escKey())
	if m.screen != scrMenu {
		t.Fatalf("screen = %v, want menu", m.screen)
	}
}

// tiny tree helper: node(path, children...)
func node(path string, children ...*scanner.DirNode) *scanner.DirNode {
	n := &scanner.DirNode{Path: path, Name: pathBase(path)}
	n.Children = children
	return n
}

func pathBase(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}
