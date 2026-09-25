package fsutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExpandDisplay(t *testing.T) {
	p := ExpandPath("~/Downloads")
	if !strings.HasSuffix(p, "/Downloads") || strings.Contains(p, "~") {
		t.Fatalf("ExpandPath = %q", p)
	}
	if DisplayPath(p) != "~/Downloads" {
		t.Fatalf("DisplayPath = %q", DisplayPath(p))
	}
}

func TestMoveToTrashAndRefusals(t *testing.T) {
	// Create a uniquely-named file under home; trash it; verify it is in
	// ~/.Trash; then clean it up from the Trash to stay tidy.
	src := filepath.Join(Home(), ".macclean-trash-test-42.txt")
	if err := os.WriteFile(src, []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(src)
	if err := MoveToTrash(src); err != nil {
		t.Fatalf("MoveToTrash: %v", err)
	}
	if _, err := os.Lstat(src); !os.IsNotExist(err) {
		t.Fatal("source should be gone")
	}
	inTrash := TrashPath(src)
	if _, err := os.Lstat(inTrash); err != nil {
		t.Fatalf("file should be in Trash at %s: %v", inTrash, err)
	}
	os.Remove(inTrash)

	// Protected locations must be refused.
	if err := MoveToTrash("/System/Library"); err == nil {
		t.Fatal("trash /System must be refused")
	}
	if err := MoveToTrash("/Applications"); err == nil {
		t.Fatal("trash /Applications must be refused")
	}
	if err := MoveToTrash("/"); err == nil {
		t.Fatal("trash / must be refused")
	}
}

func TestCollisionRename(t *testing.T) {
	base := filepath.Join(Home(), ".macclean-collision-test.txt")
	os.WriteFile(base, []byte("a"), 0o644)
	defer os.Remove(base)
	name := uniqueName(filepath.Base(base))
	if name == filepath.Base(base) {
		t.Fatalf("collision name should differ: %q", name)
	}
	// Clean any leftovers from a previous run.
	os.Remove(filepath.Join(Home(), ".Trash", name))
}
