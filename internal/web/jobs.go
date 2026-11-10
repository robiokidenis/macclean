package web

// Background scan jobs. Disk walks take minutes, so the browser starts a
// job and polls /api/jobs/:id for progress. One heavy scan runs at a time;
// further jobs queue instead of thrashing the disk.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"macclean/internal/analysis"
	"macclean/internal/dashboard"
	"macclean/internal/devcache"
	"macclean/internal/devdeps"
	"macclean/internal/duplicates"
	"macclean/internal/fsutil"
	"macclean/internal/macos"
	"macclean/internal/report"
	"macclean/internal/scanner"
	"macclean/internal/units"
)

// Job kinds the web UI can start.
const (
	kindDashboard  = "dashboard"
	kindAnalyze    = "analyze"
	kindDuplicates = "duplicates"
	kindDeps       = "deps"
	kindDownloads  = "downloads"
)

var jobKinds = map[string]bool{
	kindDashboard: true, kindAnalyze: true, kindDuplicates: true,
	kindDeps: true, kindDownloads: true,
}

// job is one running or finished scan. Fields exposed to the browser are
// JSON-tagged; mu guards the mutable ones below.
type job struct {
	ID       string  `json:"id"`
	Kind     string  `json:"kind"`
	Label    string  `json:"label"`
	Fraction float64 `json:"fraction"`
	Queued   bool    `json:"queued"`
	Done     bool    `json:"done"`
	Error    string  `json:"error,omitempty"`

	mu       sync.Mutex
	doneAt   time.Time
	result   any             // JSON payload, set on success
	scanRoot *scanner.Result // analyze only: whole tree for drill-down
	details  map[string]any  // dashboard only: per-category drill-down payloads
}

// setDetails stores dashboard drill-down payloads (call before finish).
func (j *job) setDetails(d map[string]any) {
	j.mu.Lock()
	j.details = d
	j.mu.Unlock()
}

func (j *job) progress(label string, frac float64) {
	j.mu.Lock()
	j.Label, j.Fraction = label, frac
	j.mu.Unlock()
}

func (j *job) finish(result any, scanRoot *scanner.Result) {
	j.mu.Lock()
	j.result = result
	j.scanRoot = scanRoot
	j.Done = true
	j.Queued = false
	j.Fraction = 1
	j.Label = "done"
	j.doneAt = time.Now()
	j.mu.Unlock()
}

func (j *job) fail(err error) {
	j.mu.Lock()
	j.Error = err.Error()
	j.Done = true
	j.Queued = false
	j.mu.Unlock()
}

// snapshot returns job state as JSON. The result payload is only included
// when includeResult and the job succeeded.
func (j *job) snapshot(includeResult bool) map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	m := map[string]any{
		"id": j.ID, "kind": j.Kind, "label": j.Label, "fraction": j.Fraction,
		"queued": j.Queued, "done": j.Done,
	}
	if j.Error != "" {
		m["error"] = j.Error
	}
	if includeResult && j.Done && j.result != nil {
		m["result"] = j.result
	}
	return m
}

// tree serves one directory of a finished analyze job for drill-down.
func (j *job) tree(path string) (*treeDTO, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.scanRoot == nil || j.scanRoot.Root == nil {
		return nil, false
	}
	root := j.scanRoot.Root
	if path == "" {
		path = root.Path
	}
	node := root.Find(path)
	if node == nil {
		return nil, false
	}
	return buildTreeDTO(root, node), true
}

// detailOf serves the drill-down payload for one dashboard category.
// name disambiguates per-item categories (a single macOS target or cache).
func (j *job) detailOf(kind, name string) (any, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	switch kind {
	case "macos":
		items, _ := j.details[kind].([]macos.Item)
		return filterMacos(items, name)
	case "devcache":
		caches, _ := j.details[kind].([]devcache.Cache)
		return filterCaches(caches, name)
	default:
		dto, ok := j.details[kind]
		return dto, ok
	}
}

