// Package macos scans and cleans general macOS junk that is safe to remove:
// the Trash, user logs, application caches, and (with heavy confirmation)
// iOS device backups.
//
// Boundaries, deliberately conservative:
//   - Only USER areas under ~ are touched: ~/.Trash, ~/Library/Logs,
//     ~/Library/Caches. /System, /private/var/log and friends are never
//     scanned (the scanner refuses them anyway).
//   - ~/Library/Caches subdirectories already managed by the developer
//     cache cleaners (go-build, pip, Homebrew, …) are excluded here so
//     they are neither double-counted nor cleaned by two different paths.
//   - Nothing is deleted without confirmation; class semantics match the
//     rest of MacClean: safe = regenerable, destructive = typed "yes".
package macos

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"macclean/internal/devcache"
	"macclean/internal/fsutil"
	"macclean/internal/scanner"
)

// Item kinds.
const (
	KindTrash    = "trash"
	KindLogs     = "logs"
	KindCaches   = "caches"
	KindBackups  = "ios-backups"
)

// Safety classes (same vocabulary as devcache).
const (
	Safe        = devcache.Safe
	TrashOnly   = devcache.TrashOnly
	Destructive = devcache.Destructive
)

// Item is one general-macOS cleanup target.
type Item struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Safety string `json:"safety"`
	Detail string `json:"detail,omitempty"`
	Exists bool   `json:"exists"`
}

// ManagedCacheNames lists ~/Library/Caches subdirectories owned by the
// developer cache cleaners, derived from their detector table.
func ManagedCacheNames() []string {
	prefix := fsutil.ExpandPath("~/Library/Caches") + "/"
	var names []string
	for _, d := range devcache.Detectors {
		p := fsutil.ExpandPath(d.Dir)
		if strings.HasPrefix(p, prefix) {
			names = append(names, filepath.Base(p))
		}
	}
	// Homebrew's cache path is resolved via the brew binary at runtime and
	// always lives under ~/Library/Caches/Homebrew.
	names = append(names, "Homebrew")
	return names
}

func sizeOf(ctx context.Context, path string, excludes []string) int64 {
	res, err := scanner.Scan(ctx, path, scanner.Options{
		Concurrency:  4,
		MinFileBytes: 1 << 62, // retain no per-file data; totals only
		ExcludeNames: excludes,
	})
	if err != nil || res.Cancelled {
		return 0
	}
	return res.TotalSize
}

// Scan measures every general cleanup target. Each item is independent;
// progress receives one line per item.
func Scan(ctx context.Context, progress func(string)) []Item {
	home := fsutil.Home()
	items := []Item{
		{Name: "Trash", Kind: KindTrash, Path: filepath.Join(home, ".Trash"),
			Safety: Destructive,
			Detail: "Emptying the Trash is permanent — everything currently in it is destroyed."},
		{Name: "User logs", Kind: KindLogs, Path: filepath.Join(home, "Library", "Logs"),
			Safety: Safe,
			Detail: "App and system-user logs; recreated as needed."},
		{Name: "App caches", Kind: KindCaches, Path: filepath.Join(home, "Library", "Caches"),
			Safety: Safe,
			Detail: "Application caches (developer tool caches are managed separately and skipped here)."},
		{Name: "iOS device backups", Kind: KindBackups,
			Path:   filepath.Join(home, "Library", "Application Support", "MobileSync", "Backup"),
			Safety: Destructive,
			Detail: "iTunes/Finder device backups; moved to the Trash, but they cannot be regenerated."},
	}
	managed := ManagedCacheNames()
	for i := range items {
		if ctx.Err() != nil {
			break
		}
		if progress != nil {
			progress("Measuring " + items[i].Name)
		}
		if st, err := fsutil.Lstat(items[i].Path); err != nil || !st.IsDir() {
			continue
		}
		items[i].Exists = true
		excludes := []string(nil)
		if items[i].Kind == KindCaches {
			excludes = managed
		}
		items[i].Size = sizeOf(ctx, items[i].Path, excludes)
	}
	return items
}

// Clean executes one item's cleanup. force must be true for Destructive
// items; the caller is responsible for having collected the confirmation.
func Clean(ctx context.Context, it Item, force bool) error {
	if !it.Exists {
		return fmt.Errorf("%s: nothing detected", it.Name)
	}
	if it.Safety == Destructive && !force {
		return fmt.Errorf("%s needs explicit confirmation", it.Name)
	}
	if err := fsutil.CheckDeletable(it.Path); err != nil {
		return err
	}
	switch it.Kind {
	case KindTrash:
		return removeContents(it.Path)
	case KindLogs:
		return removeContents(it.Path)
	case KindCaches:
		return cleanCaches(it.Path, ManagedCacheNames())
	case KindBackups:
		return fsutil.MoveToTrash(it.Path)
	}
	return fmt.Errorf("unknown kind %q", it.Kind)
}

// removeContents deletes every child of dir but keeps the directory
// itself, so apps (and macOS) keep a valid ~/.Trash / ~/Library/Logs.
func removeContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var firstErr error
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// cleanCaches removes every unmanaged child of the caches directory.
func cleanCaches(dir string, managed []string) error {
	skip := map[string]bool{}
	for _, m := range managed {
		skip[m] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var firstErr error
	for _, e := range entries {
		if skip[e.Name()] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
