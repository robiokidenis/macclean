// Package analysis derives human-meaningful answers from scan results:
// which folders are huge, which downloads are removable, which files are
// old or large.
package analysis

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"macclean/internal/fsutil"
)

// UserRoots are the well-known user directories the folder analyzer offers.
var UserRoots = []string{
	"~/Desktop", "~/Downloads", "~/Documents", "~/Movies",
	"~/Pictures", "~/Library", "~/Developer", "~/Projects",
}

// ExistingUserRoots returns the UserRoots that exist on this machine.
func ExistingUserRoots() []string {
	var out []string
	for _, r := range UserRoots {
		p := fsutil.ExpandPath(r)
		if st, err := fsutil.Lstat(p); err == nil && st.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

// LargeFiles returns files at or above minBytes, largest first.
func LargeFiles(files []fsutil.FileInfo, minBytes int64) []fsutil.FileInfo {
	var out []fsutil.FileInfo
	for _, f := range files {
		if f.Size >= minBytes {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	return out
}

// OldFiles returns files whose modification time is at least days old,
// oldest first. Modification time is used deliberately: access time is
// unreliable or disabled on modern macOS.
func OldFiles(files []fsutil.FileInfo, days int) []fsutil.FileInfo {
	cutoff := time.Now().AddDate(0, 0, -days).Unix()
	var out []fsutil.FileInfo
	for _, f := range files {
		if f.ModTime <= cutoff {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime < out[j].ModTime })
	return out
}

// Downloads category identifiers.
const (
	CatInstallers = "Installers"
	CatArchives   = "Archives"
	CatVideos     = "Videos"
	CatImages     = "Images"
	CatDocuments  = "Documents"
	CatDiskImages = "Disk images"
	CatOther      = "Other"
)

var categoryExts = map[string][]string{
	CatInstallers: {".dmg", ".pkg", ".mpkg", ".iso", ".exe", ".apk", ".app"},
	CatArchives:   {".zip", ".tar", ".gz", ".tgz", ".bz2", ".xz", ".7z", ".rar", ".zst"},
	CatVideos:     {".mp4", ".mov", ".avi", ".mkv", ".webm", ".m4v", ".mpg", ".mpeg"},
	CatImages:     {".jpg", ".jpeg", ".png", ".gif", ".heic", ".webp", ".tiff", ".tif", ".svg", ".raw", ".cr2", ".nef"},
	CatDocuments:  {".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx", ".txt", ".md", ".rtf", ".csv", ".pages", ".key", ".numbers", ".epub"},
	CatDiskImages: {".sparseimage", ".sparsebundle", ".img", ".vmdk", ".qcow2", ".vhd"},
}

var extLookup = buildExtLookup()

func buildExtLookup() map[string]string {
	m := map[string]string{}
	for cat, exts := range categoryExts {
		for _, e := range exts {
			m[strings.ToLower(e)] = cat
		}
	}
	return m
}

// Categorize classifies a download by extension. Installers and archives
// take precedence over media types because that is how people clean up.
func Categorize(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if cat, ok := extLookup[ext]; ok {
		return cat
	}
	return CatOther
}

// DownloadsCategory is one grouping in the Downloads report.
type DownloadsCategory struct {
	Name  string
	Files []fsutil.FileInfo
	Size  int64
}

// DownloadsReport summarizes ~/Downloads for cleanup decisions.
type DownloadsReport struct {
	Root  string
	Total int64
	Files []fsutil.FileInfo

	Categories []DownloadsCategory

	Large []fsutil.FileInfo // above LargeMinBytes
	Old   []fsutil.FileInfo // older than OldDays

	// CleanupPotential sums installers, archives, disk images, and files
	// older than OldDays — each file counted once.
	CleanupPotential    int64
	CleanupPotentialSet []fsutil.FileInfo
}

// AnalyzeDownloads groups a Downloads scan. Nothing is ever deleted
// automatically; this report only informs.
func AnalyzeDownloads(root string, files []fsutil.FileInfo, largeMinBytes int64, oldDays int) *DownloadsReport {
	rep := &DownloadsReport{Root: root}
	catIndex := map[string]int{}
	oldCutoff := time.Now().AddDate(0, 0, -oldDays).Unix()
	cleanupSeen := map[string]bool{}

	for _, f := range files {
		rep.Total += f.Size
		cat := Categorize(f.Path)
		idx, ok := catIndex[cat]
		if !ok {
			rep.Categories = append(rep.Categories, DownloadsCategory{Name: cat})
			idx = len(rep.Categories) - 1
			catIndex[cat] = idx
		}
		rep.Categories[idx].Files = append(rep.Categories[idx].Files, f)
		rep.Categories[idx].Size += f.Size

		if f.Size >= largeMinBytes {
			rep.Large = append(rep.Large, f)
		}
		if f.ModTime <= oldCutoff {
			rep.Old = append(rep.Old, f)
		}
		if cat == CatInstallers || cat == CatArchives || cat == CatDiskImages || f.ModTime <= oldCutoff {
			if !cleanupSeen[f.Path] {
				cleanupSeen[f.Path] = true
				rep.CleanupPotential += f.Size
				rep.CleanupPotentialSet = append(rep.CleanupPotentialSet, f)
			}
		}
	}
	rep.Files = files
	sort.Slice(rep.Categories, func(i, j int) bool { return rep.Categories[i].Size > rep.Categories[j].Size })
	sort.Slice(rep.Large, func(i, j int) bool { return rep.Large[i].Size > rep.Large[j].Size })
	sort.Slice(rep.Old, func(i, j int) bool { return rep.Old[i].ModTime < rep.Old[j].ModTime })
	return rep
}