func filterMacos(items []macos.Item, name string) ([]macos.Item, bool) {
	var out []macos.Item
	for _, it := range items {
		if name == "" || it.Name == name {
			out = append(out, it)
		}
	}
	return out, len(out) > 0
}

func filterCaches(caches []devcache.Cache, name string) ([]devcache.Cache, bool) {
	var out []devcache.Cache
	for _, c := range caches {
		if name == "" || c.Name == name {
			out = append(out, c)
		}
	}
	return out, len(out) > 0
}

const (
	jobTTL  = 15 * time.Minute // finished jobs stay pollable this long
	maxJobs = 16
	// scanMinRet matches the CLI: analyze keeps files >= 1 MB so trees
	// stay light; Downloads scans keep everything.
	scanMinRet = 1 << 20
)

type jobStore struct {
	mu    sync.Mutex
	jobs  map[string]*job
	seq   int
	heavy chan struct{} // capacity 1: one disk-heavy scan at a time
}

func newJobStore() *jobStore {
	return &jobStore{jobs: map[string]*job{}, heavy: make(chan struct{}, 1)}
}

// start creates a job, reusing an already-running job of the same kind so
// repeated clicks never start parallel copies of the same scan.
func (s *jobStore) start(kind string, run func(ctx context.Context, j *job)) *job {
	s.mu.Lock()
	for _, j := range s.jobs {
		j.mu.Lock()
		running := !j.Done && j.Kind == kind
		j.mu.Unlock()
		if running {
			s.mu.Unlock()
			return j
		}
	}
	s.purgeLocked()
	s.seq++
	j := &job{ID: fmt.Sprintf("j%04d", s.seq), Kind: kind, Label: "queued", Queued: true}
	s.jobs[j.ID] = j
	s.mu.Unlock()

	go func() {
		// One disk-heavy scan at a time; others wait their turn.
		j.progress("waiting for another scan to finish", 0)
		s.heavy <- struct{}{}
		defer func() { <-s.heavy }()
		ctx := context.Background()
		run(ctx, j)
	}()
	return j
}

func (s *jobStore) get(id string) *job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id]
}

// latest returns the newest job of one kind, so a reloaded page can pick
// up a finished scan the browser no longer remembers.
func (s *jobStore) latest(kind string) *job {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *job
	for _, j := range s.jobs {
		if j.Kind != kind {
			continue
		}
		if best == nil || j.ID > best.ID {
			best = j
		}
	}
	return best
}

// purgeLocked evicts finished jobs past their TTL, then oldest finished
// jobs beyond maxJobs. Caller holds the lock.
func (s *jobStore) purgeLocked() {
	now := time.Now()
	for id, j := range s.jobs {
		j.mu.Lock()
		done, old := j.Done, !j.doneAt.IsZero() && now.Sub(j.doneAt) > jobTTL
		j.mu.Unlock()
		if done && old {
			delete(s.jobs, id)
		}
	}
	// If still at capacity, drop the oldest finished job repeatedly.
	for len(s.jobs) >= maxJobs {
		var oldestID string
		var oldest time.Time
		for id, j := range s.jobs {
			j.mu.Lock()
			done, at := j.Done, j.doneAt
			j.mu.Unlock()
			if !done {
				continue
			}
			if oldestID == "" || at.Before(oldest) {
				oldestID, oldest = id, at
			}
		}
		if oldestID == "" {
			break // everything is running; let it be
		}
		delete(s.jobs, oldestID)
	}
}

// ---- runners ----

// runAnalyze scans one folder tree (cached like the CLI's 15-minute cache)
// and keeps the whole tree in memory for instant drill-down requests.
func (sv *server) runAnalyze(ctx context.Context, j *job, path string, force bool) {
	root := fsutil.ExpandPath(path)
	if root == "" {
		root = fsutil.Home()
	}
	res := sv.scanCached(ctx, j, root, scanMinRet, sv.settings.Exclusions, force)
	if res == nil {
		return
	}
	j.finish(analyzeSummary(res), res)
}

