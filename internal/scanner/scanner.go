// Package scanner implements a concurrent, cancellable directory walker
// that builds a size-rolled-up tree plus a flat file listing for analysis.
//
// Design notes:
//   - Directories are processed by a worker pool (configurable concurrency).
//   - Symlinks are never followed and never contribute size.
//   - By default the walk does not cross filesystem boundaries (du -x-like);
//     each boundary is recorded as a skipped path.
//   - File identity (device + inode) is captured so later stages can treat
//     hardlinks as one physical file.
//   - Hashes are never computed here; hashing belongs to the duplicate
//     finder and only happens for candidate groups.
package scanner

import (
	"context"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"macclean/internal/fsutil"
)

// Options controls one scan.
type Options struct {
	// Concurrency is the worker count. 0 means auto.
	Concurrency int
	// ExcludeNames are directory names skipped entirely (e.g. ".git").
	ExcludeNames []string
	// MinFileBytes retains individual file records at or above this size.
	// Tree sizes always count every file; this only prunes the retained
	// per-file listings to keep memory bounded on huge trees.
	MinFileBytes int64
	// CrossFilesystems allows descending into other filesystems.
	CrossFilesystems bool
	// Progress, when set, receives periodic updates plus one final update.
	Progress      func(Progress)
	ProgressEvery time.Duration // default 250ms
}

// Progress is a snapshot of scan activity.
type Progress struct {
	Files          int64
	Bytes          int64
	DirsDone       int64
	DirsDiscovered int64
	Done           bool
	Cancelled      bool
	Err            error
}

// Error records a per-path failure (permission denied, etc.).
type Error struct {
	Path string `json:"path"`
	Msg  string `json:"msg"`
}

// DirNode is one directory in the rolled-up tree.
type DirNode struct {
	Path      string     `json:"path"`
	Name      string     `json:"name"`
	Size      int64      `json:"size"` // recursive sum of regular file sizes
	FileCount int64      `json:"fileCount"`
	DirCount  int64      `json:"dirCount"`
	ModTime   int64      `json:"modTime"`
	Children  []*DirNode `json:"children,omitempty"`
	Files     []fsutil.FileInfo `json:"-"` // retained files (>= MinFileBytes), unsorted
}

