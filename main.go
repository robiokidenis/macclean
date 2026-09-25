// MacClean — see exactly what is using your disk.
//
// Guiding principle: never tell you the Mac is "dirty"; show real paths and
// real sizes, and let you decide. Nothing is deleted automatically.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/x/term"

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
	"macclean/internal/tui"
	"macclean/internal/units"
)

const version = "1.0.0"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		runTUI()
		return
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "analyze":
		cmdAnalyze(rest)
	case "large":
		cmdLarge(rest)
	case "downloads":
		cmdDownloads(rest)
	case "duplicates":
		cmdDuplicates(rest)
	case "deps":
		cmdDeps(rest)
	case "old":
		cmdOld(rest)
	case "scan":
		cmdScan(rest)
	case "dashboard":
		cmdDashboard(rest)
	case "cleanup":
		cmdCleanup(rest)
	case "macos":
		cmdMacos(rest)
	case "trash":
		cmdTrash(rest)
	case "version", "--version", "-v":
		fmt.Println("macclean " + version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`macclean — see exactly what is using your disk

⚠  USE AT YOUR OWN RISK — this tool can PERMANENTLY DELETE your data.
   PERHATIAN: gunakan dengan risiko Anda sendiri — alat ini dapat
   menghapus data Anda secara PERMANEN.
   Trash-emptying, destructive items and --hard removals are irreversible.
   Always review the paths shown before confirming. See "macclean help".

Usage:
  macclean                       interactive UI
  macclean analyze [paths]       largest folders and files (defaults to common user dirs)
  macclean large [paths]         files above a size threshold
  macclean downloads             ~/Downloads categorized for cleanup
  macclean duplicates [paths]    byte-identical files (size → partial hash → SHA-256)
  macclean deps [paths]          project dependency folders by last use (node_modules, vendor…)
  macclean old [paths]           files not modified for N days
  macclean scan [path]           combined report for one path
  macclean dashboard             combined overview with honest overlap accounting
  macclean cleanup               developer caches: detect, size, clean
  macclean macos                 general macOS junk: Trash, logs, app caches
  macclean trash <paths>         move files to the macOS Trash

Common flags:
  --json            machine-readable output
  --min-size 10MB   threshold for large/duplicates
  --days 180        age threshold for old
  --exclude NAME    skip directories with this name (repeatable)
  --top N           rows to show (default 15)
  --rescan          ignore the 15-minute result cache
  --concurrency N   scan workers (default auto)

Philosophy: real paths, real sizes, inspect before deleting. User files are
moved to the Trash, never rm'd. /System and friends are never touched.
`)
}

// ---- shared helpers ----

// valueFlags lists flags that consume a following value; booleans are
// everything else. Used to reorder "path --flag value" into
// "--flag value path" so the stdlib flag package sees them.
var valueFlags = map[string]bool{
	"top": true, "exclude": true, "min-size": true, "days": true,
	"dup-min-size": true, "concurrency": true, "clean": true,
}

// reorderFlagsMoves flags (and their values) in front of positionals.
func reorderFlags(args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" && a != "--" {
			name := strings.TrimLeft(a, "-")
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				flags = append(flags, a)
				continue
			}
			flags = append(flags, a)
			if valueFlags[name] && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	return append(flags, positional...)
}

type commonFlags struct {
	json        bool
	excludes    multiFlag
	concurrency int
	top         int
	rescan      bool
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

type runContext struct {
	fs     *flag.FlagSet
	c      *commonFlags
	s      settings.Settings
	ctx    context.Context
	cancel func()
}

// newRun builds a FlagSet, registers common flags plus the command's own
// via register, then parses. Command flags must be registered here so
// Parse sees them.
func newRun(name string, args []string, register func(fs *flag.FlagSet)) *runContext {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {}
	c := &commonFlags{}
	fs.BoolVar(&c.json, "json", false, "JSON output")
	fs.Var(&c.excludes, "exclude", "directory name to skip (repeatable)")
	fs.IntVar(&c.concurrency, "concurrency", 0, "scan workers")
	fs.IntVar(&c.top, "top", 15, "rows to show")
	fs.BoolVar(&c.rescan, "rescan", false, "ignore result cache")
	if register != nil {
		register(fs)
	}
	_ = fs.Parse(reorderFlags(args))
	ctx, cancel := contextCancelOnSignal()
	return &runContext{fs: fs, c: c, s: settings.Load(), ctx: ctx, cancel: cancel}
}

func contextCancelOnSignal() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		cancel()
	}()
	return ctx, func() { signal.Stop(sigs); cancel() }
}

