package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"macclean/internal/devcache"
	"macclean/internal/fsutil"
	"macclean/internal/settings"
	"macclean/internal/sysinfo"
	"macclean/internal/units"
)

var (
	accentColor   = lipgloss.Color("39")  // blue
	goodColor     = lipgloss.Color("71")  // green
	warnColor     = lipgloss.Color("214") // orange
	dangerColor   = lipgloss.Color("196") // red
	dimColor      = lipgloss.Color("241")
	checkedColor  = lipgloss.Color("213")

	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231"))
	accentStyle = lipgloss.NewStyle().Foreground(accentColor)
	dimStyle    = lipgloss.NewStyle().Foreground(dimColor)
	goodStyle   = lipgloss.NewStyle().Foreground(goodColor)
	warnStyle   = lipgloss.NewStyle().Foreground(warnColor)
	dangerStyle = lipgloss.NewStyle().Foreground(dangerColor)
	cursorStyle = lipgloss.NewStyle().Bold(true).Reverse(true)
	checkedStyle = lipgloss.NewStyle().Foreground(checkedColor)
	helpStyle   = lipgloss.NewStyle().Foreground(dimColor)
	inputStyle  = lipgloss.NewStyle().Bold(true).Foreground(accentColor)

	modalTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231"))
	modalBodyStyle  = lipgloss.NewStyle()
	dbFlagStyle     = warnStyle.Bold(true)
	deletedStyle    = lipgloss.NewStyle().Faint(true).Strikethrough(true)
)

// View renders the current screen.
func (m *model) View() string {
	if m.width == 0 {
		return "loading…"
	}
	var body string
	switch m.screen {
	case scrMenu:
		body = m.viewMenu()
	case scrRoots:
		body = m.viewRoots()
	case scrScan:
		body = m.viewScan()
	case scrBrowser:
		body = m.viewBrowser()
	case scrFileList:
		body = m.viewFileList()
	case scrDownloads:
		body = m.viewDownloads()
	case scrDuplicates:
		body = m.viewDuplicates()
	case scrDupSetup:
		body = m.viewDupSetup()
	case scrDeps:
		body = m.viewDeps()
	case scrDashboard:
		body = m.viewDashboard()
	case scrCleanup:
		body = m.viewCleanup()
	case scrSettings:
		body = m.viewSettings()
	default:
		body = "?"
	}

	out := m.header() + "\n" + body
	footer := m.footerLine()
	if footer != "" {
		out += "\n" + footer
	}
	if m.modal != nil {
		out = m.header() + "\n\n" + m.renderModal()
	}
	// Trim to terminal height to avoid bubbletea scrolling artifacts.
	lines := strings.Split(out, "\n")
	if len(lines) > m.height {
		lines = lines[:m.height]
	}
	return strings.Join(lines, "\n")
}

func (m *model) header() string {
	vol, _ := sysinfo.RootVolume()
	left := titleStyle.Render(" MacClean ") + dimStyle.Render(" v"+m.version)
	right := dimStyle.Render(fmt.Sprintf("%s · %s free", vol.Name, units.Format(int64(vol.FreeBytes))))
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right) - 2
	if gap < 1 {
		gap = 1
	}
	title := accentStyle.Render("─" + strings.Repeat("─", 0)) // noop keeps import honest
	_ = title
	return left + strings.Repeat(" ", gap) + right + "\n" +
		accentStyle.Render(strings.Repeat("─", maxInt(1, m.width))) + "\n" +
		accentStyle.Bold(true).Render(" "+m.screenTitle()) + "\n"
}

func (m *model) footerLine() string {
	status := m.statusLine
	if status != "" {
		if time.Since(m.statusAt) > 4*time.Second {
			m.statusLine = ""
		} else {
			return goodStyle.Render(" " + status + " ")
		}
	}
	return helpStyle.Render(" " + m.keyHints())
}

func (m *model) keyHints() string {
	switch m.screen {
	case scrMenu:
		return "↑↓ move · enter open · q quit"
	case scrRoots:
		return "↑↓ move · enter analyze · type path at the last row · esc back"
	case scrScan:
		return "esc cancel"
	case scrBrowser:
		return "enter drill · backspace up · esc menu · o open · f finder · i info · d trash"
	case scrFileList:
		return "space select · d trash · o open · f finder · i info · esc/⌫ back"
	case scrDownloads:
		return "enter open category · esc back"
	case scrDuplicates:
		return "space mark · s suggest · c clear · d trash marked · o open · f finder · esc/⌫ back"
	case scrDupSetup:
		return "enter start · p custom folder · d toggle dev folders · esc back"
	case scrDeps:
		return "space select · d trash · i info (reinstall cmd) · / sort age|size · o open · f finder · esc/⌫ back"
	case scrDashboard:
		return "enter inspect · r rebuild · esc back"
	case scrCleanup:
		return "enter info · c clean · r re-detect · esc/⌫ back"
	case scrSettings:
		return "space cycle · e exclusions · S save · esc back"
	}
	return ""
}