// SortedChildren returns child directories largest-first.
func (n *DirNode) SortedChildren() []*DirNode {
	out := make([]*DirNode, len(n.Children))
	copy(out, n.Children)
	sort.Slice(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	return out
}

// TopFiles returns the k largest retained files directly inside this dir.
func (n *DirNode) TopFiles(k int) []fsutil.FileInfo {
	out := make([]fsutil.FileInfo, len(n.Files))
	copy(out, n.Files)
	sort.Slice(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	if len(out) > k {
		out = out[:k]
	}
	return out
}

// Find locates a descendant by exact path, or nil.
func (n *DirNode) Find(path string) *DirNode {
	if n.Path == path {
		return n
	}
	for _, c := range n.Children {
		if r := c.Find(path); r != nil {
			return r
		}
	}
	return nil
}

// Result is the outcome of a scan.
type Result struct {
	Root       *DirNode
	Files      []fsutil.FileInfo // flat retained files across the whole tree
	TotalSize  int64
	TotalFiles int64 // all regular files seen, including unretained small ones
	TotalDirs  int64
	Skipped    []Error // permission errors, cross-FS boundaries (capped)
	Errors     int    // total error count including uncapped
	Elapsed    time.Duration
	Cancelled  bool
}

// Scan walks root concurrently. It returns a partial result if ctx is
// cancelled (Result.Cancelled = true).
func Scan(ctx context.Context, root string, opts Options) (*Result, error) {
	root = fsutil.ExpandPath(root)
	if err := CheckScannable(root); err != nil {
		return nil, err
	}
	st, err := fsutil.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, os.ErrInvalid
	}

	if opts.Concurrency <= 0 {
		opts.Concurrency = defaultConcurrency()
	}
	if opts.ProgressEvery <= 0 {
		opts.ProgressEvery = 250 * time.Millisecond
	}
	exclude := make(map[string]struct{}, len(opts.ExcludeNames))
	for _, n := range opts.ExcludeNames {
		exclude[n] = struct{}{}
	}

	w := &walker{
		ctx:        ctx,
		opts:       opts,
		exclude:    exclude,
		rootDev:    st.Dev,
		skippedCap: 500,
		started:    time.Now(),
	}
	w.cond = sync.NewCond(&w.mu)
	w.res = &Result{
		Root: &DirNode{Path: root, Name: baseName(root), ModTime: st.ModTime},
	}

	done := make(chan struct{})
	go w.monitor(done)

	w.spawn()
	for i := 0; i < opts.Concurrency; i++ {
		w.wg.Add(1)
		go w.work()
	}
	w.wg.Wait()
	close(done)

	w.res.Cancelled = ctx.Err() != nil
	if !w.res.Cancelled {
		rollup(w.res.Root)
		w.res.TotalSize = w.res.Root.Size
		w.res.TotalDirs = w.res.Root.DirCount
		w.res.TotalFiles = w.res.Root.FileCount
	}
	w.res.Elapsed = time.Since(w.started)

	if opts.Progress != nil {
		p := w.snapshot()
		p.Done = true
		p.Cancelled = w.res.Cancelled
		opts.Progress(p)
	}
	return w.res, nil
}

func defaultConcurrency() int {
	n := runtime.NumCPU()
	if n < 4 {
		return 4
	}
	if n > 16 {
		return 16
	}
	return n
}

// ScanRefused lists system internals that are never scannable.
var ScanRefused = []string{
	"/System", "/private", "/bin", "/sbin", "/usr", "/var", "/etc", "/dev", "/proc",
}

// CheckScannable rejects paths inside macOS system internals. User homes,
// /Applications, /Library, and mounted volumes under /Volumes may be scanned
// read-only when explicitly requested.
func CheckScannable(path string) error {
	abs := fsutil.ExpandPath(path)
	if abs == "/" {
		return nil
	}
	for _, root := range ScanRefused {
		if abs == root || hasPrefixDir(abs, root) {
			return &ScanRefusedError{Path: abs, Root: root}
		}
	}
	return nil
}

// ScanRefusedError explains a refused scan.
type ScanRefusedError struct{ Path, Root string }

func (e *ScanRefusedError) Error() string {
	return "refusing to scan " + e.Path + " (inside protected system area " + e.Root + ")"
}

func hasPrefixDir(path, prefix string) bool {
	return len(path) > len(prefix) && path[:len(prefix)] == prefix && path[len(prefix)] == '/'
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// ---- walker ----

type job struct {
	node *DirNode
	dev  uint64
}

type walker struct {
	ctx      context.Context
	opts     Options
	exclude  map[string]struct{}
	rootDev  uint64
	cond     *sync.Cond
	mu       sync.Mutex
	stack    []*job
	pending  int
	cancel   bool
	wg       sync.WaitGroup
	filesMu  sync.Mutex
	res      *Result
	skippedN int
	skippedCap int
	started  time.Time

	// atomics for progress
	aFiles    atomic.Int64
	aBytes    atomic.Int64
	aDirsDone atomic.Int64
	aDirsDisc atomic.Int64
}

func (w *walker) spawn() {
	w.push(&job{node: w.res.Root, dev: w.rootDev})
}

func (w *walker) push(j *job) {
	w.mu.Lock()
	w.stack = append(w.stack, j)
	w.pending++
	w.mu.Unlock()
	w.cond.Broadcast()
}

func (w *walker) markCancelled() {
	w.mu.Lock()
	w.cancel = true
	w.mu.Unlock()
	w.cond.Broadcast()
}

func (w *walker) isCancelled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cancel
}

func (w *walker) work() {
	defer w.wg.Done()
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

func (w *walker) process(j *job) {
	if w.ctx.Err() != nil {
		w.markCancelled()
		return
	}
	entries, err := os.ReadDir(j.node.Path)
	if err != nil {
		w.recordSkip(j.node.Path, err)
		return
	}
	var localSize int64
	var localFiles int64
	var localDirs int64
	for _, e := range entries {
		if w.ctx.Err() != nil {
			w.markCancelled()
			return
		}
		name := e.Name()
		full := j.node.Path + "/" + name
		st, err := fsutil.Lstat(full)
		if err != nil {
			w.recordSkip(full, err)
			continue
		}
		switch st.Mode & syscall.S_IFMT {
		case syscall.S_IFDIR:
			if _, skip := w.exclude[name]; skip {
				continue
			}
			if !w.opts.CrossFilesystems && st.Dev != w.rootDev {
				w.recordSkipNote(full, "different filesystem")
				continue
			}
			child := &DirNode{Path: full, Name: name, ModTime: st.ModTime}
			j.node.Children = append(j.node.Children, child)
			localDirs++
			w.aDirsDisc.Add(1)
			w.push(&job{node: child, dev: st.Dev})
		case syscall.S_IFREG:
			localSize += st.Size
			localFiles++
			w.aFiles.Add(1)
			w.aBytes.Add(st.Size)
			if st.Size >= w.opts.MinFileBytes {
				j.node.Files = append(j.node.Files, st)
				w.filesMu.Lock()
				w.res.Files = append(w.res.Files, st)
				w.filesMu.Unlock()
			}
		default:
			// symlinks, fifos, sockets, devices: no size contribution
		}
	}
	j.node.Size = localSize // children rolled up after traversal
	j.node.FileCount = localFiles
	j.node.DirCount = localDirs
	w.aDirsDone.Add(1)
}

// rollup sums child totals into parents, bottom-up.
func rollup(n *DirNode) {
	for _, c := range n.Children {
		rollup(c)
		n.Size += c.Size
		n.FileCount += c.FileCount
		n.DirCount += c.DirCount
	}
}

func (w *walker) recordSkip(path string, err error) {
	w.res.Errors++
	if w.skippedN < w.skippedCap {
		w.mu.Lock()
		w.res.Skipped = append(w.res.Skipped, Error{Path: path, Msg: err.Error()})
		w.skippedN++
		w.mu.Unlock()
	}
}

func (w *walker) recordSkipNote(path, note string) {
	w.res.Errors++
	if w.skippedN < w.skippedCap {
		w.mu.Lock()
		w.res.Skipped = append(w.res.Skipped, Error{Path: path, Msg: note})
		w.skippedN++
		w.mu.Unlock()
	}
}

func (w *walker) monitor(done <-chan struct{}) {
	if w.opts.Progress == nil {
		return
	}
	t := time.NewTicker(w.opts.ProgressEvery)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			w.opts.Progress(w.snapshot())
		}
	}
}

func (w *walker) snapshot() Progress {
	return Progress{
		Files:          w.aFiles.Load(),
		Bytes:          w.aBytes.Load(),
		DirsDone:       w.aDirsDone.Load(),
		DirsDiscovered: w.aDirsDisc.Load(),
	}
}
