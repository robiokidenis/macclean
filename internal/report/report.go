// Package report renders analysis results as terminal text or JSON, and
// caches the last successful result of each command so an immediate re-run
// is instant (honest caching: the output is stamped with its age).
package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"macclean/internal/analysis"
	"macclean/internal/duplicates"
	"macclean/internal/fsutil"
	"macclean/internal/scanner"
	"macclean/internal/sysinfo"
	"macclean/internal/units"
)

// Out collects lines for terminal rendering.
type Out struct {
	lines []string
}

func (o *Out) Line(format string, args ...any)   { o.lines = append(o.lines, fmt.Sprintf(format, args...)) }
func (o *Out) Raw(s string)                      { o.lines = append(o.lines, s) }
func (o *Out) String() string                    { return strings.Join(o.lines, "\n") }

// PrintDiskHeader writes the volume banner used by every report.
func PrintDiskHeader(o *Out) {
	vol, err := sysinfo.RootVolume()
	if err != nil {
		return
	}
	o.Raw("MacClean — Disk Usage")
	o.Raw("")
	o.Line("%s", vol.Name)
	o.Line("%s used · %s free", units.Format(int64(vol.UsedBytes())), units.Format(int64(vol.FreeBytes)))
	o.Raw("")
}

// FolderReport prints the largest subdirectories and files of a scan root.
// files is the scan's retained flat list; files below root are matched by
// path prefix so nested results (e.g. project-a/.next/cache) appear.
func FolderReport(o *Out, root *scanner.DirNode, files []fsutil.FileInfo, topN int) {
	o.Line("%s", fsutil.DisplayPath(root.Path))
	o.Raw("")
	o.Line("  %s  total (%s files, %s dirs)", units.Format(root.Size), humanInt(root.FileCount), humanInt(root.DirCount))
	o.Raw("")
	subs := root.SortedChildren()
	o.Raw("Largest folders")
	n := topN
	if len(subs) < n {
		n = len(subs)
	}
	for _, c := range subs[:n] {
		o.Line("  %8s  %s", units.Format(c.Size), c.Name)
	}
	if len(subs) > n {
		o.Line("  … %d more folders", len(subs)-n)
	}
	// Deepest-N largest retained files anywhere under root.
	prefix := strings.TrimSuffix(root.Path, "/") + "/"
	var nested []fsutil.FileInfo
	for _, f := range files {
		if strings.HasPrefix(f.Path, prefix) {
			nested = append(nested, f)
		}
	}
	sort.Slice(nested, func(i, j int) bool { return nested[i].Size > nested[j].Size })
	if len(nested) > topN {
		nested = nested[:topN]
	}
	if len(nested) > 0 {
		o.Raw("")
		o.Raw("Largest files")
		for _, f := range nested {
			o.Line("  %8s  %s", units.Format(f.Size), relTo(root.Path, f.Path))
		}
	}
}

func relTo(root, path string) string {
	r := strings.TrimSuffix(root, "/")
	if strings.HasPrefix(path, r+"/") {
		return path[len(r)+1:]
	}
	return path
}

func humanInt(n int64) string {
	s := fmt.Sprintf("%d", n)
	// thin thousands grouping for readability
	if len(s) > 3 {
		var out []string
		for len(s) > 3 {
			out = append([]string{s[len(s)-3:]}, out...)
			s = s[:len(s)-3]
		}
		out = append([]string{s}, out...)
		return strings.Join(out, ",")
	}
	return s
}

// FileListReport prints a size/age/path table.
func FileListReport(o *Out, title string, files []fsutil.FileInfo, total int64) {
	o.Raw(title)
	o.Raw("")
	for _, f := range files {
		flag := ""
		if fsutil.IsDatabaseFile(f.Path) {
			flag = "  ⚠ database"
		}
		if _, err := os.Lstat(f.Path); err != nil {
			o.Line("  %8s  %s  %s  · deleted", units.Format(f.Size), units.Age(time.Unix(f.ModTime, 0)), fsutil.DisplayPath(f.Path))
			continue
		}
		o.Line("  %8s  %s  %s%s", units.Format(f.Size), units.Age(time.Unix(f.ModTime, 0)), fsutil.DisplayPath(f.Path), flag)
	}
	o.Raw("")
	o.Line("Total: %s (%s files)", units.Format(total), humanInt(int64(len(files))))
}

// DownloadsReport prints the Downloads summary.
func DownloadsReport(o *Out, rep *analysis.DownloadsReport) {
	o.Raw("Downloads")
	o.Raw("")
	o.Line("  %s total · %s files", units.Format(rep.Total), humanInt(int64(len(rep.Files))))
	o.Raw("")
	for _, c := range rep.Categories {
		o.Line("  %-12s %8s  (%d files)", c.Name, units.Format(c.Size), len(c.Files))
	}
	o.Raw("")
	o.Line("Potential cleanup: %s  (installers, archives, disk images, files older than cutoff)", units.Format(rep.CleanupPotential))
}