func (r *runContext) finish() { r.cancel() }

func isTTY() bool { return term.IsTerminal(os.Stdout.Fd()) }

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// scanPaths scans each root, printing live progress on a TTY, and returns
// the combined retained file list plus per-root results.
func (r *runContext) scanPaths(paths []string, minFileBytes int64, extraExcludes ...[]string) ([]fsutil.FileInfo, []*scanner.Result) {
	var files []fsutil.FileInfo
	var results []*scanner.Result
	extra := []string{}
	for _, e := range extraExcludes {
		extra = append(extra, e...)
	}
	excludes := mergeExcludes(r.s.Exclusions, append(append([]string{}, r.c.excludes...), extra...))
	for _, p := range paths {
		root := fsutil.ExpandPath(p)
		cacheKey := fmt.Sprintf("scan-%s-min%d-ex%v-con%d", root, minFileBytes, excludes, r.c.concurrency)
		if !r.c.rescan {
			if meta := report.LoadCache(cacheKey); meta != nil {
				var res scanner.Result
				if json.Unmarshal(meta.Payload, &res) == nil {
					fmt.Fprintf(os.Stderr, "(%s: cached %s ago, --rescan to refresh)\n",
						fsutil.DisplayPath(root), units.Age(meta.SavedAt))
					files = append(files, res.Files...)
					results = append(results, &res)
					continue
				}
			}
		}
		res, err := r.scanOne(fsutil.DisplayPath(root), root, excludes, minFileBytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", fsutil.DisplayPath(root), err)
			continue
		}
		if res == nil {
			continue
		}
		report.SaveCache(cacheKey, res)
		files = append(files, res.Files...)
		results = append(results, res)
	}
	return files, results
}

func (r *runContext) scanOne(label, root string, excludes []string, minFileBytes int64) (*scanner.Result, error) {
	if err := scanner.CheckScannable(root); err != nil {
		return nil, err
	}
	st, err := fsutil.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("cannot access: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("not a directory")
	}
	tty := isTTY()
	res, err := scanner.Scan(r.ctx, root, scanner.Options{
		Concurrency:  r.c.concurrency,
		ExcludeNames: excludes,
		MinFileBytes: minFileBytes,
		Progress: func(p scanner.Progress) {
			if !tty {
				return
			}
			if p.Done {
				fmt.Fprint(os.Stderr, "\r"+strings.Repeat(" ", 110)+"\r")
				return
			}
			pct := 0.0
			if p.DirsDiscovered > 0 {
				pct = float64(p.DirsDone) / float64(p.DirsDiscovered) * 100
				if pct > 99.5 {
					pct = 99.5
				}
			}
			fmt.Fprintf(os.Stderr, "\rScanning %-24s %5.1f%%  %7s files  %8s   ",
				trunc(label, 24), pct,
				strconv.FormatInt(p.Files, 10), units.Format(p.Bytes))
		},
	})
	if err != nil {
		return nil, err
	}
	if res.Cancelled {
		fmt.Fprintln(os.Stderr, "cancelled")
		r.finish()
		os.Exit(130)
	}
	return res, nil
}

func confirm(prompt string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)
	rd := bufio.NewReader(os.Stderr)
	line, _ := rd.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}

func confirmTyped(prompt string) bool {
	fmt.Fprintf(os.Stderr, "%s\nType 'yes' to proceed: ", prompt)
	rd := bufio.NewReader(os.Stderr)
	line, _ := rd.ReadString('\n')
	return strings.TrimSpace(line) == "yes"
}