// ---- menu ----

func (m *model) viewMenu() string {
	var rows []string
	for i, item := range menuItems {
		cursor := "  "
		label := item.label
		if i == m.menuCursor {
			cursor = accentStyle.Render("> ")
			label = cursorStyle.Render(label)
		} else {
			label = "  " + label
			cursor = ""
		}
		hint := ""
		if item.hint != "" && i == m.menuCursor {
			hint = dimStyle.Render("  — " + item.hint)
		}
		rows = append(rows, cursor+label+hint)
	}
	quote := dimStyle.Render("\n  “Don't tell me my Mac is dirty.")
	quote2 := dimStyle.Render("Show me exactly what is using my disk.”")
	warn := warnStyle.Render("\n  ⚠  Use at your own risk — cleanups can permanently delete data.")
	return strings.Join(rows, "\n") + "\n" + quote + "\n" + quote2 + "\n" + warn + "\n"
}

// ---- roots ----

func (m *model) viewRoots() string {
	var rows []string
	for i, r := range m.roots {
		display := fsutil.DisplayPath(r)
		if i == m.rootsCursor {
			rows = append(rows, accentStyle.Render("> ")+cursorStyle.Render(display))
		} else {
			rows = append(rows, "  "+display)
		}
	}
	custom := "  Custom path…"
	if m.rootsCursor == len(m.roots) {
		custom = accentStyle.Render("> ") + cursorStyle.Render("Custom path…")
	}
	rows = append(rows, custom)
	out := strings.Join(rows, "\n")
	if m.pathInput != nil {
		out += "\n\n" + m.pathInput.view(m.width)
	}
	return out + "\n"
}

// ---- scan ----

func (m *model) viewScan() string {
	p := m.progress
	bar := progressBar(p.frac, minInt(60, m.width-8))
	lines := []string{
		"",
		accentStyle.Render(p.label),
		"",
		bar,
		"",
		dimStyle.Render(p.detail),
		"",
	}
	// The footer already shows the key hints (esc cancel); no duplicate here.
	return strings.Join(lines, "\n") + "\n"
}

func progressBar(frac float64, width int) string {
	if width < 4 {
		width = 4
	}
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac * float64(width))
	bar := goodStyle.Render(strings.Repeat("█", filled)) +
		dimStyle.Render(strings.Repeat("░", width-filled))
	pct := fmt.Sprintf(" %3.0f%%", frac*100)
	return bar + pct
}

// ---- browser ----

func (m *model) viewBrowser() string {
	if m.cur == nil {
		return "no scan"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("\n  %s  total · %s files · %s dirs\n\n",
		units.Format(m.cur.Size), humanCount(m.cur.FileCount), humanCount(m.cur.DirCount)))

	filesStarted := false
	for i, row := range m.browserRows {
		if row.isFile && !filesStarted {
			filesStarted = true
			b.WriteString(dimStyle.Render("\n  Largest files\n"))
		}
		cursor := "  "
		if i == m.browserCursor {
			cursor = accentStyle.Render("▸ ")
		}
		var line string
		if row.isFile {
			line = fmt.Sprintf("%9s  %s", units.Format(row.file.Size), truncate(fsutil.DisplayPath(row.file.Path), m.width-16))
		} else {
			line = fmt.Sprintf("%9s  %s%s", units.Format(row.dir.Size), row.dir.Name+"/", dimStyle.Render(fmt.Sprintf("  (%s files)", humanCount(row.dir.FileCount))))
		}
		if i == m.browserCursor {
			line = cursorStyle.Render(strings.TrimLeft(line, " ")) 
			b.WriteString(cursor + line + "\n")
		} else {
			b.WriteString(cursor + line + "\n")
		}
	}
	if len(m.browserRows) == 0 {
		b.WriteString(dimStyle.Render("  (empty)\n"))
	}
	return b.String()
}

// ---- file list ----

