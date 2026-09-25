// Package tui is the interactive MacClean interface: drill into folders,
// review findings, and move user files to the Trash — always inspecting
// before deleting.
package tui

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"macclean/internal/analysis"
	"macclean/internal/dashboard"
	"macclean/internal/devcache"
	"macclean/internal/devdeps"
	"macclean/internal/duplicates"
	"macclean/internal/fsutil"
	"macclean/internal/macos"
	"macclean/internal/report"
	"macclean/internal/scanner"
	"macclean/internal/settings"
	"macclean/internal/units"
)

type screen int

const (
	scrMenu screen = iota
	scrRoots
	scrScan
	scrBrowser
	scrFileList
	scrDownloads
	scrDuplicates
	scrDupSetup
	scrDeps
	scrDashboard
	scrCleanup
	scrSettings
)

type jobKind int

const (
	jobAnalyzeRoot jobKind = iota
	jobDownloads
	jobDuplicates
	jobUserFiles // feeds Large Files / Old Files screens
	jobDeps      // stale project dependency folders
	jobMacos     // general macOS cleanup targets
	jobDashboard
	jobCleanupDetect
)

// progressMsg flows from scanner callbacks into the UI.
type progressMsg struct {
	label  string
	frac   float64
	detail string
}

// jobDoneMsg carries whatever the finished job produced.
type jobDoneMsg struct {
	kind      jobKind
	scan      *scanner.Result
	files     []fsutil.FileInfo
	dup       *duplicates.Result
	deps      *devdeps.Report
	macosIt   []macos.Item
	dash      *dashboard.Report
	caches    []devcache.Cache
	cancelled bool
}

// trashDoneMsg reports a batch Trash move.
type trashDoneMsg struct {
	ok, fail int
	err      string
}

// cleanDoneMsg reports a dev cache cleanup.
type cleanDoneMsg struct {
	name string
	err  string
}

// flushStatus clears the transient status line.
type flushStatusMsg struct{}

type model struct {
	version string
	runCtx  context.Context
	send    func(tea.Msg)
	s       settings.Settings

	screen screen
	width  int
	height int

	// menu
	menuCursor int

	// roots picker
	roots       []string
	rootsCursor int
	pathInput   *lineInput

	// settings text input (reuses pathInput)
	settingsInput bool

	// cameFrom remembers the screen a detail view was opened from, so Esc
	// goes back where the user came from (scrMenu = no remembered origin).
	cameFrom screen

	// scan progress
	job       jobKind
	jobLabel  string
	jobCancel context.CancelFunc
	progress  progressMsg

	// browser
	pendingRoot   string
	tree          *scanner.DirNode
	treeCache     map[string]*scanner.DirNode // session cache: scanned roots by path
	cur           *scanner.DirNode
	browserRows   []browserRow
	browserCursor int

	// file list (large / old / downloads category)
	flTitle   string
	flFiles   []fsutil.FileInfo
	flCursor  int
	flChecked map[string]bool

	// downloads
	dl      *analysis.DownloadsReport
	dlCursor int

	// duplicates
	dup        *duplicates.Result
	dupRows    []dupRow
	dupCursor  int
	dupChecked map[string]bool

	// duplicates setup (choose where to scan)
	dupSetupInput    *lineInput
	dupRootsOverride []string  // custom scan root chosen in setup; nil = settings roots
	dupDevIncluded   bool      // whether the running/last scan included dev folders

	// stale project deps
	deps        *devdeps.Report
	depsCursor  int
	depsChecked map[string]bool
	depsByAge   bool // true: oldest first; false: largest first

	// dashboard
	dash       *dashboard.Report
	dashCursor int

	// cleanup screen (developer caches + general macOS items)
	cleanList  []cleanEntry
	cleanTitle string
	clCursor   int
	clFilter   string // "docker" filters to Docker entries

	// settings
	stCursor int

	// modal + status
	modal      *modalState
	statusLine string
	statusAt   time.Time
}

// Run starts the interactive UI.
func Run(ctx context.Context, version string) error {
	m := &model{version: version, runCtx: ctx, s: settings.Load()}
	p := tea.NewProgram(m, tea.WithAltScreen())
	m.send = p.Send
	_, err := p.Run()
	return err
}

func (m *model) Init() tea.Cmd { return nil }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case progressMsg:
		m.progress = msg
		return m, nil

	case jobDoneMsg:
		return m.handleJobDone(msg)

	case trashDoneMsg:
		return m.handleTrashDone(msg)

	case cleanDoneMsg:
		if msg.err != "" {
			m.setStatus("clean failed: " + msg.err)
		} else {
			m.setStatus("cleaned " + msg.name)
			report.InvalidateCache()
		}
		m.dropCleanedCache(msg.name)
		return m, nil

	case flushStatusMsg:
		m.statusLine = ""
		return m, nil
	}
	return m, nil
}

func (m *model) setStatus(s string) {
	m.statusLine = s
	m.statusAt = time.Now()
}

// goBack returns to the screen the current view was entered from, or the
// main menu when there is none.
func (m *model) goBack() {
	m.screen = m.cameFrom
	m.cameFrom = scrMenu
}

// ---- key routing ----

func (m *model) handleKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.modal != nil {
		return m.handleModalKey(k)
	}
	if k.Type == tea.KeyCtrlC {
		if m.jobCancel != nil {
			m.jobCancel()
		}
		return m, tea.Quit
	}
	// Terminals (and paste-like fast typing) can deliver several printable
	// characters in a single key event; expand so each is handled.
	if k.Type == tea.KeyRunes && len(k.Runes) > 1 && m.pathInput == nil {
		var cmd tea.Cmd
		for _, r := range k.Runes {
			mm, c := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			if next, ok := mm.(*model); ok {
				m = next
			}
			cmd = tea.Batch(cmd, c)
		}
		return m, cmd
	}
	switch m.screen {
	case scrMenu:
		return m.keyMenu(k)
	case scrRoots:
		return m.keyRoots(k)
	case scrScan:
		return m.keyScan(k)
	case scrBrowser:
		return m.keyBrowser(k)
	case scrFileList:
		return m.keyFileList(k)
	case scrDownloads:
		return m.keyDownloads(k)
	case scrDuplicates:
		return m.keyDuplicates(k)
	case scrDupSetup:
		return m.keyDupSetup(k)
	case scrDeps:
		return m.keyDeps(k)
	case scrDashboard:
		return m.keyDashboard(k)
	case scrCleanup:
		return m.keyCleanup(k)
	case scrSettings:
		return m.keySettings(k)
	}
	return m, nil
}

