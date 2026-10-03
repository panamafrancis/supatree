package tui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/panamafrancis/supatree/pkg/supatree"
	"github.com/panamafrancis/workbench/pkg/github"
	"github.com/panamafrancis/workbench/pkg/zellij"
)

func (m *Model) View() string {
	var b strings.Builder
	b.WriteString(styleHeader.Render("supatree"))
	b.WriteString("\n")

	// The reference replaces the list rather than overlaying it: the sidebar is
	// a narrow column, so there is nowhere to float a panel over.
	if m.mode == modeHelp {
		b.WriteString(m.helpView())
		return b.String()
	}

	footer := m.footer()
	if len(m.insts) == 0 && len(m.creating) == 0 {
		// The PM section is still there with no supatrees — it is reachable (and
		// can create one) before anything else exists.
		for i, r := range m.rows {
			b.WriteString(m.renderRow(r, i == m.cursor && m.mode == modeNormal))
			b.WriteString("\n")
		}
		b.WriteString(styleMuted.Render("no supatrees — press n to create one"))
		b.WriteString("\n\n")
		b.WriteString(footer)
		return b.String()
	}

	// Reserve the header line, the blank line, and the (possibly wrapped) footer;
	// the rest is the scrollable row viewport. A height of 0 (size not yet
	// reported) renders every row; a reported-but-tiny pane still windows down to
	// a single row so the cursor stays visible instead of dumping from the top.
	reserved := 1 + 1 + strings.Count(footer, "\n") + 1
	avail := m.height - reserved
	if m.height > 0 && avail < 1 {
		avail = 1
	}
	start, end := m.viewport(avail)
	for i := start; i < end; i++ {
		selected := i == m.cursor && m.mode == modeNormal
		b.WriteString(m.renderRow(m.rows[i], selected))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(footer)
	return b.String()
}

// viewport returns the [start, end) row range to render, recording the visible
// height for the paging and wheel handlers. While m.follow is set (any cursor
// movement) it pulls the scroll offset along to keep the cursor visible; the
// mouse wheel clears the flag so a scrolled-away view stays put across renders
// and background ticks. avail <= 0 (no reported size) or a list that fits shows
// everything.
func (m *Model) viewport(avail int) (int, int) {
	m.viewHeight = avail
	if avail <= 0 || len(m.rows) <= avail {
		m.scroll = 0
		return 0, len(m.rows)
	}
	if m.follow {
		if m.cursor < m.scroll {
			m.scroll = m.cursor
		}
		if m.cursor >= m.scroll+avail {
			m.scroll = m.cursor - avail + 1
		}
	}
	if maxScroll := len(m.rows) - avail; m.scroll > maxScroll {
		m.scroll = maxScroll
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
	return m.scroll, m.scroll + avail
}

func (m *Model) renderRow(r row, selected bool) string {
	switch r.kind {
	case rowHeader:
		return "  " + styleSub.Render(r.label)
	case rowCreating:
		return "  " + styleMuted.Render("◌ "+r.label+"  creating…")
	case rowPM:
		return m.renderPM(r.label, selected)
	case rowDivider:
		return styleMuted.Render(strings.Repeat("─", m.dividerWidth()))
	case rowTree:
		// The PR summary belongs to the repositories section; it is lifted onto
		// the supatree row only when that section is out of sight, so a folded
		// supatree still reports where its members stand without the count being
		// printed twice when everything is open.
		badge := ""
		collapsed := m.ui.TreeCollapsed(r.tree)
		if collapsed {
			badge = m.prCounts(r.tree)
		}
		running := ""
		if m.openTabs[r.tree] {
			running = styleRunning.Render(" ●")
		}
		fold := "▼"
		if collapsed {
			fold = "▶"
		}
		line := fold + " " + r.label + running
		if m.attention[r.tree] {
			// Something happened in this supatree that you have not looked at.
			// It sits next to the name rather than in the gutter, which the
			// "you are here" marker already owns.
			line += styleAttention.Render(" !")
		}
		if badge != "" {
			line += "  " + badge
		}
		// A gutter marker points at the supatree this sidebar's tab belongs to
		// ("you are here"), regardless of the cursor. Other rows render a blank
		// gutter so the tree names stay aligned.
		marker := "  "
		if m.activeTree != "" && r.tree == m.activeTree {
			marker = styleRunning.Render("▸ ")
		}
		return marker + sel(selected, styleTree.Render(line))
	case rowSubheader:
		return "    " + styleSub.Render(r.label)
	case rowRepos:
		fold := "▼"
		if m.ui.ReposCollapsed(r.tree) {
			fold = "▶"
		}
		// Indented one level past the supatree's own fold arrow and one level
		// short of its members, so the nesting reads at a glance.
		line := "    " + styleSub.Render(fold+" "+r.label)
		if counts := m.prCounts(r.tree); counts != "" {
			line += "  " + counts
		}
		return sel(selected, line)
	case rowAgent:
		icon := "○"
		if m.openTabs[supatree.TabName(r.tree, r.label)] {
			icon = styleRunning.Render("●")
		}
		return sel(selected, fmt.Sprintf("      %s %s", icon, r.label))
	case rowMember:
		return sel(selected, m.renderMember(r))
	}
	return ""
}

// renderPM draws a PM row. It is styled apart from the supatrees on purpose —
// a filled glyph and its own colour instead of a fold arrow — because a PM is
// not a tree and nothing folds under it.
func (m *Model) renderPM(name string, selected bool) string {
	line := stylePM.Render("◆ " + name)
	if p := m.pm(name); p != nil && m.openTabs[p.Tab()] {
		line += styleRunning.Render(" ●")
	}
	if n := m.pmPending[name]; n > 0 {
		// Requests this PM has not read yet: queued with m, by the watcher, the
		// scheduler or `supatree request`.
		line += styleDirty.Render(fmt.Sprintf("  ✉%d", n))
	}
	return "    " + sel(selected, line)
}

// dividerWidth is the PM section's rule: the pane width once it is known, and a
// short fixed rule before that.
func (m *Model) dividerWidth() int {
	if m.width > 0 {
		return m.width
	}
	return 20
}

func (m *Model) renderMember(r row) string {
	inst := m.instance(r.tree)
	dirtyMark := " "
	key := ""
	reviewed := ""
	if inst != nil {
		if mem := inst.FindMember(r.alias); mem != nil {
			// Keyed on the member, not its branch: a review member's branch is
			// tree-local and its status lives under the pull request instead.
			key = mem.CacheKey()
			if m.dirty[mem.Path] {
				dirtyMark = styleDirty.Render("*")
			}
			if !mem.Exists {
				dirtyMark = styleMuted.Render("·")
			}
			if mem.Review != nil {
				reviewed = styleMuted.Render(" " + mem.Review.HeadRef)
			}
		}
	}
	pr := ""
	if info := m.prCache.Get(key); info != nil {
		if icon := prIcon(info.Status); icon != "" {
			pr = "  " + icon
			if info.Number > 0 {
				pr += styleMuted.Render(fmt.Sprintf(" #%d", info.Number))
			}
		}
	}
	return fmt.Sprintf("      %s %-18s%s%s", dirtyMark, r.alias, pr, reviewed)
}

// prCountStatuses is the order counts are rendered in: roughly the order a PR
// travels through, with the members that have no PR yet last.
var prCountStatuses = []github.PRStatus{
	github.PRDraft, github.PROpen, github.PRMerged, github.PRClosed, github.PRNone,
}

// prCounts summarises a supatree's members as one coloured glyph-and-count per
// PR status present ("◉2 ✓1"), so a folded repositories section still says how
// far along the tree is. The colour carries the status — spelling it out would
// not fit the sidebar's width.
func (m *Model) prCounts(tree string) string {
	inst := m.instance(tree)
	if inst == nil || len(inst.Members) == 0 {
		return ""
	}
	counts := make(map[github.PRStatus]int, len(prCountStatuses))
	for _, mem := range inst.Members {
		status := github.PRNone
		if info := m.prCache.Get(mem.CacheKey()); info != nil {
			status = info.Status
		}
		counts[status]++
	}
	parts := make([]string, 0, len(prCountStatuses))
	for _, status := range prCountStatuses {
		if n := counts[status]; n > 0 {
			parts = append(parts, prStyle(status).Render(fmt.Sprintf("%s%d", prGlyph(status), n)))
		}
	}
	return strings.Join(parts, " ")
}

func (m *Model) footer() string {
	switch m.mode {
	case modeNewAgent:
		return "new agent: " + m.input.View()
	case modeNewTree:
		var b strings.Builder
		b.WriteString(styleSub.Render(wrapText("new supatree — pick stack (↑/↓, enter, esc):", m.width)))
		for i, s := range m.stCfg.Stacks {
			b.WriteString("\n")
			if i == m.stackCursor {
				b.WriteString(styleSelected.Render("› " + s.Alias))
			} else {
				b.WriteString("  " + styleRow.Render(s.Alias))
			}
		}
		return b.String()
	case modeNewTreeName:
		prompt := "new supatree — name: " + m.input.View()
		if m.inputErr != nil {
			// Live validation: the complaint sits under the field the user is
			// still typing in, and goes away with the character that caused it.
			prompt += "\n" + styleDirty.Render(wrapText(m.inputErr.Error(), m.width))
		}
		return prompt
	case modeNewStackName:
		prompt := "new stack — name: " + m.input.View()
		if m.inputErr != nil {
			prompt += "\n" + styleDirty.Render(wrapText(m.inputErr.Error(), m.width))
		}
		return prompt
	case modeConfirmDelete:
		return styleDirty.Render(wrapText(fmt.Sprintf("delete %q? [y/N]", m.actionTree), m.width))
	case modeNewPM:
		prompt := "new PM — name: " + m.input.View()
		if m.inputErr != nil {
			prompt += "\n" + styleDirty.Render(wrapText(m.inputErr.Error(), m.width))
		}
		return prompt
	case modeConfirmDeletePM:
		return styleDirty.Render(wrapText(fmt.Sprintf("remove PM %q? [y/N]", m.actionPM), m.width))
	case modeConfirmDeleteAgent:
		return styleDirty.Render(wrapText(fmt.Sprintf("remove agent %q? [y/N]", supatree.TabName(m.actionTree, m.actionAgent)), m.width))
	case modeConfirmQuit:
		return styleDirty.Render("quit sidebar? [y/N]")
	case modeHelp:
		return styleMuted.Render("press any key to close")
	case modeNormal:
	}
	if m.err != nil {
		// Only the gist: the full text (nono's complaint, a hint) can run to a
		// screenful in a pane this narrow, and would bury the list and the
		// keys. `e` opens all of it.
		return stylePRClosed.Render(errSummary(m.err, m.width)) + "\n" +
			styleStatus.Render(wrapParts([]string{"e full error", "any key hides"}, " · ", m.width))
	}
	// Enter is contextual (a member row is a place, an agent row is a process),
	// so the hint says which one the cursor is on rather than a generic "open".
	openHint := "enter open"
	if r := m.selected(); r != nil {
		switch r.kind {
		case rowMember:
			openHint = "enter shell"
		case rowPM:
			openHint = "enter PM"
		case rowTree, rowSubheader, rowRepos, rowAgent, rowDivider, rowHeader, rowCreating:
		}
	}
	// The motions moved into `?` — the footer keeps the actions, which are the
	// ones that are not guessable from vim habits.
	parts := []string{openHint, "space fold", "a agent", "p PM", "n new", "s sync", "d del", "D dash", "r refresh", "? help", "q quit"}
	if m.prHint != "" {
		parts = append(parts, "("+m.prHint+")")
	}
	hints := wrapParts(parts, " · ", m.width)
	if m.msg != "" {
		// A line of its own: as one of the parts, a message wider than the
		// pane would be clipped rather than folded.
		hints = wrapText(m.msg, m.width) + "\n" + hints
	}
	return styleStatus.Render(hints)
}

// errSummaryLines caps the footer's error summary.
const errSummaryLines = 3

// errSummary is an error's first line, folded to the pane and cut at
// errSummaryLines — the footer's short form of an error `e` shows in full.
func errSummary(err error, width int) string {
	first, _, _ := strings.Cut(err.Error(), "\n")
	lines := strings.Split(wrapText("error: "+strings.TrimSuffix(strings.TrimSpace(first), ":"), width), "\n")
	if len(lines) > errSummaryLines {
		lines = lines[:errSummaryLines]
		last := lines[errSummaryLines-1]
		if i := strings.LastIndex(last, " "); width > 0 && lipgloss.Width(last+" …") > width && i > 0 {
			last = last[:i]
		}
		lines[errSummaryLines-1] = last + " …"
	}
	return strings.Join(lines, "\n")
}

// wrapParts joins parts with sep, folding onto multiple lines so nothing is
// clipped in the narrow sidebar. A width of 0 (size not yet reported) keeps it
// on one line.
func wrapParts(parts []string, sep string, width int) string {
	if width <= 0 {
		return strings.Join(parts, sep)
	}
	var lines []string
	cur := ""
	for _, p := range parts {
		cand := p
		if cur != "" {
			cand = cur + sep + p
		}
		if cur != "" && lipgloss.Width(cand) > width {
			lines = append(lines, cur)
			cur = p
		} else {
			cur = cand
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return strings.Join(lines, "\n")
}

// prIcon is the per-member badge: the status glyph plus its name, in the status
// colour. PRStatus's own string is the name ("open", "merged", ...).
// wrapText word-wraps one message to the sidebar width — an error or a
// validation complaint is a sentence, and a narrow pane would otherwise clip it.
func wrapText(s string, width int) string {
	fields := strings.Fields(s)
	words := make([]string, 0, len(fields))
	for _, w := range fields {
		words = append(words, breakWord(w, width)...)
	}
	return wrapParts(words, " ", width)
}

// breakWord splits a word wider than the pane — a path in an error, say — into
// pieces that fit, since a word is otherwise the smallest thing wrapping moves.
func breakWord(w string, width int) []string {
	if width <= 0 || lipgloss.Width(w) <= width {
		return []string{w}
	}
	var pieces []string
	cur := ""
	for _, r := range w {
		if cur != "" && lipgloss.Width(cur+string(r)) > width {
			pieces = append(pieces, cur)
			cur = ""
		}
		cur += string(r)
	}
	return append(pieces, cur)
}

// keyColumn matches a help line up to where its description starts: the
// indent, the key, and the run of two or more spaces after it.
var keyColumn = regexp.MustCompile(`^(\s*\S.*?\s{2,})\S`)

// wrapHanging folds one line of the `?` panel to the pane width, indenting
// continuation lines to where the description started so the key column stays
// clear. A line with no key column (a continuation, or a line of an error)
// keeps its own indent.
func wrapHanging(s string, width int) string {
	if width <= 0 || lipgloss.Width(s) <= width {
		return s
	}
	prefix := s[:len(s)-len(strings.TrimLeft(s, " "))]
	if loc := keyColumn.FindStringSubmatchIndex(s); loc != nil {
		prefix = s[:loc[3]]
	}
	pad := lipgloss.Width(prefix)
	if width-pad < 10 {
		// Too narrow to indent and still fit words; fold flush left.
		return wrapText(s, width)
	}
	body := wrapText(s[len(prefix):], width-pad)
	return prefix + strings.ReplaceAll(body, "\n", "\n"+strings.Repeat(" ", pad))
}

func prIcon(s github.PRStatus) string {
	if s == github.PRNone {
		return ""
	}
	return prStyle(s).Render(prGlyph(s) + " " + string(s))
}

// prGlyph and prStyle are the one place a PR status is turned into a symbol and
// a colour, shared by the per-member badge and the folded-section counts.
func prGlyph(s github.PRStatus) string {
	switch s {
	case github.PROpen:
		return "◉"
	case github.PRDraft:
		return "◌"
	case github.PRMerged:
		return "✓"
	case github.PRClosed:
		return "✕"
	case github.PRNone:
		return "·"
	default:
		return "·"
	}
}

func prStyle(s github.PRStatus) lipgloss.Style {
	switch s {
	case github.PROpen:
		return stylePROpen
	case github.PRDraft:
		return stylePRDraft
	case github.PRMerged:
		return stylePRMerged
	case github.PRClosed:
		return stylePRClosed
	case github.PRNone:
		return styleMuted
	default:
		return styleMuted
	}
}

func sel(selected bool, s string) string {
	if selected {
		return styleSelected.Render(strings.TrimRight(s, " "))
	}
	return s
}

func zellijTabs() (map[string]bool, error) {
	return zellij.TabNames()
}

// helpView renders the `?` panel — the keybinding reference, or the full text
// of an error opened with `e` — windowed to the pane: it is taller than most
// sidebars, so j/k scroll it (see updateHelp) and the last line says so
// whenever some of it is out of sight.
func (m *Model) helpView() string {
	lines := m.panelLines()
	closeHint := styleMuted.Render(wrapText("press any key to close", m.width))
	if m.height <= 0 || len(lines) <= m.height-1-lipgloss.Height(closeHint) {
		m.helpScroll = 0
		return strings.Join(lines, "\n") + "\n" + closeHint
	}
	// Sized for the widest the counts get, so the hint cannot outgrow the
	// space reserved for it as the user scrolls.
	scrollHint := func(end int) string {
		return styleMuted.Render(wrapText(fmt.Sprintf("j/k scroll (%d/%d) · other keys close", end, len(lines)), m.width))
	}
	avail := max(m.height-1-lipgloss.Height(scrollHint(len(lines))), 1) // less the "supatree" header
	m.helpScroll = min(max(m.helpScroll, 0), len(lines)-avail)
	end := m.helpScroll + avail
	return strings.Join(lines[m.helpScroll:end], "\n") + "\n" + scrollHint(end)
}

// panelLines is what the `?` panel shows, folded to the pane width: the full
// text of the error `e` opened, else the keybinding reference.
func (m *Model) panelLines() []string {
	src := helpLines()
	if m.detail != "" {
		src = strings.Split(m.detail, "\n")
	}
	lines := make([]string, 0, len(src))
	for _, l := range src {
		lines = append(lines, strings.Split(wrapHanging(l, m.width), "\n")...)
	}
	return lines
}

// helpLines is the `?` reference: a key column and a description per entry,
// one line each however long — wrapHanging folds them to the pane, under the
// description column. A line of its own (d's) is a separate case, not a fold.
func helpLines() []string {
	return []string{
		styleHeader.Render("Navigation"),
		"  j/k ↑↓   move",
		"  ctrl+d/u half page",
		"  gg / G   first / last",
		"  } / {    next / prev tree",
		"  wheel    scroll",
		"  click    select",
		"",
		styleHeader.Render("Folding"),
		"  space    fold this section",
		"  h / l    close / open",
		"  zM / zR  fold / unfold all",
		"",
		styleHeader.Render("Open"),
		"  enter/o  agent, or shell on a repo row, PM on a PM row",
		"  a        new named agent, repo agent on a repo",
		"  p        new PM",
		"  D        dashboard",
		"  P        top PM",
		"  m        hand this row to the top PM",
		"",
		styleHeader.Render("Remove"),
		"  d        on a PM: remove it",
		"           on an agent: remove it (closes its tab)",
		"           elsewhere: delete the supatree",
		"",
		styleHeader.Render("Supatrees"),
		"  n        new supatree",
		"  s        sync members",
		"  S        new stack",
		"",
		styleHeader.Render("Global"),
		"  r        refresh",
		"  e        full text of the last error",
		"  ?        this help",
		"  q        quit",
		"",
		// Zellij's default keybindings: supatree ships no keymap of its own.
		styleHeader.Render("Zellij panes"),
		"  Alt+n       new pane",
		"  Alt+←↓↑→    move focus",
		"  Alt+= / -   grow / shrink",
		"  Alt+f       floating panes",
		"  Ctrl+p r    split right",
		"  Ctrl+p d    split down",
		"  Ctrl+p x    close pane",
		"  Ctrl+p f    fullscreen",
		"  Ctrl+p w    float / embed",
		"  Ctrl+p c    rename pane",
		"  Ctrl+n      resize mode",
		"  Ctrl+h      move mode",
		"",
		styleHeader.Render("Zellij tabs"),
		"  Ctrl+t ←/→  prev / next",
		"  Ctrl+t 1-9  go to tab",
		"  Ctrl+t n    new tab",
		"  Ctrl+t x    close tab",
		"  Ctrl+t r    rename tab",
		"  Alt+i / o   move tab",
		"",
		styleHeader.Render("Zellij session"),
		"  Ctrl+s      scroll mode",
		"  Ctrl+s s    search",
		"  Ctrl+s e    scrollback in $EDITOR",
		"  Ctrl+g      lock keys (pass through)",
		"  Ctrl+o w    sessions",
		"  Ctrl+o d    detach",
		"  Ctrl+q      quit session",
	}
}
