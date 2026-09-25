package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"macclean/internal/settings"
)

func settingsPath() string { return settings.Path() }

func containsDupIncludeDevTrue(raw string) bool {
	var s struct {
		DupIncludeDev bool `json:"dupIncludeDev"`
	}
	if json.Unmarshal([]byte(raw), &s) != nil {
		return false
	}
	return s.DupIncludeDev
}

// The setup screen decides where to scan: default roots on enter, a typed
// custom path, and the dev-folder toggle persists.
func TestDupSetupScreen(t *testing.T) {
	home, _ := os.UserHomeDir()
	dir, err := os.MkdirTemp(home, ".macclean-dupsetup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	m := newTestModel()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m.screen = scrDupSetup

	// enter → job starts (progress screen), scanning default roots.
	m.Update(enterKey())
	if m.screen != scrScan {
		t.Fatalf("screen = %v, want scan", m.screen)
	}

	// d toggles the dev-folder exclusion and persists it.
	m.screen = scrDupSetup
	if m.s.DupIncludeDev {
		t.Fatal("dev folders must be excluded by default")
	}
	m.Update(key("d"))
	if !m.s.DupIncludeDev {
		t.Fatal("d should include dev folders")
	}
	onDisk := readSettingsDupDev(t)
	if !onDisk {
		t.Fatal("toggle should persist to settings.json")
	}
	m.Update(key("d"))
	if m.s.DupIncludeDev {
		t.Fatal("d should toggle back")
	}

	// p → path input; a valid path starts a targeted scan.
	m.Update(key("p"))
	if m.dupSetupInput == nil {
		t.Fatal("p should open the path input")
	}
	for _, r := range dir {
		m.dupSetupInput.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m.Update(enterKey())
	if m.screen != scrScan || m.dupRootsOverride == nil || m.dupRootsOverride[0] != dir {
		t.Fatalf("custom path should start a targeted scan, screen=%v override=%v", m.screen, m.dupRootsOverride)
	}

	// Invalid path → error modal, no scan.
	m.screen = scrDupSetup
	m.dupRootsOverride = nil
	m.Update(key("p"))
	for _, r := range filepath.Join(dir, "does-not-exist") {
		m.dupSetupInput.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m.Update(enterKey())
	if m.modal == nil || m.screen != scrDupSetup {
		t.Fatal("invalid path should show an error, not start a scan")
	}
}

func readSettingsDupDev(t *testing.T) bool {
	t.Helper()
	data, err := os.ReadFile(settings.Path())
	if err != nil {
		t.Fatal(err)
	}
	return containsDupIncludeDevTrue(string(data))
}