func (m *model) handleModalKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	mod := m.modal
	switch mod.kind {
	case modalInfo, modalError:
		if k.Type == tea.KeyEnter || k.Type == tea.KeyEsc {
			m.modal = nil
		}
	case modalConfirm:
		switch {
		case k.Type == tea.KeyEsc:
			m.modal = nil
		case k.Type == tea.KeyEnter || string(k.Runes) == "y":
			m.modal = nil
			if mod.onYes != nil {
				mod.onYes(m)
			}
		}
	case modalConfirmTyped:
		switch k.Type {
		case tea.KeyEsc:
			m.modal = nil
		case tea.KeyEnter:
			if mod.typed == mod.needs {
				m.modal = nil
				if mod.onYes != nil {
					mod.onYes(m)
				}
			} else {
				mod.typed = ""
			}
		case tea.KeyBackspace:
			if len(mod.typed) > 0 {
				mod.typed = mod.typed[:len(mod.typed)-1]
			}
		default:
			for _, r := range k.Runes {
				mod.typed += string(r)
			}
		}
	}
	return m, nil
}

// cleanEntry is the cleanup-screen adapter: developer caches and general
// macOS items render and act identically through it.
type cleanEntry struct {
	name    string
	path    string
	detail  string
	safety  string
	size    int64
	exists  bool
	cmdText string // native command shown in info, if any
	clean   func(ctx context.Context, force bool) error
}

func devRows(cs []devcache.Cache) []cleanEntry {
	out := make([]cleanEntry, 0, len(cs))
	for _, c := range cs {
		c := c
		out = append(out, cleanEntry{
			name: c.Name, path: c.Path, detail: c.Detail, safety: c.Safety,
			size: c.Size, exists: c.Exists,
			cmdText: strings.Join(c.Cleanup, " "),
			clean:   func(ctx context.Context, force bool) error { return devcache.Clean(ctx, c, force) },
		})
	}
	return out
}

func macRows(items []macos.Item) []cleanEntry {
	out := make([]cleanEntry, 0, len(items))
	for _, it := range items {
		it := it
		out = append(out, cleanEntry{
			name: it.Name, path: it.Path, detail: it.Detail, safety: it.Safety,
			size: it.Size, exists: it.Exists,
			clean: func(ctx context.Context, force bool) error { return macos.Clean(ctx, it, force) },
		})
	}
	return out
}

// ---- menu ----

var menuItems = []struct {
	label string
	hint  string
}{
	{"Analyze", "where is my disk space going?"},
	{"Dashboard", "combined overview, honestly deduplicated"},
	{"Downloads", "installers, archives, old downloads"},
	{"Duplicates", "byte-identical files"},
	{"Large Files", "files over a threshold"},
	{"Old Files", "not modified for N days"},
	{"Stale Deps", "node_modules, vendor… ranked by last use"},
	{"Developer", "caches and build artifacts"},
	{"macOS", "Trash, logs, app caches — safe general cleanup"},
	{"Docker", "images, build cache, volumes"},
	{"Settings", "thresholds and exclusions"},
	{"Quit", ""},
}

func (m *model) keyMenu(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	n := len(menuItems)
	switch {
	case k.Type == tea.KeyUp || string(k.Runes) == "k":
		m.menuCursor = (m.menuCursor - 1 + n) % n
	case k.Type == tea.KeyDown || string(k.Runes) == "j":
		m.menuCursor = (m.menuCursor + 1) % n
	case k.Type == tea.KeyEnter:
		switch menuItems[m.menuCursor].label {
		case "Analyze":
			m.roots = analysis.ExistingUserRoots()
			m.roots = append(m.roots, volumes()...)
			m.rootsCursor = 0
			m.screen = scrRoots
		case "Dashboard":
			m.startJob(jobDashboard, "Building dashboard")
		case "Downloads":
			m.startJob(jobDownloads, "Scanning ~/Downloads")
		case "Duplicates":
			m.dupSetupInput = nil
			m.screen = scrDupSetup
		case "Large Files":
			m.flTitle = "Large files"
			m.startJob(jobUserFiles, "Scanning user folders")
		case "Old Files":
			m.flTitle = "Old files"
			m.startJob(jobUserFiles, "Scanning user folders")
		case "Stale Deps":
			m.startJob(jobDeps, "Scanning project dependencies")
		case "Developer":
			m.clFilter = ""
			m.startJob(jobCleanupDetect, "Detecting developer caches")
		case "macOS":
			m.startJob(jobMacos, "Scanning macOS cleanup targets")
		case "Docker":
			m.clFilter = "docker"
			m.startJob(jobCleanupDetect, "Checking Docker")
		case "Settings":
			m.screen = scrSettings
		case "Quit":
			return m, tea.Quit
		}
	case k.Type == tea.KeyEsc || string(k.Runes) == "q":
		return m, tea.Quit
	}
	return m, nil
}

// volumes lists mounted user volumes under /Volumes for optional analysis.
func volumes() []string {
	entries, err := os.ReadDir("/Volumes")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		p := "/Volumes/" + e.Name()
		if st, err := fsutil.Lstat(p); err == nil && st.IsDir() && !fsutil.IsProtected(p) {
			out = append(out, p)
		}
	}
	return out
}

// ---- roots picker ----

func (m *model) keyRoots(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.pathInput != nil {
		switch k.Type {
		case tea.KeyEsc:
			m.pathInput = nil
		case tea.KeyEnter:
			raw := strings.TrimSpace(m.pathInput.value())
			m.pathInput = nil
			if raw != "" {
				p := fsutil.ExpandPath(raw)
				if err := scanner.CheckScannable(p); err != nil {
					m.openError(err)
					return m, nil
				}
				if st, err := fsutil.Lstat(p); err != nil || !st.IsDir() {
					m.openError(fmt.Errorf("%s is not a readable directory", p))
					return m, nil
				}
				m.openAnalyzeRoot(p)
			}
		default:
			m.pathInput.update(k)
		}
		return m, nil
	}
	switch {
	case k.Type == tea.KeyEsc:
		m.screen = scrMenu
	case k.Type == tea.KeyUp || string(k.Runes) == "k":
		if m.rootsCursor > 0 {
			m.rootsCursor--
		}
	case k.Type == tea.KeyDown || string(k.Runes) == "j":
		if m.rootsCursor < len(m.roots) {
			m.rootsCursor++
		}
	case k.Type == tea.KeyEnter:
		if m.rootsCursor == len(m.roots) { // "custom path…" row
			m.pathInput = newLineInput("path:")
			return m, nil
		}
		m.openAnalyzeRoot(m.roots[m.rootsCursor])
	}
	return m, nil
}

// openAnalyzeRoot shows the session-cached results immediately when the
// root was scanned before; otherwise it starts a fresh scan.
func (m *model) openAnalyzeRoot(root string) {
	if cached := m.treeCache[root]; cached != nil {
		m.pendingRoot = root
		m.tree = cached
		m.cur = cached
		m.browserCursor = 0
		m.buildBrowserRows()
		m.screen = scrBrowser
		m.setStatus("cached results — press r to re-scan")
		return
	}
	m.pendingRoot = root
	m.startJob(jobAnalyzeRoot, "Analyzing "+fsutil.DisplayPath(root))
}

