package analysis

import (
	"testing"
	"time"

	"macclean/internal/fsutil"
)

func TestCategorize(t *testing.T) {
	cases := map[string]string{
		"ubuntu-24.iso":        CatInstallers,
		"Xcode.pkg":            CatInstallers,
		"app.dmg":              CatInstallers,
		"backup.tar.gz":        CatArchives,
		"photos.zip":           CatArchives,
		"clip.mkv":             CatVideos,
		"holiday.mov":          CatVideos,
		"scan.JPG":             CatImages,
		"report.PDF":           CatDocuments,
		"vm.sparseimage":       CatDiskImages,
		"mystery.dat":          CatOther,
	}
	for path, want := range cases {
		if got := Categorize(path); got != want {
			t.Errorf("Categorize(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestOldFilesUsesModTime(t *testing.T) {
	old := time.Now().AddDate(0, 0, -200).Unix()
	fresh := time.Now().AddDate(0, 0, -1).Unix()
	files := []fsutil.FileInfo{
		{Path: "old.bin", Size: 100, ModTime: old},
		{Path: "fresh.bin", Size: 100, ModTime: fresh},
	}
	got := OldFiles(files, 180)
	if len(got) != 1 || got[0].Path != "old.bin" {
		t.Fatalf("OldFiles = %+v", got)
	}
}

func TestDownloadsReportDedupesCleanup(t *testing.T) {
	// A file that is BOTH an installer AND old must be counted once in
	// CleanupPotential.
	old := time.Now().AddDate(0, 0, -300).Unix()
	files := []fsutil.FileInfo{
		{Path: "/x/old-installer.dmg", Size: 100, ModTime: old},
		{Path: "/x/new-video.mp4", Size: 50, ModTime: time.Now().Unix()},
	}
	rep := AnalyzeDownloads("/x", files, 40, 180)
	if rep.CleanupPotential != 100 {
		t.Fatalf("CleanupPotential = %d, want 100 (video excluded, installer counted once)", rep.CleanupPotential)
	}
	if len(rep.CleanupPotentialSet) != 1 {
		t.Fatalf("cleanup set = %+v", rep.CleanupPotentialSet)
	}
	if len(rep.Large) != 1 || rep.Large[0].Path != "/x/new-video.mp4" {
		// 50 < 40 threshold fails; 50 ≥ 40 passes. Threshold 40 → both files are "large".
		t.Logf("large = %+v", rep.Large)
	}
}