func rootsOrDefaults(paths []string) []string {
	if len(paths) > 0 {
		return paths
	}
	return analysis.ExistingUserRoots()
}

func mergeExcludes(setting, cli []string) []string {
	if len(setting) == 0 {
		return cli
	}
	return append(append([]string{}, setting...), cli...)
}

func sumOf(files []fsutil.FileInfo) int64 {
	var n int64
	for _, f := range files {
		n += f.Size
	}
	return n
}

func firstN[T any](s []T, n int) []T {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func jsonFiles(files []fsutil.FileInfo) []report.JSONFile {
	out := make([]report.JSONFile, 0, len(files))
	for _, f := range files {
		out = append(out, report.JSONFile{
			Path: fsutil.DisplayPath(f.Path), Size: f.Size, ModTime: f.ModTime,
			ModHuman: time.Unix(f.ModTime, 0).UTC().Format(time.RFC3339),
		})
	}
	return out
}

func jsonDupGroups(groups []duplicates.Group) []report.JSONGroup {
	out := make([]report.JSONGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, report.JSONGroup{Size: g.Size, Reclaimable: g.Reclaimable(), Files: jsonFiles(g.Files)})
	}
	return out
}

// ---- commands ----

func cmdAnalyze(args []string) {
	r := newRun("analyze", args, nil)
	defer r.finish()
	paths := rootsOrDefaults(r.fs.Args())

	if r.c.json {
		out := map[string]any{}
		_, results := r.scanPaths(paths, 1<<20)
		for _, res := range results {
			out[fsutil.DisplayPath(res.Root.Path)] = analyzeJSON(res)
		}
		report.EmitJSON(out)
		return
	}

	o := &report.Out{}
	report.PrintDiskHeader(o)
	if len(paths) > 1 {
		o.Raw("Largest folders")
		type row struct {
			path string
			size int64
		}
		var rows []row
		_, results := r.scanPaths(paths, 1<<20)
		for _, res := range results {
			rows = append(rows, row{fsutil.DisplayPath(res.Root.Path), res.TotalSize})
		}
		if len(rows) == 0 {
			fmt.Fprintln(os.Stderr, "nothing scanned")
			os.Exit(1)
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].size > rows[j].size })
		for _, rw := range rows {
			o.Line("  %8s  %s", units.Format(rw.size), rw.path)
		}
		o.Raw("")
		o.Raw("Drill in: macclean analyze <path>    Full UI: macclean")
		fmt.Println(o.String())
		return
	}
	failed := true
	for _, p := range paths {
		_, results := r.scanPaths([]string{p}, 1<<20)
		for _, res := range results {
			failed = false
			report.FolderReport(o, res.Root, res.Files, r.c.top)
		}
	}
	if failed {
		os.Exit(1)
	}
	fmt.Println(o.String())
}

func analyzeJSON(res *scanner.Result) any {
	type child struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		Size  int64  `json:"size"`
		Files int64  `json:"files"`
		Dirs  int64  `json:"dirs"`
	}
	subs := res.Root.SortedChildren()
	children := make([]child, 0, len(subs))
	for _, s := range subs {
		children = append(children, child{Name: s.Name, Path: s.Path, Size: s.Size, Files: s.FileCount, Dirs: s.DirCount})
	}
	return map[string]any{
		"totalSize": res.TotalSize,
		"files":     res.TotalFiles,
		"dirs":      res.TotalDirs,
		"children":  children,
		"elapsedMs": res.Elapsed.Milliseconds(),
	}
}