// DuplicatesReport prints duplicate groups.
func DuplicatesReport(o *Out, res *duplicates.Result) {
	o.Raw("Duplicate Files")
	o.Raw("")
	if len(res.Groups) == 0 {
		o.Raw("No duplicates found.")
		return
	}
	o.Line("Found: %s reclaimable across %d groups", units.Format(res.Reclaimable), len(res.Groups))
	o.Raw("")
	for i, g := range res.Groups {
		o.Line("Group #%d  %s each · reclaimable %s", i+1, units.Format(g.Size), units.Format(g.Reclaimable()))
		dbN := 0
		for _, f := range g.Files {
			flag := ""
			switch {
			case fsutil.IsDatabaseFile(f.Path):
				flag = "  ⚠ database"
				dbN++
			default:
				if _, err := os.Lstat(f.Path); err != nil {
					flag = "  · deleted"
				}
			}
			o.Line("    %s%s", fsutil.DisplayPath(f.Path), flag)
		}
		if dbN > 0 {
			o.Line("    ⚠ %d database file(s) — often intentional backups; verify before removing", dbN)
		}
		o.Raw("")
	}
}

// ---- JSON shapes ----

// JSONFile is one file in JSON output.
type JSONFile struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	ModTime  int64  `json:"mtime"`
	ModHuman string `json:"mtimeHuman,omitempty"`
}

func toJSONFiles(files []fsutil.FileInfo) []JSONFile {
	out := make([]JSONFile, 0, len(files))
	for _, f := range files {
		out = append(out, JSONFile{
			Path: f.Path, Size: f.Size, ModTime: f.ModTime,
			ModHuman: time.Unix(f.ModTime, 0).UTC().Format(time.RFC3339),
		})
	}
	return out
}

// ScanJSON is the shape emitted by `macclean scan --json`.
type ScanJSON struct {
	Path       string         `json:"path"`
	TotalSize  int64          `json:"totalSize"`
	Files      int64          `json:"files"`
	Dirs       int64          `json:"dirs"`
	ElapsedMs  int64          `json:"elapsedMs"`
	LargeFiles []JSONFile     `json:"largeFiles"`
	OldFiles   []JSONFile     `json:"oldFiles"`
	Duplicates []JSONGroup    `json:"duplicates"`
	Skipped    []scanner.Error `json:"skipped,omitempty"`
}

// JSONGroup is one duplicate group in JSON output.
type JSONGroup struct {
	Size        int64      `json:"size"`
	Reclaimable int64      `json:"reclaimable"`
	Files       []JSONFile `json:"files"`
}

// BuildScanJSON assembles the combined script-friendly output.
func BuildScanJSON(root string, res *scanner.Result, large, old []fsutil.FileInfo, dup *duplicates.Result, largeMin int64, oldDays int) ScanJSON {
	out := ScanJSON{
		Path:       fsutil.DisplayPath(root),
		TotalSize:  res.TotalSize,
		Files:      res.TotalFiles,
		Dirs:       res.TotalDirs,
		ElapsedMs:  res.Elapsed.Milliseconds(),
		LargeFiles: toJSONFiles(large),
		OldFiles:   toJSONFiles(old),
		Skipped:    res.Skipped,
	}
	if dup != nil {
		for _, g := range dup.Groups {
			out.Duplicates = append(out.Duplicates, JSONGroup{
				Size: g.Size, Reclaimable: g.Reclaimable(), Files: toJSONFiles(g.Files),
			})
		}
	}
	return out
}

// EmitJSON prints v indented.
func EmitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ---- last-result cache ----

const cacheTTL = 15 * time.Minute

// CacheMeta wraps a cached payload with its creation time.
type CacheMeta struct {
	SavedAt time.Time   `json:"savedAt"`
	Payload json.RawMessage `json:"payload"`
}

func cachePath(key string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, key)
	return filepath.Join(fsutil.Home(), ".cache", "macclean", safe+".json")
}

// SaveCache stores a command result under key.
func SaveCache(key string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	p := cachePath(key)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	meta := CacheMeta{SavedAt: time.Now(), Payload: data}
	blob, _ := json.Marshal(meta)
	_ = os.WriteFile(p, blob, 0o644)
}

// LoadCache returns the fresh cached payload for key, or nil.
func LoadCache(key string) *CacheMeta {
	data, err := os.ReadFile(cachePath(key))
	if err != nil {
		return nil
	}
	var meta CacheMeta
	if json.Unmarshal(data, &meta) != nil {
		return nil
	}
	if time.Since(meta.SavedAt) > cacheTTL {
		return nil
	}
	return &meta
}

// InvalidateCache drops all cached scan results. Called after anything is
// trashed or cleaned, so a rerun never resurrects deleted paths from a
// cached file list.
func InvalidateCache() {
	_ = os.RemoveAll(filepath.Join(fsutil.Home(), ".cache", "macclean"))
}
