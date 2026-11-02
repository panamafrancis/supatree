package supacmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/panamafrancis/supatree/pkg/supatree"
	"github.com/panamafrancis/supatree/pkg/supatree/reviewer"
	"github.com/panamafrancis/workbench/pkg/config"
	"github.com/panamafrancis/workbench/pkg/github"
	"github.com/panamafrancis/workbench/pkg/zellij"
)

var (
	watchOnce    bool
	watchSession string
	watchQuiet   bool
	// watchHeadless runs the watcher as a server: it never exits for want of
	// a session, logs to stderr, and leaves zellij alone — no tabs launched,
	// no panes nudged. Set by --headless or SUPATREE_HEADLESS=1, for a
	// container or a host nobody logs in to. Without it the watcher does all
	// of that whenever a supatree session is up, and skips it while none is.
	watchHeadless bool
)

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Watch every supatree and notify when something needs you",
	Long: "watch is the single background poller. It refreshes PR status on the shared\n" +
		"staleness gate, derives the activity summary, diffs it against the previous\n" +
		"round, appends what changed to the activity ledger, and delivers the few\n" +
		"events that warrant interrupting you as desktop notifications.\n\n" +
		"It is a singleton: a second instance exits quietly rather than doubling the\n" +
		"GitHub API load. `supatree start` spawns one automatically, so running this by\n" +
		"hand is for debugging or for a cron entry that covers the hours when no\n" +
		"session is open.\n\n" +
		"It exits once no supatree session is left, unless the automated reviewer is\n" +
		"enabled: then it stays up and keeps reviewing, and serves the sessions you\n" +
		"open later — launching their tabs and nudging their PMs — as a watcher\n" +
		"`supatree start` spawned would. --headless is for a server: never exit, log to\n" +
		"stderr, never touch zellij.",
	RunE: func(cmd *cobra.Command, args []string) error {
		leaveCallerDir()
		if os.Getenv("SUPATREE_HEADLESS") == "1" {
			watchHeadless = true
		}
		err := config.TryFileLock(supatree.WatchLockPath(), func() error {
			// Only the winner owns the log: a loser rotating it would move the
			// file out from under the watcher that is actually running. A
			// headless watcher logs to stderr, where a container runtime
			// collects it.
			if !watchOnce && !watchHeadless {
				openWatchLog()
				defer closeWatchLog()
			}
			return runWatch(cmd.Context())
		})
		// Losing the election is the designed outcome, not a failure: it is what
		// stops a cron entry double-fetching while the daemon is up.
		if errors.Is(err, config.ErrLockBusy) {
			if !watchQuiet {
				fmt.Fprintln(os.Stderr, "another supatree watcher is running — nothing to do")
			}
			return nil
		}
		return err
	},
}