// runDownloads scans ~/Downloads with every file retained (min 0), then
// groups it into cleanup categories.
func (sv *server) runDownloads(ctx context.Context, j *job, force bool) {
	root := fsutil.ExpandPath("~/Downloads")
	if _, err := fsutil.Lstat(root); err != nil {
		j.fail(fmt.Errorf("~/Downloads not found"))
		return
	}
	res := sv.scanCached(ctx, j, root, 0, sv.settings.Exclusions, force)
	if res == nil {
		return
	}
	s := sv.settings
	rep := analysis.AnalyzeDownloads(root, res.Files, s.LargeMinBytes, s.OldDays)
	dto := downloadsDTO{
		Root:             fsutil.DisplayPath(rep.Root),
		Total:            rep.Total,
		FileCount:        len(rep.Files),
		CleanupPotential: rep.CleanupPotential,
		CleanupFiles:     toFilesCapped(rep.CleanupPotentialSet, detailFileCap),
		Large:            toFilesCapped(rep.Large, detailFileCap),
		Old:              toFilesCapped(rep.Old, detailFileCap),
	}
	for _, c := range rep.Categories {
		dto.Categories = append(dto.Categories, downloadsCatDTO{Name: c.Name, Size: c.Size, Count: len(c.Files)})
	}
	j.finish(dto, res)
}

// runDuplicates scans the configured roots and hashes candidates, exactly
// like the CLI: dev folders are excluded unless asked for.
func (sv *server) runDuplicates(ctx context.Context, j *job, roots []string, includeDev, force bool) {
	s := sv.settings
	if len(roots) == 0 {
		roots = s.DupRoots
	}
	var excludes []string
	if !includeDev && !s.DupIncludeDev {
		excludes = append(excludes, duplicates.DevFolderNames...)
	}
	j.progress("scanning roots", 0)
	var files []fsutil.FileInfo
	perRoot := 1.0 / float64(len(roots)+1)
	for i, p := range roots {
		root := fsutil.ExpandPath(p)
		res := sv.scanCached(ctx, j, root, s.DupMinBytes, append(append([]string{}, excludes...), s.Exclusions...), force)
		if res == nil {
			return
		}
		files = append(files, res.Files...)
		j.progress(fmt.Sprintf("scanned %s", fsutil.DisplayPath(root)), float64(i+1)*perRoot)
	}
	dup, err := duplicates.Find(ctx, files, s.DupMinBytes, func(p duplicates.Progress) {
		if p.Done {
			return
		}
		j.progress(p.Phase+" duplicates", perRoot*float64(len(roots))+float64(p.FilesChecked)/float64(max(p.FilesTotal, 1))*perRoot)
	})
	if err != nil {
		j.fail(err)
		return
	}
	dto := dupDTO{Scanned: dup.Scanned, Reclaimable: dup.Reclaimable}
	for _, g := range dup.Groups {
		dto.Groups = append(dto.Groups, dupGroupDTO{
			Size: g.Size, KeepIndex: g.KeepIndex(), Reclaimable: g.Reclaimable(), Files: toFiles(g.Files),
		})
	}
	j.finish(dto, nil)
}

// runDeps finds stale project dependency folders and toolchains.
func (sv *server) runDeps(ctx context.Context, j *job) {
	s := sv.settings
	roots := s.DepRoots
	if len(roots) == 0 {
		roots = devdeps.DefaultRoots()
	}
	expanded := make([]string, 0, len(roots))
	for _, p := range roots {
		expanded = append(expanded, fsutil.ExpandPath(p))
	}
	j.progress("searching project dependency folders", 0)
	rep := devdeps.Find(ctx, expanded, devdeps.Options{
		StaleDays: s.DepStaleDays,
		SizeAll:   true,
		Progress:  func(p devdeps.Progress) { j.progress(truncate(p.Label, 80), 0.5) },
	})
	if ctx.Err() != nil {
		j.fail(fmt.Errorf("cancelled"))
		return
	}
	dto := depsDTO{StaleDays: rep.StaleDays, Roots: displayAll(rep.Roots), StaleSize: rep.StaleSize, FreshSize: rep.FreshSize, WalkedDirs: rep.WalkedDirs}
	for _, e := range rep.Stale {
		dto.Stale = append(dto.Stale, depEntryDTO{Path: fsutil.DisplayPath(e.Path), Kind: e.Kind, Size: e.Size, LastActivity: e.LastActivity, Reinstall: e.Reinstall})
	}
	for _, e := range rep.Fresh {
		dto.Fresh = append(dto.Fresh, depEntryDTO{Path: fsutil.DisplayPath(e.Path), Kind: e.Kind, Size: e.Size, LastActivity: e.LastActivity, Reinstall: e.Reinstall})
	}
	j.finish(dto, nil)
}