func (m *model) viewFileList() string {
	var total int64
	for _, f := range m.flFiles {
		total += f.Size
	}
	head := fmt.Sprintf("\n  %d files · %s\n", len(m.flFiles), units.Format(total))
	checked := 0
	var b strings.Builder
	b.WriteString(head)
	start, end := m.window(len(m.flFiles), m.flCursor)
	for i := start; i < end; i++ {
		f := m.flFiles[i]
		mark := " "
		style := lipgloss.NewStyle()
		if m.flChecked[f.Path] {
			mark = checkedStyle.Render("[x]")
			checked++
		} else {
			mark = dimStyle.Render("[ ]")
		}
		flag := ""
		if fsutil.IsDatabaseFile(f.Path) {
			flag = "  " + dbFlagStyle.Render("⚠db")
		}
		line := fmt.Sprintf("%9s  %s  %s%s", units.Format(f.Size), units.Age(time.Unix(f.ModTime, 0)), truncate(fsutil.DisplayPath(f.Path), m.width-34), flag)
		if !fileExists(f.Path) {
			line = deletedStyle.Render(line) + dimStyle.Render("  · deleted")
		} else if i == m.flCursor {
			line = cursorStyle.Render(line)
		} else {
			line = style.Render(line)
		}
		b.WriteString("  " + mark + " " + line + "\n")
	}
	if checked > 0 {
		var sum int64
		for _, f := range m.flFiles {
			if m.flChecked[f.Path] {
				sum += f.Size
			}
		}
		b.WriteString("\n" + checkedStyle.Render(fmt.Sprintf("  %d selected · %s", checked, units.Format(sum))) + "\n")
		b.WriteString(warnStyle.Render(fmt.Sprintf("  ► press d to move %d file(s) (%s) to Trash", checked, units.Format(sum))) + "\n")
	}
	return b.String()
}

// window returns [start,end) of visible rows around the cursor.
func (m *model) window(n, cursor int) (int, int) {
	visible := m.height - 10
	if visible < 5 {
		visible = 5
	}
	if visible > n {
		return 0, n
	}
	start := cursor - visible/2
	if start < 0 {
		start = 0
	}
	if start+visible > n {
		start = n - visible
	}
	return start, start + visible
}

// ---- downloads ----

func (m *model) viewDownloads() string {
	if m.dl == nil {
		return "no data"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("\n  %s total · %d files\n\n", units.Format(m.dl.Total), len(m.dl.Files)))
	for i, c := range m.dl.Categories {
		line := fmt.Sprintf("  %-14s %9s  (%d files)", c.Name, units.Format(c.Size), len(c.Files))
		if i == m.dlCursor {
			b.WriteString(accentStyle.Render("▸ ") + cursorStyle.Render(line[2:]) + "\n")
		} else {
			b.WriteString(line + "\n")
		}
	}
	b.WriteString("\n" + warnStyle.Render(fmt.Sprintf("  Potential cleanup: %s", units.Format(m.dl.CleanupPotential))) +
		dimStyle.Render("  (installers, archives, disk images, old files)") + "\n")
	b.WriteString(dimStyle.Render("\n  Nothing is ever deleted automatically — open a category to review files.\n"))
	return b.String()
}

// ---- duplicates setup ----

func (m *model) viewDupSetup() string {
	roots := m.s.DupRoots
	shown := roots
	if len(m.dupRootsOverride) > 0 {
		shown = m.dupRootsOverride
	}
	var display []string
	for _, r := range shown {
		display = append(display, fsutil.DisplayPath(r))
	}
	devLine := goodStyle.Render("excluded") + dimStyle.Render("  (d to toggle — Stale Deps is the right cleanup for those)")
	if m.s.DupIncludeDev {
		devLine = warnStyle.Render("included") + dimStyle.Render("  (d to toggle — structural duplicates, deleting one copy breaks that project)")
	}
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("  scan roots   %s\n", strings.Join(display, ", ")))
	b.WriteString(fmt.Sprintf("  dev folders  %s\n", devLine))
	b.WriteString("\n")
	b.WriteString("  " + cursorStyle.Render("enter — start scan") + "\n")
	b.WriteString("  p — scan a custom folder…\n")
	if m.dupSetupInput != nil {
		b.WriteString("\n" + m.dupSetupInput.view(m.width) + "\n")
	}
	return b.String()
}

// ---- duplicates ----

