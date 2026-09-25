// Package duplicates finds files with identical content using a staged
// funnel that avoids reading most of the disk:
//
//  1. Group by exact size; singleton groups are dropped.
//  2. Drop hardlinks (same device+inode) — one physical file costs its
//     bytes once no matter how many names it has.
//  3. Group by partial hash (head + tail 64 KB).
//  4. Confirm with a full streaming SHA-256, only for surviving groups.
package duplicates

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"

	"macclean/internal/fsutil"
)

// Progress reports funnel activity.
type Progress struct {
	Phase        string // "grouping", "hashing (quick)", "hashing (full)", "done"
	FilesTotal   int
	FilesChecked int
	BytesHashed  int64
	CurrentFile  string
	Done         bool
	Cancelled    bool
}

// Group is one set of byte-identical files.
type Group struct {
	Size  int64            `json:"size"`
	Hash  string           `json:"hash"`
	Files []fsutil.FileInfo `json:"files"`
}

// KeepIndex returns the suggested survivor: the oldest-modified copy.
// Suggestion only — the UI never auto-deletes.
func (g Group) KeepIndex() int {
	best := 0
	for i, f := range g.Files {
		if f.ModTime < g.Files[best].ModTime {
			best = i
		}
	}
	return best
}

// Reclaimable is (unique copies - 1) * size.
func (g Group) Reclaimable() int64 {
	return g.Size * int64(len(g.Files)-1)
}

// Result is the outcome of a duplicate search.
type Result struct {
	Groups      []Group `json:"groups"`
	Reclaimable int64   `json:"reclaimable"`
	Scanned     int     `json:"scanned"`             // files considered
	SizeGroups  int     `json:"sizeGroups"`          // groups after size stage
	HashedFiles int     `json:"hashedFiles"`         // files fully hashed
	BytesHashed int64   `json:"bytesHashed"`         // bytes fully hashed
	Cancelled   bool    `json:"cancelled"`
}

// Find detects duplicate content among files. It expects files from one or
// more scan roots; hardlinks across roots are recognized and excluded.
func Find(ctx context.Context, files []fsutil.FileInfo, minBytes int64, progress func(Progress)) (*Result, error) {
	r := &Result{}
	var reportMu sync.Mutex
	report := func(phase, current string, checked int, done, cancelled bool) {
		if progress == nil {
			return
		}
		reportMu.Lock()
		progress(Progress{
			Phase: phase, FilesTotal: r.Scanned, FilesChecked: checked,
			BytesHashed: r.BytesHashed, CurrentFile: current,
			Done: done, Cancelled: cancelled,
		})
		reportMu.Unlock()
	}

	// Stage 0: candidates by size.
	bySize := map[int64][]fsutil.FileInfo{}
	for _, f := range files {
		if f.Size < minBytes || f.Size == 0 {
			continue
		}
		bySize[f.Size] = append(bySize[f.Size], f)
	}
	r.Scanned = len(files)

	type bucket struct {
		size  int64
		files []fsutil.FileInfo
	}
	var sizeBuckets []bucket
	for size, group := range bySize {
		if len(group) < 2 {
			continue
		}
		// Hardlinks: same dev+ino is one physical file.
		type key struct{ dev, ino uint64 }
		seen := map[key]bool{}
		var uniq []fsutil.FileInfo
		for _, f := range group {
			k := key{f.Dev, f.Ino}
			if seen[k] {
				continue
			}
			seen[k] = true
			uniq = append(uniq, f)
		}
		if len(uniq) < 2 {
			continue
		}
		sizeBuckets = append(sizeBuckets, bucket{size: size, files: uniq})
		r.SizeGroups++
	}
	sort.Slice(sizeBuckets, func(i, j int) bool { return sizeBuckets[i].size > sizeBuckets[j].size })
	if len(sizeBuckets) == 0 {
		report("done", "", 0, true, false)
		return r, nil
	}

	// Flatten candidates for the hashing stages.
	var candidates []fsutil.FileInfo
	for _, b := range sizeBuckets {
		candidates = append(candidates, b.files...)
	}

	workers := hashWorkers()

	// Stage 1: partial hash (head+tail 64 KB).
	report("hashing (quick)", "", 0, false, false)
	partials := parallelHash(ctx, workers, candidates, partialDigest, func(n int64) {
		atomic.AddInt64(&r.BytesHashed, n)
	}, func(i int, cur string) {
		report("hashing (quick)", cur, i, false, false)
	})
	if ctx.Err() != nil {
		r.Cancelled = true
		report("done", "", len(candidates), true, true)
		return r, nil
	}

	byPartial := map[string][]int{}
	for i, d := range partials {
		if d != "" {
			byPartial[d] = append(byPartial[d], i)
		}
	}
	var fullCandidates []fsutil.FileInfo
	for _, idxs := range byPartial {
		if len(idxs) >= 2 {
			for _, i := range idxs {
				fullCandidates = append(fullCandidates, candidates[i])
			}
		}
	}

	// Stage 2: full SHA-256.
	report("hashing (full)", "", 0, false, false)
	r.HashedFiles = len(fullCandidates)
	r.BytesHashed = 0
	fulls := parallelHash(ctx, workers, fullCandidates, fullDigest, func(n int64) {
		atomic.AddInt64(&r.BytesHashed, n)
	}, func(i int, cur string) {
		report("hashing (full)", cur, i, false, false)
	})
	if ctx.Err() != nil {
		r.Cancelled = true
		report("done", "", len(fullCandidates), true, true)
		return r, nil
	}

	byFull := map[string][]fsutil.FileInfo{}
	for i, d := range fulls {
		if d != "" {
			byFull[d] = append(byFull[d], fullCandidates[i])
		}
	}
	for _, group := range byFull {
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].Path < group[j].Path })
		g := Group{Size: group[0].Size, Hash: shortHash(group), Files: group}
		r.Groups = append(r.Groups, g)
		r.Reclaimable += g.Reclaimable()
	}
	sort.Slice(r.Groups, func(i, j int) bool { return r.Groups[i].Reclaimable() > r.Groups[j].Reclaimable() })
	report("done", "", len(fullCandidates), true, false)
	return r, nil
}