func cmdLarge(args []string) {
	var minSize string
	r := newRun("large", args, func(fs *flag.FlagSet) {
		fs.StringVar(&minSize, "min-size", "", "minimum size (e.g. 1GB)")
	})
	defer r.finish()
	min := r.s.LargeMinBytes
	if minSize != "" {
		min = mustSize(minSize)
	}

	files, _ := r.scanPaths(rootsOrDefaults(r.fs.Args()), min)
	large := analysis.LargeFiles(files, min)
	total := sumOf(large)
	if r.c.json {
		report.EmitJSON(map[string]any{
			"minSize": min, "count": len(large), "totalSize": total, "files": jsonFiles(large),
		})
		return
	}
	o := &report.Out{}
	report.FileListReport(o, fmt.Sprintf("Large files (≥ %s)", units.Format(min)), firstN(large, r.c.top), total)
	fmt.Println(o.String())
}

func cmdOld(args []string) {
	var daysFlag int
	r := newRun("old", args, func(fs *flag.FlagSet) {
		fs.IntVar(&daysFlag, "days", 0, "age in days")
	})
	defer r.finish()
	days := r.s.OldDays
	if daysFlag > 0 {
		days = daysFlag
	}
	files, _ := r.scanPaths(rootsOrDefaults(r.fs.Args()), 1<<20)
	old := analysis.OldFiles(files, days)
	total := sumOf(old)
	if r.c.json {
		report.EmitJSON(map[string]any{
			"days": days, "count": len(old), "totalSize": total, "files": jsonFiles(old),
		})
		return
	}
	o := &report.Out{}
	report.FileListReport(o, fmt.Sprintf("Files not modified for %d+ days", days), firstN(old, r.c.top), total)
	fmt.Println(o.String())
}

func cmdDownloads(args []string) {
	r := newRun("downloads", args, nil)
	defer r.finish()
	root := fsutil.ExpandPath("~/Downloads")
	if _, err := fsutil.Lstat(root); err != nil {
		fmt.Fprintln(os.Stderr, "~/Downloads not found")
		os.Exit(1)
	}
	_, results := r.scanPaths([]string{root}, 0)
	if len(results) == 0 {
		os.Exit(1)
	}
	res := results[0]
	rep := analysis.AnalyzeDownloads(root, res.Files, r.s.LargeMinBytes, r.s.OldDays)
	if r.c.json {
		cats := map[string]any{}
		for _, cat := range rep.Categories {
			cats[cat.Name] = map[string]any{"size": cat.Size, "count": len(cat.Files), "files": jsonFiles(cat.Files)}
		}
		report.EmitJSON(map[string]any{
			"path":             "~" + root[len(fsutil.Home()):],
			"totalSize":        rep.Total,
			"files":            len(rep.Files),
			"categories":       cats,
			"cleanupPotential": rep.CleanupPotential,
			"cleanupFiles":     jsonFiles(rep.CleanupPotentialSet),
		})
		return
	}
	o := &report.Out{}
	report.DownloadsReport(o, rep)
	fmt.Println(o.String())
}

func cmdDuplicates(args []string) {
	var minSize string
	var includeDev bool
	r := newRun("duplicates", args, func(fs *flag.FlagSet) {
		fs.StringVar(&minSize, "min-size", "", "minimum size (e.g. 10MB)")
		fs.BoolVar(&includeDev, "include-dev", false, "scan inside node_modules/vendor/Pods (structural duplicates)")
	})
	defer r.finish()
	min := r.s.DupMinBytes
	if minSize != "" {
		min = mustSize(minSize)
	}
	paths := r.fs.Args()
	if len(paths) == 0 {
		paths = r.s.DupRoots
	}
	devIn := includeDev || r.s.DupIncludeDev
	var excludes []string
	if !devIn {
		excludes = append(excludes, duplicates.DevFolderNames...)
	}
	excludes = append(excludes, r.c.excludes...)
	files, _ := r.scanPaths(paths, min, excludes)
	dup := r.findDuplicates(files, min)
	if r.c.json {
		report.EmitJSON(map[string]any{
			"roots": paths, "minSize": min, "scanned": dup.Scanned,
			"devIncluded": devIn,
			"reclaimable": dup.Reclaimable, "groups": jsonDupGroups(dup.Groups),
		})
		return
	}
	o := &report.Out{}
	if !devIn {
		o.Raw(dimNote("dev folders (node_modules, vendor, Pods) excluded — use --include-dev to scan inside them"))
	}
	report.DuplicatesReport(o, dup)
	fmt.Println(o.String())
}