func (m *model) viewDuplicates() string {
	if m.dup == nil {
		return "no data"
	}
	var b strings.Builder
	devNote := ""
	if m.dupDevIncluded {
		devNote = warnStyle.Render(" · dev folders included (structural duplicates)")
	}
	b.WriteString(fmt.Sprintf("\n  %d groups · %s reclaimable%s\n",
		len(m.dup.Groups), units.Format(m.dup.Reclaimable), devNote))
	if len(m.dup.Groups) == 0 {
		b.WriteString("\n" + goodStyle.Render("  No duplicates found.") + "\n")
		return b.String()
	}
	// Determine visible window over rows, keeping groups readable.
	start, end := m.window(len(m.dupRows), m.dupCursor)
	var checkedCount int
	var checkedBytes int64
	for gi, g := range m.dup.Groups {
		for _, f := range g.Files {
			if m.dupChecked[f.Path] {
				checkedCount++
				checkedBytes += f.Size
			}
		}
		_ = gi
	}
	for i := start; i < end; i++ {
		row := m.dupRows[i]
		g := m.dup.Groups[row.groupIdx]
		if row.header {
			b.WriteString("\n" + titleStyle.Render(fmt.Sprintf("  Group #%d", row.groupIdx+1)) +
				dimStyle.Render(fmt.Sprintf("  %s each · reclaimable %s", units.Format(g.Size), units.Format(g.Reclaimable()))) + "\n")
			continue
		}
		f := g.Files[row.fileIdx]
		mark := dimStyle.Render("[ ]")
		if m.dupChecked[f.Path] {
			mark = checkedStyle.Render("[x]")
		}
		keep := "   "
		if row.fileIdx == g.KeepIndex() {
			keep = goodStyle.Render("(oldest)")
		}
		flag := ""
		if fsutil.IsDatabaseFile(f.Path) {
			flag = " " + dbFlagStyle.Render("⚠db")
		}
		line := fmt.Sprintf("%9s  %s %s%s", units.Format(f.Size), truncate(fsutil.DisplayPath(f.Path), maxInt(10, m.width-42)), keep, flag)
		if !fileExists(f.Path) {
			line = deletedStyle.Render(line) + dimStyle.Render("  · deleted")
		} else if i == m.dupCursor {
			line = cursorStyle.Render(strings.TrimLeft(line, " "))
		}
		b.WriteString("  " + mark + " " + line + "\n")
	}
	if checkedCount > 0 {
		b.WriteString("\n" + checkedStyle.Render(fmt.Sprintf("  %d marked · %s reclaimable", checkedCount, units.Format(checkedBytes))) + "\n")
		b.WriteString(warnStyle.Render(fmt.Sprintf("  ► press d to trash %d marked copy/copies · c to clear", checkedCount)) + "\n")
	}
	return b.String()
}

// ---- stale deps ----

func (m *model) viewDeps() string {
	if m.deps == nil || len(m.deps.Entries) == 0 {
		return "\n  no dependency folders found\n"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("\n  %d folders · stale %s (%d) · fresh %s (%d) · threshold %d days\n",
		len(m.deps.Entries), units.Format(m.deps.StaleSize), len(m.deps.Stale),
		units.Format(m.deps.FreshSize), len(m.deps.Fresh), m.deps.StaleDays))
	b.WriteString("\n")
	entries := m.depEntries()
	start, end := m.window(len(entries), m.depsCursor)
	for i := start; i < end; i++ {
		e := entries[i]
		mark := dimStyle.Render("[ ]")
		if m.depsChecked[e.Path] {
			mark = checkedStyle.Render("[x]")
		}
		status := goodStyle.Render("in use ")
		if e.Stale {
			status = warnStyle.Render("STALE  ")
		}
		flag := ""
		if fsutil.IsDatabaseFile(e.Path) {
			flag = " " + dbFlagStyle.Render("⚠db")
		}
		line := fmt.Sprintf("%9s  %s  %-13s  %s%s", units.Format(e.Size), status, e.Age(),
			truncate(fsutil.DisplayPath(e.Path), maxInt(10, m.width-52)), flag)
		if !fileExists(e.Path) {
			line = deletedStyle.Render(line) + dimStyle.Render("  · deleted")
		} else if i == m.depsCursor {
			line = cursorStyle.Render(strings.TrimLeft(line, " "))
		}
		b.WriteString("  " + mark + " " + line + "\n")
	}
	var checkedN int
	var checkedBytes int64
	for _, e := range entries {
		if m.depsChecked[e.Path] {
			checkedN++
			checkedBytes += e.Size
		}
	}
	b.WriteString("\n" + dimStyle.Render(fmt.Sprintf("  sorted by %s ('/' switches) · stale = older than threshold, reinstallable",
		sortName(m.depsByAge))) + "\n")
	if checkedN > 0 {
		b.WriteString(checkedStyle.Render(fmt.Sprintf("  %d selected · %s", checkedN, units.Format(checkedBytes))) + "\n")
		b.WriteString(warnStyle.Render(fmt.Sprintf("  ► press d to move %d item(s) (%s) to Trash · c to clear selection",
			checkedN, units.Format(checkedBytes))) + "\n")
	}
	return b.String()
}

