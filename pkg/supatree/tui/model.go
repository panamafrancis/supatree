// Package tui implements the supatree sidebar: a Bubble Tea model showing each
// supatree with its agents and member repositories (dirty + PR status), and
// keys to open/resume agents, sync, and delete.
package tui

import (
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/panamafrancis/supatree/pkg/supatree"
	"github.com/panamafrancis/workbench/pkg/github"
	"github.com/panamafrancis/workbench/pkg/zellij"
)

const (
	tickInterval = 30 * time.Second
	// wheelStep is how many rows one mouse-wheel notch scrolls.
	wheelStep = 3
	// fallbackPage is the half-page distance used by ctrl+d/ctrl+u before the
	// pane has reported its size.
	fallbackPage = 5
	// rowsTopOffset is the number of lines View renders above the first row (the
	// "supatree" header). Mouse Y coordinates are translated through it.
	rowsTopOffset = 1
)

type mode int

const (
	modeNormal mode = iota
	modeNewAgent
	modeNewTree     // choosing a stack (only when >1 stack is registered)
	modeNewTreeName // naming the supatree
	modeConfirmDelete
	modeConfirmQuit
	modeNewPM // naming a new PM (p)
	modeConfirmDeletePM
	modeHelp               // showing the keybinding reference
	modeNewStackName       // naming a new stack (S)
	modeConfirmDeleteAgent // removing an agent (d on an agent row)
)

type rowKind int

const (
	rowTree rowKind = iota
	rowSubheader
	rowRepos // the "repositories" section header — selectable and foldable
	rowAgent
	rowMember
	rowPM       // one PM agent; the section sits above every supatree
	rowDivider  // the rule separating one section from the next
	rowHeader   // a section heading: "Product Managers", "WIP", "Reviews"
	rowCreating // a supatree still being created — a placeholder until it lands
)

// selectable reports whether the cursor may rest on a row of this kind. The
// headings, the divider and a tree still being created are decoration;
// everything else is something enter acts on.
func (k rowKind) selectable() bool {
	return k != rowSubheader && k != rowDivider && k != rowHeader && k != rowCreating
}

// The section headings. Review trees get a section of their own: they are
// someone else's work, and mixed in with yours they read as more of it.
const (
	pmHeaderLabel      = "Product Managers"
	wipHeaderLabel     = "WIP"
	reviewsHeaderLabel = "Reviews"
)

// pmSectionRows is how many rows sit above the first supatree with the single
// default PM: the PM heading, the PM row, the divider and the WIP heading.
const pmSectionRows = 4

// reposLabel is the text of the repositories section header. It is also the row
// identity setReposCollapse re-selects on, so the two must agree.
const reposLabel = "repositories"

type row struct {
	kind  rowKind
	tree  string // supatree name this row belongs to
	label string // agent name / member alias / subheader text
	alias string // member alias (rowMember)
}

// Model is the supatree sidebar model.
type Model struct {
	stCfg       *supatree.Config
	ws          zellij.Workspace
	prCache     *github.Cache
	insts       []*supatree.Instance
	rows        []row
	cursor      int
	dirty       map[string]bool // member path -> dirty
	openTabs    map[string]bool
	ui          *supatree.UIState // fold state, shared on disk with the other tabs' sidebars
	isSidebar   bool
	width       int
	height      int
	scroll      int    // index of the first rendered row (viewport top)
	viewHeight  int    // rows the viewport last rendered; drives ctrl+d/ctrl+u and wheel clamping
	follow      bool   // keep the cursor in view on the next render (false while wheel-scrolled away)
	pending     string // half-typed multi-key sequence ("g" or "z")
	mode        mode
	input       textinput.Model
	inputErr    error           // live validation of the text input (cleared as the user types)
	actionTree  string          // tree targeted by the active input mode
	actionStack string          // stack chosen for a pending new-tree create
	stackCursor int             // cursor within the modeNewTree stack picker
	activeTree  string          // supatree whose Zellij tab this sidebar belongs to ("you are here")
	attention   map[string]bool // supatrees with an event you have not looked at yet
	pms         []supatree.PM   // the PMs, top first
	pmPending   map[string]int  // per PM: requests queued for it that it has not read yet
	actionPM    string          // PM targeted by the active input mode
	actionAgent string          // agent targeted by modeConfirmDeleteAgent (in actionTree)
	creating    []string        // supatrees being created by this sidebar, shown until they land
	helpScroll  int             // first line of the `?` panel in view
	detail      string          // full error text the `?` panel shows instead of the reference (opened by `e`)
	panel       []string        // the `?` panel folded to the pane, for panelKey
	panelKey    panelKey
	fetching    bool   // a PR fetch is in flight
	ghAvailable bool   // gh usable; false after a permanent error suppresses tick fetches
	prHint      string // persistent PR-fetch hint (e.g. "gh rate limited")
	msg         string
	err         error
}