// runDashboard runs the combined, overlap-aware overview.
func (sv *server) runDashboard(ctx context.Context, j *job) {
	rep := dashboard.Build(ctx, sv.settings, func(p dashboard.Phase) {
		j.progress(p.Label, p.Fraction)
	})
	if ctx.Err() != nil {
		j.fail(fmt.Errorf("cancelled"))
		return
	}
	dto := dashboardDTO{Gross: rep.Gross, Actual: rep.Actual, Categories: rep.Categories}
	if v, err := volumeDTOAt("/"); err == nil {
		dto.Volume = &v
	}
	for _, it := range rep.Macos {
		if it.Exists {
			dto.Macos = append(dto.Macos, it)
		}
	}
	for _, c := range rep.Caches {
		if c.Exists {
			dto.Caches = append(dto.Caches, c)
		}
	}
	j.setDetails(dashboardDetails(rep))
	j.finish(dto, nil)
}

// Payload caps: browser tabs should never receive (or try to render)
// unbounded lists — a huge Downloads folder must not become 50k DOM rows.
const (
	detailFileCap  = 500
	detailGroupCap = 30
	detailEntryCap = 200
)

// listDetailDTO is the drill-down payload for one dashboard category.
// Truncated reports that more rows exist than were sent; the dedicated
// screens (Duplicates, Downloads…) remain the place to see everything.
type listDetailDTO struct {
	Title      string         `json:"title"`
	Kind       string         `json:"kind"`
	TotalCount int            `json:"totalCount"`
	TotalBytes int64          `json:"totalBytes"`
	Truncated  bool           `json:"truncated"`
	Files      []fileDTO      `json:"files,omitempty"`
	Groups     []dupGroupDTO  `json:"groups,omitempty"`
	Deps       []depEntryDTO  `json:"deps,omitempty"`
}

// dashboardDetails builds every category drill-down from a dashboard
// report, each list capped to keep payloads and DOM size bounded.
func dashboardDetails(rep *dashboard.Report) map[string]any {
	d := map[string]any{
		"macos":    rep.Macos,
		"devcache": rep.Caches,
	}
	if rep.Large != nil {
		d["large"] = fileListDetail("Large files", "large", rep.Large)
	}
	if rep.Old != nil {
		d["old"] = fileListDetail("Old files", "old", rep.Old)
	}
	if rep.Downloads != nil {
		d["downloads"] = fileListDetail("Downloads cleanup", "downloads", rep.Downloads.CleanupPotentialSet)
	}
	if rep.Dup != nil {
		dto := &listDetailDTO{Title: "Duplicates", Kind: "duplicates", TotalCount: len(rep.Dup.Groups), TotalBytes: rep.Dup.Reclaimable}
		for i, g := range rep.Dup.Groups {
			if i >= detailGroupCap {
				dto.Truncated = true
				break
			}
			dto.Groups = append(dto.Groups, dupGroupDTO{Size: g.Size, KeepIndex: g.KeepIndex(), Reclaimable: g.Reclaimable(), Files: toFiles(g.Files)})
		}
		d["duplicates"] = dto
	}
	if rep.Deps != nil {
		dto := &listDetailDTO{Title: "Stale dependencies", Kind: "deps", TotalCount: len(rep.Deps.Stale), TotalBytes: rep.Deps.StaleSize}
		for i, e := range rep.Deps.Stale {
			if i >= detailEntryCap {
				dto.Truncated = true
				break
			}
			dto.Deps = append(dto.Deps, depEntryDTO{Path: fsutil.DisplayPath(e.Path), Kind: e.Kind, Size: e.Size, LastActivity: e.LastActivity, Reinstall: e.Reinstall})
		}
		d["deps"] = dto
	}
	return d
}

