package tui

import (
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"macclean/internal/devdeps"
	"macclean/internal/duplicates"
	"macclean/internal/fsutil"
	"macclean/internal/report"
)

func TestIsDatabaseFile(t *testing.T) {
	yes := []string{
		"/data/backup.sql", "/data/db.sqlite", "/data/db.sqlite-wal",
		"/var/lib/mysql/users.ibd", "/var/lib/mysql/users.frm",
		"/x/orders.MYD", "/x/queue.bson", "/x/dump.rdb", "/x/aof.aof",
		"/x/accdb.accdb", "/x/something.DB",
	}
	for _, p := range yes {
		if !fsutil.IsDatabaseFile(p) {
			t.Errorf("IsDatabaseFile(%q) should be true", p)
		}
	}
	no := []string{"/x/video.mp4", "/x/photo.JPG", "/x/app.dmg", "/x/backup.zip", "/x/node_modules/x.js"}
	for _, p := range no {
		if fsutil.IsDatabaseFile(p) {
			t.Errorf("IsDatabaseFile(%q) should be false", p)
		}
	}
}

// Database files get a visible badge in lists and a strong warning in the
// trash confirmation.
func TestDatabaseBadgeAndModalWarning(t *testing.T) {
	m := newTestModel()
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 40})
	home, _ := os.UserHomeDir()
	realDB := home + "/.macclean-dbtest-prod.sqlite"
	if err := os.WriteFile(realDB, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(realDB) })

	m.flTitle = "Large files"
	m.flFiles = []fsutil.FileInfo{
		{Path: realDB, Size: 10},
		{Path: "/u/plain-video.mov", Size: 20},
	}
	m.flChecked = map[string]bool{realDB: true}
	m.screen = scrFileList

	view := m.View()
	if !strings.Contains(view, "⚠db") {
		t.Fatal("database rows must carry the ⚠db badge")
	}

	m.Update(key("d"))
	if m.modal == nil {
		t.Fatal("d should open the confirm modal")
	}
	if !strings.Contains(strings.Join(m.modal.lines, " "), "database file(s)") {
		t.Fatalf("modal must warn about database files: %v", m.modal.lines)
	}
}

// Rows whose path no longer exists are struck through and flagged, so
// stale lists (e.g. dashboard cache) never look like live data.
func TestDeletedRowsAreStruckThrough(t *testing.T) {
	m := newTestModel()
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 40})
	m.flTitle = "Large files"
	m.flFiles = []fsutil.FileInfo{
		{Path: "/u/definitely/gone-now.bin", Size: 10}, // does not exist
		{Path: "/etc/hosts", Size: 5},                   // exists
	}
	m.flChecked = map[string]bool{}
	m.screen = scrFileList
	view := m.View()
	if !strings.Contains(view, "· deleted") {
		t.Fatal("missing-path rows must be flagged · deleted")
	}

	// Duplicates list shows the same flag.
	m2 := newTestModel()
	m2.Update(tea.WindowSizeMsg{Width: 110, Height: 40})
	m2.dup = &duplicates.Result{Groups: []duplicates.Group{{
		Size: 10,
		Files: []fsutil.FileInfo{
			{Path: "/u/gone/a.sqlite", Size: 10},
			{Path: "/u/gone/b.sqlite", Size: 10},
		},
	}}}
	m2.dupChecked = map[string]bool{}
	m2.buildDupRows()
	m2.screen = scrDuplicates
	if !strings.Contains(m2.View(), "· deleted") {
		t.Fatal("duplicate rows for missing paths must be flagged")
	}

	// Deps list too.
	m3 := newTestModel()
	m3.Update(tea.WindowSizeMsg{Width: 110, Height: 40})
	m3.deps = &devdeps.Report{Entries: []devdeps.Entry{{Path: "/u/gone/node_modules", Size: 5, Stale: true}}}
	m3.depsChecked = map[string]bool{}
	m3.screen = scrDeps
	if !strings.Contains(m3.View(), "· deleted") {
		t.Fatal("dep rows for missing paths must be flagged")
	}
}

// Trashing through the UI drops the scan-result cache so reruns cannot
// resurrect deleted files.
func TestTrashInvalidatesResultCache(t *testing.T) {
	report.SaveCache("test-resurrect", map[string]int{"x": 1})
	if report.LoadCache("test-resurrect") == nil {
		t.Fatal("precondition: cache entry exists")
	}
	m := newTestModel()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m.screen = scrFileList
	m.Update(trashDoneMsg{ok: 2})
	if report.LoadCache("test-resurrect") != nil {
		t.Fatal("successful trash must invalidate the result cache")
	}
}
