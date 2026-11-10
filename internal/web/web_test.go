package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"macclean/internal/dashboard"
	"macclean/internal/fsutil"
	"macclean/internal/settings"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(Handler("test", settings.Defaults()))
	t.Cleanup(srv.Close)
	return srv
}

// do performs a JSON request and decodes the JSON response body.
func do(t *testing.T, srv *httptest.Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, srv.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// fixture builds a small throwaway tree inside the home directory (temp
// dirs live under /var, which the scanner refuses) and registers cleanup.
func fixture(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp(fsutil.Home(), ".macclean-web-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]int{
		filepath.Join(dir, "big.bin"):   3 << 20,
		filepath.Join(dir, "small.txt"): 10,
		filepath.Join(sub, "nested.bin"): 2 << 20,
	}
	for p, n := range files {
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// waitForJob polls until the job is done and returns its snapshot.
func waitForJob(t *testing.T, srv *httptest.Server, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		code, m := do(t, srv, "GET", "/api/jobs/"+id, nil)
		if code != 200 {
			t.Fatalf("job get: %d %v", code, m)
		}
		if b, _ := m["done"].(bool); b {
			return m
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("job did not finish in time")
	return nil
}

func TestHostGuard(t *testing.T) {
	srv := newTestServer(t)
	for _, host := range []string{"127.0.0.1", "localhost", "127.0.0.1:9999"} {
		req, _ := http.NewRequest("GET", srv.URL+"/api/overview", nil)
		req.Host = host
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("host %q: got %d, want 200", host, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/overview", nil)
	req.Host = "evil.example.com"
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("rebound host: got %d, want 403", resp.StatusCode)
	}
}

func TestOverview(t *testing.T) {
	srv := newTestServer(t)
	code, m := do(t, srv, "GET", "/api/overview", nil)
	if code != 200 {
		t.Fatalf("got %d %v", code, m)
	}
	v, _ := m["volume"].(map[string]any)
	if total, _ := v["total"].(float64); total <= 0 {
		t.Errorf("volume.total = %v, want > 0", v["total"])
	}
}

func TestAnalyzeJobTreeAndTrash(t *testing.T) {
	srv := newTestServer(t)
	dir := fixture(t)

	// Start an analyze job on the fixture.
	code, m := do(t, srv, "POST", "/api/jobs", map[string]any{"kind": "analyze", "path": dir})
	if code != http.StatusAccepted {
		t.Fatalf("job start: %d %v", code, m)
	}
	id, _ := m["id"].(string)
	j := waitForJob(t, srv, id)
	if e, _ := j["error"].(string); e != "" {
		t.Fatalf("job error: %s", e)
	}

	// Tree drill-down: root contains "sub", sub contains nested.bin.
	code, m = do(t, srv, "GET", "/api/jobs/"+id+"/tree", nil)
	if code != 200 {
		t.Fatalf("tree: %d %v", code, m)
	}
	names := map[string]bool{}
	for _, d := range m["dirs"].([]any) {
		dm := d.(map[string]any)
		names[dm["name"].(string)] = true
	}
	if !names["sub"] {
		t.Errorf("sub not in tree dirs: %v", m["dirs"])
	}
	code, m = do(t, srv, "GET", "/api/jobs/"+id+"/tree?path="+filepath.Join(dir, "sub"), nil)
	if code != 200 {
		t.Fatalf("sub tree: %d %v", code, m)
	}
	if len(m["crumbs"].([]any)) < 2 {
		t.Errorf("crumbs too short: %v", m["crumbs"])
	}

	// Trash: ordinary file moves; a .db file demands confirmation first.
	big := filepath.Join(dir, "big.bin")
	db := filepath.Join(dir, "notes.db")
	if err := os.WriteFile(db, []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, m = do(t, srv, "POST", "/api/trash", map[string]any{"paths": []string{big}})
	if code != 200 {
		t.Fatalf("trash: %d %v", code, m)
	}
	if n := len(m["moved"].([]any)); n != 1 {
		t.Fatalf("moved = %v", m["moved"])
	}
	if _, err := os.Lstat(big); !os.IsNotExist(err) {
		t.Errorf("big.bin should be gone: %v", err)
	}
	_ = os.Remove(fsutil.TrashPath(big))

	code, m = do(t, srv, "POST", "/api/trash", map[string]any{"paths": []string{db}})
	if code != http.StatusPreconditionFailed {
		t.Fatalf("db without confirm: %d %v", code, m)
	}
	if need, _ := m["needDbConfirm"].([]any); len(need) != 1 {
		t.Fatalf("needDbConfirm = %v", m["needDbConfirm"])
	}
	code, m = do(t, srv, "POST", "/api/trash", map[string]any{"paths": []string{db}, "confirmedDb": true})
	if code != 200 {
		t.Fatalf("db with confirm: %d %v", code, m)
	}
	_ = os.Remove(fsutil.TrashPath(db))

	// Protected path is refused with a per-path failure entry.
	code, m = do(t, srv, "POST", "/api/trash", map[string]any{"paths": []string{"/System"}})
	if code != 200 {
		t.Fatalf("protected trash should 200 with a failed entry, got %d", code)
	}
	if failed, _ := m["failed"].([]any); len(failed) != 1 {
		t.Fatalf("failed = %v, want the /System entry", m["failed"])
	}
}

func TestUnknownJobKindRejected(t *testing.T) {
	srv := newTestServer(t)
	code, _ := do(t, srv, "POST", "/api/jobs", map[string]any{"kind": "rm-rf"})
	if code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", code)
	}
}

func TestDashboardDetailsCappedAndHonest(t *testing.T) {
	var files []fsutil.FileInfo
	for i := 0; i < detailFileCap+37; i++ {
		files = append(files, fsutil.FileInfo{Path: fmt.Sprintf("/tmp/f%d.bin", i), Size: 100, ModTime: 1, Dev: 1, Ino: uint64(i + 1)})
	}
	rep := &dashboard.Report{Large: files}
	d := dashboardDetails(rep)
	dto, ok := d["large"].(*listDetailDTO)
	if !ok {
		t.Fatalf("large detail missing: %T", d["large"])
	}
	if len(dto.Files) != detailFileCap {
		t.Errorf("sent %d rows, want cap %d", len(dto.Files), detailFileCap)
	}
	if !dto.Truncated {
		t.Error("truncated flag not set")
	}
	if dto.TotalCount != detailFileCap+37 {
		t.Errorf("totalCount = %d, want %d", dto.TotalCount, detailFileCap+37)
	}
	if dto.TotalBytes != int64(detailFileCap+37)*100 {
		t.Errorf("totalBytes must count every file even when truncated, got %d", dto.TotalBytes)
	}

	// Detail endpoint wiring: a job with details serves them, unknown kinds 404.
	j := &job{details: dashboardDetails(rep)}
	if _, ok := j.detailOf("nope", ""); ok {
		t.Error("unknown kind should not resolve")
	}
	got, ok := j.detailOf("large", "")
	if !ok {
		t.Fatal("large detail not served")
	}
	if got.(*listDetailDTO).TotalCount != detailFileCap+37 {
		t.Error("endpoint returned wrong payload")
	}
}