// New builds the sidebar model.
func New(stCfg *supatree.Config, ws zellij.Workspace) *Model {
	cache := github.NewCache(supatree.PRCachePath())
	_ = cache.Load()
	m := &Model{
		stCfg:       stCfg,
		ws:          ws,
		prCache:     cache,
		dirty:       map[string]bool{},
		openTabs:    map[string]bool{},
		ui:          supatree.LoadUIState(),
		ghAvailable: true,
		follow:      true,
		isSidebar:   os.Getenv("SUPATREE_SIDEBAR") == "1",
		activeTree:  treeFromTab(os.Getenv("SUPATREE_ACTIVE_TREE")),
		attention:   map[string]bool{},
		pmPending:   map[string]int{},
		input:       textinput.New(),
	}
	m.reload()
	return m
}

func (m *Model) reload() {
	insts, _ := supatree.List(m.stCfg)
	m.insts = insts
	// The section is never empty: an unreadable registry still shows the
	// default PM, which is what opening one would migrate it to anyway.
	if pms, err := supatree.LoadPMs(); err == nil && len(pms) > 0 {
		m.pms = pms
	} else if len(m.pms) == 0 {
		m.pms = []supatree.PM{{Name: supatree.DefaultPMName}}
	}
	// Fold state lives on disk so every tab's sidebar shows the same shape; a
	// reload is exactly when a fold made in another tab should appear here.
	m.ui = supatree.LoadUIState()
	m.rebuildRows()
}

// attentionWindow bounds how far back the marker looks. An event older than
// this has either been dealt with or stopped mattering, and a marker that never
// clears is a marker people stop reading.
const attentionWindow = 7 * 24 * time.Hour

// attentionFor reads the ledger and returns the supatrees holding news you have
// not seen. A missing or unreadable ledger simply means no markers: the sidebar
// must render whether or not the watcher has ever run.

// reloadWithSelection re-reads live state (so newly created/removed supatrees
// appear without a manual refresh) while keeping the cursor pinned to the same
// logical row across the rebuild, since another tab may have reordered rows.
func (m *Model) reloadWithSelection() {
	var want *row
	if r := m.selected(); r != nil {
		cp := *r
		want = &cp
	}
	m.reload()
	if want != nil {
		m.selectRow(*want)
	}
}

// selectRow moves the cursor to the row matching want's identity (kind + tree +
// label + alias), leaving it where clampCursor lands if there is no match.
func (m *Model) selectRow(want row) {
	for i, r := range m.rows {
		if r.kind == want.kind && r.tree == want.tree && r.label == want.label && r.alias == want.alias {
			m.cursor = i
			break
		}
	}
	m.clampCursor()
}

// setCollapse folds or unfolds a supatree's agents/repos and parks the cursor on
// its (still-visible) tree row so it never lands in the rows that just vanished.
func (m *Model) setCollapse(tree string, collapsed bool) {
	if m.ui.TreeCollapsed(tree) == collapsed {
		return
	}
	m.persistUI(func(u *supatree.UIState) { u.SetTreeCollapsed(tree, collapsed) })
	m.rebuildRows()
	m.selectRow(row{kind: rowTree, tree: tree, label: tree})
	m.follow = true
}

// setReposCollapse folds or unfolds one supatree's repositories section, parking
// the cursor on the section header — the row that survives either way.
func (m *Model) setReposCollapse(tree string, collapsed bool) {
	if m.ui.ReposCollapsed(tree) == collapsed {
		return
	}
	m.persistUI(func(u *supatree.UIState) { u.SetReposCollapsed(tree, collapsed) })
	m.rebuildRows()
	m.selectRow(row{kind: rowRepos, tree: tree, label: reposLabel})
	m.follow = true
}

// setAllCollapsed folds or unfolds every supatree at once (vim's zM / zR),
// keeping the cursor on the supatree it was already in. It moves the repository
// sections with the trees: "unfold all" that left them shut would not be all.
func (m *Model) setAllCollapsed(collapsed bool) {
	cur := ""
	if r := m.selected(); r != nil {
		cur = r.tree
	}
	names := make([]string, 0, len(m.insts))
	for _, inst := range m.insts {
		names = append(names, inst.Name)
	}
	m.persistUI(func(u *supatree.UIState) {
		for _, name := range names {
			u.SetTreeCollapsed(name, collapsed)
			u.SetReposCollapsed(name, collapsed)
		}
	})
	m.rebuildRows()
	if cur != "" {
		m.selectRow(row{kind: rowTree, tree: cur, label: cur})
	}
	m.follow = true
}

