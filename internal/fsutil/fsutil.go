// Package fsutil holds macOS-specific file helpers: protected path checks,
// Trash moves, Finder integration, and stat metadata.
package fsutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// ProtectedRoots are never scanned or touched by any cleanup action.
var ProtectedRoots = []string{
	"/System",
	"/private",
	"/bin",
	"/sbin",
	"/usr",
	"/var",
	"/Applications",
	"/Library",
	"/Users", // individual homes are fine; the container dir itself is not a scan target
	"/Volumes",
	"/dev",
	"/proc",
	"/opt",
	"/etc",
	"/tmp",
}

// Home returns the current user's home directory, resolved per call so
// tests can redirect it via $HOME.
func Home() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "/Users/unknown"
}

// ExpandPath turns "~/..." into an absolute path and cleans it.
func ExpandPath(p string) string {
	if p == "~" {
		return Home()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(Home(), p[2:])
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return abs
}

// DisplayPath abbreviates the home directory back to "~" for compact output.
func DisplayPath(p string) string {
	home := Home()
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}

// IsProtected reports whether a path is inside (or equal to) a system
// location that MacClean refuses to operate on. The user's own home is
// allowed; system homes are not.
func IsProtected(path string) bool {
	abs := ExpandPath(path)
	if strings.HasPrefix(abs, Home()+"/") || abs == Home() {
		return false
	}
	for _, root := range ProtectedRoots {
		if abs == root || strings.HasPrefix(abs, root+"/") {
			return true
		}
	}
	return false
}

// ProtectionError explains a refused operation.
type ProtectionError struct{ Path string }

func (e *ProtectionError) Error() string {
	return fmt.Sprintf("%s is a protected system location; refusing to operate on it", e.Path)
}

// CheckDeletable returns an error if a path must not be deleted, checked
// before every trash/rm/remove action.
func CheckDeletable(path string) error {
	if IsProtected(path) {
		return &ProtectionError{Path: path}
	}
	if ExpandPath(path) == "/" {
		return &ProtectionError{Path: "/"}
	}
	return nil
}

// MoveToTrash moves a file or directory into the user's Trash by renaming it
// into ~/.Trash, degrading to an osascript Finder delete when the rename
// fails (e.g. across volumes). Nothing is ever unlinked directly.
func MoveToTrash(path string) error {
	abs := ExpandPath(path)
	if err := CheckDeletable(abs); err != nil {
		return err
	}
	if _, err := os.Lstat(abs); err != nil {
		return fmt.Errorf("cannot trash %s: %w", abs, err)
	}
	trash := filepath.Join(Home(), ".Trash")
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(trash, filepath.Base(abs))
	if _, err := os.Lstat(dst); err == nil {
		dst = filepath.Join(trash, uniqueName(filepath.Base(abs)))
	}
	if err := os.Rename(abs, dst); err == nil {
		return nil
	}
	// Cross-volume or permission-blocked rename: let Finder do the move.
	return trashViaFinder(abs)
}

func trashViaFinder(abs string) error {
	script := fmt.Sprintf(`tell application "Finder" to delete POSIX file %q`, abs)
	cmd := exec.Command("osascript", "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("Finder trash failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func uniqueName(base string) string {
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s (copy %d)%s", stem, i, ext)
		if _, err := os.Lstat(filepath.Join(Home(), ".Trash", candidate)); err != nil {
			return candidate
		}
	}
}

// TrashPath returns the absolute path a file would land at in the Trash.
func TrashPath(path string) string {
	return filepath.Join(Home(), ".Trash", filepath.Base(ExpandPath(path)))
}

// dbExtensions are database-related file suffixes. "Duplicates" of these
// are frequently intentional backups, and live database files must never
// be touched lightly — cleanups flag them for extra attention instead of
// treating them like ordinary files.
var dbExtensions = map[string]bool{
	".sql": true, ".dump": true,
	".db": true, ".sqlite": true, ".sqlite3": true, ".db3": true,
	".sqlite-wal": true, ".sqlite-shm": true,
	".mdb": true, ".accdb": true, // MS Access
	".ibd": true, ".frm": true, ".myd": true, ".myi": true, // MySQL
	".trg": true, ".trn": true, // MySQL triggers
	".bson": true,              // MongoDB
	".rdb": true, ".aof": true, // Redis
}

// IsDatabaseFile reports whether a path looks like a database file.
func IsDatabaseFile(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	for ext := range dbExtensions {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// RevealInFinder opens a Finder window with the path selected.
func RevealInFinder(path string) error {
	return exec.Command("open", "-R", ExpandPath(path)).Run()
}

// Open opens a file or directory with its default application.
func Open(path string) error {
	return exec.Command("open", ExpandPath(path)).Run()
}

// FileInfo carries the identity fields analyses need from lstat.
type FileInfo struct {
	Path    string
	Size    int64
	ModTime int64 // unix seconds
	Dev     uint64
	Ino     uint64
	Mode    uint16 // raw syscall mode bits
}

// IsDir reports whether the stat described a directory.
func (fi FileInfo) IsDir() bool { return fi.Mode&syscall.S_IFMT == syscall.S_IFDIR }

// IsSymlink reports whether the stat described a symbolic link.
func (fi FileInfo) IsSymlink() bool { return fi.Mode&syscall.S_IFMT == syscall.S_IFLNK }

// Lstat fills a FileInfo using lstat (never follows symlinks).
func Lstat(path string) (FileInfo, error) {
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return FileInfo{}, err
	}
	return FileInfo{
		Path:    path,
		Size:    st.Size,
		ModTime: st.Mtimespec.Sec,
		Dev:     uint64(st.Dev),
		Ino:     uint64(st.Ino),
		Mode:    st.Mode,
	}, nil
}