func runWatch(ctx context.Context) error {
	if watchOnce {
		runOps()
		pmMailTick()
		return watchRound(ctx)
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Tree operations get their own goroutine: a create fetches every member
	// and may take a minute, which must hold up neither launches nor rounds.
	go runOpsLoop(ctx)

	interval := stCfg.ResolveWatchInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// Launches have their own, much faster tick: someone is waiting to see the
	// agent appear, and checking is one stat of a file, not a GitHub round.
	launchTicker := time.NewTicker(launchPollInterval)
	defer launchTicker.Stop()
	self := newBinaryWatch()
	idle := false // no session left, staying up for the reviewer

	if err := watchRound(ctx); err != nil {
		logWatch("round failed: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-launchTicker.C:
			pmMailTick()
			runLaunches(ctx)
		case <-ticker.C:
			// The daemon is spawned by `supatree start` and outlives its parent
			// (start execs into zellij), so nothing else will ever reap it. Its
			// own exit condition is the absence of the sessions it exists to
			// serve — without this it polls GitHub forever — unless the
			// reviewer, which serves no session, is on. A headless watcher
			// runs until it is stopped.
			if !watchHeadless {
				switch {
				case watchSessionsAlive():
					idle = false
				case reviewerEnabled():
					if !idle {
						logWatch("no supatree sessions left — staying up for the reviewer")
						idle = true
					}
				default:
					logWatch("no supatree sessions left — exiting")
					return nil
				}
			}
			if self.replaced() {
				restartWatcher(self.path)
			}
			if err := watchRound(ctx); err != nil {
				logWatch("round failed: %v", err)
			}
		}
	}
}

// watchRound is one full pass: refresh, derive, diff, record, notify.
func watchRound(ctx context.Context) error {
	// Re-read the registry every round: a stack registered while the daemon has
	// been up must not need a restart to be watched.
	cfg, err := supatree.Load()
	if err != nil {
		return fmt.Errorf("load supatree config: %w", err)
	}
	insts, err := supatree.List(cfg)
	if err != nil {
		return err
	}

	cache := github.NewCache(supatree.PRCachePath())
	_ = cache.Load()
	targets := supatree.FetchTargets(insts)
	// The reviewer's repositories ride on the same round: one conditional
	// listing per repo, which a branch in it would have cost anyway.
	if rs, err := reviewer.Resolve(cfg); err == nil {
		targets = append(targets, reviewer.WatchTargets(cfg, rs)...)
	} else if !errors.Is(err, reviewer.ErrDisabled) {
		logWatch("reviewer: %v", err)
	}
	if !cache.InBackoff(github.ResourceCore, time.Now()) {
		if out := supatree.FetchPRs(targets, cache, false); out.Err != nil {
			// A failed fetch is not a failed round: the summary below is still
			// derivable from local git plus whatever the cache already holds.
			logWatch("PR fetch incomplete: %v", out.Err)
		}
	}

	cur := supatree.Status(insts, cache, supatree.StatusOptions{})
	evs := supatree.Diff(supatree.LoadLastSummary(), cur)
	// Drain before recording: an outbox event is news like any other, and a
	// board-tier one exists precisely to be written down rather than delivered.
	// Recording only the diff would silently discard everything the PM said it
	// had recorded quietly.
	evs = append(evs, drainNotifyOutbox()...)

	if err := supatree.AppendEvents(evs); err != nil {
		return err
	}
	// Persist the baseline before notifying. A notifier that hangs or crashes
	// must not cause the same round to be diffed again on restart, which would
	// replay every event in it.
	if err := supatree.SaveLastSummary(cur); err != nil {
		return err
	}

	deliver(ctx, cfg, evs, cur.At)
	runSchedule(cur.At)
	runReviewer(ctx, cfg, cache)
	runLaunches(ctx)
	if known, err := supatree.KnownTreeNames(cfg); err == nil {
		for _, line := range supatree.SweepRemoved(supatreeWorkspace(), known) {
			logWatch("sweep: %s", line)
		}
	}
	return nil
}

// theReviewer is the watcher's automated reviewer (pkg/supatree/reviewer). Its
// tree operations share the op lock, so they never interleave with the op
// queue's or with a restart.
var theReviewer = &reviewer.Reviewer{Lock: &opsMu, Logf: logWatch}

// runReviewer starts a reviewer round in the background: a round can create
// trees, which takes a while, and must not hold up the watcher's own. A round
// still going when the next is due makes that one a no-op.
func runReviewer(ctx context.Context, cfg *supatree.Config, cache *github.Cache) {
	if watchOnce {
		// --once exits when the round returns, which would cut a review run
		// off as soon as it started: plan and act, then wait for the runs.
		theReviewer.Round(ctx, cfg, cache)
		theReviewer.Wait()
		return
	}
	go theReviewer.Round(ctx, cfg, cache)
}

// pmMailTick moves tree agents' messages for a PM into the request queue and,
// with pm_nudge on, nudges each PM that has new requests. It runs on the fast
// launch tick, not in watchRound: a round is a GitHub fetch that can take
// seconds and returns early on a ledger error, and a reply that waits on either
// is a reply the PM reports as never sent. Forwarding first means a reply
// forwarded now is nudged now.
func pmMailTick() {
	cfg, err := supatree.Load()
	if err != nil {
		logWatch("forward PM mail: load config: %v", err)
		return
	}
	if insts, err := supatree.List(cfg); err != nil {
		logWatch("forward PM mail: %v", err)
	} else if _, err := supatree.ForwardPMMail(insts); err != nil {
		logWatch("forward PM mail: %v", err)
	}
	if !cfg.PMNudge || watchHeadless {
		return
	}
	nudged, err := supatree.NudgePMs()
	for _, name := range nudged {
		logWatch("nudged PM %s", name)
	}
	if err != nil {
		logWatch("nudge PMs: %v", err)
	}
}

// binaryWatch notices the watcher's own executable being replaced on disk.
//
// The daemon outlives every upgrade: it exits only when no session is left, and
// a newly spawned watcher loses the election to it quietly. Without this an
// upgraded supatree keeps running the old watcher for days, launching agents
// through the new binary (runLaunches re-execs it) while forwarding mail and
// running ops with the old code.
type binaryWatch struct {
	path    string
	stamp   binaryStamp
	pending binaryStamp // a changed stamp seen once, not yet trusted
}

type binaryStamp struct {
	mod  time.Time
	size int64
}

func statBinary(path string) (binaryStamp, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return binaryStamp{}, false
	}
	return binaryStamp{mod: fi.ModTime(), size: fi.Size()}, true
}

