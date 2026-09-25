package dashboard

import (
	"testing"

	"macclean/internal/duplicates"
	"macclean/internal/fsutil"
)

// The core promise: when categories point at the same physical files,
// Actual must be the union, not the sum.
func TestOverlapAccounting(t *testing.T) {
	// Simulate: one 1GB file that is large, old, and a Downloads installer;
	// plus a duplicate pair (2 × 500MB, reclaimable 500MB) where both copies
	// are also large.
	big := fsutil.FileInfo{Path: "/u/Downloads/x.dmg", Size: 1 << 30, Dev: 1, Ino: 100}
	dupA := fsutil.FileInfo{Path: "/u/Downloads/a.bin", Size: 500 << 20, Dev: 1, Ino: 201}
	dupB := fsutil.FileInfo{Path: "/u/Movies/a.bin", Size: 500 << 20, Dev: 1, Ino: 202}

	r := &Report{}
	// Build a claim set exactly like Build() does.
	seen := map[fileKey]string{}
	claim := func(name, kind string, files []fsutil.FileInfo, naive int64, items int) Category {
		cat := Category{Name: name, Gross: naive, Items: items, Kind: kind}
		local := claimSet{}
		for _, f := range files {
			addFile(local, f)
		}
		for k, bytes := range local {
			if _, taken := seen[k]; !taken {
				seen[k] = kind
				cat.Actual += bytes
			}
		}
		return cat
	}

	dupGroup := duplicates.Group{Size: 500 << 20, Files: []fsutil.FileInfo{dupA, dupB}}
	_ = dupGroup

	// duplicates: keep dupA (oldest), trash dupB → claims dupB only
	r.Categories = append(r.Categories, claim("Duplicates", "duplicates", []fsutil.FileInfo{dupB}, 500<<20, 1))
	// downloads cleanup: big + dupB
	r.Categories = append(r.Categories, claim("Downloads cleanup", "downloads", []fsutil.FileInfo{big, dupB}, (1<<30)+(500<<20), 2))
	// large: all three
	r.Categories = append(r.Categories, claim("Large files", "large", []fsutil.FileInfo{big, dupA, dupB}, (1<<30)+(500<<20)+(500<<20), 3))
	// old: big only
	r.Categories = append(r.Categories, claim("Old files", "old", []fsutil.FileInfo{big}, 1<<30, 1))

	for _, c := range r.Categories {
		r.Gross += c.Gross
		r.Actual += c.Actual
	}

	wantActual := int64((1 << 30) + (500 << 20) + (500 << 20)) // big + dupA + dupB, each once
	if r.Actual != wantActual {
		t.Fatalf("Actual = %d, want %d (each physical file once)", r.Actual, wantActual)
	}
	if r.Gross != (1<<30)*3+(500<<20)*4 {
		t.Fatalf("Gross = %d", r.Gross)
	}
	if r.Gross <= r.Actual {
		t.Fatal("gross should exceed actual in this scenario")
	}
}