func (r *runContext) findDuplicates(files []fsutil.FileInfo, min int64) *duplicates.Result {
	tty := isTTY()
	if tty {
		fmt.Fprintln(os.Stderr, "Finding duplicates…")
	}
	dup, err := duplicates.Find(r.ctx, files, min, func(p duplicates.Progress) {
		if !tty || p.Done {
			return
		}
		fmt.Fprintf(os.Stderr, "\r%-20s %6d/%d  %s   ", p.Phase, p.FilesChecked, p.FilesTotal,
			trunc(fsutil.DisplayPath(p.CurrentFile), 52))
	})
	if tty {
		fmt.Fprint(os.Stderr, "\r"+strings.Repeat(" ", 110)+"\r")
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "duplicate search failed: %v\n", err)
		os.Exit(1)
	}
	if r.ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "cancelled")
		os.Exit(130)
	}
	return dup
}

// cmdDeps lists project dependency folders ranked by last activity, so
// untouched projects' node_modules/vendor surfaces as cleanup candidates
// while recently used ones stay marked as in use.
func cmdDeps(args []string) {
	var daysFlag int
	var minSize string
	var clean string
	var hard bool
	r := newRun("deps", args, func(fs *flag.FlagSet) {
		fs.IntVar(&daysFlag, "days", 0, "staleness threshold in days")
		fs.StringVar(&minSize, "min-size", "", "skip folders smaller than this")
		fs.StringVar(&clean, "clean", "", "move matching folders out (substring of path); asks first")
		fs.BoolVar(&hard, "hard", false, "with --clean: delete instead of Trash")
	})
	defer r.finish()
	days := r.s.DepStaleDays
	if daysFlag > 0 {
		days = daysFlag
	}
	var minBytes int64
	if minSize != "" {
		minBytes = mustSize(minSize)
	}

	roots := r.fs.Args()
	if len(roots) == 0 {
		roots = r.s.DepRoots
		if len(roots) == 0 {
			roots = devdeps.DefaultRoots()
		}
	}
	expanded := make([]string, 0, len(roots))
	for _, p := range roots {
		expanded = append(expanded, fsutil.ExpandPath(p))
	}

	if isTTY() {
		fmt.Fprintln(os.Stderr, "Searching project dependency folders…")
	}
	rep := devdeps.Find(r.ctx, expanded, devdeps.Options{
		StaleDays: days,
		SizeAll:   true,
		MinSize:   minBytes,
		Progress: func(p devdeps.Progress) {
			if isTTY() {
				fmt.Fprintf(os.Stderr, "\r%-70s   ", trunc(p.Label, 70))
			}
		},
	})
	if isTTY() {
		fmt.Fprint(os.Stderr, "\r"+strings.Repeat(" ", 110)+"\r")
	}
	if r.ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "cancelled")
		os.Exit(130)
	}

	if clean != "" {
		// Only suggested (stale) entries are ever offered to --clean, so a
		// substring like "nvm" cannot sweep up in-use versions.
		var targets []devdeps.Entry
		for _, e := range rep.Stale {
			if strings.Contains(strings.ToLower(e.Path), strings.ToLower(clean)) {
				targets = append(targets, e)
			}
		}
		if len(targets) == 0 {
			fmt.Fprintf(os.Stderr, "no STALE dependency or toolchain matches %q\n", clean)
			os.Exit(1)
		}
		for _, e := range targets {
			fmt.Printf("%s (%s · last touched %s)\n  reinstall with: %s\n",
				fsutil.DisplayPath(e.Path), units.Format(e.Size), e.Age(), e.Reinstall)
			verb := "Move to Trash?"
			if hard {
				verb = "Delete permanently?"
			}
			if !confirm("  " + verb) {
				fmt.Println("  skipped")
				continue
			}
			var err error
			if hard {
				err = devdeps.HardRemove(r.ctx, []string{e.Path})
			} else {
				err = fsutil.MoveToTrash(e.Path)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "  failed: %v\n", err)
			} else if hard {
				fmt.Println("  deleted")
			} else {
				fmt.Println("  moved to Trash")
			}
		}
		return
	}

	if r.c.json {
		report.EmitJSON(map[string]any{
			"roots": expanded, "staleDays": days,
			"staleSize": rep.StaleSize, "freshSize": rep.FreshSize,
			"walkedDirs": rep.WalkedDirs,
			"entries":    rep.Entries,
		})
		return
	}

	o := &report.Out{}
	o.Raw(fmt.Sprintf("Dependencies & toolchains (older than %d days = suggested)", days))
	o.Raw("")
	o.Line("  %d items · stale %s (%d) · fresh %s (%d) · %d dirs walked",
		len(rep.Entries), units.Format(rep.StaleSize), len(rep.Stale), units.Format(rep.FreshSize), len(rep.Fresh), rep.WalkedDirs)
	if len(rep.Stale) > 0 {
		o.Raw("")
		o.Raw("Suggested — untouched projects (oldest first)")
		for _, e := range rep.Stale {
			o.Line("  %9s  %-14s  %s", units.Format(e.Size), e.Age(), fsutil.DisplayPath(e.Path))
			o.Line("  %9s  reinstall: %s", "", e.Reinstall)
		}
	}
	if len(rep.Fresh) > 0 {
		o.Raw("")
		o.Raw("Fresh — recently used (kept)")
		for _, e := range firstN(rep.Fresh, r.c.top) {
			o.Line("  %9s  %-14s  %s", units.Format(e.Size), e.Age(), fsutil.DisplayPath(e.Path))
		}
		if len(rep.Fresh) > r.c.top {
			o.Line("  … %d more", len(rep.Fresh)-r.c.top)
		}
	}
	fmt.Println(o.String())
}