func newBinaryWatch() *binaryWatch {
	path, err := os.Executable()
	if err != nil {
		return &binaryWatch{}
	}
	stamp, _ := statBinary(path)
	return &binaryWatch{path: path, stamp: stamp}
}

// replaced reports a new binary at the watcher's path once it has been seen
// unchanged on two consecutive checks — a binary still being written must not
// be exec'd half-copied.
func (w *binaryWatch) replaced() bool {
	if w.path == "" {
		return false
	}
	cur, ok := statBinary(w.path)
	if !ok || cur == w.stamp {
		w.pending = binaryStamp{}
		return false
	}
	if cur != w.pending {
		w.pending = cur
		return false
	}
	return true
}

// opsMu is held while an op runs, so a restart never cuts a tree create or
// removal in half.
var opsMu sync.Mutex

// restartWatcher replaces this process with the binary at path, keeping its
// arguments. The singleton lock goes with the old image (Go opens every file
// close-on-exec), so the new one wins the election straight away. On failure
// the old watcher carries on.
func restartWatcher(path string) {
	logWatch("watcher binary %s changed on disk — restarting", path)
	opsMu.Lock()
	err := syscall.Exec(path, os.Args, os.Environ())
	opsMu.Unlock()
	logWatch("restart failed, carrying on: %v", err)
}

// leaveCallerDir moves the watcher out of the directory it was started in.
// It inherits `supatree start`'s, which is often a tree's — and once that tree
// is removed, every git the watcher runs fails with "Unable to read current
// working directory", which failed a burst of review requests in a row. The
// state root is supatree's own and outlives every tree.
func leaveCallerDir() {
	if err := os.Chdir(supatree.StateRoot()); err != nil {
		if home, herr := os.UserHomeDir(); herr == nil {
			_ = os.Chdir(home)
		}
	}
}

// launchPollInterval is how often the watcher checks the launch queue.
const launchPollInterval = 2 * time.Second

// runLaunches opens the agents the PM has asked to have started.
//
// The PM cannot open a tab itself: it runs inside nono, and the watcher is the
// only supatree process outside it. Each launch runs as a child `supatree open
// --background`, which reuses the ordinary open path unchanged — the child is
// pointed at the session through ZELLIJ_SESSION_NAME, exactly as `--session`
// already does — and keeps one failed launch from touching the daemon.
// opsPollInterval is how often the watcher looks for tree operations. A tool
// is blocked waiting on the answer, so it is short; looking is a readdir.
const opsPollInterval = time.Second

// runOpsLoop carries out the tree operations sandboxed processes ask for
// (supatree.Op): creating and removing trees for the PM, syncing a tree for
// its own agents. Only the watcher can: those write worktrees and clones no
// sandbox is granted.
func runOpsLoop(ctx context.Context) {
	if cfg, err := supatree.Load(); err == nil {
		insts, _ := supatree.List(cfg)
		supatree.AbandonInterrupted(supatree.OpSources(insts))
	}
	ticker := time.NewTicker(opsPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOps()
		}
	}
}

