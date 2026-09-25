// Package devdeps finds regenerable dependency folders inside projects
// (node_modules, vendor, Pods, target, .next, …) and ranks them by how
// recently the project was touched. The idea: an old dependency folder of
// an untouched project is safe to delete — one command reinstalls it.
//
// "Last activity" is the newest of: the dependency folder's mtime (it was
// (re)installed), the project directory's mtime (files were added/removed),
// and the project manifest/lockfile mtimes (the developer edited the
// project). A fresh date means the module is still in use.
package devdeps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"macclean/internal/fsutil"
	"macclean/internal/scanner"
	"macclean/internal/units"
)

// DepKind describes one regenerable dependency folder type.
type DepKind struct {
	// Name is the directory name to look for.
	Name string
	// Manifests are project files in the PARENT directory that indicate the
	// project type and carry activity signal.
	Manifests []string
	// Reinstall is the fallback reinstall hint.
	Reinstall string
}

// DepKinds is the catalog of dependency folders worth suggesting.
var DepKinds = []DepKind{
	{Name: "node_modules", Manifests: []string{"package.json", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "bun.lockb"}, Reinstall: "npm install"},
	{Name: "vendor", Manifests: []string{"composer.json", "composer.lock"}, Reinstall: "composer install"},
	{Name: "Pods", Manifests: []string{"Podfile", "Podfile.lock"}, Reinstall: "pod install"},
	{Name: "target", Manifests: []string{"Cargo.toml", "pom.xml", "build.gradle", "build.gradle.kts", "Makefile"}, Reinstall: "rebuild"},
	{Name: "build", Manifests: []string{"build.gradle", "build.gradle.kts", "pom.xml", "CMakeLists.txt", "Makefile"}, Reinstall: "rebuild"},
	{Name: ".next", Manifests: []string{"package.json", "next.config.js", "next.config.mjs"}, Reinstall: "npm run build"},
	{Name: ".nuxt", Manifests: []string{"package.json", "nuxt.config.ts"}, Reinstall: "npm run build"},
	{Name: ".output", Manifests: []string{"package.json", "nuxt.config.ts"}, Reinstall: "npm run build"},
	{Name: ".gradle", Manifests: []string{"build.gradle", "build.gradle.kts", "settings.gradle"}, Reinstall: "gradle build"},
	{Name: ".terraform", Manifests: []string{"*.tf"}, Reinstall: "terraform init"},
	{Name: "bower_components", Manifests: []string{"bower.json"}, Reinstall: "bower install"},
	{Name: ".parcel-cache", Manifests: []string{"package.json"}, Reinstall: "parcel build"},
	{Name: ".turbo", Manifests: []string{"package.json", "turbo.json"}, Reinstall: "npx turbo build"},
}

// Toolchain is a version manager whose installed versions can be scanned:
// old, unreferenced versions are safe to remove and quick to reinstall.
type Toolchain struct {
	// Name labels entries ("nvm").
	Name string
	// VersionsDir holds one directory per installed version.
	VersionsDir string
	// VersionFiles are project files that pin a version (content, e.g.
	// "18" or "18.17.0").
	VersionFiles []string
	// DefaultFile holds the manager's default version, if any.
	DefaultFile string
	// AliasDir resolves alias defaults like "lts/hydrogen" (nvm).
	AliasDir string
	// InstallCmd is the reinstall hint, %s = version.
	InstallCmd string
}

// Toolchains is the catalog of supported version managers.
var Toolchains = []Toolchain{
	{Name: "nvm", VersionsDir: "~/.nvm/versions/node", VersionFiles: []string{".nvmrc"},
		DefaultFile: "~/.nvm/alias/default", AliasDir: "~/.nvm/alias", InstallCmd: "nvm install %s"},
	{Name: "pyenv", VersionsDir: "~/.pyenv/versions", VersionFiles: []string{".python-version"},
		DefaultFile: "~/.pyenv/version", InstallCmd: "pyenv install %s"},
	{Name: "rbenv", VersionsDir: "~/.rbenv/versions", VersionFiles: []string{".ruby-version"},
		DefaultFile: "~/.rbenv/version", InstallCmd: "rbenv install %s"},
}

