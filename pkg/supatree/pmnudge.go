package supatree

import (
	"errors"
	"fmt"

	"github.com/panamafrancis/workbench/pkg/zellij"
)

// PMNudgeText is what the watcher types into a PM's prompt when mail arrives.
// It is fixed on purpose, like WakePrompt: the requests themselves ride in the
// queue, so nothing anyone queued ever reaches a prompt.
const PMNudgeText = "You have new requests. Call the `requests` tool and act on them."

// Seams for tests: the process registry and the zellij calls.
var (
	listProcs    = ListProcs
	sessionAlive = zellij.SessionAlive
	agentPane    = AgentPane
	submitToPane = zellij.SubmitToPane
)

// nudgeState maps a PM name to the queue position of the newest request it was
// nudged about: the byte offset just past it, and the queue size then. An
// offset, not the request's At — a forwarded message keeps the time its tree
// agent wrote into it, which can be older than the last nudge (so it would
// never nudge) or far in the future (so nothing after it would).
type nudgeState map[string]readOffset

// nudgeDue reports whether the newest request ending at end is past the one
// st records. A queue shorter than st was taken against has been truncated or
// rotated, so st means nothing — the same rule readQueueFrom applies to a
// read offset.
func nudgeDue(st readOffset, end, size int64) bool {
	if size < st.Offset || size < st.Size {
		return end > 0
	}
	return end > st.Offset
}

// NudgePMs types PMNudgeText into the agent pane of every running PM with
// mail it has not been nudged about, so it starts a turn and drains `requests`
// rather than waiting for a human to say something. It returns the PMs it
// nudged. The watcher calls it on its launch tick, right after forwarding
// tree agents' replies into the queue; it is the only caller, which is what
// lets the state go unlocked.
//
// A PM is skipped, and retried next call because its state is not advanced,
// when it has no live process, its session is gone, or no pane in its tab is
// certainly the agent: text typed into the wrong terminal is worse than a
// nudge that never comes. Focus is deliberately not checked: zellij reports a
// tab focused whenever it is the client's current one, which a PM tab left
// open in a backgrounded terminal always is, so the check suppressed every
// nudge for the commonest setup there is. The opt-in is the guard instead. A nudge is attempted at most once — the state
// advances even when submitting fails, since a half-delivered one retyped
// would leave the text twice in the prompt.
func NudgePMs() ([]string, error) {
	pms, err := LoadPMs()
	if err != nil {
		return nil, err
	}
	size := queueSize()
	state := nudgeState{}
	_ = readJSON(PMNudgeStatePath(), &state) // missing or torn: at most one extra nudge

	type dueNudge struct {
		pm  PM
		end int64
	}
	var due []dueNudge
	var errs []error
	for _, p := range pms {
		n, end, err := pendingRequestsEnd(p, pms)
		if err != nil {
			errs = append(errs, fmt.Errorf("pending for %s: %w", p.Name, err))
			continue
		}
		if n > 0 && nudgeDue(state[p.Name], end, size) {
			due = append(due, dueNudge{p, end})
		}
	}
	if len(due) == 0 {
		return nil, errors.Join(errs...)
	}

	sessions := map[string]string{}
	for _, r := range listProcs() {
		if r.IsPM() && r.Session != "" {
			sessions[r.PM] = r.Session
		}
	}
	var nudged []string
	changed := false
	for _, d := range due {
		session, ok := sessions[d.pm.Name]
		if !ok {
			continue
		}
		// dump-layout to a server that is shutting down makes zellij panic.
		if alive, err := sessionAlive(session); err != nil || !alive {
			continue
		}
		pane, ok, err := agentPane(session, d.pm.Tab())
		if err != nil {
			errs = append(errs, fmt.Errorf("nudge %s: %w", d.pm.Name, err))
			continue
		}
		if !ok {
			continue
		}
		state[d.pm.Name] = readOffset{Offset: d.end, Size: size}
		changed = true
		if err := submitToPane(session, pane.ID, PMNudgeText); err != nil {
			errs = append(errs, fmt.Errorf("nudge %s: %w", d.pm.Name, err))
			continue
		}
		nudged = append(nudged, d.pm.Name)
	}
	if changed {
		if err := writeJSONAtomic(PMNudgeStatePath(), state); err != nil {
			errs = append(errs, err)
		}
	}
	return nudged, errors.Join(errs...)
}
