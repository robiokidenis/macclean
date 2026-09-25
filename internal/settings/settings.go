// Package settings persists user preferences to
// ~/.config/macclean/settings.json. Values here are defaults for the
// interactive screens; CLI flags override them.
package settings

import (
	"encoding/json"
	"os"
	"path/filepath"

	"macclean/internal/fsutil"
)

// Settings is the user-adjustable configuration.
type Settings struct {
	// Exclusions are directory names skipped during general analysis scans
	// (e.g. ".git", "node_modules"). Developer-artifact inspection ignores
	// this list when explicitly asked.
	Exclusions []string `json:"exclusions"`
	// LargeMinBytes is the default threshold for "large files" (default 1 GB).
	LargeMinBytes int64 `json:"largeMinBytes"`
	// OldDays is the default age threshold for "old files" (default 180).
	OldDays int `json:"oldDays"`
	// DupMinBytes is the minimum size considered by the duplicate finder
	// (default 1 MB).
	DupMinBytes int64 `json:"dupMinBytes"`
	// DupRoots are the directories searched for duplicates by default.
	DupRoots []string `json:"dupRoots"`
	// DupIncludeDev scans inside node_modules/vendor/Pods for duplicates.
	// Off by default: those duplicates are structural, and the safe
	// cleanup unit is the whole dependency folder (Stale Deps), not
	// individual files.
	DupIncludeDev bool `json:"dupIncludeDev"`
	// DepStaleDays: project dependency folders untouched for longer than
	// this are suggested for cleanup (default 90).
	DepStaleDays int `json:"depStaleDays"`
	// DepRoots are the directories searched for dependency folders.
	DepRoots []string `json:"depRoots"`
	// Concurrency caps scan workers (0 = auto).
	Concurrency int `json:"concurrency"`
}

// Defaults returns the built-in configuration.
func Defaults() Settings {
	return Settings{
		Exclusions:     []string{},
		LargeMinBytes:  1 << 30, // 1 GB
		OldDays:        180,
		DupMinBytes:    1 << 20, // 1 MB
		DupRoots:       defaultDupRoots(),
		DepStaleDays:   90,
		DepRoots:       defaultDepRoots(),
		Concurrency:    0,
	}
}

func defaultDepRoots() []string {
	var roots []string
	for _, d := range []string{"~/Projects", "~/Developer", "~/Documents"} {
		p := fsutil.ExpandPath(d)
		if st, err := os.Lstat(p); err == nil && st.IsDir() {
			roots = append(roots, p)
		}
	}
	return roots
}

func defaultDupRoots() []string {
	var roots []string
	for _, d := range []string{"~/Downloads", "~/Desktop", "~/Documents", "~/Movies", "~/Pictures"} {
		p := fsutil.ExpandPath(d)
		if st, err := os.Lstat(p); err == nil && st.IsDir() {
			roots = append(roots, p)
		}
	}
	return roots
}

// Path returns the settings file location.
func Path() string {
	return filepath.Join(fsutil.Home(), ".config", "macclean", "settings.json")
}

// Load reads settings, falling back to defaults (and merging them for
// missing fields).
func Load() Settings {
	s := Defaults()
	data, err := os.ReadFile(Path())
	if err != nil {
		return s
	}
	var onDisk Settings
	if json.Unmarshal(data, &onDisk) != nil {
		return s
	}
	if onDisk.Exclusions != nil {
		s.Exclusions = onDisk.Exclusions
	}
	if onDisk.LargeMinBytes > 0 {
		s.LargeMinBytes = onDisk.LargeMinBytes
	}
	if onDisk.OldDays > 0 {
		s.OldDays = onDisk.OldDays
	}
	if onDisk.DupMinBytes > 0 {
		s.DupMinBytes = onDisk.DupMinBytes
	}
	if onDisk.DupRoots != nil {
		s.DupRoots = onDisk.DupRoots
	}
	s.DupIncludeDev = onDisk.DupIncludeDev
	if onDisk.DepStaleDays > 0 {
		s.DepStaleDays = onDisk.DepStaleDays
	}
	if onDisk.DepRoots != nil {
		s.DepRoots = onDisk.DepRoots
	}
	s.Concurrency = onDisk.Concurrency
	return s
}

// Save persists settings, creating the directory if needed.
func Save(s Settings) error {
	p := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}
