package tui

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/panamafrancis/supatree/pkg/supatree"
	"github.com/panamafrancis/workbench/pkg/github"
	"github.com/panamafrancis/workbench/pkg/testutil"
	"github.com/panamafrancis/workbench/pkg/zellij"
)

func key(s string) tea.KeyMsg {
	if len(s) == 1 {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestViewEmptyDoesNotPanic(t *testing.T) {
	testutil.IsolateHome(t)
	m := New(supatree.DefaultConfig(), zellij.Workspace{})
	out := m.View()
	if !strings.Contains(out, "supatree") {
		t.Errorf("view missing header:\n%s", out)
	}
	if !strings.Contains(out, "no supatrees") {
		t.Errorf("empty view should prompt to create one:\n%s", out)
	}
}

func TestRebuildRowsStructure(t *testing.T) {
	testutil.IsolateHome(t)
	m := New(supatree.DefaultConfig(), zellij.Workspace{})
	// Inject a synthetic instance and rebuild.
	m.insts = []*supatree.Instance{{
		Name: "berlin",
		Slug: "berlin",
		Members: []supatree.Member{
			{Alias: "terraform", Branch: "st/berlin/terraform"},
			{Alias: "keystone", Branch: "st/berlin/keystone"},
		},
	}}
	m.rebuildRows()
	// The PM section always leads. Repositories start folded, so out of the
	// box: tree, "agents" subheader, main agent, "repositories" header — and no
	// member rows.
	want := []rowKind{rowHeader, rowPM, rowDivider, rowHeader, rowTree, rowSubheader, rowAgent, rowRepos}
	if got := rowKinds(m); !slices.Equal(got, want) {
		t.Fatalf("folded rows = %v, want %v", got, want)
	}

	// Unfolding the section adds the members under it.
	expandRepos(m)
	want = []rowKind{rowHeader, rowPM, rowDivider, rowHeader, rowTree, rowSubheader, rowAgent, rowRepos, rowMember, rowMember}
	if got := rowKinds(m); !slices.Equal(got, want) {
		t.Fatalf("unfolded rows = %v, want %v", got, want)
	}
	// Cursor must never rest on a heading, a subheader or the divider.
	for _, i := range []int{0, pmSectionRows - 1, pmSectionRows + 1} {
		m.cursor = i
		m.clampCursor()
		if !m.rows[m.cursor].kind.selectable() {
			t.Errorf("cursor rested on %v after clamp from %d", m.rows[m.cursor].kind, i)
		}
	}
}

func TestTreeFromTab(t *testing.T) {
	cases := map[string]string{
		"paris":          "paris",
		"paris:reviewer": "paris",
		"":               "",
	}
	for in, want := range cases {
		if got := treeFromTab(in); got != want {
			t.Errorf("treeFromTab(%q) = %q, want %q", in, got, want)
		}
	}
}

// Pressing n with a single stack skips the stack prompt and asks for a name;
// the stack prompt only appears when more than one stack is registered.
func TestNewTreeSingleStackAsksForName(t *testing.T) {
	testutil.IsolateHome(t)
	cfg := supatree.DefaultConfig()
	cfg.Stacks = []supatree.Stack{{Alias: "only", Path: "/tmp/only"}}
	m := New(cfg, zellij.Workspace{})

	m.updateNormal(key("n"))
	if m.mode != modeNewTreeName {
		t.Fatalf("single stack: mode = %v, want modeNewTreeName", m.mode)
	}
	if m.actionStack != "" {
		t.Errorf("single stack: actionStack = %q, want empty (resolve default)", m.actionStack)
	}
}

func TestNewTreeMultiStackAsksForStackThenName(t *testing.T) {
	testutil.IsolateHome(t)
	cfg := supatree.DefaultConfig()
	cfg.Stacks = []supatree.Stack{{Alias: "a", Path: "/tmp/a"}, {Alias: "b", Path: "/tmp/b"}}
	m := New(cfg, zellij.Workspace{})

	m.updateNormal(key("n"))
	if m.mode != modeNewTree {
		t.Fatalf("multi stack: mode = %v, want modeNewTree", m.mode)
	}
	// The picker lists stacks; move to the second ("b") and select it. This
	// should advance to the name prompt carrying the choice.
	m.updateInput(key("j"))
	m.updateInput(key("enter"))
	if m.mode != modeNewTreeName {
		t.Fatalf("after stack: mode = %v, want modeNewTreeName", m.mode)
	}
	if m.actionStack != "b" {
		t.Errorf("actionStack = %q, want %q", m.actionStack, "b")
	}
}

// reloadWithSelection keeps the cursor on the same logical row even when a new
// supatree sorts in above it and shifts every row index down.
func TestReloadWithSelectionPinsRow(t *testing.T) {
	testutil.IsolateHome(t)
	m := New(supatree.DefaultConfig(), zellij.Workspace{})
	m.insts = []*supatree.Instance{{
		Name:    "milan",
		Members: []supatree.Member{{Alias: "web", Branch: "st/milan/web"}},
	}}
	expandRepos(m)
	// Select the member row of "milan".
	for i, r := range m.rows {
		if r.kind == rowMember && r.tree == "milan" {
			m.cursor = i
		}
	}
	want := *m.selected()

	// A tree that sorts before "milan" appears and shifts every row index down.
	m.insts = []*supatree.Instance{
		{Name: "athens", Members: []supatree.Member{{Alias: "api", Branch: "st/athens/api"}}},
		{Name: "milan", Members: []supatree.Member{{Alias: "web", Branch: "st/milan/web"}}},
	}
	expandRepos(m)
	m.selectRow(want)
	if sel := m.selected(); sel == nil || sel.tree != "milan" || sel.kind != rowMember {
		t.Fatalf("selection not pinned to milan member after row shift: %+v", sel)
	}
}

func TestWrapPartsFoldsToWidth(t *testing.T) {
	parts := []string{"aaaa", "bbbb", "cccc"}
	// Width fits "aaaa · bbbb" (11) but not a third part.
	got := wrapParts(parts, " · ", 11)
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("wrapParts folded into %d lines, want 2:\n%s", len(lines), got)
	}
	// Width 0 keeps everything on one line.
	if one := wrapParts(parts, " · ", 0); strings.Contains(one, "\n") {
		t.Errorf("wrapParts(width=0) should not wrap: %q", one)
	}
}

// Collapsing a tree hides its agents/repos and parks the cursor on the tree row.
func TestCollapseHidesChildrenAndKeepsCursor(t *testing.T) {
	m := osloModel(t)
	full := len(m.rows)

	// Fold via the tree row; children vanish, cursor stays on the tree row.
	m.cursor = rowIndex(m, rowTree)
	m.updateNormal(key(" "))
	if len(m.rows) != pmSectionRows+1 || m.rows[pmSectionRows].kind != rowTree {
		t.Fatalf("collapsed rows = %d, want the PM section and 1 tree row", len(m.rows))
	}
	if sel := m.selected(); sel == nil || sel.kind != rowTree || sel.tree != "oslo" {
		t.Fatalf("cursor not on tree row after collapse: %+v", sel)
	}

	// Unfold restores the children.
	m.updateNormal(key(" "))
	if len(m.rows) != full {
		t.Fatalf("expanded rows = %d, want %d", len(m.rows), full)
	}
}

// space folds the innermost section the cursor is in: from a member row that is
// the repositories list, not the whole supatree around it.
func TestSpaceOnMemberRowFoldsOnlyRepos(t *testing.T) {
	m := osloModel(t)

	m.cursor = rowIndex(m, rowMember)
	m.updateNormal(key(" "))
	if rowIndex(m, rowMember) != -1 {
		t.Fatal("space on a member row left the member rows visible")
	}
	if rowIndex(m, rowAgent) == -1 {
		t.Fatal("space on a member row folded the whole supatree, not just its repos")
	}
	// The cursor parks on the section header, the row that survived the fold.
	if sel := m.selected(); sel == nil || sel.kind != rowRepos {
		t.Fatalf("cursor = %+v, want the repositories header", sel)
	}

	// h on the already-folded header steps out and folds the supatree itself.
	m.updateNormal(key("h"))
	if rowIndex(m, rowAgent) != -1 {
		t.Fatal("h on a folded repositories header did not fold the supatree")
	}
}

// The repositories section is folded until asked otherwise, and its header
// carries the members' PR statuses as coloured counts while it is.
func TestReposFoldedByDefaultWithCountBadge(t *testing.T) {
	testutil.IsolateHome(t)
	m := New(supatree.DefaultConfig(), zellij.Workspace{})
	m.insts = []*supatree.Instance{{
		Name: "oslo",
		Members: []supatree.Member{
			{Alias: "web", Branch: "st/oslo/web"},
			{Alias: "api", Branch: "st/oslo/api"},
			{Alias: "db", Branch: "st/oslo/db"},
		},
	}}
	if err := m.prCache.Mutate(func(w *github.Writable) error {
		w.Set("st/oslo/web", &github.PRInfo{Status: github.PROpen, Number: 1})
		w.Set("st/oslo/api", &github.PRInfo{Status: github.PRMerged, Number: 2})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.rebuildRows()

	if rowIndex(m, rowMember) != -1 {
		t.Fatal("repositories section was not folded on first render")
	}
	got := m.prCounts("oslo")
	// One open, one merged, one member with no PR at all.
	for _, want := range []string{"◉1", "✓1", "·1"} {
		if !strings.Contains(got, want) {
			t.Errorf("prCounts = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "open") || strings.Contains(got, "merged") {
		t.Errorf("prCounts = %q, want colours and glyphs rather than status words", got)
	}
	if !strings.Contains(m.View(), got) {
		t.Error("the count badge is missing from the rendered sidebar")
	}
}

// Folds are shared state: one sidebar per Zellij tab means a fold made in one
// tab has to show up in the next tab's reload, not just in the tab that made it.
func TestFoldStateIsSharedAcrossSidebars(t *testing.T) {
	testutil.IsolateHome(t)
	one := New(supatree.DefaultConfig(), zellij.Workspace{})
	two := New(supatree.DefaultConfig(), zellij.Workspace{})

	one.setCollapse("oslo", true)
	one.setReposCollapse("bergen", false)

	two.reload()
	if !two.ui.TreeCollapsed("oslo") {
		t.Error("a fold made in one sidebar did not reach the other")
	}
	if two.ui.ReposCollapsed("bergen") {
		t.Error("an unfolded repositories section did not reach the other sidebar")
	}
}

// The viewport keeps the cursor visible when the list is taller than the pane.
func TestViewportScrollsToCursor(t *testing.T) {
	testutil.IsolateHome(t)
	m := New(supatree.DefaultConfig(), zellij.Workspace{})
	m.rows = make([]row, 20)
	for i := range m.rows {
		m.rows[i] = row{kind: rowTree, tree: "t", label: "t"}
	}

	// Cursor near the bottom with room for only 5 rows must scroll into view.
	m.cursor = 18
	start, end := m.viewport(5)
	if m.cursor < start || m.cursor >= end {
		t.Fatalf("cursor %d outside viewport [%d,%d)", m.cursor, start, end)
	}
	if end-start != 5 {
		t.Fatalf("viewport height = %d, want 5", end-start)
	}

	// Moving back to the top scrolls the window back up.
	m.cursor = 0
	start, _ = m.viewport(5)
	if start != 0 {
		t.Fatalf("scroll did not return to top: start = %d", start)
	}

	// A list that fits shows everything with no offset.
	m.cursor = 3
	start, end = m.viewport(50)
	if start != 0 || end != len(m.rows) {
		t.Fatalf("small list windowed unexpectedly: [%d,%d)", start, end)
	}
}

// A permanent gh error marks gh unavailable so the tick/focus loop stops
// re-fetching every 30s; a transient/successful result restores it and clears
// the hint.
func TestPrMsgGhAvailabilityAndHint(t *testing.T) {
	testutil.IsolateHome(t)
	m := New(supatree.DefaultConfig(), zellij.Workspace{})

	// Permanent error: unavailable, hint set, background loop drops the fetch.
	m.Update(prMsg{err: github.ErrGHAuth})
	if m.ghAvailable {
		t.Fatal("permanent error should mark gh unavailable")
	}
	if m.prHint != "gh auth required" {
		t.Fatalf("prHint = %q, want gh auth required", m.prHint)
	}
	// Asserted as a delta rather than an absolute count, so adding an unrelated
	// background command does not fail a test about the PR fetch.
	down := len(m.backgroundCmds())
	m.ghAvailable = true
	up := len(m.backgroundCmds())
	m.ghAvailable = false
	if up != down+1 {
		t.Fatalf("backgroundCmds = %d with gh down, %d with gh up; want exactly one more (the fetch)", down, up)
	}

	// Transient error clears a stale hint without flipping availability back.
	m.prHint = "gh rate limited"
	m.Update(prMsg{err: errors.New("network blip")})
	if m.prHint != "" {
		t.Fatalf("transient error left stale hint %q", m.prHint)
	}

	// Success restores availability and re-enables the fetch.
	m.Update(prMsg{err: nil})
	if !m.ghAvailable || m.prHint != "" {
		t.Fatalf("success should restore gh: ghAvailable=%v prHint=%q", m.ghAvailable, m.prHint)
	}
	if got := len(m.backgroundCmds()); got != up {
		t.Fatalf("backgroundCmds after recovery = %d, want %d (the fetch is back)", got, up)
	}
}

func TestActiveTreeMarkerInView(t *testing.T) {
	testutil.IsolateHome(t)
	m := New(supatree.DefaultConfig(), zellij.Workspace{})
	m.insts = []*supatree.Instance{
		{Name: "here", Members: []supatree.Member{{Alias: "x", Branch: "st/here/x"}}},
		{Name: "there", Members: []supatree.Member{{Alias: "y", Branch: "st/there/y"}}},
	}
	m.rebuildRows()
	m.activeTree = "here"
	out := m.View()
	if !strings.Contains(out, "▸") {
		t.Errorf("active-tree marker missing from view:\n%s", out)
	}
}

// Tree names shared by the navigation tests.
const (
	berlin = "berlin"
	cairo  = "cairo"
	delhi  = "delhi"
	oslo   = "oslo" // osloModel's one tree
	paris  = "paris"
)

// threeTrees builds a model holding three synthetic supatrees, each with two
// member repos, for the navigation tests.
func threeTrees(t *testing.T) *Model {
	t.Helper()
	testutil.IsolateHome(t)
	m := New(supatree.DefaultConfig(), zellij.Workspace{})
	for _, name := range []string{berlin, cairo, delhi} {
		m.insts = append(m.insts, &supatree.Instance{
			Name: name, Slug: name,
			Members: []supatree.Member{
				{Alias: "terraform", Branch: "st/" + name + "/terraform"},
				{Alias: "keystone", Branch: "st/" + name + "/keystone"},
			},
		})
	}
	// The navigation tests are about moving over member rows, so they start from
	// a fully unfolded list rather than the folded-repos default.
	expandRepos(m)
	return m
}

// expandRepos unfolds every supatree's repositories section — the sidebar keeps
// them folded by default, and most tests want the member rows on screen.
func expandRepos(m *Model) {
	names := make([]string, 0, len(m.insts))
	for _, inst := range m.insts {
		names = append(names, inst.Name)
	}
	// Through the persisting path, not straight into m.ui: every other fold goes
	// to disk and is read back, so a test fold that only lived in memory would be
	// dropped by the next one.
	m.persistUI(func(u *supatree.UIState) {
		for _, name := range names {
			u.SetReposCollapsed(name, false)
		}
	})
	m.rebuildRows()
}

// rowKinds is the shape of the rendered list, for structural assertions.
func rowKinds(m *Model) []rowKind {
	kinds := make([]rowKind, 0, len(m.rows))
	for _, r := range m.rows {
		kinds = append(kinds, r.kind)
	}
	return kinds
}

// osloModel is a one-supatree, one-member model with its repositories unfolded.
func osloModel(t *testing.T) *Model {
	t.Helper()
	testutil.IsolateHome(t)
	m := New(supatree.DefaultConfig(), zellij.Workspace{})
	m.insts = []*supatree.Instance{{
		Name:    "oslo",
		Members: []supatree.Member{{Alias: "web", Branch: "st/oslo/web"}},
	}}
	expandRepos(m)
	return m
}

func treeAt(m *Model, i int) string {
	if i < 0 || i >= len(m.rows) {
		return ""
	}
	return m.rows[i].tree
}

func TestGotoTopAndBottom(t *testing.T) {
	m := threeTrees(t)

	m.cursor = 5
	if _, _ = m.Update(key("G")); m.cursor != len(m.rows)-1 {
		t.Fatalf("G: cursor = %d, want %d", m.cursor, len(m.rows)-1)
	}

	// gg is a two-key sequence: the first g only arms the prefix.
	_, _ = m.Update(key("g"))
	if m.pending != "g" {
		t.Fatalf("first g did not arm the prefix: pending = %q", m.pending)
	}
	top := rowIndex(m, rowPM) // the first selectable row, under the PM heading
	if m.cursor == top {
		t.Fatal("a lone g must not move the cursor")
	}
	_, _ = m.Update(key("g"))
	if m.cursor != top {
		t.Fatalf("gg: cursor = %d, want %d (the top PM)", m.cursor, top)
	}
	if m.pending != "" {
		t.Fatalf("prefix not cleared: %q", m.pending)
	}
}

// An unrecognized second key cancels the prefix and is handled on its own, so a
// mistyped g doesn't swallow the next command.
func TestPendingPrefixFallsThrough(t *testing.T) {
	m := threeTrees(t)
	m.cursor = 0
	_, _ = m.Update(key("g"))
	_, _ = m.Update(key("j"))
	if m.pending != "" {
		t.Fatalf("prefix not cleared: %q", m.pending)
	}
	if m.cursor == 0 {
		t.Fatal("j after a cancelled g prefix should still move down")
	}
}

func TestJumpTreeMovesBetweenSupatrees(t *testing.T) {
	m := threeTrees(t)
	m.cursor = rowIndex(m, rowTree) // berlin's tree row

	_, _ = m.Update(key("}"))
	if got := treeAt(m, m.cursor); got != cairo || m.rows[m.cursor].kind != rowTree {
		t.Fatalf("} from berlin landed on %q (kind %v), want cairo tree row", got, m.rows[m.cursor].kind)
	}
	_, _ = m.Update(key("}"))
	if got := treeAt(m, m.cursor); got != delhi {
		t.Fatalf("} again landed on %q, want delhi", got)
	}
	// Past the last tree the cursor stays put rather than falling off the end.
	_, _ = m.Update(key("}"))
	if got := treeAt(m, m.cursor); got != delhi {
		t.Fatalf("} past the last tree moved to %q", got)
	}
	_, _ = m.Update(key("{"))
	if got := treeAt(m, m.cursor); got != cairo {
		t.Fatalf("{ landed on %q, want cairo", got)
	}
}

// From inside a tree, { goes to that tree's own header first — "up one level"
// before "up one tree".
func TestJumpTreeBackwardFromMemberRow(t *testing.T) {
	m := threeTrees(t)
	m.selectRow(row{kind: rowMember, tree: cairo, label: "keystone", alias: "keystone"})
	_, _ = m.Update(key("{"))
	if got := treeAt(m, m.cursor); got != cairo || m.rows[m.cursor].kind != rowTree {
		t.Fatalf("{ from a cairo member landed on %q (kind %v), want cairo tree row", got, m.rows[m.cursor].kind)
	}
}

func TestFoldAllAndUnfoldAll(t *testing.T) {
	m := threeTrees(t)
	full := len(m.rows)

	_, _ = m.Update(key("z"))
	_, _ = m.Update(key("M"))
	if len(m.rows) != pmSectionRows+3 {
		t.Fatalf("zM: %d rows, want the PM section and 3 (one per collapsed tree)", len(m.rows))
	}
	for _, name := range []string{berlin, cairo, delhi} {
		if !m.ui.TreeCollapsed(name) {
			t.Errorf("zM did not collapse %s", name)
		}
	}

	_, _ = m.Update(key("z"))
	_, _ = m.Update(key("R"))
	if len(m.rows) != full {
		t.Fatalf("zR: %d rows, want %d", len(m.rows), full)
	}
}

// The wheel pans the viewport without moving the cursor, and the view stays
// where it was left instead of snapping back on the next render.
func TestWheelScrollsWithoutMovingCursor(t *testing.T) {
	m := threeTrees(t)
	m.cursor = 0
	m.viewport(5) // establish viewHeight

	_, _ = m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	if m.cursor != 0 {
		t.Fatalf("wheel moved the cursor to %d", m.cursor)
	}
	if m.scroll != wheelStep {
		t.Fatalf("scroll = %d, want %d", m.scroll, wheelStep)
	}
	if start, _ := m.viewport(5); start != wheelStep {
		t.Fatalf("render snapped back to the cursor: start = %d", start)
	}

	// Wheeling up past the top clamps rather than going negative.
	for range 5 {
		_, _ = m.Update(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	}
	if m.scroll != 0 {
		t.Fatalf("scroll = %d, want 0", m.scroll)
	}

	// A cursor move re-arms follow, pulling the window back to the cursor.
	m.scroll = 10
	m.follow = false
	_, _ = m.Update(key("j"))
	start, end := m.viewport(5)
	if m.cursor < start || m.cursor >= end {
		t.Fatalf("cursor %d outside viewport [%d,%d) after a key press", m.cursor, start, end)
	}
}

func TestClickSelectsRow(t *testing.T) {
	m := threeTrees(t)
	m.cursor = 0
	m.viewport(50) // everything visible, scroll 0

	// berlin's first member follows the PM section, then tree, agents, main,
	// repositories.
	member := pmSectionRows + 4
	_, _ = m.Update(tea.MouseMsg{
		Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease, Y: member + rowsTopOffset,
	})
	if m.cursor != member || m.rows[m.cursor].kind != rowMember {
		t.Fatalf("click selected row %d, want member row %d", m.cursor, member)
	}

	// Clicking a subheader or the divider is ignored — the cursor never rests on
	// either.
	for _, y := range []int{0, pmSectionRows - 1, pmSectionRows + 1} {
		_, _ = m.Update(tea.MouseMsg{
			Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease, Y: y + rowsTopOffset,
		})
		if m.cursor != member {
			t.Fatalf("click on non-selectable row %d moved the cursor to %d", y, m.cursor)
		}
	}
}

// A background reload (tick/focus) must not yank a wheel-scrolled view back to
// the cursor — that would make the sidebar unreadable while scrolling.
func TestReloadKeepsScrollPosition(t *testing.T) {
	m := threeTrees(t)
	m.cursor = 0
	m.viewport(5)
	m.scrollBy(wheelStep)

	m.reloadWithSelection()
	if m.follow {
		t.Fatal("reload re-armed follow, which would snap the view back to the cursor")
	}
}

// rowIndex returns the index of the first row of the given kind.
func rowIndex(m *Model, k rowKind) int {
	for i, r := range m.rows {
		if r.kind == k {
			return i
		}
	}
	return -1
}

// memberModel builds a one-supatree model whose single member is checked out on
// disk, so the open paths get past their existence check.
func memberModel(t *testing.T) (*Model, string) {
	t.Helper()
	testutil.IsolateHome(t)
	path := t.TempDir()
	m := New(supatree.DefaultConfig(), zellij.Workspace{})
	m.insts = []*supatree.Instance{{
		Name: berlin, Slug: berlin,
		Members: []supatree.Member{
			{Alias: "terraform", Path: path, Branch: "st/berlin/terraform", Exists: true},
		},
	}}
	expandRepos(m)
	return m, path
}

// Enter on a member row stands in the repo (a shell pane); it must not launch
// the repo-scoped agent, which is what `a` is for.
func TestEnterOnMemberRowOpensShell(t *testing.T) {
	m, path := memberModel(t)
	t.Setenv("ZELLIJ", "")

	m.cursor = rowIndex(m, rowMember)
	_, cmd := m.updateNormal(key("enter"))
	if cmd == nil {
		t.Fatal("enter on a member row returned no command")
	}
	done, ok := cmd().(actionDoneMsg)
	if !ok {
		t.Fatalf("enter produced %T, want actionDoneMsg", cmd())
	}
	// Outside zellij there is no pane to open, and the shell path says so while
	// naming the directory. The agent path would have failed on the sandbox
	// instead, so this is what distinguishes the two.
	if done.err == nil || !strings.Contains(done.err.Error(), "not inside zellij") {
		t.Fatalf("err = %v, want the shell path's not-inside-zellij error", done.err)
	}
	if !strings.Contains(done.err.Error(), path) {
		t.Errorf("err = %v, want it to name the member path %q", done.err, path)
	}
}

// `a` means "give me an agent here" on every row: a name prompt at the tree
// level, the repo-scoped agent on a member row.
func TestAgentKeyIsContextual(t *testing.T) {
	m, _ := memberModel(t)

	m.cursor = rowIndex(m, rowMember)
	if _, cmd := m.updateNormal(key("a")); cmd == nil {
		t.Error("a on a member row returned no command")
	}
	if m.mode != modeNormal {
		t.Errorf("a on a member row: mode = %v, want modeNormal (no name prompt)", m.mode)
	}

	m.cursor = rowIndex(m, rowTree)
	m.updateNormal(key("a"))
	if m.mode != modeNewAgent {
		t.Fatalf("a on a tree row: mode = %v, want modeNewAgent", m.mode)
	}
	if m.actionTree != berlin {
		t.Errorf("actionTree = %q, want %q", m.actionTree, berlin)
	}
}

// The footer names whatever enter does on the row under the cursor, so the
// split between "stand in it" and "open it" is discoverable without the docs.
func TestFooterHintFollowsRowKind(t *testing.T) {
	m, _ := memberModel(t)

	m.cursor = rowIndex(m, rowMember)
	if got := m.footer(); !strings.Contains(got, "enter shell") {
		t.Errorf("member row footer = %q, want it to hint a shell", got)
	}
	m.cursor = rowIndex(m, rowAgent)
	if got := m.footer(); !strings.Contains(got, "enter open") {
		t.Errorf("agent row footer = %q, want it to hint open", got)
	}
}

// An invalid supatree name is reported while it is still being typed, the
// prompt stays open so it can be fixed in place, and the complaint disappears
// with the character that caused it — rather than being left in the footer to
// sit under the next attempt.
func TestInvalidTreeNameWarnsInlineAndClears(t *testing.T) {
	testutil.IsolateHome(t)
	m := New(supatree.DefaultConfig(), zellij.Workspace{})

	m.updateNormal(key("n"))
	if m.mode != modeNewTreeName {
		t.Fatalf("n: mode = %v, want modeNewTreeName", m.mode)
	}
	for _, r := range "feature-v1.1" {
		m.Update(key(string(r)))
	}
	if m.inputErr == nil {
		t.Fatal("a dotted name typed into the prompt raised no warning")
	}
	if !strings.Contains(m.footer(), m.inputErr.Error()) {
		t.Errorf("footer %q does not show the warning", m.footer())
	}

	// Enter keeps the prompt open rather than firing a create that would fail.
	if _, cmd := m.Update(key("enter")); cmd != nil {
		t.Error("enter on an invalid name started a create")
	}
	if m.mode != modeNewTreeName {
		t.Fatalf("enter on an invalid name: mode = %v, want the prompt still open", m.mode)
	}

	// Deleting the offending characters clears the warning immediately.
	for range 2 {
		m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	}
	if m.inputErr != nil {
		t.Errorf("warning survived the fix: %v", m.inputErr)
	}
}

// A name already taken by a supatree is refused too — it would collide on the
// generated branch and tab names. Workbench's worktree names are not consulted:
// the two tools share no namespace.
func TestTreeNameValidationRejectsDuplicates(t *testing.T) {
	testutil.IsolateHome(t)
	m := New(supatree.DefaultConfig(), zellij.Workspace{})
	m.insts = []*supatree.Instance{{Name: berlin}}

	if err := m.validateTreeName(""); err != nil {
		t.Errorf("a blank name is auto-generated, not an error: %v", err)
	}
	if err := m.validateTreeName("madrid"); err != nil {
		t.Errorf("valid name rejected: %v", err)
	}
	if err := m.validateTreeName(berlin); err == nil {
		t.Error("an existing supatree name was accepted")
	}
}

// A message or error from a finished action is cleared by the next keystroke,
// so it never outlives the moment it described.
func TestKeypressClearsLastActionResult(t *testing.T) {
	m := osloModel(t)
	m.err = errors.New("name must be lowercase alphanumeric and hyphens")
	m.msg = "created oslo"

	m.updateNormal(key("j"))
	if m.err != nil || m.msg != "" {
		t.Errorf("stale result survived a keystroke: err = %v, msg = %q", m.err, m.msg)
	}
}

// ? opens the keybinding reference and any key but a motion closes it again.
func TestHelpOpensAndCloses(t *testing.T) {
	m := osloModel(t)

	m.Update(key("?"))
	if m.mode != modeHelp {
		t.Fatalf("?: mode = %v, want modeHelp", m.mode)
	}
	out := m.View()
	for _, want := range []string{"Navigation", "Folding", "zM / zR", "dashboard"} {
		if !strings.Contains(out, want) {
			t.Errorf("help view missing %q:\n%s", want, out)
		}
	}

	m.Update(key("x"))
	if m.mode != modeNormal {
		t.Fatalf("a key did not dismiss the help: mode = %v", m.mode)
	}
	if strings.Contains(m.View(), "press any key to close") {
		t.Error("help still rendered after being dismissed")
	}
}

// The PM section leads the list even with no supatrees, under its heading, is
// where gg lands, and the tree-scoped keys do nothing on it rather than acting
// on a tree named "".
func TestPMRowPinnedAtTop(t *testing.T) {
	testutil.IsolateHome(t)
	m := New(supatree.DefaultConfig(), zellij.Workspace{})
	if got := rowKinds(m); !slices.Equal(got, []rowKind{rowHeader, rowPM, rowDivider}) {
		t.Fatalf("empty rows = %v, want the PM section alone", got)
	}
	out := m.View()
	if !strings.Contains(out, "Product Managers") || !strings.Contains(out, "◆ pm") {
		t.Errorf("empty view missing the PM section:\n%s", out)
	}

	m = threeTrees(t)
	m.cursor = rowIndex(m, rowTree)
	_, _ = m.Update(key("k"))
	if sel := m.selected(); sel == nil || sel.kind != rowPM {
		t.Fatalf("k from the first tree landed on %+v, want the PM row (skipping the divider)", sel)
	}
	_, _ = m.Update(key("k"))
	if sel := m.selected(); sel == nil || sel.kind != rowPM {
		t.Fatalf("k from the PM row landed on %+v, want it to stay off the heading", sel)
	}
	if !strings.Contains(m.footer(), "enter PM") {
		t.Errorf("footer on the PM row = %q, want the PM hint", m.footer())
	}

	before := len(m.rows)
	for _, k := range []string{" ", "h", "l", "s"} {
		_, cmd := m.Update(key(k))
		if m.mode != modeNormal || cmd != nil || len(m.rows) != before {
			t.Fatalf("%q on the PM row acted: mode %v, cmd %v, rows %d→%d", k, m.mode, cmd != nil, before, len(m.rows))
		}
	}
	if cmd := m.handToPM(); cmd != nil {
		t.Error("m on the PM row queued a request about no supatree")
	}
}

func TestPMRowShowsPendingAndRunning(t *testing.T) {
	m := threeTrees(t)
	pm := m.renderPM(supatree.DefaultPMName, false)
	if strings.Contains(pm, "✉") || strings.Contains(pm, "●") {
		t.Fatalf("idle PM row has badges: %q", pm)
	}
	_, _ = m.Update(pmPendingMsg{n: map[string]int{supatree.DefaultPMName: 2}})
	_, _ = m.Update(runningMsg{tabs: map[string]bool{supatree.PMTab: true}})
	pm = m.renderPM(supatree.DefaultPMName, false)
	if !strings.Contains(pm, "✉2") || !strings.Contains(pm, "●") {
		t.Fatalf("PM row = %q, want the pending count and the running dot", pm)
	}
}

// Every PM gets a row, top first, and each badge is its own.
func TestPMSectionListsEveryPM(t *testing.T) {
	m := threeTrees(t)
	if _, err := supatree.AddPM("research", ""); err != nil {
		t.Fatal(err)
	}
	m.reload()
	if got := rowKinds(m)[:4]; !slices.Equal(got, []rowKind{rowHeader, rowPM, rowPM, rowDivider}) {
		t.Fatalf("PM section = %v, want a heading, both PMs and the divider", got)
	}
	if m.rows[1].label != supatree.DefaultPMName || m.rows[2].label != "research" {
		t.Errorf("PM rows = %q, %q; want the default on top", m.rows[1].label, m.rows[2].label)
	}
	_, _ = m.Update(runningMsg{tabs: map[string]bool{supatree.PMTab + ":research": true}})
	if strings.Contains(m.renderPM(supatree.DefaultPMName, false), "●") || !strings.Contains(m.renderPM("research", false), "●") {
		t.Error("the running dot follows the wrong PM's tab")
	}
}

// p names a new PM (a on a PM row only points at p); d removes one, after a
// confirmation, and is refused for the only PM.
func TestPMSectionAddAndRemove(t *testing.T) {
	m := threeTrees(t)
	m.cursor = rowIndex(m, rowPM)

	_, cmd := m.Update(key("d"))
	if m.mode != modeNormal || cmd != nil || !strings.Contains(m.msg, "only PM") {
		t.Fatalf("d on the only PM: mode %v, cmd %v, msg %q; want a refusal", m.mode, cmd != nil, m.msg)
	}

	_, _ = m.Update(key("a"))
	if m.mode != modeNormal || !strings.Contains(m.msg, "p adds a PM") {
		t.Fatalf("a on the PM row: mode %v, msg %q; want a pointer to p", m.mode, m.msg)
	}
	_, _ = m.Update(key("p"))
	if m.mode != modeNewPM {
		t.Fatalf("p: mode %v, want the new-PM prompt", m.mode)
	}
	for _, r := range supatree.DefaultPMName {
		_, _ = m.Update(key(string(r)))
	}
	if m.inputErr == nil {
		t.Error("a duplicate PM name was not flagged")
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	if _, err := supatree.AddPM("research", ""); err != nil {
		t.Fatal(err)
	}
	m.reload()
	m.cursor = rowIndex(m, rowPM) + 1
	_, _ = m.Update(key("d"))
	if m.mode != modeConfirmDeletePM || m.actionPM != "research" {
		t.Fatalf("d on research: mode %v, target %q; want a confirmation", m.mode, m.actionPM)
	}
	if !strings.Contains(m.footer(), `remove PM "research"`) {
		t.Errorf("footer = %q, want the confirmation", m.footer())
	}
	_, cmd = m.Update(key("n"))
	if m.mode != modeNormal || cmd != nil {
		t.Fatal("declining the removal still removed")
	}
}

// The reference is taller than most sidebars: j/k scroll it within the pane
// instead of closing it, and the footer says there is more.
func TestHelpScrolls(t *testing.T) {
	m := osloModel(t)
	m.height = 20
	m.Update(key("?"))
	out := m.View()
	if strings.Contains(out, "Zellij session") || !strings.Contains(out, "j/k scroll") {
		t.Fatalf("a short pane should window the help and say how to scroll:\n%s", out)
	}
	m.Update(key("G"))
	if m.mode != modeHelp {
		t.Fatalf("G closed the help")
	}
	out = m.View()
	if !strings.Contains(out, "quit session") || strings.Count(out, "\n") > m.height {
		t.Fatalf("G should show the end of the help within the pane:\n%s", out)
	}
}

// d on an agent row removes that agent after a confirmation — not the supatree
// around it — and refuses the main agent, which goes with the tree.
func TestDeleteAgent(t *testing.T) {
	m := osloModel(t)
	inst := m.instance("oslo")
	inst.Root = filepath.Join(t.TempDir(), "oslo")
	if _, _, err := supatree.EnsureAgent(inst.Root, "oslo", "reviewer", "claude", time.Now()); err != nil {
		t.Fatal(err)
	}
	m.rebuildRows()

	m.selectRow(row{kind: rowAgent, tree: "oslo", label: "reviewer"})
	if sel := m.selected(); sel == nil || sel.label != "reviewer" {
		t.Fatalf("no reviewer row: %v", rowKinds(m))
	}
	_, _ = m.Update(key("d"))
	if m.mode != modeConfirmDeleteAgent || !strings.Contains(m.footer(), "oslo:reviewer") {
		t.Fatalf("d on an agent: mode %v, footer %q; want the agent confirmation", m.mode, m.footer())
	}
	_, cmd := m.Update(key("y"))
	if cmd == nil {
		t.Fatal("confirming returned no command")
	}
	if done, ok := cmd().(actionDoneMsg); !ok || done.err != nil {
		t.Fatalf("removing the agent: %+v", done)
	}
	agents, _ := supatree.LoadAgents(inst.Root)
	if supatree.FindAgent(agents, "reviewer") != nil {
		t.Fatalf("reviewer still registered: %+v", agents)
	}

	m.rebuildRows()
	m.selectRow(row{kind: rowAgent, tree: "oslo", label: supatree.MainAgent})
	_, cmd = m.Update(key("d"))
	if m.mode != modeNormal || cmd != nil || !strings.Contains(m.msg, "main") {
		t.Fatalf("d on main: mode %v, msg %q; want a refusal", m.mode, m.msg)
	}
}

// Review trees sit in a Reviews section of their own, below the WIP one.
func TestReviewTreesHaveTheirOwnSection(t *testing.T) {
	m := threeTrees(t)
	m.insts[1].Mode = supatree.ModeReviewing
	m.rebuildRows()
	var headings []string
	reviewsAt, cairoAt := -1, -1
	for i, r := range m.rows {
		if r.kind == rowHeader {
			headings = append(headings, r.label)
			if r.label == reviewsHeaderLabel {
				reviewsAt = i
			}
		}
		if r.kind == rowTree && r.tree == m.insts[1].Name {
			cairoAt = i
		}
	}
	if !slices.Equal(headings, []string{pmHeaderLabel, wipHeaderLabel, reviewsHeaderLabel}) {
		t.Fatalf("headings = %v", headings)
	}
	if cairoAt < reviewsAt {
		t.Fatalf("the review tree (row %d) is above the Reviews heading (row %d)", cairoAt, reviewsAt)
	}
}

// A supatree being created shows up at once, under WIP, and gives way to the
// real row when the creation settles.
func TestCreatingTreeShowsPlaceholder(t *testing.T) {
	m := threeTrees(t)
	m.creating = []string{"lima"}
	m.rebuildRows()
	if i := slices.IndexFunc(m.rows, func(r row) bool { return r.kind == rowCreating && r.tree == "lima" }); i < 0 {
		t.Fatalf("no placeholder row: %v", rowKinds(m))
	}
	if !strings.Contains(m.View(), "lima  creating…") {
		t.Errorf("placeholder not rendered:\n%s", m.View())
	}
	_, _ = m.Update(actionDoneMsg{err: errors.New("boom"), settled: "lima"})
	if slices.ContainsFunc(m.rows, func(r row) bool { return r.kind == rowCreating }) {
		t.Fatal("placeholder outlived its creation")
	}
}

// main is listed first in every tree, whether or not agents.yml has recorded
// it: a named agent added before main was ever opened used to replace it.
func TestMainAgentAlwaysListed(t *testing.T) {
	m := osloModel(t)
	inst := m.instance(oslo)
	inst.Root = filepath.Join(t.TempDir(), oslo)
	agentLabels := func() []string {
		var got []string
		for _, r := range m.rows {
			if r.kind == rowAgent && r.tree == oslo {
				got = append(got, r.label)
			}
		}
		return got
	}

	if _, _, err := supatree.EnsureAgent(inst.Root, oslo, "reviewer", "claude", time.Now()); err != nil {
		t.Fatal(err)
	}
	m.rebuildRows()
	if got := agentLabels(); !slices.Equal(got, []string{supatree.MainAgent, "reviewer"}) {
		t.Fatalf("agent rows = %v, want main then reviewer", got)
	}

	// Once main is opened it is recorded too — after reviewer — and is still
	// listed once, first.
	if _, _, err := supatree.EnsureAgent(inst.Root, oslo, supatree.MainAgent, "claude", time.Now()); err != nil {
		t.Fatal(err)
	}
	m.rebuildRows()
	if got := agentLabels(); !slices.Equal(got, []string{supatree.MainAgent, "reviewer"}) {
		t.Fatalf("agent rows = %v, want main then reviewer", got)
	}
}

// assertFits fails if any line of out is wider than width.
func assertFits(t *testing.T, what, out string, width int) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if w := lipgloss.Width(line); w > width {
			t.Errorf("%s: line %q is %d wide, pane is %d:\n%s", what, line, w, width, out)
		}
	}
}

// A pane narrower than the help, a confirmation or a message folds them
// rather than clipping them: the end of each is still on screen.
func TestNarrowPaneFoldsText(t *testing.T) {
	const width = 24
	m := osloModel(t)
	m.width = width

	m.Update(key("?"))
	out := strings.Join(m.panelLines(), "\n")
	assertFits(t, "help", out, width)
	// Folding moves words onto continuation lines; read it back as prose.
	words := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{"hand this row to the top PM", "it (closes its tab)", "repo agent on a repo"} {
		if !strings.Contains(words, want) {
			t.Errorf("folded help lost %q:\n%s", want, out)
		}
	}
	m.Update(key("x"))

	m.mode, m.actionTree, m.actionAgent = modeConfirmDeleteAgent, oslo, "a-rather-long-agent-name"
	out = m.footer()
	assertFits(t, "confirmation", out, width)
	if !strings.Contains(out, "[y/N]") {
		t.Errorf("confirmation lost its [y/N]:\n%s", out)
	}

	m.mode = modeNormal
	m.msg = "main goes with the supatree — d on the tree row deletes it"
	out = m.footer()
	assertFits(t, "message", out, width)
	if !strings.Contains(out, "deletes it") {
		t.Errorf("message clipped:\n%s", out)
	}
}

// An error takes a few lines of the footer, not the pane: its first line, cut
// short, and the keys. e opens the whole of it in the panel the help uses.
func TestErrorSummaryAndDetail(t *testing.T) {
	const width = 28
	m := osloModel(t)
	m.width = width
	m.err = errors.New(`PM infra: nono cannot load profile "supatree-agent", so the agent would exit as soon as it started:
  [err] File read error: Profile read error at supatree-agent: profile file not found
if nono was upgraded, check: nono outdated`)

	out := m.footer()
	assertFits(t, "error footer", out, width)
	if n := strings.Count(out, "\n") + 1; n > errSummaryLines+1 {
		t.Errorf("error footer is %d lines, want at most %d:\n%s", n, errSummaryLines+1, out)
	}
	if strings.Contains(out, "profile file not found") || !strings.Contains(out, "e full error") {
		t.Errorf("footer should show the summary and how to see the rest:\n%s", out)
	}

	m.Update(key("e"))
	if m.mode != modeHelp {
		t.Fatalf("e: mode = %v, want the panel", m.mode)
	}
	out = strings.Join(m.panelLines(), "\n")
	assertFits(t, "error detail", out, width)
	if !strings.Contains(out, "found") || !strings.Contains(out, "nono outdated") {
		t.Errorf("detail is missing the rest of the error:\n%s", out)
	}

	m.Update(key("x"))
	if m.mode != modeNormal || m.detail != "" || m.err != nil {
		t.Fatalf("closing the detail: mode %v, detail %q, err %v", m.mode, m.detail, m.err)
	}
	m.Update(key("?"))
	if !strings.Contains(m.View(), "Navigation") {
		t.Error("? after an error detail should show the reference again")
	}
}

// The awkward errors: a long path that has to be broken mid-word (and then cut
// short), a tab, and a double space that is not a help entry's key column.
func TestErrorTextEdgeCases(t *testing.T) {
	const width = 20
	m := osloModel(t)
	m.width = width

	m.err = errors.New("open /Users/someone/supatree/trees/some-very-long-tree-name/repos/api/.git/config: no such file")
	assertFits(t, "broken-word summary", m.footer(), width)
	if !strings.Contains(m.footer(), "…") {
		t.Errorf("a cut summary should say so:\n%s", m.footer())
	}

	m.err = errors.New("failed:\n\tindented by a tab, long enough to fold\nprofile  not found because the file is missing")
	m.Update(key("e"))
	out := strings.Join(m.panelLines(), "\n")
	assertFits(t, "error detail", out, width)
	if strings.Contains(out, "\t") {
		t.Errorf("a tab reached the screen:\n%s", out)
	}
	for _, line := range m.panelLines() {
		if strings.HasPrefix(line, strings.Repeat(" ", 9)) {
			t.Errorf("a double space was taken for a key column: %q\n%s", line, out)
		}
	}
}

// e ends a half-typed two-key sequence like any other key, so closing the
// detail does not leave a g waiting to make the next g a jump to the top.
func TestErrorDetailClearsPendingPrefix(t *testing.T) {
	m := osloModel(t)
	m.Update(key("g"))
	m.err = errors.New("boom")
	m.Update(key("e"))
	if m.pending != "" {
		t.Fatalf("pending = %q after e, want none", m.pending)
	}
}

// wrapHanging keeps a help entry's description beside its key while there is
// room, folds it under the key when there is not, and fits even a pane
// narrower than its own indent.
func TestWrapHanging(t *testing.T) {
	const entry = "  a        new named agent, repo agent on a repo"
	for _, tc := range []struct {
		width int
		first string // the first folded line
	}{
		{28, "  a        new named agent,"},
		{16, "  a"},
		{4, "a"}, // narrower than the hang: folded flush, words broken
		{3, "a"},
	} {
		out := wrapHanging(entry, tc.width, true)
		assertFits(t, fmt.Sprintf("width %d", tc.width), out, tc.width)
		if first, _, _ := strings.Cut(out, "\n"); first != tc.first {
			t.Errorf("width %d: first line %q, want %q\n%s", tc.width, first, tc.first, out)
		}
		// Words may be broken at tiny widths, but none of the text is lost.
		if got, want := strings.Join(strings.Fields(out), ""), strings.Join(strings.Fields(entry), ""); got != want {
			t.Errorf("width %d lost text: %q", tc.width, got)
		}
	}
	const detail = "        indented error line that is long"
	out := wrapHanging(detail, 4, false)
	assertFits(t, "detail at 4", out, 4)
	if got, want := strings.Join(strings.Fields(out), ""), strings.Join(strings.Fields(detail), ""); got != want {
		t.Errorf("a detail line at width 4 lost text: %q", got)
	}
}
