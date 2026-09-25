// Package devcache detects developer caches and build artifacts, sizes
// them, and knows the safe cleanup action for each.
//
// Safety classes follow the product rule:
//   - Safe: regenerable caches and build artifacts. Native cleanup command
//     or direct removal, after a normal confirmation.
//   - TrashOnly: user-visible data; moved to Trash, never rm'd.
//   - Destructive: data that does not regenerate (archives, simulators,
//     volumes, SDKs). Typed confirmation required.
package devcache

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"macclean/internal/fsutil"
	"macclean/internal/scanner"
)

// Safety labels displayed with every entry.
const (
	Safe       = "safe"
	TrashOnly  = "trash"
	Destructive = "destructive"
)

// Cache is one detected developer cache or artifact directory.
type Cache struct {
	Name     string   `json:"name"`
	Path     string   `json:"path"`
	Size     int64    `json:"size"`
	Safety   string   `json:"safety"`
	Detail   string   `json:"detail,omitempty"`
	Cleanup  []string `json:"cleanupCommand,omitempty"` // native command, if any
	Exists   bool     `json:"exists"`
	Error    string   `json:"error,omitempty"`
}

// Detector declares one thing to look for.
type Detector struct {
	Name     string
	Dir      string // "~"-relative or absolute path
	Safety   string
	Detail   string
	Cleanup  []string
}

// Detectors is the catalog of known developer caches.
var Detectors = []Detector{
	{Name: "Xcode DerivedData", Dir: "~/Library/Developer/Xcode/DerivedData", Safety: Safe, Detail: "Build artifacts; Xcode regenerates on next build."},
	{Name: "Xcode Archives", Dir: "~/Library/Developer/Xcode/Archives", Safety: Destructive, Detail: "Released build archives. Cannot be regenerated."},
	{Name: "Xcode iOS DeviceSupport", Dir: "~/Library/Developer/Xcode/iOS DeviceSupport", Safety: Safe, Detail: "Device symbol caches; re-created when a device is connected."},
	{Name: "Xcode Simulators (runtimes)", Dir: "~/Library/Developer/CoreSimulator/Caches/dyld", Safety: Safe, Detail: "Simulator dynamic-linker caches."},
	{Name: "iOS Simulator devices", Dir: "~/Library/Developer/CoreSimulator/Devices", Safety: Destructive, Detail: "Simulator devices and their data."},
	{Name: "Swift Package Manager", Dir: "~/Library/Caches/org.swift.swiftpm", Safety: Safe, Detail: "Resolved package caches."},
	{Name: "npm cache", Dir: "~/.npm", Safety: Safe, Detail: "Downloaded package tarballs.", Cleanup: []string{"npm", "cache", "clean", "--force"}},
	{Name: "yarn cache", Dir: "~/Library/Caches/yarn", Safety: Safe, Cleanup: []string{"yarn", "cache", "clean"}},
	{Name: "pnpm store", Dir: "~/Library/pnpm/store", Safety: Safe, Cleanup: []string{"pnpm", "store", "prune"}},
	{Name: "pip cache", Dir: "~/Library/Caches/pip", Safety: Safe, Cleanup: []string{"pip3", "cache", "purge"}},
	{Name: "uv cache", Dir: "~/Library/Caches/uv", Safety: Safe, Cleanup: []string{"uv", "cache", "clean"}},
	{Name: "Go build cache", Dir: "~/Library/Caches/go-build", Safety: Safe, Cleanup: []string{"go", "clean", "-cache"}},
	{Name: "Go module cache", Dir: "~/go/pkg/mod", Safety: Safe, Cleanup: []string{"go", "clean", "-modcache"}},
	{Name: "Gradle caches", Dir: "~/.gradle/caches", Safety: Safe},
	{Name: "Maven repository", Dir: "~/.m2/repository", Safety: Safe, Detail: "Re-downloads on demand."},
	{Name: "CocoaPods cache", Dir: "~/Library/Caches/CocoaPods", Safety: Safe, Cleanup: []string{"pod", "cache", "clean", "--all"}},
	{Name: "Cargo registry cache", Dir: "~/Library/Caches/cargo", Safety: Safe, Detail: "Registry index and .crate downloads."},
	{Name: "Hugging Face models", Dir: "~/.cache/huggingface", Safety: TrashOnly, Detail: "Downloaded model weights; large and slow to re-download."},
	{Name: "Playwright browsers", Dir: "~/Library/Caches/ms-playwright", Safety: Safe, Detail: "Re-download with: npx playwright install."},
	{Name: "Puppeteer Chrome", Dir: "~/.cache/puppeteer", Safety: Safe},
	{Name: "Turbo cache", Dir: "~/Library/Caches/turbo", Safety: Safe},
	{Name: "Homebrew cache", Dir: specialBrew, Safety: Safe, Detail: "Downloaded bottles.", Cleanup: []string{"brew", "cleanup", "--prune=all"}},
}