// ---- scan / progress ----

func (m *model) keyScan(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.Type == tea.KeyEsc {
		if m.jobCancel != nil {
			m.jobCancel()
		}
	}
	return m, nil
}

// startJob switches to the progress screen and launches the work in a
// goroutine; every callback funnels through m.send.
func (m *model) startJob(kind jobKind, label string) {
	m.screen = scrScan
	m.cameFrom = scrMenu // a fresh job starts a fresh navigation context
	m.job = kind
	m.jobLabel = label
	m.progress = progressMsg{label: label}
	ctx, cancel := context.WithCancel(m.runCtx)
	m.jobCancel = cancel
	send := m.send
	s := m.s

	switch kind {
	case jobAnalyzeRoot:
		root := m.pendingRoot
		go func() {
			res, _ := scanner.Scan(ctx, root, scanner.Options{
				Concurrency:  s.Concurrency,
				ExcludeNames: s.Exclusions,
				MinFileBytes: 1 << 20,
				Progress: func(p scanner.Progress) {
					if p.Done {
						return
					}
					detail := fmt.Sprintf("%s files · %s", humanCount(p.Files), units.Format(p.Bytes))
					frac := 0.0
					if p.DirsDiscovered > 0 {
						frac = float64(p.DirsDone) / float64(p.DirsDiscovered)
					}
					send(progressMsg{label: "Scanning " + fsutil.DisplayPath(root), frac: frac, detail: detail})
				},
			})
			send(jobDoneMsg{kind: jobAnalyzeRoot, scan: res, cancelled: ctx.Err() != nil})
		}()

	case jobDownloads:
		root := fsutil.ExpandPath("~/Downloads")
		go func() {
			res, _ := scanner.Scan(ctx, root, scanner.Options{
				Concurrency:  s.Concurrency,
				ExcludeNames: s.Exclusions,
				MinFileBytes: 0,
				Progress: func(p scanner.Progress) {
					if p.Done {
						return
					}
					frac := 0.0
					if p.DirsDiscovered > 0 {
						frac = float64(p.DirsDone) / float64(p.DirsDiscovered)
					}
					send(progressMsg{label: "Scanning ~/Downloads", frac: frac,
						detail: fmt.Sprintf("%s files · %s", humanCount(p.Files), units.Format(p.Bytes))})
				},
			})
			send(jobDoneMsg{kind: jobDownloads, scan: res, cancelled: ctx.Err() != nil})
		}()

	case jobDuplicates:
		roots := s.DupRoots
		if len(m.dupRootsOverride) > 0 {
			roots = m.dupRootsOverride
		}
		// Dev folders (node_modules, vendor, Pods) are excluded unless the
		// user opted in: their duplicates are structural, and Stale Deps is
		// the right cleanup unit for them.
		excludes := append([]string{}, s.Exclusions...)
		m.dupDevIncluded = s.DupIncludeDev
		if !s.DupIncludeDev {
			excludes = append(excludes, duplicates.DevFolderNames...)
		}
		go func() {
			var files []fsutil.FileInfo
			for i, root := range roots {
				if ctx.Err() != nil {
					break
				}
				base := float64(i) / float64(len(roots))
				res, _ := scanner.Scan(ctx, root, scanner.Options{
					Concurrency:  s.Concurrency,
					ExcludeNames: excludes,
					MinFileBytes: s.DupMinBytes,
					Progress: func(p scanner.Progress) {
						if p.Done {
							return
						}
						frac := 0.0
						if p.DirsDiscovered > 0 {
							frac = float64(p.DirsDone) / float64(p.DirsDiscovered)
						}
						send(progressMsg{label: "Scanning " + fsutil.DisplayPath(root), frac: (base + frac/float64(len(roots))) * 0.5,
							detail: fmt.Sprintf("%s files · %s", humanCount(p.Files), units.Format(p.Bytes))})
					},
				})
				if res != nil {
					files = append(files, res.Files...)
				}
			}
			if ctx.Err() != nil {
				send(jobDoneMsg{kind: jobDuplicates, cancelled: true})
				return
			}
			dup, _ := duplicates.Find(ctx, files, s.DupMinBytes, func(p duplicates.Progress) {
				if p.Done {
					return
				}
				frac := 0.0
				if p.FilesTotal > 0 {
					frac = float64(p.FilesChecked) / float64(p.FilesTotal)
				}
				if p.Phase == "hashing (full)" {
					frac = 0.5 + frac*0.5
				} else {
					frac = 0.5 + frac*0.25
				}
				send(progressMsg{label: p.Phase + " duplicates", frac: frac,
					detail: fmt.Sprintf("%d/%d · %s hashed", p.FilesChecked, p.FilesTotal, units.Format(p.BytesHashed))})
			})
			send(jobDoneMsg{kind: jobDuplicates, dup: dup, files: files, cancelled: ctx.Err() != nil})
		}()

	case jobUserFiles:
		roots := analysis.ExistingUserRoots()
		go func() {
			var files []fsutil.FileInfo
			for i, root := range roots {
				if ctx.Err() != nil {
					break
				}
				base := float64(i) / float64(len(roots))
				res, _ := scanner.Scan(ctx, root, scanner.Options{
					Concurrency:  s.Concurrency,
					ExcludeNames: s.Exclusions,
					MinFileBytes: 1 << 20,
					Progress: func(p scanner.Progress) {
						if p.Done {
							return
						}
						frac := 0.0
						if p.DirsDiscovered > 0 {
							frac = float64(p.DirsDone) / float64(p.DirsDiscovered)
						}
						send(progressMsg{label: "Scanning " + fsutil.DisplayPath(root),
							frac: (base + frac/float64(len(roots))),
							detail: fmt.Sprintf("%s files · %s", humanCount(p.Files), units.Format(p.Bytes))})
					},
				})
				if res != nil {
					files = append(files, res.Files...)
				}
			}
			send(jobDoneMsg{kind: jobUserFiles, files: files, cancelled: ctx.Err() != nil})
		}()

	case jobDeps:
		depRoots := s.DepRoots
		if len(depRoots) == 0 {
			depRoots = devdeps.DefaultRoots()
		}
		go func() {
			rep := devdeps.Find(ctx, depRoots, devdeps.Options{
				StaleDays: s.DepStaleDays,
				SizeAll:   true,
				Progress: func(p devdeps.Progress) {
					frac := 0.05
					if p.Total > 0 {
						frac = 0.1 + 0.9*float64(p.Done)/float64(p.Total)
					}
					send(progressMsg{label: p.Label, frac: frac,
						detail: fmt.Sprintf("%d/%d measured · node_modules, vendor, nvm versions…", p.Done, p.Total)})
				},
			})
			send(jobDoneMsg{kind: jobDeps, deps: rep, cancelled: ctx.Err() != nil})
		}()

	case jobMacos:
		go func() {
			items := macos.Scan(ctx, func(name string) {
				send(progressMsg{label: "Measuring " + name, frac: 0.5})
			})
			send(jobDoneMsg{kind: jobMacos, macosIt: items, cancelled: ctx.Err() != nil})
		}()

	case jobDashboard:
		go func() {
			rep := dashboard.Build(ctx, s, func(p dashboard.Phase) {
				if p.Done {
					return
				}
				send(progressMsg{label: p.Label, frac: p.Fraction})
			})
			send(jobDoneMsg{kind: jobDashboard, dash: rep, cancelled: ctx.Err() != nil})
		}()

	case jobCleanupDetect:
		go func() {
			caches := devcache.Detect(ctx, func(name string) {
				send(progressMsg{label: "Measuring " + name, frac: 0.5})
			})
			send(jobDoneMsg{kind: jobCleanupDetect, caches: caches, cancelled: ctx.Err() != nil})
		}()
	}
}

