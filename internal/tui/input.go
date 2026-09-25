package tui

import (
	"strings"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// lineInput is a minimal single-line text editor for path entry.
type lineInput struct {
	prompt string
	buf    []rune
	cursor int
}

func newLineInput(prompt string) *lineInput {
	return &lineInput{prompt: prompt}
}

func (l *lineInput) value() string { return string(l.buf) }

func (l *lineInput) update(msg tea.Msg) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return
	}
	switch key.Type {
	case tea.KeyRunes, tea.KeySpace:
		for _, r := range key.Runes {
			l.buf = append(l.buf, r)
			l.cursor++
		}
	case tea.KeyBackspace:
		if l.cursor > 0 {
			l.buf = append(l.buf[:l.cursor-1], l.buf[l.cursor:]...)
			l.cursor--
		}
	case tea.KeyLeft:
		if l.cursor > 0 {
			l.cursor--
		}
	case tea.KeyRight:
		if l.cursor < len(l.buf) {
			l.cursor++
		}
	case tea.KeyCtrlU:
		l.buf = nil
		l.cursor = 0
	}
}

func (l *lineInput) view(width int) string {
	text := string(l.buf)
	before := len(string(l.buf[:l.cursor]))
	after := text[before:]
	cursorChar := " "
	if l.cursor < len(text) {
		cursorChar = string(l.buf[l.cursor])
		after = text[before+len(cursorChar):]
	}
	return inputStyle.Render(l.prompt) + " " +
		cursorStyle.Render(string(l.buf[:l.cursor])+cursorChar) + after +
		"  " + helpStyle.Render("enter ⏎ · esc cancel")
}

// modalKind distinguishes overlay dialogs.
type modalKind int

const (
	modalInfo modalKind = iota
	modalConfirm
	modalConfirmTyped // destructive: must type "yes"
	modalError
)

type modalState struct {
	kind    modalKind
	title   string
	lines   []string
	onYes   func(m *model)
	typed   string
	needs   string // text required for modalConfirmTyped
}

func (m *model) openInfo(title string, lines []string) {
	m.modal = &modalState{kind: modalInfo, title: title, lines: lines}
}

func (m *model) openConfirm(title string, lines []string, onYes func(m *model)) {
	m.modal = &modalState{kind: modalConfirm, title: title, lines: lines, onYes: onYes}
}

func (m *model) openConfirmTyped(title string, lines []string, onYes func(m *model)) {
	m.modal = &modalState{kind: modalConfirmTyped, title: title, lines: lines, onYes: onYes, needs: "yes"}
}

func (m *model) openError(err error) {
	m.modal = &modalState{kind: modalError, title: "Error", lines: strings.Split(err.Error(), "\n")}
}

func (m *model) closeModal() { m.modal = nil }

func (m *model) modalWidth() int {
	w := 60
	for _, l := range m.modal.lines {
		if len(l) > w && len(l) < 100 {
			w = len(l)
		}
	}
	if m.width-4 < w {
		w = m.width - 4
	}
	return w
}

func (m *model) renderModal() string {
	if m.modal == nil {
		return ""
	}
	mod := m.modal
	var body []string
	title := modalTitleStyle.Render(" " + mod.title + " ")
	for _, l := range mod.lines {
		body = append(body, modalBodyStyle.Render(l))
	}
	switch mod.kind {
	case modalInfo, modalError:
		body = append(body, "", helpStyle.Render("enter/esc · close"))
	case modalConfirm:
		body = append(body, "", helpStyle.Render("y = proceed · esc = cancel"))
	case modalConfirmTyped:
		body = append(body, "",
			modalBodyStyle.Render("Type "+mod.needs+" : ")+cursorStyle.Render(mod.typed+"▏"),
			helpStyle.Render("enter = proceed · esc = cancel"))
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(accentColor).
		Padding(1, 2).
		Width(m.modalWidth()).
		Render(lipgloss.JoinVertical(lipgloss.Left, title, strings.Join(body, "\n")))
	return overlayCenter(m.width, m.height, box)
}

func overlayCenter(w, h int, box string) string {
	lines := strings.Split(box, "\n")
	bw := 0
	for _, l := range lines {
		if lipgloss.Width(l) > bw {
			bw = lipgloss.Width(l)
		}
	}
	padTop := (h - len(lines)) / 2
	if padTop < 0 {
		padTop = 0
	}
	padLeft := (w - bw) / 2
	if padLeft < 0 {
		padLeft = 0
	}
	blank := strings.Repeat(" ", w)
	var out []string
	for i := 0; i < padTop; i++ {
		out = append(out, blank)
	}
	for _, l := range lines {
		out = append(out, strings.Repeat(" ", padLeft)+l)
	}
	return lipgloss.JoinVertical(lipgloss.Left, out...)
}