// runOps claims and answers every waiting request, one at a time: two creates
// at once would race for the same generated name.
func runOps() {
	opsMu.Lock()
	defer opsMu.Unlock()
	cfg, err := supatree.Load()
	if err != nil {
		logWatch("ops: load config: %v", err)
		return
	}
	insts, _ := supatree.List(cfg)
	for _, claimed := range supatree.ClaimOps(supatree.OpSources(insts)) {
		res := cfg.RunOp(claimed, supatree.OpHooks{Removed: cleanupTreeTabs})
		if res.Err != "" {
			logWatch("op %s %s: %s", claimed.Op.Kind, claimed.Op.ID, res.Err)
		} else {
			logWatch("op %s %s: done (%s)", claimed.Op.Kind, claimed.Op.ID, res.Tree)
		}
		if err := supatree.AnswerOp(claimed.Dir, res); err != nil {
			logWatch("op %s: answer: %v", claimed.Op.ID, err)
		}
	}
}

func runLaunches(ctx context.Context) {
	if watchHeadless {
		// Every queued launch opens a tab, and there is no session to open it
		// in. The queue is left as it is, for a watcher that has one.
		return
	}
	if _, err := os.Stat(supatree.LaunchPath()); err != nil {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		logWatch("launch: %v", err)
		return
	}
	cfg, err := supatree.Load()
	if err != nil {
		logWatch("launch: load config: %v", err)
		return
	}
	insts, _ := supatree.List(cfg)
	gate := newLaunchGate(cfg, insts, time.Now())
	reqs, err := supatree.TakeLaunches(gate.Admit)
	if err != nil {
		logWatch("take launches: %v", err)
		return
	}
	if n := supatree.QueuedLaunches(); n > 0 && n != lastDeferred {
		logWatch("holding %d launch(es): %d/%d agents busy", n, gate.Busy, gate.Max)
	}
	lastDeferred = supatree.QueuedLaunches()

	// A second request for one agent rides on the first's launch, and gets
	// the same answer.
	answered := map[string]supatree.LaunchResult{}
	for _, r := range reqs {
		key := supatree.TabName(r.Tree, r.Agent)
		res, done := answered[key]
		if !done {
			res = launchOne(ctx, exe, r)
			answered[key] = res
			if res.Outcome == supatree.LaunchStarted {
				recentLaunches[key] = time.Now()
			}
		}
		res.ID = r.ID
		if err := supatree.WriteLaunchResult(res); err != nil {
			logWatch("launch %s: answer: %v", key, err)
		}
	}
}

// lastDeferred is the queue length last logged, so a held launch is logged
// when the hold changes rather than every two seconds.
var lastDeferred int

// recentLaunches remembers what this watcher started, and when. A new agent
// has no transcript for its first seconds, so without this the next tick
// would count it idle and admit another launch in its place.
var recentLaunches = map[string]time.Time{}

// newLaunchGate counts the busy agents across every tree: a live tab whose
// transcript has grown within BusyWindow, one this watcher started within it,
// or a headless run.
func newLaunchGate(cfg *supatree.Config, insts []*supatree.Instance, now time.Time) *supatree.LaunchGate {
	for k, at := range recentLaunches {
		if now.Sub(at) >= supatree.BusyWindow {
			delete(recentLaunches, k)
		}
	}
	live := liveAgentTabs()
	gate := &supatree.LaunchGate{Max: cfg.ResolveMaxRunningAgents(), Live: func(tab string) bool { return live[tab] }}
	// A headless run has no tab to be live in, but it is an agent at work:
	// it counts for as long as its process exists.
	gate.Busy += supatree.HeadlessRunning()
	for _, inst := range insts {
		agents, err := supatree.LoadAgents(inst.Root)
		if err != nil {
			continue
		}
		for _, a := range agents {
			tab := supatree.TabName(inst.Name, a.Name)
			_, recent := recentLaunches[tab]
			if live[tab] && (recent || supatree.AgentBusy(cfg, inst.Root, a, now)) {
				gate.Busy++
			}
		}
	}
	return gate
}

