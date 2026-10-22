package supatree

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/panamafrancis/workbench/pkg/config"
	"github.com/panamafrancis/workbench/pkg/sandbox"
	"github.com/panamafrancis/workbench/pkg/zellij"
)

// MainAgent is the primary agent every supatree has: the one `enter` opens, and
// the one a bare tab named after the tree belongs to.
const MainAgent = "main"

// TabName is the Zellij tab identity for an agent: "<name>" for the primary
// "main" agent, "<name>:<agent>" otherwise.
func TabName(tree, agent string) string {
	if agent == MainAgent {
		return tree
	}
	return tree + ":" + agent
}

// OpenOptions are the ways a root-agent open can differ.
type OpenOptions struct {
	Model string // model override
	// Wake is set for a launch nobody at the keyboard asked for (the watcher,
	// on a PM's start_agent). An agent that is already running is then woken
	// in place — WakePrompt typed into its pane when mail waits for it —
	// rather than focused: nobody is there to look at the tab, and the agent
	// would otherwise not read its brief until someone typed. A tab it does
	// open is opened in the background: the focus is put back where it was.
	Wake bool
	// Opened, when set, is told what the open came to as soon as that is
	// known — before the background focus watch (keepFocus), which takes
	// focusSettle more. The watcher's child reports through it, so the
	// launch queue does not wait out every watch.
	Opened func(LaunchOutcome, error)
}

// OpenRootAgent opens or resumes a named agent running at the supatree root
// (nono --allow the whole tree). Several agents share the root but resume
// independently via their session IDs. Warnings that do not stop the open go
// to startupW.
//
// The open runs under the tab's lock (openLockPath), so a sidebar enter and a
// watcher launch of the same agent cannot both find no tab and both start it;
// every open is recorded in the launch log.
func OpenRootAgent(inst *Instance, c *Config, ws zellij.Workspace, sidebarWidth, agentName string, opts OpenOptions, startupW io.Writer) (outcome LaunchOutcome, err error) {
	if agentName == "" {
		agentName = MainAgent
	}
	if err := ValidateAgentName(agentName); err != nil {
		return LaunchFailed, err
	}
	if err := checkMainModel(agentName, opts.Model, inst.Model); err != nil {
		return LaunchFailed, err
	}
	tab := TabName(inst.Name, agentName)
	// Going to an agent that is already running is a tab switch, and nothing
	// more. Doing it through the launch path below put every switch behind the
	// tree's open lock — held for seconds by any launch in the same tree — and
	// a parse of Claude's whole config, which is why switching lagged most
	// exactly when other agents were being started. Focusing a live tab starts
	// nothing, so it needs no lock.
	// A named model still goes the full way, where checkAgentModel refuses
	// one the running agent is not on.
	if !opts.Wake && opts.Model == "" && focusLive(tab) {
		logLaunch("agent "+tab, tab, string(LaunchRunning))
		return LaunchRunning, nil
	}
	// Codex chooses its session ID after launch. Serialize opens within one
	// tree so a second agent cannot launch before the first has recorded its
	// start time and the pending-session check below can see it.
	var prev string
	lockErr := config.WithFileLock(openLockPath("\x00"+inst.Name), func() error {
		return config.WithFileLock(openLockPath(tab), func() error {
			outcome, prev, err = openRootAgentLocked(inst, c, ws, sidebarWidth, agentName, tab, opts, startupW)
			return nil
		})
	})
	if lockErr != nil {
		outcome, err = LaunchFailed, lockErr
	}
	what := string(outcome)
	if err != nil {
		what += ": " + err.Error()
	}
	logLaunch("agent "+tab, tab, what)
	if opts.Opened != nil {
		opts.Opened(outcome, err)
	}
	// Outside the locks: watching the focus settle takes a while, and the
	// next launch in this tree need not wait for it.
	if prev != "" && prev != tab {
		keepFocus(prev, tab)
	}
	return outcome, err
}

