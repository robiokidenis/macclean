// Package web serves the MacClean UI in a browser: same scanners, same
// safety model, same honest accounting as the TUI. The server binds
// 127.0.0.1 only and rejects foreign Host headers (DNS-rebinding guard).
//
// Actions are deliberately limited to "reveal in Finder" and "move to
// Trash" — both reversible. Destructive cleanups stay in the TUI/CLI
// where they can demand a typed confirmation.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"macclean/internal/fsutil"
	"macclean/internal/report"
	"macclean/internal/settings"
	"macclean/internal/sysinfo"
)

//go:embed static
var staticFS embed.FS

// Options configures Run.
type Options struct {
	Port int  // 0 = pick a free port
	Open bool // open the browser after listening
}

type server struct {
	settings settings.Settings
	jobs     *jobStore
	version  string
}

// Handler returns the HTTP handler (also used by tests).
func Handler(version string, s settings.Settings) http.Handler {
	sv := &server{settings: s, jobs: newJobStore(), version: version}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", sv.serveIndex)
	mux.HandleFunc("GET /static/", sv.serveStatic)
	mux.HandleFunc("GET /api/overview", sv.handleOverview)
	mux.HandleFunc("POST /api/jobs", sv.handleJobStart)
	mux.HandleFunc("GET /api/jobs/{id}", sv.handleJobGet)
	mux.HandleFunc("GET /api/jobs/latest", sv.handleJobLatest)
	mux.HandleFunc("GET /api/jobs/{id}/tree", sv.handleJobTree)
	mux.HandleFunc("GET /api/jobs/{id}/detail", sv.handleJobDetail)
	mux.HandleFunc("POST /api/reveal", sv.handleReveal)
	mux.HandleFunc("POST /api/trash", sv.handleTrash)
	return sv.guard(mux)
}

// Run serves until ctx is cancelled. It never returns on its own except
// on startup errors.
func Run(ctx context.Context, version string, s settings.Settings, o Options) error {
	h := Handler(version, s)
	addr := fmt.Sprintf("127.0.0.1:%d", o.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)
	fmt.Printf("macclean %s — web UI at %s\n", version, url)
	fmt.Println("Ctrl-C to stop. Actions: reveal in Finder, move to Trash (recoverable).")
	srv := &http.Server{Handler: h}
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		close(done)
	}()
	if o.Open {
		go func() {
			time.Sleep(300 * time.Millisecond) // let the listener settle
			_ = exec.Command("open", url).Start()
		}()
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done
	return nil
}

// guard rejects requests whose Host is not this server, so a remote page
// resolved to a rebound DNS name cannot reach the API from the browser.
func (sv *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := strings.Split(r.Host, ":")[0]
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			http.Error(w, "host not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- static ----

func (sv *server) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "index missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

func (sv *server) serveStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	data, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	}
	_, _ = w.Write(data)
}

// ---- API helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad JSON: " + err.Error()})
		return false
	}
	return true
}

func volumeDTOAt(path string) (volumeDTO, error) {
	v, err := sysinfo.RootVolume()
	if err != nil {
		return volumeDTO{}, err
	}
	return volumeDTO{Name: v.Name, Total: v.TotalBytes, Free: v.FreeBytes}, nil
}

// ---- endpoints ----

// handleOverview answers instantly from statfs — no scan needed.
func (sv *server) handleOverview(w http.ResponseWriter, r *http.Request) {
	v, err := volumeDTOAt("/")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": sv.version,
		"volume":  v,
	})
}

type jobReq struct {
	Kind       string   `json:"kind"`
	Path       string   `json:"path,omitempty"`
	Roots      []string `json:"roots,omitempty"`
	IncludeDev bool     `json:"includeDev,omitempty"`
	Force      bool     `json:"force,omitempty"`
}

func (sv *server) handleJobStart(w http.ResponseWriter, r *http.Request) {
	var req jobReq
	if !readJSON(w, r, &req) {
		return
	}
	if !jobKinds[req.Kind] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown kind " + req.Kind})
		return
	}
	var run func(ctx context.Context, j *job)
	switch req.Kind {
	case kindDashboard:
		run = func(ctx context.Context, j *job) { sv.runDashboard(ctx, j) }
	case kindAnalyze:
		path := req.Path
		if path == "" {
			path = "~"
		}
		run = func(ctx context.Context, j *job) { sv.runAnalyze(ctx, j, path, req.Force) }
	case kindDuplicates:
		run = func(ctx context.Context, j *job) { sv.runDuplicates(ctx, j, req.Roots, req.IncludeDev, req.Force) }
	case kindDeps:
		run = func(ctx context.Context, j *job) { sv.runDeps(ctx, j) }
	case kindDownloads:
		run = func(ctx context.Context, j *job) { sv.runDownloads(ctx, j, req.Force) }
	}
	j := sv.jobs.start(req.Kind, run)
	writeJSON(w, http.StatusAccepted, j.snapshot(false))
}