func (m *model) handleJobDone(msg jobDoneMsg) (tea.Model, tea.Cmd) {
	m.jobCancel = nil
	if msg.cancelled {
		m.screen = scrMenu
		m.setStatus("cancelled")
		return m, nil
	}
	switch msg.kind {
	case jobAnalyzeRoot:
		if msg.scan == nil {
			m.screen = scrRoots
			m.setStatus("scan failed")
			return m, nil
		}
		m.tree = msg.scan.Root
		m.cur = msg.scan.Root
		m.browserCursor = 0
		m.buildBrowserRows()
		if m.treeCache == nil {
			m.treeCache = map[string]*scanner.DirNode{}
		}
		m.treeCache[m.tree.Path] = m.tree
		m.screen = scrBrowser
	case jobDownloads:
		if msg.scan == nil {
			m.screen = scrMenu
			m.setStatus("scan failed")
			return m, nil
		}
		m.dl = analysis.AnalyzeDownloads(fsutil.ExpandPath("~/Downloads"), msg.scan.Files, m.s.LargeMinBytes, m.s.OldDays)
		m.dlCursor = 0
		m.screen = scrDownloads
	case jobDuplicates:
		m.dup = msg.dup
		m.dupChecked = map[string]bool{}
		m.dupCursor = 0
		m.buildDupRows()
		m.screen = scrDuplicates
	case jobDeps:
		m.deps = msg.deps
		m.depsChecked = map[string]bool{}
		m.depsCursor = 0
		m.depsByAge = true
		m.screen = scrDeps
	case jobUserFiles:
		var files []fsutil.FileInfo
		if m.flTitle == "Large files" {
			files = analysis.LargeFiles(msg.files, m.s.LargeMinBytes)
		} else {
			files = analysis.OldFiles(msg.files, m.s.OldDays)
		}
		m.flFiles = files
		m.flCursor = 0
		m.flChecked = map[string]bool{}
		m.screen = scrFileList
	case jobDashboard:
		m.dash = msg.dash
		m.dashCursor = 0
		m.screen = scrDashboard
	case jobCleanupDetect:
		m.cleanList = devRows(msg.caches)
		m.cleanTitle = "Developer caches"
		m.clFilter = ""
		m.clCursor = 0
		m.screen = scrCleanup
	case jobMacos:
		m.cleanList = macRows(msg.macosIt)
		m.cleanTitle = "macOS — general cleanup"
		m.clFilter = ""
		m.clCursor = 0
		m.screen = scrCleanup
	}
	return m, nil
}

// ---- trash ----

func (m *model) trashPaths(paths []string) {
	send := m.send
	go func() {
		var ok, fail int
		var lastErr string
		for _, p := range paths {
			if err := fsutil.MoveToTrash(p); err != nil {
				fail++
				lastErr = err.Error()
			} else {
				ok++
			}
		}
		send(trashDoneMsg{ok: ok, fail: fail, err: lastErr})
	}()
}

func (m *model) handleTrashDone(msg trashDoneMsg) (tea.Model, tea.Cmd) {
	if msg.ok == 0 && msg.fail == 0 {
		return m, nil
	}
	if msg.ok > 0 {
		m.setStatus(fmt.Sprintf("moved %d item(s) to Trash", msg.ok))
		// Cached scan results still list the trashed paths; drop them so a
		// rerun never shows deleted files as if they existed.
		report.InvalidateCache()
	} else {
		m.setStatus("trash failed: " + msg.err)
		return m, nil
	}
	// Drop trashed paths from every active view.
	trashed := func(p string) bool { return !fileExists(p) }
	switch m.screen {
	case scrFileList:
		var kept []fsutil.FileInfo
		for _, f := range m.flFiles {
			if trashed(f.Path) {
				delete(m.flChecked, f.Path)
			} else {
				kept = append(kept, f)
			}
		}
		m.flFiles = kept
		if m.flCursor >= len(m.flFiles) && m.flCursor > 0 {
			m.flCursor = len(m.flFiles) - 1
		}
	case scrBrowser:
		m.buildBrowserRows()
		if m.browserCursor >= len(m.browserRows) && m.browserCursor > 0 {
			m.browserCursor = len(m.browserRows) - 1
		}
	case scrDuplicates:
		for g := range m.dup.Groups {
			var kept []fsutil.FileInfo
			for _, f := range m.dup.Groups[g].Files {
				if trashed(f.Path) {
					delete(m.dupChecked, f.Path)
				} else {
					kept = append(kept, f)
				}
			}
			m.dup.Groups[g].Files = kept
		}
		var live []duplicates.Group
		var reclaim int64
		for _, g := range m.dup.Groups {
			if len(g.Files) >= 2 {
				live = append(live, g)
				reclaim += g.Reclaimable()
			}
		}
		m.dup.Groups = live
		m.dup.Reclaimable = reclaim
		m.buildDupRows()
		if m.dupCursor >= len(m.dupRows) && m.dupCursor > 0 {
			m.dupCursor = len(m.dupRows) - 1
		}
	case scrDeps:
		if m.deps != nil {
			var kept []devdeps.Entry
			for _, e := range m.deps.Entries {
				if trashed(e.Path) {
					delete(m.depsChecked, e.Path)
					continue
				}
				kept = append(kept, e)
			}
			m.deps.Entries = kept
			m.deps.Stale = m.deps.Stale[:0]
			m.deps.Fresh = m.deps.Fresh[:0]
			m.deps.StaleSize, m.deps.FreshSize = 0, 0
			for _, e := range kept {
				if e.Stale {
					m.deps.Stale = append(m.deps.Stale, e)
					m.deps.StaleSize += e.Size
				} else {
					m.deps.Fresh = append(m.deps.Fresh, e)
					m.deps.FreshSize += e.Size
				}
			}
			if m.depsCursor >= len(kept) && m.depsCursor > 0 {
				m.depsCursor = len(kept) - 1
			}
		}
	}
	return m, nil
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func humanCount(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) > 3 {
		var groups []string
		for len(s) > 3 {
			groups = append([]string{s[len(s)-3:]}, groups...)
			s = s[:len(s)-3]
		}
		groups = append([]string{s}, groups...)
		return strings.Join(groups, ",")
	}
	return s
}