func openRootAgentLocked(inst *Instance, c *Config, ws zellij.Workspace, sidebarWidth, agentName, tab string, opts OpenOptions, startupW io.Writer) (outcome LaunchOutcome, prev string, err error) {
	if opts.Wake && zellij.TabLiveness("", tab) == zellij.LivenessAlive {
		return wakeAgent(inst.Root, tab, agentName), prev, nil
	}
	// nono refuses a grant on a path that does not exist.
	if err := EnsureLayout(); err != nil {
		return LaunchFailed, prev, err
	}
	// Best effort: an agent without a roster is told the address pattern.
	_ = PublishPMRoster()
	key := inst.Model
	if opts.Model != "" {
		key = opts.Model
	}
	agent, created, err := EnsureAgent(inst.Root, inst.Name, agentName, key, time.Now())
	if err != nil {
		return LaunchFailed, prev, err
	}
	if err := checkAgentModel(agent, opts.Model); err != nil {
		return LaunchFailed, prev, err
	}
	// A record this call made is kept only if the agent launches: one left
	// behind by a refused launch (nono rejecting its profile, say) would be
	// listed as if it were running. A record made earlier — start_agent
	// registers an agent before the watcher launches it — is not ours to drop.
	defer func() {
		if created && err != nil && !LaunchUncertain(err) {
			_, _ = forgetAgent(inst.Root, agentName)
		}
	}()
	// Several agents share this directory, which is what makes Claude's folder
	// trust never stick here (see sandbox.TrustDir). Seed it before launching;
	// failing to is a prompt the user answers, not a reason to refuse to open.
	if err := sandbox.TrustDir(inst.Root); err != nil && startupW != nil {
		_, _ = fmt.Fprintf(startupW, "warning: could not pre-trust %s with claude: %v\n", inst.Root, err)
	}
	// Resume only when this agent's session transcript actually exists; a freshly
	// created agent (or one whose id was never launched) starts a new session so
	// it never lands in another agent's chat.
	model, err := c.Model(agent.Model)
	if err != nil {
		return LaunchFailed, prev, err
	}
	// A CLI that picked its own session ID last time is resumed by that ID
	// once it is known; until then the agent starts fresh again.
	if agent, err = bindSession(inst.Root, agent, model); err != nil {
		return LaunchFailed, prev, err
	}
	resume := sandbox.SessionExistsFor(model, inst.Root, agent.SessionID)
	if !resume && sandbox.PicksOwnSessionID(model) {
		if err := bindEarlierCodexSessions(c, inst.Root, agentName); err != nil {
			return LaunchFailed, prev, err
		}
	}
	// The session a fresh launch starts is found later by its start time, so
	// the time is taken now and recorded only once a launch really happened:
	// recorded on a mere focus, it would move past the running session's
	// start and that session could never be found.
	launchedAt := time.Now()
	if err := NonoPreflight(model.NonoProfile); err != nil {
		return LaunchFailed, prev, err
	}
	nonoArgs := sandbox.BuildGrantedAgentNonoArgs(model, TreeGrants(c, inst), inst.Root, agent.SessionID, agent.Address, resume)
	// Mail waiting means somebody briefed this agent before it was running —
	// the PM starting it, or a sibling. Without a first message it would sit
	// at an empty prompt until a human typed, and the brief would go unread.
	// Ignored when the tab is already live: OpenOrFocusTab only focuses it.
	if HasMail(inst.Root, agentName) {
		nonoArgs = sandbox.AppendPrompt(nonoArgs, model, KickoffPrompt)
	}
	env := withAgentExec(inst.AgentEnv(agentName))
	// Opening a tab focuses it. A launch nobody at the keyboard asked for must
	// not take the terminal away from whoever is typing, so the focus goes
	// back — to the tab focused just before the open, not the one focused when
	// this call began seconds ago, which the human may well have left since.
	// The caller restores it even when the open failed: it may have got as
	// far as the new tab.
	if opts.Wake {
		var why string
		// A launch still watching its own tab settle would have this one
		// record that tab as where the human is: wait for its watch to end.
		_ = config.WithFileLock(focusLockPath(), func() error {
			prev, why = prevTab()
			return nil
		})
		if prev == "" {
			// Nothing to put the focus back on: say so, or a launch that took
			// the terminal leaves no trace of why.
			logLaunch("focus "+tab, tab, "focused tab unreadable before the open, focus will not be restored: "+why)
		}
	}
	action, err := ws.OpenOrFocusTabAction(tab, inst.Root, sidebarWidth, nonoArgs, env)
	if err != nil {
		return LaunchFailed, prev, &LaunchError{Err: err}
	}
	if action != zellij.TabFocused {
		if !resume && sandbox.PicksOwnSessionID(model) {
			if err := updateAgent(inst.Root, agentName, func(r *Agent) { r.LaunchedAt = launchedAt }); err != nil {
				// The agent is running: its record must stay (LaunchError keeps
				// it), even though its session can now not be found.
				return LaunchStarted, prev, &LaunchError{Err: fmt.Errorf("agent started, but its launch time was not recorded, so its session will not resume: %w", err)}
			}
		}
		return LaunchStarted, prev, nil
	}
	// Focused: the agent was running after all (its liveness was unknown
	// above, or this is not a wake). Mail for it is still unread.
	if opts.Wake {
		return wakeAgent(inst.Root, tab, agentName), prev, nil
	}
	return LaunchRunning, prev, nil
}