// liveAgentTabs is the set of tabs, in the watcher's session, whose agent is
// running. Unreadable is empty: the cap then counts nobody busy and lets
// launches through, as the watcher did before it had a cap.
func liveAgentTabs() map[string]bool {
	live := map[string]bool{}
	session := servedSession()
	if session == "" {
		return live
	}
	panes, err := zellij.ListPanes(session)
	if err != nil {
		return live
	}
	for _, p := range panes {
		if zellij.LivenessIn(panes, p.TabName) == zellij.LivenessAlive {
			live[p.TabName] = true
		}
	}
	return live
}

// launchOne runs one launch as a child `supatree open --background` and reads
// back what it came to.
func launchOne(ctx context.Context, exe string, r supatree.LaunchRequest) supatree.LaunchResult {
	res := supatree.LaunchResult{Tree: r.Tree, Agent: r.Agent, Outcome: supatree.LaunchFailed}
	session := r.Session
	if session == "" {
		session = servedSession()
	}
	if session == "" {
		res.Detail = "no supatree zellij session is open to launch it in"
		logWatch("launch %s:%s: %s", r.Tree, r.Agent, res.Detail)
		return res
	}
	// An unreadable session list is not proof the session is gone, so only
	// a definite "not running" skips the launch.
	if alive, err := zellij.SessionAlive(session); err == nil && !alive {
		res.Detail = fmt.Sprintf("zellij session %s is not running — open the agent from the sidebar instead", session)
		logWatch("launch %s:%s: %s", r.Tree, r.Agent, res.Detail)
		return res
	}
	outcome, detail := runBackgroundOpen(ctx, exe, "open", r.Tree, "--agent", r.Agent, "--session", session, "--background")
	if detail != "" {
		res.Detail = detail
		logWatch("launch %s:%s: %s", r.Tree, r.Agent, res.Detail)
		return res
	}
	res.Outcome = outcome
	logWatch("launch %s:%s in %s: %s", r.Tree, r.Agent, session, outcome)
	return res
}

// runBackgroundOpen runs a `supatree open --background` child and returns
// what it came to, or why it failed.
//
// The child prints its outcome as soon as the open is done, then spends a
// moment more keeping the focus where the human left it. A launch that went
// through is answered at that line, and the child is left to finish on its
// own — waiting it out held every launch behind the queue for the whole
// watch. A failure is read to the end, for its error.
func runBackgroundOpen(ctx context.Context, exe string, args ...string) (supatree.LaunchOutcome, string) {
	lctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	cmd := exec.CommandContext(lctx, exe, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		cancel()
		return supatree.LaunchFailed, err.Error()
	}
	var out strings.Builder
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := sc.Text()
		out.WriteString(line + "\n")
		if outcome, ok := supatree.ParseOpenOutcome(line); ok && outcome != supatree.LaunchFailed {
			go func() {
				_, _ = io.Copy(io.Discard, stdout)
				_ = cmd.Wait()
				cancel()
			}()
			return outcome, ""
		}
	}
	err = cmd.Wait()
	cancel()
	if err != nil {
		return supatree.LaunchFailed, fmt.Sprintf("%v: %s", err, strings.TrimSpace(out.String()+stderr.String()))
	}
	outcome, ok := supatree.ParseOpenOutcome(out.String())
	if !ok {
		outcome = supatree.LaunchStarted
	}
	return outcome, ""
}