const specialBrew = "\x00brew"

// Detect finds and sizes all existing caches. Paths are sized with
// mini-scans; progress receives one line per detector for UI feedback.
func Detect(ctx context.Context, progress func(string)) []Cache {
	var out []Cache
	for _, d := range Detectors {
		if ctx.Err() != nil {
			break
		}
		if progress != nil {
			progress(d.Name)
		}
		switch {
		case d.Dir == specialBrew:
			out = append(out, detectBrew())
		default:
			out = append(out, detectDir(ctx, d))
		}
	}
	// Docker is its own world.
	if progress != nil {
		progress("Docker")
	}
	out = append(out, detectDocker(ctx)...)
	return out
}

func detectDir(ctx context.Context, d Detector) Cache {
	c := Cache{Name: d.Name, Safety: d.Safety, Detail: d.Detail, Cleanup: d.Cleanup}
	c.Path = fsutil.ExpandPath(d.Dir)
	st, err := fsutil.Lstat(c.Path)
	if err != nil || !st.IsDir() {
		return c
	}
	c.Exists = true
	res, err := scanner.Scan(ctx, c.Path, scanner.Options{Concurrency: 4, MinFileBytes: 1 << 62})
	if err != nil {
		c.Error = err.Error()
		return c
	}
	c.Size = res.TotalSize
	return c
}

func detectBrew() Cache {
	c := Cache{Name: "Homebrew cache", Safety: Safe, Detail: "Downloaded bottles.", Cleanup: Detectors[len(Detectors)-1].Cleanup}
	brew, err := exec.LookPath("brew")
	if err != nil {
		return c
	}
	out, err := exec.Command(brew, "--cache").Output()
	if err != nil {
		return c
	}
	c.Path = strings.TrimSpace(string(out))
	if st, err := fsutil.Lstat(c.Path); err != nil || !st.IsDir() {
		return c
	}
	c.Exists = true
	res, err := scanner.Scan(context.Background(), c.Path, scanner.Options{Concurrency: 4, MinFileBytes: 1 << 62})
	if err == nil {
		c.Size = res.TotalSize
	}
	return c
}

// dockerDFRow mirrors `docker system df --format '{{json .}}'` output.
type dockerDFRow struct {
	Type        string `json:"Type"`
	Reclaimable string `json:"Reclaimable"`
	Size        string `json:"Size"`
	Count       int    `json:"Count"`
}