// focusLive focuses tab when its agent is running, and reports whether it did.
// One pane listing answers both "is there such a tab" and "is it alive"; any
// doubt — an unreadable listing, a dead or missing tab, a failed focus — is
// false, and the caller takes the full open path, which judges it carefully.
func focusLive(tab string) bool {
	if !zellij.IsInZellij() {
		return false
	}
	panes, err := zellij.ListPanes("")
	if err != nil || zellij.LivenessIn(panes, tab) != zellij.LivenessAlive {
		return false
	}
	return zellij.GoToTab(tab) == nil
}

// focusedTab, currentTab and goToTab are swappable in tests, which have no
// zellij session.
var (
	focusedTab = func() string { return zellij.FocusedTab("") }
	currentTab = func() (string, error) { return zellij.CurrentTab("") }
	goToTab    = zellij.GoToTab
)

// prevTabTries and prevTabRetry bound prevTab's attempts.
var (
	prevTabTries = 3
	prevTabRetry = 150 * time.Millisecond
)

// prevTab is the tab a background open must hand the focus back to, and when
// it cannot be read, why. One `dump-layout` was all it used to ask: a reply
// that marked no tab focused — rare, and only ever seen mid-launch — left the
// launch nothing to restore, and the new tab kept the terminal. It now asks
// current-tab-info, falls back to the layout, and tries again before giving up.
func prevTab() (string, string) {
	var why []string
	for i := 0; i < prevTabTries; i++ {
		if i > 0 {
			time.Sleep(prevTabRetry)
		}
		name, err := currentTab()
		if name != "" {
			return name, ""
		}
		if err != nil {
			why = append(why, err.Error())
		}
		if name := focusedTab(); name != "" {
			return name, ""
		}
		why = append(why, "dump-layout marks no tab focused")
	}
	return "", strings.Join(why, "; ")
}

// focusSettle is how long keepFocus watches for the launched tab taking the
// focus back, and focusPoll how often it looks.
// The trace of real launches has zellij's late switch land within half a
// second; the window is kept short because a human who goes to the new tab
// on purpose inside it is sent back.
var (
	focusSettle = 1500 * time.Millisecond
	focusPoll   = 250 * time.Millisecond
)

// focusLockPath serializes background focus handling across launches: one
// launch's keepFocus against the next one's prevTab.
func focusLockPath() string {
	return filepath.Join(StateRoot(), "locks", "focus.lock")
}

// keepFocus returns the focus to prev after tab was opened in the background.
// One go-to-tab straight after the open was not enough: zellij applies a new
// tab's layout after `new-tab` returns, and focuses the tab when it does, so
// on a loaded machine the switch landed after the restore and stuck. It
// therefore keeps watching for a while, and only ever moves the focus off the
// launched tab — never off a tab the human has since gone to.
//
// What it saw goes to the launch log as one line: each change of focus, each
// restore and whether it was refused, timed from the open.
func keepFocus(prev, tab string) {
	_ = config.WithFileLock(focusLockPath(), func() error {
		watchFocus(prev, tab)
		return nil
	})
}

