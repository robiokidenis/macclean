// Package dashboard combines every scanner into one overview and — the
// important part — deduplicates overlapping claims.
//
// The same physical file can appear in Large Files, Old Files, Downloads,
// and Duplicates at once. Naively summing categories would promise the same
// bytes two, three, four times. Every category therefore emits per-file
// claims keyed by (device, inode); actual reclaimable space is the union of
// claimed bytes, so each physical file is counted exactly once.
package dashboard

import (
	"context"
	"sort"

	"macclean/internal/analysis"
	"macclean/internal/devcache"
	"macclean/internal/devdeps"
	"macclean/internal/duplicates"
	"macclean/internal/fsutil"
	"macclean/internal/macos"
	"macclean/internal/scanner"
	"macclean/internal/settings"
	"macclean/internal/sysinfo"
)

// Phase reports overall progress with a stable label.
type Phase struct {
	Label    string
	Fraction float64 // 0..1
	Done     bool
}

// Category is one dashboard row.
type Category struct {
	Name  string `json:"name"`
	Gross int64  `json:"gross"` // naive total of this category alone
	Actual int64 `json:"actual"` // bytes this category adds after overlap removal
	Items int    `json:"items"`
	Kind  string `json:"kind"` // devcache | duplicates | large | old | downloads
}

// Report is the combined overview.
type Report struct {
	Volume     sysinfo.Volume
	Categories []Category
	Gross      int64 `json:"gross"`
	Actual     int64 `json:"actual"`

	Caches    []devcache.Cache
	Deps      *devdeps.Report
	Macos     []macos.Item
	Downloads *analysis.DownloadsReport
	Dup       *duplicates.Result
	Large     []fsutil.FileInfo
	Old       []fsutil.FileInfo
}

// claim keys a physical file. Hardlinked paths collapse to one key.
type fileKey struct{ dev, ino uint64 }

type claimSet map[fileKey]int64

func addFile(c claimSet, f fsutil.FileInfo) {
	c[fileKey{f.Dev, f.Ino}] = f.Size
}

