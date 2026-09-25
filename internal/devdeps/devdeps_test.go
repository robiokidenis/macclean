package devdeps

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixture builds a hermetic fake HOME under the real home (TMPDIR lives
// under protected /var) containing projects and a fake nvm:
//   active-app/  package.json fresh, node_modules old   → NOT stale (project active)
//   dead-app/    package.json old,  node_modules old   → stale
//   shop/        composer.lock old, vendor old          → stale, composer hint
//   rustsvc/     Cargo.toml old, target old             → stale, cargo hint
//   locked/      yarn.lock fresh, node_modules old      → NOT stale, yarn hint
//   pinned/      .nvmrc "18" → protects nvm v18.17.0
//   nested: dead-app/node_modules/inner/node_modules    → must NOT be listed (pruned)
//
// Fake nvm: v20.11.0 = alias default; v18.17.0 = pinned by .nvmrc;
// v16.20.0 = unreferenced and old → the only stale toolchain version.
func fixture(t *testing.T) string {
	t.Helper()
	home, _ := os.UserHomeDir()
	root, err := os.MkdirTemp(home, ".macclean-deps-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	t.Setenv("HOME", root) // redirect fsutil.Home() for hermetic toolchain scan

	old := time.Now().AddDate(-1, 0, 0)
	fresh := time.Now().AddDate(0, 0, -3)
	touch := func(p string, at time.Time) {
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(rel string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	wf := func(rel string, size int) string {
		p := filepath.Join(root, rel)
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// active app: old node_modules but freshly edited manifest
	nm := mk("work/active-app/node_modules")
	wf("work/active-app/node_modules/left-pad-1.0.0.tgz", 1000)
	wf("work/active-app/package.json", 10)
	touch(wf("work/active-app/package-lock.json", 10), fresh)
	touch(nm, old)

	// dead app: everything old
	nm2 := mk("work/dead-app/node_modules")
	wf("work/dead-app/node_modules/left-pad-1.0.0.tgz", 2000)
	wf("work/dead-app/package.json", 10)
	wf("work/dead-app/package-lock.json", 10)
	// nested node_modules inside node_modules — pruned walker must skip
	mk("work/dead-app/node_modules/inner-project/node_modules")
	touch(nm2, old)
	touch(filepath.Join(root, "work", "dead-app"), old)
	touch(filepath.Join(root, "work", "dead-app", "package.json"), old)
	touch(filepath.Join(root, "work", "dead-app", "package-lock.json"), old)

	// laravel shop with vendor
	vendor := mk("work/shop/vendor")
	wf("work/shop/vendor/autoload.php", 3000)
	wf("work/shop/composer.json", 10)
	touch(vendor, old)
	touch(filepath.Join(root, "work", "shop"), old)
	touch(filepath.Join(root, "work", "shop", "composer.json"), old)

	// rust service with target
	target := mk("work/rustsvc/target")
	wf("work/rustsvc/target/debug_bin", 4000)
	wf("work/rustsvc/Cargo.toml", 10)
	touch(target, old)
	touch(filepath.Join(root, "work", "rustsvc"), old)
	touch(filepath.Join(root, "work", "rustsvc", "Cargo.toml"), old)

	// yarn project, fresh lockfile but old node_modules
	nm3 := mk("work/locked/node_modules")
	wf("work/locked/node_modules/pkg.tgz", 500)
	touch(wf("work/locked/yarn.lock", 10), fresh)
	touch(nm3, old)

	// project pinning node 18 via .nvmrc (major-only pin)
	mk("work/pinned")
	if err := os.WriteFile(filepath.Join(root, "work/pinned/.nvmrc"), []byte("18\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// fake nvm with three versions
	v18 := mk(".nvm/versions/node/v18.17.0")
	v20 := mk(".nvm/versions/node/v20.11.0")
	v16 := mk(".nvm/versions/node/v16.20.0")
	wf(".nvm/versions/node/v16.20.0/node-binary", 6000)
	mk(".nvm/alias")
	if err := os.WriteFile(filepath.Join(root, ".nvm/alias/default"), []byte("20.11.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	touch(v18, old)
	touch(v20, old)
	touch(v16, old)

	return filepath.Join(root, "work")
}

func TestFindStaleVsFresh(t *testing.T) {
	root := fixture(t)
	rep := Find(context.Background(), []string{root}, Options{StaleDays: 90, SizeAll: true})
	if len(rep.Entries) != 8 {
		t.Fatalf("want 8 entries (5 dep folders + 3 nvm versions), got %d:", len(rep.Entries))
		for _, e := range rep.Entries {
			t.Logf("  %s (%s)", e.Path, e.Kind)
		}
	}
	byPath := map[string]Entry{}
	for _, e := range rep.Entries {
		byPath[e.Path] = e
	}
	dep := func(rel string) Entry { return byPath[filepath.Join(root, rel)] }

	active := dep("active-app/node_modules")
	if active.Stale {
		t.Error("active-app must NOT be stale: package-lock.json is 3 days old")
	}
	if active.Reinstall != "npm ci" {
		t.Errorf("active-app hint = %q, want npm ci (package-lock.json)", active.Reinstall)
	}
	locked := dep("locked/node_modules")
	if locked.Stale {
		t.Error("locked must NOT be stale: yarn.lock is fresh")
	}
	if locked.Reinstall != "yarn install" {
		t.Errorf("locked hint = %q, want yarn install", locked.Reinstall)
	}
	dead := dep("dead-app/node_modules")
	if !dead.Stale {
		t.Error("dead-app must be stale (everything a year old)")
	}
	if dead.Files != 1 {
		t.Errorf("dead-app files = %d, want 1 (only the tgz; nested dirs add no files)", dead.Files)
	}
	shop := dep("shop/vendor")
	if !shop.Stale || shop.Kind != "vendor" {
		t.Errorf("shop vendor wrong: %+v", shop)
	}
	if shop.Reinstall != "composer install" {
		t.Errorf("shop hint = %q, want composer install", shop.Reinstall)
	}
	rust := dep("rustsvc/target")
	if rust.Reinstall != "cargo build" {
		t.Errorf("rust hint = %q, want cargo build", rust.Reinstall)
	}

	// nvm versions: home is redirected into the fixture, so ~/.nvm is fake.
	home, _ := os.UserHomeDir()
	v18 := byPath[filepath.Join(home, ".nvm/versions/node/v18.17.0")]
	if v18.Stale {
		t.Error("v18.17.0 is pinned by .nvmrc '18' — must NOT be stale")
	}
	v20 := byPath[filepath.Join(home, ".nvm/versions/node/v20.11.0")]
	if v20.Stale {
		t.Error("v20.11.0 is the nvm default — must NOT be stale")
	}
	v16 := byPath[filepath.Join(home, ".nvm/versions/node/v16.20.0")]
	if !v16.Stale {
		t.Error("v16.20.0 is unreferenced and old — must be stale")
	}
	if v16.Reinstall != "nvm install v16.20.0" {
		t.Errorf("v16 hint = %q, want 'nvm install v16.20.0'", v16.Reinstall)
	}
	if v16.Size != 6000 {
		t.Errorf("v16 size = %d, want 6000", v16.Size)
	}

	wantStale := dead.Size + shop.Size + rust.Size + v16.Size
	if rep.StaleSize != wantStale {
		t.Errorf("StaleSize = %d, want %d", rep.StaleSize, wantStale)
	}

	// Sorted stalest first: the four stale entries lead the list.
	staleSeen := 0
	for _, e := range rep.Entries {
		if !e.Stale {
			break
		}
		staleSeen++
	}
	if staleSeen != 4 {
		t.Errorf("stalest-first order broken: %d stale entries before first fresh", staleSeen)
	}
}

func TestFindCancelled(t *testing.T) {
	root := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep := Find(ctx, []string{root}, Options{StaleDays: 90})
	if len(rep.Entries) != 0 {
		t.Fatalf("cancelled search should return empty, got %d", len(rep.Entries))
	}
}

func TestSizeOnlyStale(t *testing.T) {
	root := fixture(t)
	rep := Find(context.Background(), []string{root}, Options{StaleDays: 90, SizeAll: false})
	for _, e := range rep.Entries {
		if !e.Stale && e.Size != 0 {
			t.Errorf("fresh entry %s should be unsized in SizeAll=false mode", e.Path)
		}
	}
}

// Progress must report real Done/Total so UI bars can be honest.
func TestProgressCountsAreReal(t *testing.T) {
	root := fixture(t)
	var last Progress
	var nonMonotonic bool
	prev := -1
	rep := Find(context.Background(), []string{root}, Options{
		StaleDays: 90, SizeAll: true,
		Progress: func(p Progress) {
			if p.Total > 0 {
				if p.Done < prev {
					nonMonotonic = true
				}
				prev = p.Done
				last = p
			}
		},
	})
	if nonMonotonic {
		t.Fatal("progress Done must never go backwards")
	}
	_ = rep
	if last.Total == 0 {
		t.Fatal("no measurable progress reported")
	}
	if last.Done != last.Total {
		t.Fatalf("final progress = %d/%d, want %d/%d", last.Done, last.Total, last.Total, last.Total)
	}
}