func watchFocus(prev, tab string) {
	start := time.Now()
	deadline := start.Add(focusSettle)
	var trace []string
	last := ""
	for {
		cur := focusedTab()
		if cur != last {
			trace = append(trace, fmt.Sprintf("%dms:%q", time.Since(start).Milliseconds(), cur))
			last = cur
		}
		if cur == tab {
			if err := goToTab(prev); err != nil {
				trace = append(trace, "restore failed: "+err.Error())
			} else {
				trace = append(trace, "restored")
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(focusPoll)
	}
	logLaunch("focus "+tab, tab, fmt.Sprintf("prev=%q %s", prev, strings.Join(trace, " ")))
}

// wakeAgent hands a running agent the mail waiting for it, by typing
// WakePrompt into its pane. With no mail there is nothing to wake it for, and
// with no agent pane to be sure of, nothing is typed: a mailed brief the agent
// reads on its next turn is better than text typed into the wrong terminal.
func wakeAgent(root, tab, agent string) LaunchOutcome {
	if !HasMail(root, agent) {
		return LaunchRunning
	}
	pane, ok, err := AgentPane("", tab)
	if err != nil || !ok {
		return LaunchRunning
	}
	if err := zellij.SubmitToPane("", pane.ID, WakePrompt); err != nil {
		return LaunchRunning
	}
	return LaunchWoken
}

// LaunchError is a failure in opening an agent's tab. Zellij may have opened
// it anyway — a command that timed out after doing its work — so the agent may
// be running, and its record must not be taken back.
type LaunchError struct{ Err error }

func (e *LaunchError) Error() string { return e.Err.Error() }
func (e *LaunchError) Unwrap() error { return e.Err }

// LaunchUncertain reports whether err came from opening the tab, after which
// the agent may or may not be running.
func LaunchUncertain(err error) bool {
	var le *LaunchError
	return errors.As(err, &le)
}

// requireMember resolves a member alias to a worktree that actually exists on
// disk — the shared precondition of every "open this member" entry point.
func requireMember(inst *Instance, alias string) (*Member, error) {
	m := inst.FindMember(alias)
	if m == nil {
		return nil, fmt.Errorf("repo %q is not a member of supatree %q", alias, inst.Name)
	}
	if !m.Exists {
		return nil, fmt.Errorf("member %q not created yet — run: supatree sync %s", alias, inst.Name)
	}
	return m, nil
}

// OpenMemberAgent opens an agent scoped to a single member repo (MemberGrants:
// that worktree and its clone's git dir). Member worktrees have unique paths, so directory-based
// resume works without session IDs.
func OpenMemberAgent(inst *Instance, c *Config, ws zellij.Workspace, sidebarWidth, alias, modelOverride string) (bool, error) {
	m, err := requireMember(inst, alias)
	if err != nil {
		return false, err
	}
	if err := EnsureLayout(); err != nil {
		return false, err
	}
	_ = PublishPMRoster()
	key := inst.Model
	if modelOverride != "" {
		key = modelOverride
	}
	model, err := c.Model(key)
	if err != nil {
		return false, err
	}
	if err := NonoPreflight(model.NonoProfile); err != nil {
		return false, err
	}
	nonoArgs := sandbox.BuildGrantedAgentNonoArgs(model, MemberGrants(c, inst, alias), m.Path, "", "", true)
	env := inst.AgentEnv(alias)
	env["SUPATREE_MEMBER"] = alias
	return ws.OpenOrFocusTab(TabName(inst.Name, alias), m.Path, sidebarWidth, nonoArgs, withAgentExec(env))
}

// OpenMemberShell opens a plain shell pane in the caller's current tab, rooted
// at a member repo's worktree. It is the "take me there" counterpart to
// OpenMemberAgent: no sandbox, no session, no tab — the member rows in the
// sidebar report state (branch, dirty, PR), so the obvious thing to do with one
// is stand in it.
func OpenMemberShell(inst *Instance, alias string) error {
	m, err := requireMember(inst, alias)
	if err != nil {
		return err
	}
	if !zellij.IsInZellij() {
		return fmt.Errorf("not inside zellij — cd %s", m.Path)
	}
	// Named for the location, matching the agent panes' "{repo}/{worktree}"
	// display-name convention.
	return zellij.NewPane(inst.Name+"/"+alias, m.Path)
}
