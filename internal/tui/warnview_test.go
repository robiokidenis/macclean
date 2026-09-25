package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestMenuShowsRiskWarning(t *testing.T) {
	m := newTestModel()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	view := m.View()
	if !strings.Contains(view, "own risk") || !strings.Contains(view, "permanently delete data") {
		t.Fatalf("menu must show the at-your-own-risk warning:\n%s", view)
	}
	if strings.Contains(view, "risiko") || strings.Contains(view, "Perhatian") {
		t.Fatal("repo-facing text must be English-only")
	}
}