// IsToolchainKind reports whether an entry Kind names a version manager.
func IsToolchainKind(kind string) bool {
	for _, tc := range Toolchains {
		if tc.Name == kind {
			return true
		}
	}
	return false
}

// skipInside names never descended into while searching (they are not
// projects) — .git first because it is everywhere.
var skipInside = map[string]bool{
	".git": true, "Library": true, ".Trash": true, ".venv": true,
	"venv": true, "env": true, "__pycache__": true,
}

func kindByName() map[string]DepKind {
	m := make(map[string]DepKind, len(DepKinds))
	for _, k := range DepKinds {
		m[k.Name] = k
	}
	return m
}

// Entry is one discovered dependency folder.
type Entry struct {
	Path         string `json:"path"`
	Project      string `json:"project"` // parent project directory
	Kind         string `json:"kind"`    // node_modules, vendor, …
	Size         int64  `json:"size"`
	Files        int64  `json:"files"`
	LastActivity int64  `json:"lastActivity"` // unix seconds
	Stale        bool   `json:"stale"`
	Reinstall    string `json:"reinstall"`
}

// Age renders how long ago the entry was last touched.
func (e Entry) Age() string { return units.Age(time.Unix(e.LastActivity, 0)) }

// Report is the result of a search.
type Report struct {
	Roots     []string `json:"roots"`
	StaleDays int      `json:"staleDays"`
	Entries   []Entry  `json:"entries"` // stalest first
	Stale     []Entry  `json:"stale"`
	Fresh     []Entry  `json:"fresh"`
	StaleSize int64    `json:"staleSize"`
	FreshSize int64    `json:"freshSize"`
	// WalkedDirs counts directories visited while searching.
	WalkedDirs int64 `json:"walkedDirs"`
}

// Options controls a search.
type Options struct {
	// StaleDays: entries older than this are suggested (default 90).
	StaleDays int
	// SizeAll sizes every entry; otherwise only stale ones (faster).
	SizeAll bool
	// MinSize skips entries smaller than this after sizing (0 = keep all).
	MinSize int64
	// MaxDepth caps how deep the search descends (default 12).
	MaxDepth int
	// Progress receives phase updates for UI feedback.
	Progress func(Progress)
}

// Progress is one phase update: Done of Total measurable steps finished.
type Progress struct {
	Label string
	Done  int
	Total int
}

// DefaultRoots returns sensible project search roots for this machine.
func DefaultRoots() []string {
	var roots []string
	for _, d := range []string{"~/Projects", "~/Developer", "~/Documents", "~/Desktop", "~/Downloads"} {
		p := fsutil.ExpandPath(d)
		if st, err := fsutil.Lstat(p); err == nil && st.IsDir() {
			roots = append(roots, p)
		}
	}
	return roots
}