// detectDocker reports Docker disk usage when the daemon is reachable, and
// falls back to the raw Docker.vmdk/Docker.raw disk size otherwise.
func detectDocker(ctx context.Context) []Cache {
	var out []Cache
	docker, err := exec.LookPath("docker")
	if err == nil {
		cmd := exec.CommandContext(ctx, docker, "system", "df", "--format", "{{json .}}")
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = nil
		if err := cmd.Run(); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				if line == "" {
					continue
				}
				var row dockerDFRow
				if json.Unmarshal([]byte(line), &row) != nil {
					continue
				}
				rec, ok := dockerRowToCache(row, docker)
				if ok {
					out = append(out, rec)
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	// Daemon not reachable: report the VM disk file size informationally.
	for _, candidate := range []string{
		"~/Library/Containers/com.docker.docker/Data/vms/0/data/Docker.raw",
		"~/Library/Containers/com.docker.docker/Data/vms/0/Docker.raw",
	} {
		p := fsutil.ExpandPath(candidate)
		if st, err := fsutil.Lstat(p); err == nil {
			out = append(out, Cache{
				Name:   "Docker VM disk",
				Path:   p,
				Size:   st.Size,
				Safety: Destructive,
				Detail: "Docker daemon unreachable; size shown is the whole VM disk. Start Docker for per-category reclaimable data.",
			})
			break
		}
	}
	return out
}

func dockerRowToCache(row dockerDFRow, docker string) (Cache, bool) {
	c := Cache{Path: "docker"}
	switch strings.ToLower(row.Type) {
	case "images":
		c.Name = "Docker images"
		c.Safety = Safe
		c.Cleanup = []string{docker, "image", "prune", "-a", "-f"}
	case "build cache":
		c.Name = "Docker build cache"
		c.Safety = Safe
		c.Cleanup = []string{docker, "builder", "prune", "-a", "-f"}
	case "local volumes":
		c.Name = "Docker volumes"
		c.Safety = Destructive
		c.Detail = "Volume data (databases etc.) is not recoverable."
		c.Cleanup = []string{docker, "volume", "prune", "-a", "-f"}
	case "containers":
		c.Name = "Docker stopped containers"
		c.Safety = Safe
		c.Cleanup = []string{docker, "container", "prune", "-f"}
	default:
		return Cache{}, false
	}
	c.Exists = true
	c.Size = parseDockerBytes(row.Size)
	c.Detail = strings.TrimSpace(c.Detail + " Reclaimable now: " + row.Reclaimable + ".")
	return c, true
}

// parseDockerBytes understands docker's human sizes ("1.2GB", "350MB").
func parseDockerBytes(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	var mult int64 = 1
	switch {
	case strings.HasSuffix(s, "GB"):
		mult = 1 << 30
	case strings.HasSuffix(s, "MB"):
		mult = 1 << 20
	case strings.HasSuffix(s, "kB"):
		mult = 1 << 10
	case strings.HasSuffix(s, "B"):
		mult = 1
	}
	num := strings.TrimRight(s, "GMk B")
	var f float64
	if _, err := fmt.Sscanf(num, "%f", &f); err != nil {
		return 0
	}
	return int64(f * float64(mult))
}

// Clean executes the cleanup for a cache. Native commands run as-is;
// directories without one are removed (Safe) or trashed (TrashOnly).
// Destructive entries require the caller to have collected an explicit
// confirmation — this function double-checks by refusing unless force is set.
func Clean(ctx context.Context, c Cache, force bool) error {
	if !c.Exists {
		return fmt.Errorf("%s: nothing detected", c.Name)
	}
	if c.Safety == Destructive && !force {
		return fmt.Errorf("%s is marked destructive; refusing without explicit confirmation", c.Name)
	}
	if len(c.Cleanup) > 0 {
		cmd := exec.CommandContext(ctx, c.Cleanup[0], c.Cleanup[1:]...)
		cmd.Stdout = nil
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	switch c.Safety {
	case Safe:
		if err := checkUnderHome(c.Path); err != nil {
			return err
		}
		if err := os.RemoveAll(c.Path); err != nil {
			return err
		}
		return os.MkdirAll(c.Path, 0o755)
	case TrashOnly, Destructive:
		return fsutil.MoveToTrash(c.Path)
	}
	return fmt.Errorf("unknown safety class %q", c.Safety)
}

// checkUnderHome refuses direct removal outside the user home, where
// developer caches do not live; anything there must go through Trash.
func checkUnderHome(path string) error {
	if strings.HasPrefix(path, fsutil.Home()+string(filepath.Separator)) {
		return nil
	}
	return fmt.Errorf("refusing direct removal of %s (outside home); use trash instead", path)
}