func cmdScan(args []string) {
	var minSize string
	var daysFlag int
	var dupMin string
	r := newRun("scan", args, func(fs *flag.FlagSet) {
		fs.StringVar(&minSize, "min-size", "", "large-file threshold (e.g. 1GB)")
		fs.IntVar(&daysFlag, "days", 0, "old-file threshold in days")
		fs.StringVar(&dupMin, "dup-min-size", "", "duplicate minimum size; 0 disables duplicate search")
	})
	defer r.finish()
	min := r.s.LargeMinBytes
	if minSize != "" {
		min = mustSize(minSize)
	}
	days := r.s.OldDays
	if daysFlag > 0 {
		days = daysFlag
	}
	dmin := r.s.DupMinBytes
	if dupMin != "" {
		dmin = mustSize(dupMin)
	}
	paths := r.fs.Args()
	if len(paths) == 0 {
		paths = []string{"~/Downloads"}
	}
	for i, p := range paths {
		if i > 0 {
			fmt.Println()
		}
		files, results := r.scanPaths([]string{p}, 0)
		for _, res := range results {
			large := analysis.LargeFiles(files, min)
			old := analysis.OldFiles(files, days)
			var dup *duplicates.Result
			if dmin > 0 {
				dup = r.findDuplicates(files, dmin)
			}
			if r.c.json {
				report.EmitJSON(report.BuildScanJSON(fsutil.DisplayPath(res.Root.Path), res, large, old, dup, min, days))
				continue
			}
			o := &report.Out{}
			report.FolderReport(o, res.Root, res.Files, r.c.top)
			o.Raw("")
			report.FileListReport(o, fmt.Sprintf("Large files (≥ %s)", units.Format(min)), firstN(large, r.c.top), sumOf(large))
			o.Raw("")
			report.FileListReport(o, fmt.Sprintf("Not modified for %d+ days", days), firstN(old, r.c.top), sumOf(old))
			if dup != nil {
				o.Raw("")
				report.DuplicatesReport(o, dup)
			}
			fmt.Println(o.String())
		}
	}
}