// runSchedule fires any due jobs by queueing them for the PM.
//
// It lives in the watcher rather than in its own daemon because the watcher
// already has a loop, already holds the singleton lock, and already runs
// whether or not a PM tab is open. Firing a job is an append to the same
// request queue the sidebar uses, so the PM needs no timer and no new channel:
// a scheduled job and a pressed `m` arrive identically.
func runSchedule(now time.Time) {
	sched, err := supatree.LoadSchedule()
	if err != nil {
		logWatch("schedule: %v", err)
		return
	}
	if len(sched.Jobs) == 0 {
		return
	}
	// Scheduled work reads the cache and never fetches, which is why N jobs
	// across M trees cost nothing: the events below are the ones this round
	// already derived.
	evs, err := supatree.ReadEvents(now.Add(-scheduleEventWindow))
	if err != nil {
		logWatch("schedule events: %v", err)
		return
	}
	due, next := supatree.Due(sched.Jobs, supatree.LoadScheduleState(), evs, now)
	if err := supatree.SaveScheduleState(next); err != nil {
		logWatch("save schedule state: %v", err)
	}
	for _, j := range due {
		if err := supatree.AppendRequest(j.ToRequest(now)); err != nil {
			logWatch("queue %s: %v", j.ID, err)
			continue
		}
		if j.Once {
			if err := supatree.RemoveJob(j.ID); err != nil {
				logWatch("remove one-shot %s: %v", j.ID, err)
			}
		}
	}
}

// scheduleEventWindow bounds how far back the `events_since_last` gate looks.
// A job that has not fired in longer than this asks about a window it cannot
// see, which is the right way round: it fires, and reports that it has been a
// while, rather than replaying a week.
const scheduleEventWindow = 7 * 24 * time.Hour

// deliver applies the tier policy and sends what survives it.
func deliver(ctx context.Context, cfg *supatree.Config, evs []supatree.Event, now time.Time) {
	if len(evs) == 0 {
		return
	}
	state := supatree.LoadNotifyState()
	due, next := supatree.Decide(evs, state, supatree.DecideOptions{FocusedTree: focusedTree()})
	if err := supatree.SaveNotifyState(supatree.PruneNotifyState(next, now)); err != nil {
		logWatch("save notify state: %v", err)
	}

	argv := cfg.ResolveNotifyCommand()
	for _, ev := range due {
		title := ev.Tree
		if ev.Member != "" {
			title = ev.Tree + "/" + ev.Member
		}
		if err := supatree.Notify(ctx, argv, title, ev.Text); err != nil {
			logWatch("notify: %v", err)
		}
	}
}

// drainNotifyOutbox takes everything sandboxed processes have queued for
// delivery and empties the file. The watcher is the only process outside nono,
// so it is the only one that can reach the desktop; the PM and anything else
// append here. They go through the same Decide call as the watcher's own
// events, because an outbox that bypassed the tier policy would be used as a
// way around it.
func drainNotifyOutbox() []supatree.Event {
	var evs []supatree.Event
	err := config.WithFileLock(supatree.NotifyLockPath(), func() error {
		data, err := os.ReadFile(supatree.NotifyPath())
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var ev supatree.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				continue
			}
			evs = append(evs, ev)
		}
		// Truncate rather than remove, so the outbox the PM appends to is
		// always the same file.
		return os.Truncate(supatree.NotifyPath(), 0)
	})
	if err != nil {
		logWatch("drain notify outbox: %v", err)
	}
	return evs
}

// focusedTree is the supatree you are currently looking at, whose notifications
// would be telling you what is already on your screen. Tab names are the tree
// name for the main agent and "<tree>:<agent>" otherwise.
//
// The watcher runs outside every zellij session, so a bare `zellij action` has
// nothing to target and the session must be named explicitly. Failure yields
// "", which suppresses nothing — deliberately, since a missed suppression is an
// annoyance and a missed notification is a bug.
func focusedTree() string {
	// servedSession only names a session it has just seen alive: dump-layout
	// sent to one that is shutting down makes zellij's server panic ("Failed
	// to dump layout").
	session := servedSession()
	if session == "" {
		return ""
	}
	tab := zellij.FocusedTab(session)
	if tab == "" {
		return ""
	}
	name, _, _ := strings.Cut(tab, ":")
	return name
}

// watchSessionsAlive reports whether any supatree zellij session is still up.
func watchSessionsAlive() bool {
	live, err := liveSessions()
	if err != nil {
		// Treat an unreadable session list as "still alive": exiting on a
		// transient zellij error would silently stop notifications for the rest
		// of the day, which is far worse than one extra round of polling.
		return true
	}
	return len(live) > 0
}

