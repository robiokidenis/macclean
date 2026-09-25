package scanner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"macclean/internal/fsutil"
)

// fixtureDir builds a scratch dir under the home directory: TMPDIR lives
// under /var, which CheckScannable rightly refuses.
func fixtureDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(home, ".macclean-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func buildFixture(t *testing.T) string {
	t.Helper()
	dir := fixtureDir(t)
	mk := func(rel string, size int) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("big.bin", 5000)
	mk("small.txt", 10)
	mk("sub/inner.bin", 3000)
	mk("sub/node_modules/pkg.bin", 9000) // excluded when requested
	mk("sub/.git/objects.bin", 7000)     // excluded when requested
	if err := os.Symlink(filepath.Join(dir, "big.bin"), filepath.Join(dir, "link.bin")); err != nil {
		t.Skipf("symlink failed: %v", err)
	}
	return dir
}

func TestScanRollup(t *testing.T) {
	dir := buildFixture(t)
	res, err := Scan(context.Background(), dir, Options{MinFileBytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	// total = 5000+10+3000+9000+7000 (symlink contributes 0)
	if res.TotalSize != 24010 {
		t.Fatalf("TotalSize = %d, want 24010", res.TotalSize)
	}
	if res.TotalFiles != 5 {
		t.Fatalf("TotalFiles = %d, want 5", res.TotalFiles)
	}
	if res.TotalDirs != 3 { // sub, node_modules, .git (root itself not counted)
		t.Fatalf("TotalDirs = %d, want 3", res.TotalDirs)
	}
	if len(res.Files) != 4 { // small.txt (10 B) falls below MinFileBytes
		t.Fatalf("retained files = %d, want 4", len(res.Files))
	}
	// sub must roll up 3000+9000+7000
	sub := res.Root.Find(filepath.Join(dir, "sub"))
	if sub == nil || sub.Size != 19000 {
		t.Fatalf("sub rollup wrong: %+v", sub)
	}
}

func TestScanExcludes(t *testing.T) {
	dir := buildFixture(t)
	res, err := Scan(context.Background(), dir, Options{MinFileBytes: 100, ExcludeNames: []string{"node_modules", ".git"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalSize != 8010 {
		t.Fatalf("TotalSize = %d, want 8010", res.TotalSize)
	}
}

func TestScanCancellation(t *testing.T) {
	dir := buildFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := Scan(ctx, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Cancelled {
		t.Fatal("expected cancelled")
	}
}

func TestProtectedPaths(t *testing.T) {
	for _, p := range []string{"/System", "/usr/bin", "/private/etc", "/bin"} {
		if err := CheckScannable(p); err == nil {
			t.Errorf("CheckScannable(%s) should refuse", p)
		}
	}
	for _, p := range []string{fsutil.Home(), "/Applications", "/Volumes/External"} {
		if err := CheckScannable(p); err != nil {
			t.Errorf("CheckScannable(%s) should allow: %v", p, err)
		}
	}
	// Deletion protection is stricter than scan protection.
	if !fsutil.IsProtected("/Applications/Safari.app") {
		t.Error("deleting from /Applications must be refused")
	}
	if fsutil.IsProtected(fsutil.Home() + "/Downloads") {
		t.Error("home must be deletable (to Trash)")
	}
	if err := fsutil.CheckDeletable("/System/Library"); err == nil {
		t.Error("CheckDeletable(/System/Library) must refuse")
	}
}

func TestScanEmptyDir(t *testing.T) {
	dir := fixtureDir(t)
	res, err := Scan(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalSize != 0 || res.TotalFiles != 0 {
		t.Fatalf("empty dir should be empty, got %+v", res)
	}
}