// Find searches roots for dependency folders. The walk is pruned: it never
// descends into a found dependency folder (nested node_modules inside
// node_modules are skipped) and never follows symlinks or crosses
// filesystem boundaries.
func Find(ctx context.Context, roots []string, opts Options) *Report {
	if opts.StaleDays <= 0 {
		opts.StaleDays = 90
	}
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = 12
	}
	rep := &Report{Roots: roots, StaleDays: opts.StaleDays}
	kinds := kindByName()

	matches, pins := walk(ctx, roots, kinds, opts.MaxDepth, &rep.WalkedDirs)
	if ctx.Err() != nil {
		return rep
	}

	cutoff := time.Now().AddDate(0, 0, -opts.StaleDays).Unix()
	for _, m := range matches {
		e := buildEntry(m, kinds[m.name], cutoff)
		rep.Entries = append(rep.Entries, e)
	}

	// Toolchain versions (nvm, pyenv, rbenv…): stale when unreferenced,
	// not the default, and untouched for the threshold.
	for _, tc := range Toolchains {
		if ctx.Err() != nil {
			break
		}
		if opts.Progress != nil {
			opts.Progress(Progress{Label: "Checking " + tc.Name + " versions"})
		}
		rep.Entries = append(rep.Entries, toolchainEntries(tc, pins[tc.Name], cutoff)...)
	}

	// Size: everything when asked, otherwise only stale candidates (the
	// only ones that count toward suggestions).
	toSize := map[*Entry]bool{}
	for i := range rep.Entries {
		if opts.SizeAll || rep.Entries[i].Stale {
			toSize[&rep.Entries[i]] = true
		}
	}
	i := 0
	for idx := range rep.Entries {
		if !toSize[&rep.Entries[idx]] {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		i++
		if opts.Progress != nil {
			opts.Progress(Progress{
				Label: "Measuring " + fsutil.DisplayPath(rep.Entries[idx].Path),
				Done:  i, Total: len(toSize),
			})
		}
		res, err := scanner.Scan(ctx, rep.Entries[idx].Path, scanner.Options{Concurrency: 4, MinFileBytes: 1 << 62})
		if err == nil && !res.Cancelled {
			rep.Entries[idx].Size = res.TotalSize
			rep.Entries[idx].Files = res.TotalFiles
		}
	}

	// Filter by MinSize and split stale/fresh.
	var kept []Entry
	for _, e := range rep.Entries {
		if e.Size < opts.MinSize {
			continue
		}
		kept = append(kept, e)
		if e.Stale {
			rep.Stale = append(rep.Stale, e)
			rep.StaleSize += e.Size
		} else {
			rep.Fresh = append(rep.Fresh, e)
			rep.FreshSize += e.Size
		}
	}
	// Stalest first — the cleanup candidates lead; fresh (in-use) entries
	// trail visibly so the "still used" story is obvious.
	sort.Slice(kept, func(a, b int) bool { return kept[a].LastActivity < kept[b].LastActivity })
	rep.Entries = kept
	sort.Slice(rep.Stale, func(a, b int) bool { return rep.Stale[a].LastActivity < rep.Stale[b].LastActivity })
	sort.Slice(rep.Fresh, func(a, b int) bool { return rep.Fresh[a].LastActivity < rep.Fresh[b].LastActivity })
	return rep
}

// buildEntry computes activity across the dep folder, the project dir, and
// manifest files, then classifies and picks the reinstall hint.
func buildEntry(m match, kind DepKind, cutoff int64) Entry {
	e := Entry{
		Path:    m.path,
		Project: filepath.Dir(m.path),
		Kind:    kind.Name,
	}
	last := m.mtime
	if st, err := fsutil.Lstat(e.Project); err == nil && st.ModTime > last {
		last = st.ModTime
	}
	for _, mf := range kind.Manifests {
		if st, err := fsutil.Lstat(filepath.Join(e.Project, mf)); err == nil && st.ModTime > last {
			last = st.ModTime
		}
	}
	e.LastActivity = last
	e.Stale = last < cutoff
	e.Reinstall = reinstallHint(e.Project, kind)
	return e
}

// toolchainEntries lists installed versions of one manager. A version is
// suggested only when it is NOT the default, NOT referenced by any version
// file found during the walk, and older than the cutoff.
func toolchainEntries(tc Toolchain, pins []string, cutoff int64) []Entry {
	vdir := fsutil.ExpandPath(tc.VersionsDir)
	dirs, err := os.ReadDir(vdir)
	if err != nil {
		return nil
	}
	def := resolveToolchainDefault(tc)
	var out []Entry
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		name := d.Name()
		if name == "system" {
			continue
		}
		path := filepath.Join(vdir, name)
		st, err := fsutil.Lstat(path)
		if err != nil {
			continue
		}
		ver := strings.TrimPrefix(name, "v")
		referenced := false
		for _, pin := range pins {
			if versionPinned(pin, ver) {
				referenced = true
				break
			}
		}
		out = append(out, Entry{
			Path:         path,
			Project:      vdir,
			Kind:         tc.Name,
			LastActivity: st.ModTime,
			Stale:        !referenced && ver != def && st.ModTime < cutoff,
			Reinstall:    fmt.Sprintf(tc.InstallCmd, name),
		})
	}
	return out
}