func cmdDashboard(args []string) {
	r := newRun("dashboard", args, nil)
	defer r.finish()
	tty := isTTY()
	rep := dashboard.Build(r.ctx, r.s, func(p dashboard.Phase) {
		if !tty || p.Done {
			return
		}
		fmt.Fprintf(os.Stderr, "\r%-44s %5.1f%%   ", trunc(p.Label, 44), p.Fraction*100)
	})
	if tty {
		fmt.Fprint(os.Stderr, "\r"+strings.Repeat(" ", 110)+"\r")
	}
	if r.ctx.Err() != nil {
		fmt.Fprintln(os.Stderr, "cancelled")
		os.Exit(130)
	}
	if r.c.json {
		report.EmitJSON(rep)
		return
	}
	o := &report.Out{}
	report.PrintDiskHeader(o)
	o.Raw("Potentially reclaimable")
	o.Raw(strings.Repeat("─", 52))
	for _, cat := range rep.Categories {
		if cat.Actual <= 0 {
			continue
		}
		o.Line("  %-30s %9s", cat.Name, units.Format(cat.Actual))
	}
	o.Raw(strings.Repeat("─", 52))
	o.Line("  %-30s %9s", "Gross (categories summed)", units.Format(rep.Gross))
	o.Line("  %-30s %9s", "After overlap", units.Format(rep.Actual))
	o.Raw("")
	o.Raw("Run `macclean` for the interactive review, or the individual")
	o.Raw("commands (downloads, duplicates, large, old, cleanup) to inspect")
	o.Raw("exact paths before removing anything.")
	fmt.Println(o.String())
}

func cmdCleanup(args []string) {
	var clean string
	var skipConfirm bool
	r := newRun("cleanup", args, func(fs *flag.FlagSet) {
		fs.StringVar(&clean, "clean", "", "clean caches whose name contains this substring")
		fs.BoolVar(&skipConfirm, "yes", false, "skip confirmation (safe items only)")
	})
	defer r.finish()

	if isTTY() {
		fmt.Fprintln(os.Stderr, "Detecting developer caches…")
	}
	caches := devcache.Detect(r.ctx, func(string) {})
	if r.ctx.Err() != nil {
		os.Exit(130)
	}

	if clean == "" {
		if r.c.json {
			report.EmitJSON(caches)
			return
		}
		o := &report.Out{}
		o.Raw("Developer caches")
		o.Raw("")
		for _, cache := range caches {
			if !cache.Exists {
				continue
			}
			o.Line("  %9s  %-34s [%s]", units.Format(cache.Size), cache.Name, cache.Safety)
			o.Line("            %s", cache.Path)
		}
		o.Raw("")
		o.Raw("safe = regenerable · trash = moved to Trash · destructive = typed confirmation required")
		o.Raw("Clean one: macclean cleanup --clean <name>")
		fmt.Println(o.String())
		return
	}

	var matches []devcache.Cache
	for _, cache := range caches {
		if cache.Exists && strings.Contains(strings.ToLower(cache.Name), strings.ToLower(clean)) {
			matches = append(matches, cache)
		}
	}
	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "no detected cache matches %q\n", clean)
		os.Exit(1)
	}
	for _, cache := range matches {
		fmt.Printf("Cleaning %s (%s at %s)\n", cache.Name, units.Format(cache.Size), cache.Path)
		switch cache.Safety {
		case devcache.Destructive:
			if !confirmTyped(fmt.Sprintf("  %s is DESTRUCTIVE. %s", cache.Name, cache.Detail)) {
				fmt.Println("  skipped")
				continue
			}
			if err := devcache.Clean(r.ctx, cache, true); err != nil {
				fmt.Fprintf(os.Stderr, "  failed: %v\n", err)
			} else {
				fmt.Println("  done")
			}
		case devcache.TrashOnly:
			if !skipConfirm && !confirm(fmt.Sprintf("  Move %s to Trash?", cache.Name)) {
				fmt.Println("  skipped")
				continue
			}
			if err := devcache.Clean(r.ctx, cache, true); err != nil {
				fmt.Fprintf(os.Stderr, "  failed: %v\n", err)
			} else {
				fmt.Println("  moved to Trash")
			}
		default:
			if !skipConfirm && !confirm(fmt.Sprintf("  Delete %s (%s)?", cache.Name, units.Format(cache.Size))) {
				fmt.Println("  skipped")
				continue
			}
			if err := devcache.Clean(r.ctx, cache, false); err != nil {
				fmt.Fprintf(os.Stderr, "  failed: %v\n", err)
			} else {
				fmt.Println("  done")
			}
		}
	}
}

