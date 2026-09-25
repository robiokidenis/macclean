package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"macclean/internal/dashboard"
	"macclean/internal/devcache"
	"macclean/internal/devdeps"
	"macclean/internal/duplicates"
	"macclean/internal/fsutil"
	"macclean/internal/macos"
	"macclean/internal/scanner"
	"macclean/internal/settings"
	"macclean/internal/sysinfo"
)

func newTestModel() *model {
	m := &model{version: "test", runCtx: context.Background(), s: settings.Defaults()}
	m.send = func(tea.Msg) {}
	return m
}

func key(s string) tea.KeyMsg  { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
func arrowUp() tea.KeyMsg     { return tea.KeyMsg{Type: tea.KeyUp} }
func arrowDown() tea.KeyMsg   { return tea.KeyMsg{Type: tea.KeyDown} }
func enterKey() tea.KeyMsg    { return tea.KeyMsg{Type: tea.KeyEnter} }
func escKey() tea.KeyMsg      { return tea.KeyMsg{Type: tea.KeyEsc} }
func backspaceKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyBackspace} }
func spaceKey() tea.KeyMsg    { return tea.KeyMsg{Type: tea.KeySpace} }

func TestMenuNavigation(t *testing.T) {
	m := newTestModel()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	if m.screen != scrMenu {
		t.Fatal("should start at menu")
	}
	m.Update(arrowDown())
	if m.menuCursor != 1 {
		t.Fatalf("cursor = %d", m.menuCursor)
	}
	m.Update(arrowUp())
	if m.menuCursor != 0 {
		t.Fatalf("cursor = %d", m.menuCursor)
	}
	// Enter on Analyze → roots picker.
	m.Update(enterKey())
	if m.screen != scrRoots {
		t.Fatalf("screen = %v", m.screen)
	}
	m.Update(escKey())
	if m.screen != scrMenu {
		t.Fatal("esc should return to menu")
	}
}

func TestSettingsCycleAndQuit(t *testing.T) {
	m := newTestModel()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	// Walk to Settings by label (index-independent).
	steps := 0
	for m.menuCursor != indexOfType("Settings") && steps < len(menuItems) {
		m.Update(arrowDown())
		steps++
	}
	m.Update(enterKey())
	if m.screen != scrSettings {
		t.Fatalf("screen = %v", m.screen)
	}
	m.Update(arrowDown()) // to old-days row
	m.Update(arrowDown()) // to dup-min row
	m.Update(arrowDown()) // to dep-staleness row
	before := m.s.DepStaleDays
	m.Update(spaceKey())
	if m.s.DepStaleDays == before {
		t.Fatal("space should cycle the dep staleness setting")
	}
	m.Update(escKey())
	if m.screen != scrMenu {
		t.Fatal("esc should leave settings")
	}
}

func indexOfType(label string) int {
	for i, item := range menuItems {
		if item.label == label {
			return i
		}
	}
	return -1
}

func TestBrowserDrillDownAndBack(t *testing.T) {
	m := newTestModel()
	root := &scanner.DirNode{Path: "/u/root", Name: "root"}
	sub := &scanner.DirNode{Path: "/u/root/sub", Name: "sub", Size: 500}
	root.Children = []*scanner.DirNode{sub}
	root.Files = []fsutil.FileInfo{{Path: "/u/root/big.bin", Size: 900}}
	m.tree = root
	m.cur = root
	m.buildBrowserRows()
	m.screen = scrBrowser

	if len(m.browserRows) != 2 { // 1 dir + 1 file
		t.Fatalf("rows = %d", len(m.browserRows))
	}
	m.Update(enterKey()) // first row is the dir
	if m.cur.Path != "/u/root/sub" {
		t.Fatalf("did not drill into sub: %s", m.cur.Path)
	}
	m.Update(backspaceKey())
	if m.cur.Path != "/u/root" {
		t.Fatalf("backspace should go up: %s", m.cur.Path)
	}
	m.Update(backspaceKey()) // at root → back to roots picker
	if m.screen != scrRoots {
		t.Fatalf("screen = %v", m.screen)
	}
}

func TestFileListSelectionAndTrashConfirm(t *testing.T) {
	m := newTestModel()
	m.screen = scrFileList
	m.flTitle = "Large files"
	m.flFiles = []fsutil.FileInfo{
		{Path: "/u/a.bin", Size: 10},
		{Path: "/u/b.bin", Size: 20},
	}
	m.flChecked = map[string]bool{}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	m.Update(spaceKey())
	if !m.flChecked["/u/a.bin"] {
		t.Fatal("space should select")
	}
	m.Update(arrowDown())
	m.Update(key("d"))
	if m.modal == nil || m.modal.kind != modalConfirm {
		t.Fatal("d should open a confirm modal")
	}
	// Say yes → trash executed via goroutine with send stub; modal closed.
	m.Update(key("y"))
	if m.modal != nil {
		t.Fatal("modal should close")
	}
}

func TestDuplicatesSuggestAndMark(t *testing.T) {
	m := newTestModel()
	g := duplicates.Group{Size: 100, Files: []fsutil.FileInfo{
		{Path: "/u/old.bin", Size: 100, ModTime: 1000},
		{Path: "/u/new.bin", Size: 100, ModTime: 9999},
	}}
	m.dup = &duplicates.Result{Groups: []duplicates.Group{g}, Reclaimable: 100}
	m.dupChecked = map[string]bool{}
	m.buildDupRows()
	m.screen = scrDuplicates

	// Cursor starts at the first non-header row (old.bin).
	m.Update(key("s")) // suggest: keep oldest, mark the rest
	if !m.dupChecked["/u/new.bin"] {
		t.Fatal("suggest should mark the newer copy")
	}
	if m.dupChecked["/u/old.bin"] {
		t.Fatal("suggest must not mark the kept copy")
	}
	m.Update(key("c")) // clear
	if len(m.dupChecked) != 0 {
		t.Fatal("c should clear marks")
	}
}