func (m *model) dropCleanedCache(name string) {
	var kept []cleanEntry
	for _, c := range m.cleanList {
		// Developer-cache dirs disappear after cleaning; macOS targets
		// (Trash, Logs, …) stay as locations — just reset their size.
		if strings.EqualFold(c.name, name) {
			if !c.exists || fileExists(c.path) {
				c.size = 0
				kept = append(kept, c)
			}
			continue
		}
		kept = append(kept, c)
	}
	m.cleanList = kept
	if m.clCursor >= len(m.cleanList) && m.clCursor > 0 {
		m.clCursor = len(m.cleanList) - 1
	}
}

// ---- browser ----

type browserRow struct {
	isFile bool
	dir    *scanner.DirNode
	file   fsutil.FileInfo
}

func (m *model) buildBrowserRows() {
	m.browserRows = nil
	if m.cur == nil {
		return
	}
	for _, c := range m.cur.SortedChildren() {
		m.browserRows = append(m.browserRows, browserRow{dir: c})
	}
	for _, f := range m.cur.TopFiles(30) {
		m.browserRows = append(m.browserRows, browserRow{isFile: true, file: f})
	}
}

func (m *model) currentBrowserRow() (browserRow, bool) {
	if m.browserCursor >= 0 && m.browserCursor < len(m.browserRows) {
		return m.browserRows[m.browserCursor], true
	}
	return browserRow{}, false
}

func (m *model) keyBrowser(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case k.Type == tea.KeyUp || string(k.Runes) == "k":
		if m.browserCursor > 0 {
			m.browserCursor--
		}
	case k.Type == tea.KeyDown || string(k.Runes) == "j":
		if m.browserCursor < len(m.browserRows)-1 {
			m.browserCursor++
		}
	case k.Type == tea.KeyEnter:
		if row, ok := m.currentBrowserRow(); ok && !row.isFile {
			m.cur = row.dir
			m.browserCursor = 0
			m.buildBrowserRows()
		} else if row, ok := m.currentBrowserRow(); ok {
			m.fileInfoModal(row.file)
		}
	// esc and backspace both go UP one folder; at the top of the scanned
	// tree they return to the roots picker. Results stay cached for the
	// session, so leaving and coming back is instant.
	case k.Type == tea.KeyBackspace, k.Type == tea.KeyEsc:
		if m.cur != nil && m.cur.Path != m.tree.Path {
			parent := parentDir(m.cur.Path)
			if node := m.tree.Find(parent); node != nil {
				m.cur = node
				m.browserCursor = 0
				m.buildBrowserRows()
			} else {
				m.screen = scrRoots
			}
		} else {
			m.screen = scrRoots
		}
	case k.Type == tea.KeyRunes:
		switch string(k.Runes) {
		case "r": // re-scan the current root, refreshing cached results
			if m.tree != nil {
				root := m.tree.Path
				delete(m.treeCache, root)
				m.pendingRoot = root
				m.startJob(jobAnalyzeRoot, "Re-analyzing "+fsutil.DisplayPath(root))
			}
		case "o":
			if row, ok := m.currentBrowserRow(); ok {
				path := row.dirPath()
				if err := fsutil.Open(path); err != nil {
					m.openError(err)
				}
			}
		case "f":
			if row, ok := m.currentBrowserRow(); ok {
				if err := fsutil.RevealInFinder(row.dirPath()); err != nil {
					m.openError(err)
				}
			}
		case "d", "t":
			if row, ok := m.currentBrowserRow(); ok {
				path := row.dirPath()
				size := row.size()
				m.openConfirm("Move to Trash",
					[]string{fsutil.DisplayPath(path), "Size: " + units.Format(size),
						"", "The item will be in the Trash and recoverable."},
					func(m *model) { m.trashPaths([]string{path}) })
			}
		case "i":
			if row, ok := m.currentBrowserRow(); ok {
				if row.isFile {
					m.fileInfoModal(row.file)
				} else {
					m.dirInfoModal(row.dir)
				}
			}
		}
	}
	return m, nil
}

func (r browserRow) dirPath() string {
	if r.isFile {
		return r.file.Path
	}
	return r.dir.Path
}

func (r browserRow) size() int64 {
	if r.isFile {
		return r.file.Size
	}
	return r.dir.Size
}

func parentDir(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i <= 0 {
		return "/"
	}
	return p[:i]
}

func (m *model) fileInfoModal(f fsutil.FileInfo) {
	lines := []string{
		fsutil.DisplayPath(f.Path),
		"",
		"Size:     " + units.Format(f.Size),
		"Modified: " + units.Age(time.Unix(f.ModTime, 0)) + " (" + time.Unix(f.ModTime, 0).Format("2006-01-02 15:04") + ")",
		"Where:    " + analysis.Categorize(f.Path) + " · dev " + fmt.Sprint(f.Dev) + " inode " + fmt.Sprint(f.Ino),
	}
	m.openInfo("File info", lines)
}

func (m *model) dirInfoModal(d *scanner.DirNode) {
	lines := []string{
		fsutil.DisplayPath(d.Path),
		"",
		"Size:  " + units.Format(d.Size),
		"Files: " + humanCount(d.FileCount),
		"Dirs:  " + humanCount(d.DirCount),
	}
	m.openInfo("Folder info", lines)
}

// ---- file list ----

func (m *model) keyFileList(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case k.Type == tea.KeyUp || string(k.Runes) == "k":
		if m.flCursor > 0 {
			m.flCursor--
		}
	case k.Type == tea.KeyDown || string(k.Runes) == "j":
		if m.flCursor < len(m.flFiles)-1 {
			m.flCursor++
		}
	case k.Type == tea.KeyEsc, k.Type == tea.KeyBackspace:
		m.goBack()
	case k.Type == tea.KeySpace:
		if m.flCursor < len(m.flFiles) {
			p := m.flFiles[m.flCursor].Path
			m.flChecked[p] = !m.flChecked[p]
		}
	case k.Type == tea.KeyRunes:
		switch string(k.Runes) {
		case "o":
			if m.flCursor < len(m.flFiles) {
				fsutil.Open(m.flFiles[m.flCursor].Path)
			}
		case "f":
			if m.flCursor < len(m.flFiles) {
				fsutil.RevealInFinder(m.flFiles[m.flCursor].Path)
			}
		case "i":
			if m.flCursor < len(m.flFiles) {
				m.fileInfoModal(m.flFiles[m.flCursor])
			}
		case "d", "t", "D", "T":
			var targets []fsutil.FileInfo
			for _, f := range m.flFiles {
				if m.flChecked[f.Path] {
					targets = append(targets, f)
				}
			}
			if len(targets) == 0 && m.flCursor < len(m.flFiles) {
				targets = append(targets, m.flFiles[m.flCursor])
			}
			if len(targets) == 0 {
				return m, nil
			}
			var total int64
			var paths []string
			dbN := 0
			for _, t := range targets {
				total += t.Size
				paths = append(paths, t.Path)
				if fsutil.IsDatabaseFile(t.Path) {
					dbN++
				}
			}
			lines := []string{
				fmt.Sprintf("%d file(s), %s total", len(targets), units.Format(total)),
				"Recoverable from the Trash afterwards.", "",
			}
			if dbN > 0 {
				lines = append(lines, warnStyle.Render(fmt.Sprintf("⚠ %d database file(s) — often intentional backups or live data; verify before removing.", dbN)), "")
			}
			m.openConfirm("Move to Trash", lines, func(m *model) { m.trashPaths(paths) })
		}
	}
	return m, nil
}