func shortHash(group []fsutil.FileInfo) string {
	h := sha256.New()
	for _, f := range group {
		fmt.Fprintf(h, "%d:%s\n", f.Size, f.Path)
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

func hashWorkers() int {
	n := runtime.NumCPU()
	if n < 2 {
		return 2
	}
	if n > 8 {
		return 8
	}
	return n
}

// digestFn computes a digest, reporting bytes read. It returns "" when the
// file cannot be read (treated as "no duplicate" rather than an error,
// because unreadable files cannot be cleaned anyway). It honors ctx between
// chunks so multi-gigabyte reads cancel promptly.
type digestFn func(ctx context.Context, path string, size int64, onBytes func(int64)) string

// partialHashLen is the head and tail sample size.
const partialHashLen = 64 * 1024

func partialDigest(ctx context.Context, path string, size int64, onBytes func(int64)) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	head := make([]byte, min64(partialHashLen, size))
	n, err := io.ReadFull(f, head)
	if err != nil && n == 0 {
		return ""
	}
	h.Write(head[:n])
	onBytes(int64(n))
	if size > partialHashLen {
		tail := make([]byte, min64(partialHashLen, size-int64(n)))
		m, err := f.ReadAt(tail, size-int64(len(tail)))
		if err != nil && m == 0 {
			return ""
		}
		h.Write(tail[:m])
		onBytes(int64(m))
	}
	return string(h.Sum(nil))
}

func fullDigest(ctx context.Context, path string, size int64, onBytes func(int64)) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1024*1024)
	for {
		if ctx.Err() != nil {
			return ""
		}
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			onBytes(int64(n))
		}
		if err != nil {
			break
		}
	}
	return string(h.Sum(nil))
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// parallelHash maps digestFn over files with a worker pool. Digests are
// returned in input order. Cancellation short-circutes promptly; remaining
// entries are "".
func parallelHash(ctx context.Context, workers int, files []fsutil.FileInfo, fn digestFn, onBytes func(int64), onFile func(i int, current string)) []string {
	out := make([]string, len(files))
	type item struct {
		idx int
		f   fsutil.FileInfo
	}
	var wg sync.WaitGroup
	work := make(chan item)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range work {
				if ctx.Err() != nil {
					out[it.idx] = ""
					continue
				}
				onFile(it.idx, it.f.Path)
				out[it.idx] = fn(ctx, it.f.Path, it.f.Size, onBytes)
			}
		}()
	}
	for i, f := range files {
		if ctx.Err() != nil {
			break
		}
		work <- item{idx: i, f: f}
	}
	close(work)
	wg.Wait()
	return out
}