// Build runs all analyses. It never deletes anything; it answers "how much
// could I get back, honestly?".
func Build(ctx context.Context, s settings.Settings, prog func(Phase)) *Report {
	rep := &Report{}
	rep.Volume, _ = sysinfo.RootVolume()

	roots := analysis.ExistingUserRoots()
	steps := len(roots) + 3 // + duplicates + deps + dev caches
	stepW := 1.0 / float64(steps)

	var allFiles []fsutil.FileInfo
	for i, root := range roots {
		if ctx.Err() != nil {
			return rep
		}
		base := float64(i) * stepW
		minFile := int64(1 << 20) // retain >=1MB outside Downloads for speed
		if root == fsutil.ExpandPath("~/Downloads") {
			minFile = 0
		}
		res, err := scanner.Scan(ctx, root, scanner.Options{
			Concurrency:  s.Concurrency,
			ExcludeNames: s.Exclusions,
			MinFileBytes: minFile,
			Progress: func(p scanner.Progress) {
				if prog == nil {
					return
				}
				frac := 0.0
				if p.DirsDiscovered > 0 {
					frac = float64(p.DirsDone) / float64(p.DirsDiscovered)
				}
				prog(Phase{Label: "Scanning " + fsutil.DisplayPath(root), Fraction: base + frac*stepW})
			},
		})
		if err != nil || res.Cancelled {
			continue
		}
		allFiles = append(allFiles, res.Files...)
	}

	// Duplicates.
	dupBase := float64(len(roots)) * stepW
	dup, _ := duplicates.Find(ctx, allFiles, s.DupMinBytes, func(p duplicates.Progress) {
		if prog == nil {
			return
		}
		frac := 0.5
		if p.FilesTotal > 0 {
			frac = float64(p.FilesChecked) / float64(p.FilesTotal)
		}
		if p.Phase == "hashing (full)" {
			frac += 1.0
			frac /= 2.0
		}
		prog(Phase{Label: p.Phase + " duplicates", Fraction: dupBase + frac*stepW})
	})
	if ctx.Err() != nil {
		return rep
	}
	rep.Dup = dup

	// Stale project dependencies (node_modules, vendor, …): pruned walk,
	// sizes only the stale ones.
	depRoots := s.DepRoots
	if len(depRoots) == 0 {
		depRoots = devdeps.DefaultRoots()
	}
	if len(depRoots) > 0 {
		if prog != nil {
			prog(Phase{Label: "Scanning project dependencies", Fraction: dupBase + stepW})
		}
		rep.Deps = devdeps.Find(ctx, depRoots, devdeps.Options{StaleDays: s.DepStaleDays})
		if ctx.Err() != nil {
			return rep
		}
	}

	// General macOS targets (Trash, logs, app caches). The caches scan
	// excludes developer-managed subdirectories, so these rows cannot
	// double-count the dev cache rows below.
	if prog != nil {
		prog(Phase{Label: "Scanning macOS cleanup targets", Fraction: dupBase + stepW})
	}
	rep.Macos = macos.Scan(ctx, func(string) {})
	if ctx.Err() != nil {
		return rep
	}
	for _, it := range rep.Macos {
		if it.Exists && it.Size > 0 {
			name := it.Name
			if it.Kind == macos.KindCaches {
				name = "App caches"
			}
			rep.Categories = append(rep.Categories, Category{
				Name: name, Gross: it.Size, Actual: it.Size, Items: 1, Kind: "macos",
			})
		}
	}

	// Dev caches.
	if prog != nil {
		prog(Phase{Label: "Detecting developer caches", Fraction: dupBase + stepW})
	}
	rep.Caches = devcache.Detect(ctx, func(name string) {})

	// Derived views.
	downloadsRoot := fsutil.ExpandPath("~/Downloads")
	var downloadsFiles []fsutil.FileInfo
	for _, f := range allFiles {
		if len(f.Path) > len(downloadsRoot) && f.Path[:len(downloadsRoot)+1] == downloadsRoot+"/" {
			downloadsFiles = append(downloadsFiles, f)
		}
	}
	rep.Downloads = analysis.AnalyzeDownloads(downloadsRoot, downloadsFiles, s.LargeMinBytes, s.OldDays)
	rep.Large = analysis.LargeFiles(allFiles, s.LargeMinBytes)
	rep.Old = analysis.OldFiles(allFiles, s.OldDays)

	// ---- overlap-aware accounting ----
	seen := map[fileKey]string{} // key -> owning category (first claimant)
	claim := func(name, kind string, files []fsutil.FileInfo, naive int64, items int) Category {
		cat := Category{Name: name, Gross: naive, Actual: 0, Items: items, Kind: kind}
		local := claimSet{}
		for _, f := range files {
			addFile(local, f)
		}
		for k, bytes := range local {
			if _, taken := seen[k]; !taken {
				seen[k] = kind
				cat.Actual += bytes
			}
		}
		return cat
	}

	// Order fixes attribution when files overlap: duplicates are the most
	// specific answer, then downloads cleanup, large, old.
	var dupClaimFiles []fsutil.FileInfo
	if dup != nil {
		for _, g := range dup.Groups {
			keep := g.KeepIndex()
			for i, f := range g.Files {
				if i != keep {
					dupClaimFiles = append(dupClaimFiles, f)
				}
			}
		}
		rep.Categories = append(rep.Categories, claim("Duplicates", "duplicates", dupClaimFiles, dup.Reclaimable, len(dup.Groups)))
	}
	rep.Categories = append(rep.Categories, claim("Downloads cleanup", "downloads", rep.Downloads.CleanupPotentialSet, rep.Downloads.CleanupPotential, len(rep.Downloads.CleanupPotentialSet)))
	rep.Categories = append(rep.Categories, claim("Large files", "large", rep.Large, sumSizes(rep.Large), len(rep.Large)))
	rep.Categories = append(rep.Categories, claim("Old files", "old", rep.Old, sumSizes(rep.Old), len(rep.Old)))

	// Stale project dependencies and unused toolchain versions are whole
	// directories on their own paths; they cannot overlap the file claims
	// above, so they count 1:1.
	if rep.Deps != nil {
		var depBytes, toolBytes int64
		var depN, toolN int
		for _, e := range rep.Deps.Stale {
			if devdeps.IsToolchainKind(e.Kind) {
				toolBytes += e.Size
				toolN++
			} else {
				depBytes += e.Size
				depN++
			}
		}
		if depBytes > 0 {
			rep.Categories = append(rep.Categories, Category{
				Name: "Stale project deps", Gross: depBytes, Actual: depBytes,
				Items: depN, Kind: "deps",
			})
		}
		if toolBytes > 0 {
			rep.Categories = append(rep.Categories, Category{
				Name: "Unused toolchains", Gross: toolBytes, Actual: toolBytes,
				Items: toolN, Kind: "deps",
			})
		}
	}

	// Developer caches are separate physical trees; they cannot overlap the
	// file categories above, but sum them through the same machinery for
	// consistency (keyed by path, treated as unique keys).
	for _, c := range rep.Caches {
		if c.Exists && c.Size > 0 {
			rep.Categories = append(rep.Categories, Category{
				Name: c.Name, Gross: c.Size, Actual: c.Size, Items: 1, Kind: "devcache",
			})
		}
	}

	for _, c := range rep.Categories {
		rep.Gross += c.Gross
		rep.Actual += c.Actual
	}
	sort.Slice(rep.Categories, func(i, j int) bool { return rep.Categories[i].Actual > rep.Categories[j].Actual })

	if prog != nil {
		prog(Phase{Label: "done", Fraction: 1, Done: true})
	}
	return rep
}

func sumSizes(files []fsutil.FileInfo) int64 {
	var n int64
	for _, f := range files {
		n += f.Size
	}
	return n
}