// ---- downloads ----

func (m *model) keyDownloads(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	cats := m.dl.Categories
	switch {
	case k.Type == tea.KeyUp || string(k.Runes) == "k":
		if m.dlCursor > 0 {
			m.dlCursor--
		}
	case k.Type == tea.KeyDown || string(k.Runes) == "j":
		if m.dlCursor < len(cats)-1 {
			m.dlCursor++
		}
	case k.Type == tea.KeyEnter:
		if m.dlCursor < len(cats) {
			m.cameFrom = m.screen
			m.flTitle = "Downloads · " + cats[m.dlCursor].Name
			m.flFiles = append([]fsutil.FileInfo{}, cats[m.dlCursor].Files...)
			sort.Slice(m.flFiles, func(i, j int) bool { return m.flFiles[i].Size > m.flFiles[j].Size })
			m.flCursor = 0
			m.flChecked = map[string]bool{}
			m.screen = scrFileList
		}
	case k.Type == tea.KeyEsc:
		m.goBack()
	}
	return m, nil
}

// ---- duplicates setup ----

// keyDupSetup chooses where to scan: default roots, a custom path, or
// toggling whether dev folders (node_modules, vendor, Pods) are inspected.
func (m *model) keyDupSetup(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.dupSetupInput != nil {
		switch k.Type {
		case tea.KeyEsc:
			m.dupSetupInput = nil
		case tea.KeyEnter:
			raw := strings.TrimSpace(m.dupSetupInput.value())
			m.dupSetupInput = nil
			if raw == "" {
				return m, nil
			}
			p := fsutil.ExpandPath(raw)
			if err := scanner.CheckScannable(p); err != nil {
				m.openError(err)
				return m, nil
			}
			if st, err := fsutil.Lstat(p); err != nil || !st.IsDir() {
				m.openError(fmt.Errorf("%s is not a readable directory", p))
				return m, nil
			}
			m.dupRootsOverride = []string{p}
			m.startJob(jobDuplicates, "Finding duplicates in "+fsutil.DisplayPath(p))
		default:
			m.dupSetupInput.update(k)
		}
		return m, nil
	}
	switch {
	case k.Type == tea.KeyEsc:
		m.screen = scrMenu
	case k.Type == tea.KeyEnter:
		m.dupRootsOverride = nil
		m.startJob(jobDuplicates, "Finding duplicates")
	case k.Type == tea.KeyRunes:
		switch string(k.Runes) {
		case "p":
			m.dupSetupInput = newLineInput("path:")
		case "d":
			m.s.DupIncludeDev = !m.s.DupIncludeDev
			if err := settings.Save(m.s); err != nil {
				m.openError(err)
			}
			if m.s.DupIncludeDev {
				m.setStatus("dev folders will be scanned (node_modules, vendor, Pods)")
			} else {
				m.setStatus("dev folders excluded — use Stale Deps for those")
			}
		}
	}
	return m, nil
}

// ---- duplicates ----

type dupRow struct {
	groupIdx int
	fileIdx  int
	header   bool
}

func (m *model) buildDupRows() {
	m.dupRows = nil
	for gi, g := range m.dup.Groups {
		m.dupRows = append(m.dupRows, dupRow{groupIdx: gi, header: true})
		for fi := range g.Files {
			m.dupRows = append(m.dupRows, dupRow{groupIdx: gi, fileIdx: fi})
		}
	}
}

func (m *model) keyDuplicates(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch {
	case k.Type == tea.KeyUp || string(k.Runes) == "k":
		m.moveDup(-1)
	case k.Type == tea.KeyDown || string(k.Runes) == "j":
		m.moveDup(1)
	case k.Type == tea.KeyEsc, k.Type == tea.KeyBackspace:
		m.goBack()
	case k.Type == tea.KeySpace:
		if row, ok := m.currentDupRow(); ok && !row.header {
			g := m.dup.Groups[row.groupIdx]
			f := g.Files[row.fileIdx]
			m.dupChecked[f.Path] = !m.dupChecked[f.Path]
		}
	case k.Type == tea.KeyRunes:
		switch string(k.Runes) {
		case "s": // suggest: trash every copy except the oldest
			for _, g := range m.dup.Groups {
				keep := g.KeepIndex()
				for i, f := range g.Files {
					m.dupChecked[f.Path] = i != keep
				}
			}
			m.setStatus("suggested: keep oldest copy of each group — review, then d")
		case "c": // clear all checks
			m.dupChecked = map[string]bool{}
		case "o":
			if row, ok := m.currentDupRow(); ok && !row.header {
				fsutil.Open(m.dup.Groups[row.groupIdx].Files[row.fileIdx].Path)
			}
		case "f":
			if row, ok := m.currentDupRow(); ok && !row.header {
				fsutil.RevealInFinder(m.dup.Groups[row.groupIdx].Files[row.fileIdx].Path)
			}
		case "i":
			if row, ok := m.currentDupRow(); ok && !row.header {
				m.fileInfoModal(m.dup.Groups[row.groupIdx].Files[row.fileIdx])
			}
		case "d", "t", "D", "T":
			var paths []string
			var total int64
			dbN := 0
			for _, g := range m.dup.Groups {
				for _, f := range g.Files {
					if m.dupChecked[f.Path] {
						paths = append(paths, f.Path)
						total += f.Size
						if fsutil.IsDatabaseFile(f.Path) {
							dbN++
						}
					}
				}
			}
			if len(paths) == 0 {
				m.setStatus("nothing selected — space to mark copies, or s to suggest")
				return m, nil
			}
			lines := []string{
				fmt.Sprintf("%d file(s), %s reclaimable", len(paths), units.Format(total)),
				"Each group keeps its unmarked copies.", "",
			}
			if dbN > 0 {
				lines = append(lines, warnStyle.Render(fmt.Sprintf("⚠ %d database file(s) — often intentional backups or live data; verify before removing.", dbN)), "")
			}
			m.openConfirm("Move duplicate copies to Trash", lines,
				func(m *model) { m.trashPaths(paths) })
		}
	}
	return m, nil
}