// versionPinned matches a pin like "18", "18.17", "18.17.0", "v18.17.0"
// against an installed version. Major-only pins protect every minor of
// that major, matching how nvm resolves .nvmrc.
func versionPinned(pin, ver string) bool {
	pin = strings.TrimSpace(pin)
	pin = strings.TrimPrefix(pin, "v")
	if pin == "" || strings.Contains(pin, "/") { // "lts/*" etc. — unresolvable
		return false
	}
	return ver == pin || strings.HasPrefix(ver, pin+".")
}

// resolveToolchainDefault reads the manager's default version file,
// following one alias hop (nvm "lts/hydrogen" → alias file → version).
func resolveToolchainDefault(tc Toolchain) string {
	if tc.DefaultFile == "" {
		return ""
	}
	raw := strings.TrimSpace(readSmallFile(fsutil.ExpandPath(tc.DefaultFile)))
	for hop := 0; hop < 2 && raw != ""; hop++ {
		if !strings.Contains(raw, "/") {
			return strings.TrimPrefix(raw, "v")
		}
		if tc.AliasDir == "" {
			return ""
		}
		raw = strings.TrimSpace(readSmallFile(filepath.Join(fsutil.ExpandPath(tc.AliasDir), raw)))
	}
	return ""
}

func readSmallFile(p string) string {
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	s := string(data)
	if len(s) > 64 {
		return ""
	}
	return strings.TrimSpace(s)
}

// reinstallHint refines the hint from the lockfiles present in the project.
func reinstallHint(project string, kind DepKind) string {
	has := func(name string) bool {
		_, err := os.Lstat(filepath.Join(project, name))
		return err == nil
	}
	switch kind.Name {
	case "node_modules":
		switch {
		case has("pnpm-lock.yaml"):
			return "pnpm install"
		case has("yarn.lock"):
			return "yarn install"
		case has("bun.lockb") || has("bun.lock"):
			return "bun install"
		case has("package-lock.json"):
			return "npm ci"
		}
	case "target":
		switch {
		case has("Cargo.toml"):
			return "cargo build"
		case has("pom.xml"):
			return "mvn package"
		case has("build.gradle") || has("build.gradle.kts"):
			return "./gradlew build"
		case has("Makefile"):
			return "make"
		}
	case "build":
		switch {
		case has("CMakeLists.txt"):
			return "cmake --build build"
		case has("build.gradle") || has("build.gradle.kts"):
			return "./gradlew build"
		case has("Makefile"):
			return "make"
		}
	}
	return kind.Reinstall
}

// ---- pruned directory walk ----

type match struct {
	path  string
	name  string
	mtime int64
}

type dirJob struct {
	path  string
	dev   uint64
	depth int
}

// walk performs the bounded parallel pruned search, recording dependency
// folders instead of descending into them, and collecting version pins
// (.nvmrc, .python-version, …) for the toolchain check.
func walk(ctx context.Context, roots []string, kinds map[string]DepKind, maxDepth int, walked *int64) ([]match, map[string][]string) {
	versionFiles := map[string]string{}
	for _, tc := range Toolchains {
		for _, vf := range tc.VersionFiles {
			versionFiles[vf] = tc.Name
		}
	}
	w := &prunedWalker{
		ctx:         ctx,
		kinds:       kinds,
		maxDepth:    maxDepth,
		walked:      walked,
		versionFiles: versionFiles,
		pins:        map[string][]string{},
	}
	w.cond = sync.NewCond(&w.mu)
	for _, r := range roots {
		st, err := fsutil.Lstat(r)
		if err != nil || !st.IsDir() {
			continue
		}
		if err := scanner.CheckScannable(r); err != nil {
			continue
		}
		w.push(dirJob{path: r, dev: st.Dev, depth: 0})
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.work()
		}()
	}
	wg.Wait()
	return w.matches, w.pins
}