func fileListDetail(title, kind string, files []fsutil.FileInfo) *listDetailDTO {
	dto := &listDetailDTO{Title: title, Kind: kind, TotalCount: len(files)}
	for _, f := range files {
		dto.TotalBytes += f.Size // honest grand total, also when truncated
	}
	for i, f := range files {
		if i >= detailFileCap {
			dto.Truncated = true
			break
		}
		dto.Files = append(dto.Files, toFile(f))
	}
	return dto
}

// scanCached scans one root, reusing the CLI's 15-minute result cache so
// repeated visits (and CLI runs) warm-start. Returns nil if the job was
// failed. Progress is reported as label + fraction within this scan.
func (sv *server) scanCached(ctx context.Context, j *job, root string, minFile int64, excludes []string, force bool) *scanner.Result {
	if err := scanner.CheckScannable(root); err != nil {
		j.fail(fmt.Errorf("%s: %w", fsutil.DisplayPath(root), err))
		return nil
	}
	cacheKey := fmt.Sprintf("scan-%s-min%d-ex%v-con%d", root, minFile, excludes, sv.settings.Concurrency)
	if !force {
		if meta := report.LoadCache(cacheKey); meta != nil {
			var res scanner.Result
			if json.Unmarshal(meta.Payload, &res) == nil && res.Root != nil {
				j.progress(fmt.Sprintf("%s (cached)", fsutil.DisplayPath(root)), 1)
				return &res
			}
		}
	}
	res, err := scanner.Scan(ctx, root, scanner.Options{
		Concurrency:  sv.settings.Concurrency,
		ExcludeNames: excludes,
		MinFileBytes: minFile,
		Progress: func(p scanner.Progress) {
			frac := 0.0
			if p.DirsDiscovered > 0 {
				frac = float64(p.DirsDone) / float64(p.DirsDiscovered)
			}
			j.progress(fmt.Sprintf("scanning %s — %d files, %s", fsutil.DisplayPath(root), p.Files, units.Format(p.Bytes)), frac)
		},
	})
	if err != nil {
		j.fail(err)
		return nil
	}
	report.SaveCache(cacheKey, res)
	return res
}

// ---- DTOs ----

type fileDTO struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"`
	Dir     bool   `json:"dir,omitempty"`
	DB      bool   `json:"db,omitempty"`
}

func toFile(f fsutil.FileInfo) fileDTO {
	return fileDTO{Path: fsutil.DisplayPath(f.Path), Size: f.Size, ModTime: f.ModTime, Dir: f.IsDir(), DB: fsutil.IsDatabaseFile(f.Path)}
}

func toFiles(files []fsutil.FileInfo) []fileDTO {
	out := make([]fileDTO, 0, len(files))
	for _, f := range files {
		out = append(out, toFile(f))
	}
	return out
}

// toFilesCapped sends at most cap rows — a Downloads folder can hold
// tens of thousands of old files, which must not become one DOM table.
func toFilesCapped(files []fsutil.FileInfo, cap int) []fileDTO {
	if len(files) > cap {
		return toFiles(files[:cap])
	}
	return toFiles(files)
}

type dirDTO struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Files   int64  `json:"files"`
	Dirs    int64  `json:"dirs"`
	ModTime int64  `json:"modTime"`
}

type treeDTO struct {
	Node       dirDTO   `json:"node"`
	Crumbs     []dirDTO `json:"crumbs"`
	Dirs       []dirDTO `json:"dirs"`
	Files      []fileDTO `json:"files"`
	TotalFiles int64    `json:"totalFiles"`
}