func (m *model) currentDupRow() (dupRow, bool) {
	if m.dupCursor >= 0 && m.dupCursor < len(m.dupRows) {
		return m.dupRows[m.dupCursor], true
	}
	return dupRow{}, false
}

func (m *model) moveDup(delta int) {
	for i := m.dupCursor + delta; i >= 0 && i < len(m.dupRows); i += delta {
		if !m.dupRows[i].header {
			m.dupCursor = i
			return
		}
	}
}

// ---- stale project deps ----

func (m *model) depEntries() []devdeps.Entry {
	if m.deps == nil {
		return nil
	}
	entries := append([]devdeps.Entry{}, m.deps.Entries...)
	if m.depsByAge {
		sort.Slice(entries, func(i, j int) bool { return entries[i].LastActivity < entries[j].LastActivity })
	} else {
		sort.Slice(entries, func(i, j int) bool { return entries[i].Size > entries[j].Size })
	}
	return entries
}

func (m *model) keyDeps(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	entries := m.depEntries()
	switch {
	case k.Type == tea.KeyUp || string(k.Runes) == "k":
		if m.depsCursor > 0 {
			m.depsCursor--
		}
	case k.Type == tea.KeyDown || string(k.Runes) == "j":
		if m.depsCursor < len(entries)-1 {
			m.depsCursor++
		}
	case k.Type == tea.KeyEsc, k.Type == tea.KeyBackspace:
		m.goBack()
	case k.Type == tea.KeySpace:
		if m.depsCursor < len(entries) {
			p := entries[m.depsCursor].Path
			m.depsChecked[p] = !m.depsChecked[p]
		}
	case k.Type == tea.KeyRunes:
		e := devdeps.Entry{}
		if m.depsCursor < len(entries) {
			e = entries[m.depsCursor]
		}
		switch string(k.Runes) {
		case "/":
			m.depsByAge = !m.depsByAge
			if m.depsByAge {
				m.setStatus("sorted by last use (stalest first)")
			} else {
				m.setStatus("sorted by size (largest first)")
			}
		case "c": // clear selection
			m.depsChecked = map[string]bool{}
			m.setStatus("selection cleared")
		case "o":
			if e.Path != "" {
				fsutil.Open(e.Path)
			}
		case "f":
			if e.Path != "" {
				fsutil.RevealInFinder(e.Path)
			}
		case "i":
			if e.Path != "" {
				m.depInfoModal(e)
			}
		case "d", "t", "D", "T":
			var targets []devdeps.Entry
			for _, en := range entries {
				if m.depsChecked[en.Path] {
					targets = append(targets, en)
				}
			}
			if len(targets) == 0 && e.Path != "" {
				targets = append(targets, e)
			}
			if len(targets) == 0 {
				return m, nil
			}
			var total int64
			var paths []string
			hints := map[string]string{}
			inUse := 0
			for _, t := range targets {
				total += t.Size
				paths = append(paths, t.Path)
				hints[t.Reinstall] = t.Reinstall
				if !t.Stale {
					inUse++
				}
			}
			var hintList []string
			for h := range hints {
				hintList = append(hintList, h)
			}
			sort.Strings(hintList)
			lines := []string{
				fmt.Sprintf("%d folder(s) · %s", len(targets), units.Format(total)),
				"Reinstall afterwards: " + strings.Join(hintList, " / "),
				"Recoverable from the Trash.",
			}
			if inUse > 0 {
				lines = append(lines, warnStyle.Render(fmt.Sprintf("Note: %d of these are still IN USE (recently touched) — make sure those projects are closed.", inUse)))
			}
			m.openConfirm("Move dependency folders to Trash", lines,
				func(m *model) { m.trashPaths(paths) })
		}
	}
	return m, nil
}

func (m *model) depInfoModal(e devdeps.Entry) {
	status := "in use (recently touched)"
	if e.Stale {
		status = "stale — suggested for cleanup"
	}
	lines := []string{
		fsutil.DisplayPath(e.Path),
		"",
		"Size:       " + units.Format(e.Size) + fmt.Sprintf(" (%s files)", humanCount(e.Files)),
		"Kind:       " + e.Kind,
		"Last used:  " + e.Age() + " (" + time.Unix(e.LastActivity, 0).Format("2006-01-02") + ")",
		"Status:     " + status,
		"Project:    " + fsutil.DisplayPath(e.Project),
		"Reinstall:  " + e.Reinstall,
	}
	m.openInfo("Dependency folder", lines)
}

// ---- dashboard ----

func (m *model) keyDashboard(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.dash == nil {
		m.screen = scrMenu
		return m, nil
	}
	cats := m.dash.Categories
	switch {
	case k.Type == tea.KeyUp || string(k.Runes) == "k":
		if m.dashCursor > 0 {
			m.dashCursor--
		}
	case k.Type == tea.KeyDown || string(k.Runes) == "j":
		if m.dashCursor < len(cats)-1 {
			m.dashCursor++
		}
	case k.Type == tea.KeyEsc:
		m.screen = scrMenu
	case k.Type == tea.KeyEnter:
		if m.dashCursor >= len(cats) {
			return m, nil
		}
		switch cats[m.dashCursor].Kind {
		case "duplicates":
			m.cameFrom = m.screen
			m.dup = m.dash.Dup
			m.dupChecked = map[string]bool{}
			m.dupCursor = 0
			m.buildDupRows()
			m.screen = scrDuplicates
		case "downloads":
			m.cameFrom = m.screen
			m.dl = m.dash.Downloads
			m.dlCursor = 0
			m.screen = scrDownloads
		case "large":
			m.cameFrom = m.screen
			m.flTitle = "Large files"
			m.flFiles = m.dash.Large
			m.flCursor = 0
			m.flChecked = map[string]bool{}
			m.screen = scrFileList
		case "old":
			m.cameFrom = m.screen
			m.flTitle = "Old files"
			m.flFiles = m.dash.Old
			m.flCursor = 0
			m.flChecked = map[string]bool{}
			m.screen = scrFileList
		case "deps":
			if m.dash.Deps != nil {
				m.cameFrom = m.screen
				m.deps = m.dash.Deps
				m.depsChecked = map[string]bool{}
				m.depsCursor = 0
				m.depsByAge = true
				m.screen = scrDeps
			}
		case "devcache":
			m.cameFrom = m.screen
			m.cleanList = devRows(m.dash.Caches)
			m.cleanTitle = "Developer caches"
			m.clFilter = ""
			m.clCursor = 0
			m.screen = scrCleanup
		case "macos":
			if m.dash.Macos != nil {
				m.cameFrom = m.screen
				m.cleanList = macRows(m.dash.Macos)
				m.cleanTitle = "macOS — general cleanup"
				m.clFilter = ""
				m.clCursor = 0
				m.screen = scrCleanup
			}
		}
	case k.Type == tea.KeyRunes && string(k.Runes) == "r":
		m.startJob(jobDashboard, "Rebuilding dashboard")
	}
	return m, nil
}

