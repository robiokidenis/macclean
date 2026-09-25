package duplicates

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"macclean/internal/fsutil"
)

// makeTree builds test files and returns their FileInfos.
func makeTree(t *testing.T) (dir string, files []fsutil.FileInfo) {
	t.Helper()
	dir = t.TempDir()
	w := func(rel string, content string) fsutil.FileInfo {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		fi, err := fsutil.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi
	}
	files = append(files,
		w("a.bin", "same-content-exactly"),
		w("sub/b.bin", "same-content-exactly"),        // duplicate of a.bin
		w("c.bin", "same-content-exactly-but-longer"), // partial-head match only
		w("d.bin", "different"),
		w("e.bin", "another-different"),
		w("small.txt", "x"), // below min size
		w("small2.txt", "x"),
	)
	return dir, files
}

func TestFindBasic(t *testing.T) {
	_, files := makeTree(t)
	res, err := Find(context.Background(), files, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Groups) != 1 {
		t.Fatalf("want 1 group, got %d: %+v", len(res.Groups), res.Groups)
	}
	g := res.Groups[0]
	if len(g.Files) != 2 {
		t.Fatalf("group should have 2 files, got %d", len(g.Files))
	}
	if g.Size != int64(len("same-content-exactly")) {
		t.Fatalf("unexpected size %d", g.Size)
	}
}

func TestFindCancelled(t *testing.T) {
	_, files := makeTree(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := Find(ctx, files, 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Cancelled {
		t.Fatal("expected cancelled result")
	}
}

func TestHardlinkNotDuplicate(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "one.bin")
	if err := os.WriteFile(p1, []byte("hardlinked-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	p2 := filepath.Join(dir, "two.bin")
	if err := os.Link(p1, p2); err != nil {
		t.Skipf("hardlink failed: %v", err)
	}
	var files []fsutil.FileInfo
	for _, p := range []string{p1, p2} {
		fi, err := fsutil.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, fi)
	}
	res, err := Find(context.Background(), files, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Groups) != 0 {
		t.Fatalf("hardlinks must not be reported as duplicates, got %d groups", len(res.Groups))
	}
}

func TestHeadTailPartial(t *testing.T) {
	// Two files with identical 64KB heads but different tails must NOT be
	// merged by the partial stage, and must not be confirmed as duplicates.
	dir := t.TempDir()
	head := make([]byte, 128*1024)
	for i := range head {
		head[i] = byte(i % 251)
	}
	a := append(append([]byte{}, head...), []byte("tail-A")...)
	b := append(append([]byte{}, head...), []byte("tail-B")...)
	pa, pb := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.WriteFile(pa, a, 0o644)
	os.WriteFile(pb, b, 0o644)
	var files []fsutil.FileInfo
	for _, p := range []string{pa, pb} {
		fi, err := fsutil.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, fi)
	}
	res, err := Find(context.Background(), files, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Groups) != 0 {
		t.Fatalf("head-identical files wrongly grouped: %+v", res.Groups)
	}
}
