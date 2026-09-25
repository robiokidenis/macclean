package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"macclean/internal/settings"
)

// ScreenshotView renders one screen's View() output for documentation
// images — no terminal needed. Colors are forced to ANSI-256 so the output
// matches what a real terminal shows. Currently supports the main menu,
// which is where the usage warning lives.
func ScreenshotView(name string, width, height int) string {
	lipgloss.SetColorProfile(termenv.ANSI256)
	m := &model{version: "1.0.0", runCtx: context.Background(), s: settings.Defaults()}
	m.send = func(tea.Msg) {}
	m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	switch name {
	case "menu":
		// default state is the menu
	default:
		return ""
	}
	return m.View()
}