func buildTreeDTO(root, node *scanner.DirNode) *treeDTO {
	dto := &treeDTO{Node: toDir(node), TotalFiles: node.FileCount}
	// Breadcrumb: walk up from node to root by path prefix.
	for p := node; p != nil; p = parentDir(root, p.Path) {
		dto.Crumbs = append(dto.Crumbs, toDir(p))
		if p.Path == root.Path {
			break
		}
	}
	for i, j2 := 0, len(dto.Crumbs)-1; i < j2; i, j2 = i+1, j2-1 {
		dto.Crumbs[i], dto.Crumbs[j2] = dto.Crumbs[j2], dto.Crumbs[i]
	}
	for _, c := range node.SortedChildren() {
		dto.Dirs = append(dto.Dirs, toDir(c))
	}
	for _, f := range node.TopFiles(100) {
		dto.Files = append(dto.Files, toFile(f))
	}
	return dto
}

func toDir(n *scanner.DirNode) dirDTO {
	return dirDTO{Path: fsutil.DisplayPath(n.Path), Name: n.Name, Size: n.Size, Files: n.FileCount, Dirs: n.DirCount, ModTime: n.ModTime}
}

// parentDir finds the immediate ancestor of path within the scanned tree.
func parentDir(root *scanner.DirNode, path string) *scanner.DirNode {
	pp := parentPathOf(path, root.Path)
	if pp == "" || pp == path {
		return nil
	}
	return root.Find(pp)
}

// parentPathOf strips the last path component, stopping at rootPath.
func parentPathOf(path, rootPath string) string {
	if path == rootPath || path == "/" {
		return ""
	}
	i := strings.LastIndexByte(path, '/')
	if i <= 0 {
		return ""
	}
	return path[:i]
}

type analyzeSummaryDTO struct {
	Root       dirDTO `json:"root"`
	TotalFiles int64  `json:"totalFiles"`
	TotalDirs  int64  `json:"totalDirs"`
	ElapsedMs  int64  `json:"elapsedMs"`
}

func analyzeSummary(res *scanner.Result) analyzeSummaryDTO {
	return analyzeSummaryDTO{Root: toDir(res.Root), TotalFiles: res.TotalFiles, TotalDirs: res.TotalDirs, ElapsedMs: res.Elapsed.Milliseconds()}
}

type downloadsCatDTO struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Count int    `json:"count"`
}

type downloadsDTO struct {
	Root             string            `json:"root"`
	Total            int64             `json:"total"`
	FileCount        int               `json:"fileCount"`
	Categories       []downloadsCatDTO `json:"categories"`
	CleanupPotential int64             `json:"cleanupPotential"`
	CleanupFiles     []fileDTO         `json:"cleanupFiles"`
	Large            []fileDTO         `json:"large"`
	Old              []fileDTO         `json:"old"`
}

type dupGroupDTO struct {
	Size        int64    `json:"size"`
	KeepIndex   int      `json:"keepIndex"`
	Reclaimable int64    `json:"reclaimable"`
	Files       []fileDTO `json:"files"`
}

type dupDTO struct {
	Scanned     int           `json:"scanned"`
	Reclaimable int64         `json:"reclaimable"`
	Groups      []dupGroupDTO `json:"groups"`
}

type depEntryDTO struct {
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	Size         int64  `json:"size"`
	LastActivity int64  `json:"lastActivity"`
	Reinstall    string `json:"reinstall"`
}

type depsDTO struct {
	StaleDays  int           `json:"staleDays"`
	Roots      []string      `json:"roots"`
	Stale      []depEntryDTO `json:"stale"`
	Fresh      []depEntryDTO `json:"fresh"`
	StaleSize  int64         `json:"staleSize"`
	FreshSize  int64         `json:"freshSize"`
	WalkedDirs int64         `json:"walkedDirs"`
}

type volumeDTO struct {
	Name string `json:"name"`
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
}

type dashboardDTO struct {
	Volume     *volumeDTO          `json:"volume,omitempty"`
	Gross      int64               `json:"gross"`
	Actual     int64               `json:"actual"`
	Categories []dashboard.Category `json:"categories"`
	Macos      []macos.Item        `json:"macos"`
	Caches     []devcache.Cache    `json:"caches"`
}

func displayAll(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, fsutil.DisplayPath(p))
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