func (sv *server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	j := sv.jobs.get(r.PathValue("id"))
	if j == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such job"})
		return
	}
	writeJSON(w, http.StatusOK, j.snapshot(true))
}

func (sv *server) handleJobTree(w http.ResponseWriter, r *http.Request) {
	j := sv.jobs.get(r.PathValue("id"))
	if j == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such job"})
		return
	}
	q := r.URL.Query().Get("path")
	if q != "" {
		q = fsutil.ExpandPath(q)
	}
	dto, ok := j.tree(q)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "path not in scan"})
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

// handleJobLatest serves the newest job of ?kind= so a reloaded page can
// resume following (or immediately show) the last scan of that kind.
func (sv *server) handleJobLatest(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if !jobKinds[kind] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown kind " + kind})
		return
	}
	j := sv.jobs.latest(kind)
	if j == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such job"})
		return
	}
	writeJSON(w, http.StatusOK, j.snapshot(true))
}

// handleJobDetail serves the drill-down payload for one dashboard
// category (?kind=large|old|downloads|duplicates|deps|macos|devcache,
// &name= disambiguates per-item rows).
func (sv *server) handleJobDetail(w http.ResponseWriter, r *http.Request) {
	j := sv.jobs.get(r.PathValue("id"))
	if j == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such job"})
		return
	}
	kind := r.URL.Query().Get("kind")
	dto, ok := j.detailOf(kind, r.URL.Query().Get("name"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no detail for " + kind})
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

type pathReq struct {
	Path string `json:"path"`
}

func (sv *server) handleReveal(w http.ResponseWriter, r *http.Request) {
	var req pathReq
	if !readJSON(w, r, &req) || req.Path == "" {
		return
	}
	if err := fsutil.RevealInFinder(req.Path); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type trashReq struct {
	Paths       []string `json:"paths"`
	ConfirmedDB bool     `json:"confirmedDb"`
}

type trashResp struct {
	Moved         []string `json:"moved"`
	Failed        []struct {
		Path  string `json:"path"`
		Error string `json:"error"`
	} `json:"failed"`
	NeedDBConfirm []string `json:"needDbConfirm,omitempty"`
}

// handleTrash moves paths to the Finder Trash — never unlinks. Database
// files (a duplicate could be a live database) require a second request
// with confirmedDb, mirroring the TUI's extra warning for them.
func (sv *server) handleTrash(w http.ResponseWriter, r *http.Request) {
	var req trashReq
	if !readJSON(w, r, &req) || len(req.Paths) == 0 {
		return
	}
	resp := trashResp{Moved: []string{}}
	var pending []string
	for _, p := range req.Paths {
		if err := fsutil.CheckDeletable(fsutil.ExpandPath(p)); err != nil {
			resp.Failed = append(resp.Failed, trashErr(p, err.Error()))
			continue
		}
		if !req.ConfirmedDB && fsutil.IsDatabaseFile(fsutil.ExpandPath(p)) {
			resp.NeedDBConfirm = append(resp.NeedDBConfirm, p)
			continue
		}
		pending = append(pending, p)
	}
	if len(resp.NeedDBConfirm) > 0 {
		writeJSON(w, http.StatusPreconditionFailed, resp)
		return
	}
	for _, p := range pending {
		if err := fsutil.MoveToTrash(p); err != nil {
			resp.Failed = append(resp.Failed, trashErr(p, err.Error()))
			continue
		}
		resp.Moved = append(resp.Moved, p)
	}
	if len(resp.Moved) > 0 {
		report.InvalidateCache()
	}
	writeJSON(w, http.StatusOK, resp)
}

func trashErr(p, msg string) struct {
	Path  string `json:"path"`
	Error string `json:"error"`
} {
	return struct {
		Path  string `json:"path"`
		Error string `json:"error"`
	}{Path: p, Error: msg}
}