func sortName(byAge bool) string {
	if byAge {
		return "last use"
	}
	return "size"
}

func boolWord(b bool) string {
	if b {
		return "included"
	}
	return "excluded"
}

// ---- dashboard ----

func (m *model) viewDashboard() string {
	if m.dash == nil {
		return "no data"
	}
	d := m.dash
	var b strings.Builder
	b.WriteString(fmt.Sprintf("\n  %s used · %s free\n", units.Format(int64(d.Volume.UsedBytes())), units.Format(int64(d.Volume.FreeBytes))))
	b.WriteString("\n" + titleStyle.Render("  Potentially reclaimable") + "\n")
	for i, c := range d.Categories {
		if c.Actual <= 0 {
			continue
		}
		note := ""
		if c.Gross > c.Actual {
			note = dimStyle.Render(fmt.Sprintf("  (gross %s, overlap %s)", units.Format(c.Gross), units.Format(c.Gross-c.Actual)))
		}
		line := fmt.Sprintf("  %-30s %9s", c.Name, units.Format(c.Actual)) + note
		if i == m.dashCursor {
			b.WriteString(accentStyle.Render("▸ ") + cursorStyle.Render(strings.TrimLeft(line, " ")) + "\n")
		} else {
			b.WriteString(line + "\n")
		}
	}
	b.WriteString("\n" + strings.Repeat("  ", 0))
	b.WriteString("  " + titleStyle.Render("Potential total") + fmt.Sprintf("  %9s", units.Format(d.Actual)) + "\n")
	if d.Gross > d.Actual {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  Gross %s · overlap removed %s — categories pointed at the same files\n",
			units.Format(d.Gross), units.Format(d.Gross-d.Actual))))
	}
	b.WriteString("\n" + helpStyle.Render("  enter — inspect a category · r — rebuild") + "\n")
	return b.String()
}

// ---- cleanup ----

func safetyBadge(s string) string {
	switch s {
	case devcache.Safe:
		return goodStyle.Render("safe       ")
	case devcache.TrashOnly:
		return warnStyle.Render("trash      ")
	case devcache.Destructive:
		return dangerStyle.Render("destructive")
	}
	return s
}

func (m *model) viewCleanup() string {
	caches := m.visibleCaches()
	if len(caches) == 0 {
		return "\n  nothing detected\n"
	}
	var b strings.Builder
	b.WriteString("\n")
	for i, c := range caches {
		if !c.exists {
			continue
		}
		line := fmt.Sprintf("  %9s  %-34s ", units.Format(c.size), truncate(c.name, 34)) + safetyBadge(c.safety)
		if i == m.clCursor {
			b.WriteString(accentStyle.Render("▸ ") + cursorStyle.Render(strings.TrimLeft(line, " ")) + "\n")
		} else {
			b.WriteString(line + "\n")
		}
	}
	b.WriteString(dimStyle.Render("\n  safe = regenerable · trash = moved to Trash · destructive = typed confirmation\n"))
	return b.String()
}

// ---- settings ----

func (m *model) viewSettings() string {
	s := m.s
	rows := []struct {
		label string
		value string
	}{
		{"Large-file threshold", units.Format(s.LargeMinBytes)},
		{"Old-file age", fmt.Sprintf("%d days", s.OldDays)},
		{"Duplicate minimum size", units.Format(s.DupMinBytes)},
		{"Duplicates in dev folders", boolWord(s.DupIncludeDev)},
		{"Dep staleness", fmt.Sprintf("%d days", s.DepStaleDays)},
		{"Exclusions", strings.Join(s.Exclusions, ", ")},
		{"Save", settings.Path()},
	}
	var b strings.Builder
	b.WriteString("\n")
	for i, r := range rows {
		line := fmt.Sprintf("  %-24s %s", r.label, truncate(r.value, maxInt(10, m.width-30)))
		if i == m.stCursor {
			b.WriteString(accentStyle.Render("▸ ") + cursorStyle.Render(strings.TrimLeft(line, " ")) + "\n")
		} else {
			b.WriteString(line + "\n")
		}
	}
	b.WriteString(dimStyle.Render("\n  space — cycle value · e — edit exclusions · S — save\n"))
	if m.settingsInput && m.pathInput != nil {
		b.WriteString("\n" + m.pathInput.view(m.width) + "\n")
	}
	return b.String()
}

// ---- small helpers ----

func truncate(s string, n int) string {
	if n <= 0 {
		return s
	}
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return s[:n-1] + "…"
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