type prunedWalker struct {
	ctx          context.Context
	kinds        map[string]DepKind
	maxDepth     int
	walked       *int64
	versionFiles map[string]string // file name → toolchain name
	pins         map[string][]string

	mu       sync.Mutex
	cond     *sync.Cond
	stack    []dirJob
	pending  int
	cancel   bool
	matches  []match
}

func (w *prunedWalker) push(j dirJob) {
	w.mu.Lock()
	w.stack = append(w.stack, j)
	w.pending++
	w.mu.Unlock()
	w.cond.Broadcast()
}

func (w *prunedWalker) work() {
	for {
		w.mu.Lock()
		for len(w.stack) == 0 && w.pending > 0 && !w.cancel {
			w.cond.Wait()
		}
		if w.cancel || w.pending == 0 {
			w.mu.Unlock()
			return
		}
		j := w.stack[len(w.stack)-1]
		w.stack = w.stack[:len(w.stack)-1]
		w.mu.Unlock()

		w.process(j)

		w.mu.Lock()
		w.pending--
		w.mu.Unlock()
		w.cond.Broadcast()
	}
}

func (w *prunedWalker) process(j dirJob) {
	if w.ctx.Err() != nil {
		w.mu.Lock()
		w.cancel = true
		w.mu.Unlock()
		w.cond.Broadcast()
		return
	}
	entries, err := os.ReadDir(j.path)
	if err != nil {
		return
	}
	atomic.AddInt64(w.walked, 1)
	nextDepth := j.depth + 1
	for _, e := range entries {
		if !e.IsDir() {
			// Version pins (.nvmrc, .python-version…) are files; record
			// their content for the toolchain check.
			if tc, ok := w.versionFiles[e.Name()]; ok {
				if data, err := os.ReadFile(filepath.Join(j.path, e.Name())); err == nil {
					w.mu.Lock()
					w.pins[tc] = append(w.pins[tc], strings.TrimSpace(string(data)))
					w.mu.Unlock()
				}
			}
			continue
		}
		// DirEntry.Type knows symlinks without an lstat.
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		name := e.Name()
		full := filepath.Join(j.path, name)
		if kind, ok := w.kinds[name]; ok {
			st, err := fsutil.Lstat(full)
			if err != nil {
				continue
			}
			w.mu.Lock()
			w.matches = append(w.matches, match{path: full, name: kind.Name, mtime: st.ModTime})
			w.mu.Unlock()
			continue // found one — do not descend
		}
		if skipInside[name] {
			continue
		}
		if nextDepth > w.maxDepth {
			continue
		}
		st, err := fsutil.Lstat(full)
		if err != nil {
			continue
		}
		if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			continue
		}
		if st.Dev != j.dev {
			continue // other filesystem
		}
		w.push(dirJob{path: full, dev: st.Dev, depth: nextDepth})
	}
}

// HardRemove deletes entries outright (after the caller confirmed);
// normally entries go to the Trash instead.
func HardRemove(ctx context.Context, paths []string) error {
	for _, p := range paths {
		if err := fsutil.CheckDeletable(p); err != nil {
			return err
		}
		if err := os.RemoveAll(p); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		_ = ctx
	}
	return nil
}

// StringSummary renders the one-line-per-entry text used by the CLI.
func (r *Report) StringSummary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d folders · %s stale (%d) · %s fresh (%d)\n",
		len(r.Entries), units.Format(r.StaleSize), len(r.Stale), units.Format(r.FreshSize), len(r.Fresh))
	return b.String()
}