// persistUI applies a fold change to the shared on-disk state and adopts the
// result, so the other tabs' sidebars pick it up on their next reload. A write
// failure still leaves m.ui mutated, so this tab stays responsive — the fold is
// simply not shared.
func (m *Model) persistUI(mutate func(*supatree.UIState)) {
	mutate(m.ui)
	if ui, err := supatree.UpdateUIState(mutate); err == nil {
		m.ui = ui
	}
}

// gotoTop / gotoBottom are vim's gg and G.
func (m *Model) gotoTop() {
	m.cursor = 0
	m.clampCursor()
	m.follow = true
}

func (m *Model) gotoBottom() {
	m.cursor = len(m.rows) - 1
	m.clampCursor()
	m.follow = true
}

// jumpTree moves the cursor to the next (delta > 0) or previous (delta < 0)
// supatree header row — vim's paragraph motions over the tree blocks. A jump
// backwards from inside a tree lands on that tree's own header first, which is
// what "go up one tree" means when the cursor sits on an agent or member row.
func (m *Model) jumpTree(delta int) {
	for i := m.cursor + delta; i >= 0 && i < len(m.rows); i += delta {
		if m.rows[i].kind == rowTree {
			m.cursor = i
			m.follow = true
			return
		}
	}
}

// halfPage is the ctrl+d / ctrl+u distance: half the visible rows, or a fixed
// fallback before the pane has reported its size.
func (m *Model) halfPage() int {
	if m.viewHeight <= 0 {
		return fallbackPage
	}
	if half := m.viewHeight / 2; half > 0 {
		return half
	}
	return 1
}

// scrollBy pans the viewport without moving the cursor (mouse wheel). It clears
// the follow flag so the view stays where the user left it instead of snapping
// back to the cursor on the next render or tick.
func (m *Model) scrollBy(delta int) {
	if m.viewHeight <= 0 || len(m.rows) <= m.viewHeight {
		return
	}
	m.follow = false
	m.scroll += delta
	if maxScroll := len(m.rows) - m.viewHeight; m.scroll > maxScroll {
		m.scroll = maxScroll
	}
	if m.scroll < 0 {
		m.scroll = 0
	}
}

// selectByRow moves the cursor to the row at a viewport-relative offset (a mouse
// click). Subheaders and the divider are not selectable, so a click on one is
// ignored.
func (m *Model) selectByRow(visible int) {
	i := m.scroll + visible
	if i < 0 || i >= len(m.rows) || !m.rows[i].kind.selectable() {
		return
	}
	m.cursor = i
	m.follow = true
}

// treeFromTab extracts the supatree name from a Zellij tab identity, which is
// "<tree>" for the main agent and "<tree>:<agent>" otherwise (see TabName).
func treeFromTab(tab string) string {
	name, _, _ := strings.Cut(tab, ":")
	return name
}

func (m *Model) rebuildRows() {
	// The PMs come first and are always there, even with no supatrees: they are
	// the agents that are not inside any of them, so they get a section of their
	// own rather than being mistaken for more trees.
	rows := []row{{kind: rowHeader, label: pmHeaderLabel}}
	for _, p := range m.pms {
		rows = append(rows, row{kind: rowPM, label: p.Name})
	}
	var wip, reviews []*supatree.Instance
	for _, inst := range m.insts {
		if inst.Reviewing() {
			reviews = append(reviews, inst)
		} else {
			wip = append(wip, inst)
		}
	}
	// A tree being created is listed under WIP straight away — creation clones
	// and runs the stack's setup, which can take a while — and gives way to the
	// real row as soon as the tree's state is on disk.
	var creating []string
	for _, name := range m.creating {
		if m.instance(name) == nil {
			creating = append(creating, name)
		}
	}
	// Each tree section appears only once it has something in it, so a fresh
	// install shows the PMs alone and the Reviews heading waits for a review.
	if len(wip) > 0 || len(creating) > 0 {
		rows = append(rows, row{kind: rowDivider}, row{kind: rowHeader, label: wipHeaderLabel})
		for _, inst := range wip {
			rows = m.appendTree(rows, inst)
		}
		for _, name := range creating {
			rows = append(rows, row{kind: rowCreating, tree: name, label: name})
		}
	}
	if len(reviews) > 0 {
		rows = append(rows, row{kind: rowDivider}, row{kind: rowHeader, label: reviewsHeaderLabel})
		for _, inst := range reviews {
			rows = m.appendTree(rows, inst)
		}
	}
	if len(wip) == 0 && len(reviews) == 0 && len(creating) == 0 {
		rows = append(rows, row{kind: rowDivider})
	}
	m.rows = rows
	m.clampCursor()
}