// cmdMacos scans and cleans general macOS junk (Trash, user logs, app
// caches, iOS backups). Safe items ask y/N; destructive ones need a typed
// "yes" — emptying the Trash is permanent, so it belongs in that class.
func cmdMacos(args []string) {
	var clean string
	var skipConfirm bool
	r := newRun("macos", args, func(fs *flag.FlagSet) {
		fs.StringVar(&clean, "clean", "", "clean the item whose name contains this substring; asks first")
		fs.BoolVar(&skipConfirm, "yes", false, "skip confirmation (safe items only)")
	})
	defer r.finish()

	if isTTY() {
		fmt.Fprintln(os.Stderr, "Scanning general macOS cleanup targets…")
	}
	items := macos.Scan(r.ctx, func(string) {})
	if r.ctx.Err() != nil {
		os.Exit(130)
	}

	if clean == "" {
		if r.c.json {
			report.EmitJSON(items)
			return
		}
		o := &report.Out{}
		o.Raw("macOS — general cleanup")
		o.Raw("")
		for _, it := range items {
			if !it.Exists {
				continue
			}
			o.Line("  %9s  %-22s [%s]", units.Format(it.Size), it.Name, it.Safety)
			o.Line("            %s", it.Path)
			if it.Detail != "" {
				o.Line("            %s", it.Detail)
			}
		}
		o.Raw("")
		o.Raw("safe = regenerable · destructive = typed confirmation required")
		o.Raw("Clean one: macclean macos --clean <name>   (e.g. --clean trash)")
		fmt.Println(o.String())
		return
	}

	var matches []macos.Item
	for _, it := range items {
		if it.Exists && strings.Contains(strings.ToLower(it.Name), strings.ToLower(clean)) {
			matches = append(matches, it)
		}
	}
	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "no macOS item matches %q\n", clean)
		os.Exit(1)
	}
	for _, it := range matches {
		fmt.Printf("%s (%s at %s)\n", it.Name, units.Format(it.Size), it.Path)
		switch it.Safety {
		case macos.Destructive:
			if !confirmTyped(fmt.Sprintf("  %s is DESTRUCTIVE. %s", it.Name, it.Detail)) {
				fmt.Println("  skipped")
				continue
			}
		default:
			if !skipConfirm && !confirm(fmt.Sprintf("  Clean %s (%s)?", it.Name, units.Format(it.Size))) {
				fmt.Println("  skipped")
				continue
			}
		}
		force := it.Safety == macos.Destructive
		if err := macos.Clean(r.ctx, it, force); err != nil {
			fmt.Fprintf(os.Stderr, "  failed: %v\n", err)
		} else {
			fmt.Println("  done")
		}
	}
}

func cmdTrash(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: macclean trash <path>…")
		os.Exit(2)
	}
	for _, p := range args {
		if err := fsutil.MoveToTrash(p); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", p, err)
			os.Exit(1)
		}
		fmt.Printf("%s → Trash\n", p)
	}
}

func mustSize(s string) int64 {
	v, err := units.ParseSize(s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad size %q: %v\n", s, err)
		os.Exit(2)
	}
	return v
}

func runTUI() {
	ctx, cancel := contextCancelOnSignal()
	defer cancel()
	if err := tui.Run(ctx, version); err != nil {
		fmt.Fprintf(os.Stderr, "ui error: %v\n", err)
		os.Exit(1)
	}
}

// dimNote renders a secondary explanation line for report headers.
func dimNote(s string) string { return s }
