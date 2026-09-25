package macos

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeHome builds:
//   ~/.Trash/one.bin (1KB) + nested/dir two.bin (2KB)   → 3 KB
//   ~/Library/Logs/app.log (500B) + Crash/current.panic (300B)
//   ~/Library/Caches/{go-build(1KB, dev-managed), SomeApp(2KB), loose.tmp(100B)}
//   ~/Library/Application Support/MobileSync/Backup/phone (4KB)
func fakeHome(t *testing.T) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	root, err := os.MkdirTemp(home, ".macclean-macos-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	t.Setenv("HOME", root)

	wf := func(rel string, size int) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wf(".Trash/one.bin", 1000)
	wf(".Trash/nested/two.bin", 2000)
	wf("Library/Logs/app.log", 500)
	wf("Library/Logs/DiagnosticReports/current.panic", 300)
	wf("Library/Caches/go-build/chunk-1", 1000)
	wf("Library/Caches/SomeApp/cache.db", 2000)
	wf("Library/Caches/loose.tmp", 100)
	wf("Library/Application Support/MobileSync/Backup/phone/backup.tar", 4000)
	return root
}

func TestScanMeasuresAndExcludesManaged(t *testing.T) {
	_ = fakeHome(t)
	items := Scan(context.Background(), nil)
	byKind := map[string]Item{}
	for _, it := range items {
		if it.Kind == KindCaches && !it.Exists {
			t.Fatal("caches item should exist")
		}
		byKind[it.Kind] = it
	}

	if got := byKind[KindTrash].Size; got != 3000 {
		t.Errorf("trash size = %d, want 3000", got)
	}
	if got := byKind[KindLogs].Size; got != 800 {
		t.Errorf("logs size = %d, want 800", got)
	}
	// go-build is dev-managed and must be excluded: 2000+100 only.
	if got := byKind[KindCaches].Size; got != 2100 {
		t.Errorf("caches size = %d, want 2100 (dev-managed go-build excluded)", got)
	}
	if got := byKind[KindBackups].Size; got != 4000 {
		t.Errorf("backups size = %d, want 4000", got)
	}
	if byKind[KindTrash].Safety != Destructive {
		t.Error("emptying the Trash must be destructive-class")
	}
	if byKind[KindLogs].Safety != Safe || byKind[KindCaches].Safety != Safe {
		t.Error("logs and caches must be safe-class")
	}
}

func TestCleanCachesKeepsManaged(t *testing.T) {
	root := fakeHome(t)
	items := Scan(context.Background(), nil)
	var caches Item
	for _, it := range items {
		if it.Kind == KindCaches {
			caches = it
		}
	}
	if err := Clean(context.Background(), caches, false); err != nil {
		t.Fatal(err)
	}
	cdir := filepath.Join(root, "Library", "Caches")
	if _, err := os.Stat(filepath.Join(cdir, "go-build")); err != nil {
		t.Error("dev-managed go-build must survive macOS cache cleaning")
	}
	if _, err := os.Stat(filepath.Join(cdir, "SomeApp")); err == nil {
		t.Error("SomeApp cache should be removed")
	}
	if _, err := os.Stat(filepath.Join(cdir, "loose.tmp")); err == nil {
		t.Error("loose cache file should be removed")
	}
	if _, err := os.Stat(cdir); err != nil {
		t.Error("the Caches directory itself must be kept")
	}
}

func TestCleanTrashAndLogs(t *testing.T) {
	root := fakeHome(t)
	items := Scan(context.Background(), nil)
	byKind := map[string]Item{}
	for _, it := range items {
		byKind[it.Kind] = it
	}

	// Destructive without force must refuse.
	if err := Clean(context.Background(), byKind[KindTrash], false); err == nil {
		t.Fatal("trash clean must refuse without force")
	}
	if err := Clean(context.Background(), byKind[KindTrash], true); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, ".Trash")); len(entries) != 0 {
		t.Fatalf("trash should be empty, %d entries remain", len(entries))
	}
	if _, err := os.Stat(filepath.Join(root, ".Trash")); err != nil {
		t.Error(".Trash directory itself must be kept")
	}

	if err := Clean(context.Background(), byKind[KindLogs], false); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "Library", "Logs")); len(entries) != 0 {
		t.Fatalf("logs should be empty, %d entries remain", len(entries))
	}
}

func TestCleanBackupsNeedsForce(t *testing.T) {
	root := fakeHome(t)
	items := Scan(context.Background(), nil)
	var backups Item
	for _, it := range items {
		if it.Kind == KindBackups {
			backups = it
		}
	}
	if err := Clean(context.Background(), backups, false); err == nil {
		t.Fatal("backups clean must refuse without force")
	}
	if err := Clean(context.Background(), backups, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "Library", "Application Support", "MobileSync", "Backup")); err == nil {
		t.Error("backup folder should be moved to the Trash (gone from its place)")
	}
	// Clean up the trashed copy so the test home disappears entirely.
	_ = os.RemoveAll(filepath.Join(root, ".Trash", "Backup"))
}