// appendTree adds one supatree's rows: its own, then (unless folded) its agents
// and its repositories section.
func (m *Model) appendTree(rows []row, inst *supatree.Instance) []row {
	rows = append(rows, row{kind: rowTree, tree: inst.Name, label: inst.Name})
	if m.ui.TreeCollapsed(inst.Name) {
		return rows
	}
	agents, _ := supatree.LoadAgents(inst.Root)
	rows = append(rows, row{kind: rowSubheader, tree: inst.Name, label: "agents"})
	// Every tree has a main agent, listed first, whether or not agents.yml has
	// recorded it yet — it is written there only once main is first opened, so
	// a tree given a named agent before that would otherwise lose its main row.
	rows = append(rows, row{kind: rowAgent, tree: inst.Name, label: supatree.MainAgent})
	for _, a := range agents {
		if a.Name == supatree.MainAgent {
			continue
		}
		rows = append(rows, row{kind: rowAgent, tree: inst.Name, label: a.Name})
	}
	// The repositories header is a row of its own rather than a plain
	// subheader: it folds, and folded it still reports every member's PR
	// status as a count badge.
	rows = append(rows, row{kind: rowRepos, tree: inst.Name, label: reposLabel})
	if m.ui.ReposCollapsed(inst.Name) {
		return rows
	}
	for _, mem := range inst.Members {
		rows = append(rows, row{kind: rowMember, tree: inst.Name, label: mem.Alias, alias: mem.Alias})
	}
	return rows
}

func (m *Model) instance(name string) *supatree.Instance {
	for _, i := range m.insts {
		if i.Name == name {
			return i
		}
	}
	return nil
}

func (m *Model) Init() tea.Cmd {
	// Non-forced fetch: honor the on-disk cache. The sidebar runs in a restart
	// loop, so forcing here would re-hit the gh API for every member on every
	// restart and exhaust the rate limit.
	return tea.Batch(m.tickCmd(), m.refreshDirtyCmd(), m.refreshRunningCmd(),
		m.refreshAttentionCmd(), m.refreshPMPendingCmd(), m.fetchPRCmd(false), m.coldStartCmd())
}

func (m *Model) clampCursor() {
	if m.cursor >= len(m.rows) {
		m.cursor = len(m.rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	// Never rest on a subheader or the divider.
	for m.cursor < len(m.rows) && !m.rows[m.cursor].kind.selectable() {
		m.cursor++
	}
	if m.cursor >= len(m.rows) {
		for m.cursor >= 0 && (m.cursor >= len(m.rows) || !m.rows[m.cursor].kind.selectable()) {
			m.cursor--
		}
	}
}

// pm returns the PM named name, or nil.
func (m *Model) pm(name string) *supatree.PM {
	return supatree.FindPM(m.pms, name)
}

func (m *Model) selected() *row {
	if m.cursor >= 0 && m.cursor < len(m.rows) {
		return &m.rows[m.cursor]
	}
	return nil
}

// selectedInTree is the selected row when it belongs to a supatree, and nil on
// the PM row — so the tree-scoped keys (fold, sync, delete, new agent, hand to
// the PM) do nothing there instead of acting on a supatree named "".
func (m *Model) selectedInTree() *row {
	if r := m.selected(); r != nil && r.tree != "" {
		return r
	}
	return nil
}

func (m *Model) tickCmd() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg{} })
}

type tickMsg struct{}
type dirtyMsg struct{ dirty map[string]bool }
type attentionMsg struct{ attention map[string]bool }
type pmPendingMsg struct{ n map[string]int }
type runningMsg struct{ tabs map[string]bool }
type prMsg struct {
	err error
	// deferred counts branches held back to stay above the shared rate-limit
	// reserve; the footer surfaces it so a missing status has a reason.
	deferred int
}

// prSkippedMsg is emitted when a fetch round was ceded to another sidebar
// process (the shared fetch lock was busy); it only clears the in-flight flag,
// leaving the rate-limit hint and gh availability untouched.
type prSkippedMsg struct{}
type actionDoneMsg struct {
	msg string
	err error
	// reveal, when set, is a supatree name to move the cursor onto after the
	// post-action reload — used so a freshly created tree is scrolled into view
	// instead of being added off-screen while the cursor stays where it was.
	reveal string
	// settled, when set, is a supatree whose creation has finished, one way or
	// the other: its placeholder row comes out.
	settled string
}