func TestJobDoneTransitions(t *testing.T) {
	m := newTestModel()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})

	// Simulate a finished downloads job.
	m.flChecked = nil
	m.Update(jobDoneMsg{kind: jobDownloads, scan: &scanner.Result{
		Root:  &scanner.DirNode{Path: "/u/Downloads"},
		Files: []fsutil.FileInfo{{Path: "/u/Downloads/x.dmg", Size: 5}},
	}})
	if m.screen != scrDownloads || m.dl == nil {
		t.Fatalf("screen=%v dl=%v", m.screen, m.dl)
	}
	if m.dl.Total != 5 {
		t.Fatal("downloads report should be built")
	}

	// Enter a category → file list.
	m.Update(enterKey())
	if m.screen != scrFileList || len(m.flFiles) != 1 {
		t.Fatalf("screen=%v files=%d", m.screen, len(m.flFiles))
	}

	// Dashboard done → dashboard screen.
	dashRep := &dashboard.Report{Volume: sysinfo.Volume{Name: "Macintosh HD"}}
	m.Update(jobDoneMsg{kind: jobDashboard, dash: dashRep})
	if m.screen != scrDashboard {
		t.Fatalf("screen=%v", m.screen)
	}

	// Cleanup detection done → cleanup screen.
	m.Update(jobDoneMsg{kind: jobCleanupDetect, caches: []devcache.Cache{{Name: "npm cache", Path: "/x", Exists: true, Size: 1}}})
	if m.screen != scrCleanup || len(m.cleanList) != 1 {
		t.Fatalf("screen=%v", m.screen)
	}

	// macOS scan done → same cleanup screen with macOS rows.
	m.Update(jobDoneMsg{kind: jobMacos, macosIt: []macos.Item{{Name: "Trash", Kind: macos.KindTrash, Path: "/t", Exists: true, Safety: macos.Destructive}}})
	if m.screen != scrCleanup || len(m.cleanList) != 1 || m.cleanList[0].name != "Trash" {
		t.Fatalf("screen=%v list=%v", m.screen, m.cleanList)
	}

	// Cancelled job → back to menu.
	m.Update(jobDoneMsg{kind: jobDuplicates, cancelled: true})
	if m.screen != scrMenu {
		t.Fatal("cancelled job should return to menu")
	}
}

func TestDepsScreen(t *testing.T) {
	m := newTestModel()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	stale := devdeps.Entry{
		Path: "/u/dead/node_modules", Kind: "node_modules", Size: 5000,
		LastActivity: 1000000000, Stale: true, Reinstall: "npm ci", Files: 900,
	}
	fresh := devdeps.Entry{
		Path: "/u/live/node_modules", Kind: "node_modules", Size: 9000,
		LastActivity: time.Now().Unix(), Stale: false, Reinstall: "yarn install", Files: 90,
	}
	m.deps = &devdeps.Report{Entries: []devdeps.Entry{fresh, stale}, StaleDays: 90,
		Stale: []devdeps.Entry{stale}, Fresh: []devdeps.Entry{fresh}, StaleSize: 5000, FreshSize: 9000}
	m.depsChecked = map[string]bool{}
	m.depsByAge = true
	m.screen = scrDeps

	// Stalest-first regardless of construction order.
	entries := m.depEntries()
	if entries[0].Path != "/u/dead/node_modules" {
		t.Fatalf("stale entry should lead, got %s", entries[0].Path)
	}
	// Sort toggle flips to size order.
	m.Update(key("/"))
	if m.depEntries()[0].Size != 9000 {
		t.Fatal("size sort should put the 9000-byte folder first")
	}
	m.Update(key("/"))

	// Select then d opens a confirm listing reinstall hints.
	m.Update(spaceKey())
	m.Update(key("d"))
	if m.modal == nil || m.modal.kind != modalConfirm {
		t.Fatal("d should open confirm modal")
	}
	if !strings.Contains(strings.Join(m.modal.lines, " "), "npm ci") {
		t.Fatalf("modal should mention reinstall hint: %v", m.modal.lines)
	}
	// Info modal shows staleness and hint.
	m.modal = nil
	m.Update(key("i"))
	if m.modal == nil || !strings.Contains(strings.Join(m.modal.lines, " "), "stale") {
		t.Fatal("info modal should describe the entry")
	}
}

func TestModalTypedConfirmation(t *testing.T) {
	m := newTestModel()
	called := false
	m.openConfirmTyped("DESTRUCTIVE", []string{"x"}, func(m *model) { called = true })
	m.Update(key("y"))
	m.Update(key("e"))
	m.Update(key("s"))
	m.Update(enterKey())
	if !called {
		t.Fatal("typed 'yes' + enter must trigger action")
	}

	called = false
	m.openConfirmTyped("DESTRUCTIVE", []string{"x"}, func(m *model) { called = true })
	m.Update(key("n"))
	m.Update(key("o"))
	m.Update(enterKey())
	if called {
		t.Fatal("wrong typed text must not trigger")
	}
}