// liveSessions lists the supatree zellij sessions that are up.
func liveSessions() ([]string, error) {
	sessions, err := zellij.ListSessions()
	if err != nil {
		return nil, err
	}
	prefix := supatreeWorkspace().SessionPrefix
	var live []string
	for _, s := range sessions {
		if strings.HasPrefix(s.Name, prefix) && !s.Exited {
			live = append(live, s.Name)
		}
	}
	return live, nil
}

// servedSession is the supatree session the watcher serves now — for a
// launch that names none, the busy-agent count and focus suppression: the one
// it was started for (--session) while that is up, else any that is. A
// watcher that outlived its session, or was started before any, so serves
// whichever you open next. "" while none is up, and always for a headless
// watcher, which leaves zellij alone.
func servedSession() string {
	if watchHeadless {
		return ""
	}
	live, err := liveSessions()
	if err != nil {
		return ""
	}
	return pickSession(watchSession, live)
}

// pickSession prefers the session the watcher was started for, else the
// first live one.
func pickSession(own string, live []string) string {
	for _, s := range live {
		if s == own {
			return s
		}
	}
	if len(live) > 0 {
		return live[0]
	}
	return ""
}

// reviewerEnabled reports whether the config turns the reviewer on — the one
// reason for a watcher to outlive every session.
func reviewerEnabled() bool {
	// Enabled, not Resolve: a reviewer block with a mistake in it is still
	// one someone wants running, and the round logs what the mistake is.
	cfg, err := supatree.Load()
	return err == nil && cfg.Reviewer != nil && cfg.Reviewer.Enabled
}

// watchLogMax is the size at which watch.log is rotated to watch.log.1. One
// generation, like the event ledger: enough to see what led up to a problem,
// bounded so a repeating error cannot fill the disk.
const watchLogMax = 1 << 20

var watchLog struct {
	f    *os.File
	size int64
}

// openWatchLog points logWatch at <state>/logs/watch.log. Best effort: if
// it cannot be opened, lines go to stderr as before.
func openWatchLog() {
	if err := os.MkdirAll(supatree.LogsDir(), 0755); err != nil {
		return
	}
	path := watchLogPath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	watchLog.f = f
	if st, err := f.Stat(); err == nil {
		watchLog.size = st.Size()
	}
}

func closeWatchLog() {
	if watchLog.f != nil {
		_ = watchLog.f.Close()
		watchLog.f = nil
	}
}

func watchLogPath() string { return filepath.Join(supatree.LogsDir(), "watch.log") }

// rotateWatchLog moves a full log aside and starts a fresh one. Rotating
// before the write, not after, so the line that tripped it lands in the new
// file rather than the one being renamed away.
func rotateWatchLog() {
	closeWatchLog()
	_ = os.Rename(watchLogPath(), watchLogPath()+".1")
	openWatchLog()
}

// logWatch writes a diagnostic line to watch.log, or to stderr when the
// watcher has no log of its own (a --once run from a shell or cron).
func logWatch(format string, args ...any) {
	line := fmt.Sprintf("[%s] "+format+"\n", append([]any{time.Now().Format(time.RFC3339)}, args...)...)
	if watchLog.f == nil {
		fmt.Fprint(os.Stderr, line)
		return
	}
	if watchLog.size+int64(len(line)) > watchLogMax {
		rotateWatchLog()
		if watchLog.f == nil {
			fmt.Fprint(os.Stderr, line)
			return
		}
	}
	n, _ := watchLog.f.WriteString(line)
	watchLog.size += int64(n)
}

func init() {
	watchCmd.Flags().BoolVar(&watchOnce, "once", false, "run a single round and exit (for cron or scripts)")
	watchCmd.Flags().StringVar(&watchSession, "session", "", "zellij session to serve first, while it is up (otherwise any supatree session that is)")
	watchCmd.Flags().BoolVar(&watchQuiet, "quiet", false, "exit silently when another watcher holds the lock")
	watchCmd.Flags().BoolVar(&watchHeadless, "headless", false, "run as a server: never exit for want of a session, log to stderr, never touch zellij (also SUPATREE_HEADLESS=1)")
}