// ---- cleanup ----

func (m *model) visibleCaches() []cleanEntry {
	if m.clFilter == "" {
		return m.cleanList
	}
	var out []cleanEntry
	for _, c := range m.cleanList {
		if strings.Contains(strings.ToLower(c.name), m.clFilter) {
			out = append(out, c)
		}
	}
	return out
}

func (m *model) keyCleanup(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	caches := m.visibleCaches()
	switch {
	case k.Type == tea.KeyUp || string(k.Runes) == "k":
		if m.clCursor > 0 {
			m.clCursor--
		}
	case k.Type == tea.KeyDown || string(k.Runes) == "j":
		if m.clCursor < len(caches)-1 {
			m.clCursor++
		}
	case k.Type == tea.KeyEsc, k.Type == tea.KeyBackspace:
		m.goBack()
	case k.Type == tea.KeyEnter, k.Type == tea.KeyRunes && string(k.Runes) == "i":
		if m.clCursor < len(caches) {
			c := caches[m.clCursor]
			lines := []string{
				c.path,
				"",
				"Size:   " + units.Format(c.size),
				"Safety: " + c.safety,
			}
			if c.detail != "" {
				lines = append(lines, "Note:   "+c.detail)
			}
			if c.cmdText != "" {
				lines = append(lines, "Runs:   "+c.cmdText)
			}
			m.openInfo("Cleanup target", lines)
		}
	case k.Type == tea.KeyRunes:
		switch string(k.Runes) {
		case "r":
			if m.cleanTitle == "macOS — general cleanup" {
				m.startJob(jobMacos, "Re-scanning macOS cleanup targets")
			} else {
				m.startJob(jobCleanupDetect, "Re-detecting developer caches")
			}
		case "c":
			if m.clCursor >= len(caches) {
				return m, nil
			}
			c := caches[m.clCursor]
			act := func(m *model) {
				send := m.send
				go func() {
					err := c.clean(m.runCtx, c.safety == devcache.Destructive)
					msg := cleanDoneMsg{name: c.name}
					if err != nil {
						msg.err = err.Error()
					}
					send(msg)
				}()
			}
			head := []string{
				c.name + " — " + units.Format(c.size),
				c.path,
			}
			if c.cmdText != "" {
				head = append(head, "Will run: "+c.cmdText)
			}
			switch c.safety {
			case devcache.Destructive:
				if c.name == "Trash" {
					head = append(head, "Everything currently in the Trash will be destroyed.")
				} else {
					head = append(head, c.detail)
				}
				m.openConfirmTyped("DESTRUCTIVE cleanup", append(head, "", "This data will NOT be recoverable."), act)
			case devcache.TrashOnly:
				head = append(head, "The folder will be moved to the Trash.")
				m.openConfirm("Move to Trash", append(head, "", "Recoverable from the Trash."), act)
			default:
				head = append(head, "Contents will be deleted; apps regenerate them.")
				m.openConfirm("Clean", append(head, "", "Regenerable files will be deleted."), act)
			}
		}
	}
	return m, nil
}

// ---- settings ----

func (m *model) keySettings(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The exclusions text input swallows keys while active.
	if m.settingsInput && m.pathInput != nil {
		switch k.Type {
		case tea.KeyEnter:
			raw := m.pathInput.value()
			var ex []string
			for _, part := range strings.Split(raw, ",") {
				if p := strings.TrimSpace(part); p != "" {
					ex = append(ex, p)
				}
			}
			m.s.Exclusions = ex
			m.pathInput = nil
			m.settingsInput = false
			m.setStatus("exclusions updated (press S to save)")
		case tea.KeyEsc:
			m.pathInput = nil
			m.settingsInput = false
		default:
			m.pathInput.update(k)
		}
		return m, nil
	}
	rows := 7
	switch {
	case k.Type == tea.KeyUp || string(k.Runes) == "k":
		if m.stCursor > 0 {
			m.stCursor--
		}
	case k.Type == tea.KeyDown || string(k.Runes) == "j":
		if m.stCursor < rows-1 {
			m.stCursor++
		}
	case k.Type == tea.KeyEsc:
		m.screen = scrMenu
	case k.Type == tea.KeySpace:
		switch m.stCursor {
		case 0: // large threshold
			m.s.LargeMinBytes = cycle(m.s.LargeMinBytes, []int64{100 << 20, 500 << 20, 1 << 30, 2 << 30, 5 << 30})
		case 1: // old days
			m.s.OldDays = cycleInt(m.s.OldDays, []int{30, 90, 180, 365})
		case 2: // dup min
			m.s.DupMinBytes = cycle(m.s.DupMinBytes, []int64{1 << 20, 10 << 20, 50 << 20, 100 << 20})
		case 3: // duplicates inside dev folders
			m.s.DupIncludeDev = !m.s.DupIncludeDev
		case 4: // dep stale days
			m.s.DepStaleDays = cycleInt(m.s.DepStaleDays, []int{30, 90, 180, 365})
		}
	case k.Type == tea.KeyRunes:
		switch string(k.Runes) {
		case "e":
			m.pathInput = newLineInput("exclusions (comma-separated):")
			m.pathInput.buf = []rune(strings.Join(m.s.Exclusions, ","))
			m.pathInput.cursor = len(m.pathInput.buf)
			m.settingsInput = true
		case "S":
			if err := settings.Save(m.s); err != nil {
				m.openError(err)
			} else {
				m.setStatus("saved to " + settings.Path())
			}
		}
	}
	return m, nil
}

func cycle(cur int64, opts []int64) int64 {
	for i, v := range opts {
		if v == cur {
			return opts[(i+1)%len(opts)]
		}
	}
	return opts[0]
}

func cycleInt(cur int, opts []int) int {
	for i, v := range opts {
		if v == cur {
			return opts[(i+1)%len(opts)]
		}
	}
	return opts[0]
}

// screenTitle returns the one-line header for the current screen.
func (m *model) screenTitle() string {
	switch m.screen {
	case scrMenu:
		return "MacClean"
	case scrRoots:
		return "Analyze — choose a starting point"
	case scrScan:
		return m.progress.label
	case scrBrowser:
		if m.cur != nil {
			return fsutil.DisplayPath(m.cur.Path)
		}
		return "Analyze"
	case scrFileList:
		return m.flTitle
	case scrDownloads:
		return "Downloads"
	case scrDuplicates:
		return "Duplicate files"
	case scrDupSetup:
		return "Duplicates — scan where?"
	case scrDeps:
		return "Project dependencies"
	case scrDashboard:
		return "Dashboard"
	case scrCleanup:
		if m.cleanTitle != "" {
			return m.cleanTitle
		}
		if m.clFilter != "" {
			return "Docker"
		}
		return "Developer caches"
	case scrSettings:
		return "Settings"
	}
	return "MacClean"
}
