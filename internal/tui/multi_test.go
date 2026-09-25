package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Fast typing can batch "jj" into one key event — each rune must register.
func TestBatchedRunes(t *testing.T) {
	m := newTestModel()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("jj")})
	if m.menuCursor != 2 {
		t.Fatalf("cursor = %d, want 2", m.menuCursor)
	}
	// Batched "jjq" still quits after moving.
	m2 := newTestModel()
	m2.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("jjq")})
	if m2.menuCursor != 2 {
		t.Fatalf("cursor = %d, want 2 (quit may follow; cursor must move first)", m2.menuCursor)
	}
}
