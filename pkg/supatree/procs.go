package supatree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/panamafrancis/workbench/pkg/zellij"
)

// ProcRecord is one running agent process, written by `supatree agent-exec`
// — which sits between every agent pane and its nono — when it starts nono, and
// removed when nono exits.
//
// It exists because a tab is not an agent. Closing a pane only hangs up on it,
// and an agent that outlives the hangup keeps running with no pane to close
// (#1, #2); a PM opened twice in a race is two processes behind one tab name.
// Nothing could see those without `ps`, which no sandbox may run.
type ProcRecord struct {
	PID     int    `json:"pid"`      // agent-exec itself; signalled to stop the agent
	NonoPID int    `json:"nono_pid"` // its child, which runs the agent
	Tree    string `json:"tree,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Member  string `json:"member,omitempty"`  // a member agent's alias
	PM      string `json:"pm,omitempty"`      // the PM's name, when this is a PM
	Session string `json:"session,omitempty"` // the zellij session its pane is in
	// SessionID is the agent's conversation, read from its --session-id or
	// --resume: two processes with the same one are the same agent twice.
	SessionID string    `json:"session_id,omitempty"`
	Cwd       string    `json:"cwd,omitempty"`
	Started   time.Time `json:"started"`
	// Headless is a run with no terminal (RunHeadless): it has no tab, and
	// counts against the launch cap by existing.
	Headless bool `json:"headless,omitempty"`
}

// IsPM reports whether the record is a PM's.
func (r ProcRecord) IsPM() bool { return r.PM != "" }

// Tab is the zellij tab the process was launched into.
func (r ProcRecord) Tab() string {
	switch {
	case r.IsPM():
		return PM{Name: r.PM}.Tab()
	case r.Member != "":
		return TabName(r.Tree, r.Member)
	default:
		return TabName(r.Tree, r.Agent)
	}
}

// Who names the process for a person: "tree:agent", or "PM <name>".
func (r ProcRecord) Who() string {
	if r.IsPM() {
		return "PM " + r.PM
	}
	return r.Tab()
}

// ProcsDir holds one ProcRecord per live agent process. Under the state root,
// which no sandbox is granted: a record is signalled by the unsandboxed
// watcher and CLI, so no agent may write one.
func ProcsDir() string {
	return filepath.Join(StateRoot(), "procs")
}

// procRecordFromEnv describes the agent-exec process about to run nono with
// args, from the environment its pane was launched with.
func procRecordFromEnv(args []string) ProcRecord {
	r := ProcRecord{
		PID:     os.Getpid(),
		Tree:    os.Getenv("SUPATREE_NAME"),
		Agent:   os.Getenv("SUPATREE_AGENT"),
		Member:  os.Getenv("SUPATREE_MEMBER"),
		Session: os.Getenv("ZELLIJ_SESSION_NAME"),
		Started: time.Now().UTC(),
		// Set only by RunHeadless, which also strips every ZELLIJ variable.
		Headless: os.Getenv(HeadlessEnv) == "headless",
	}
	if os.Getenv("SUPATREE_PM") == "1" {
		r.PM = os.Getenv("SUPATREE_PM_NAME")
		if r.PM == "" {
			r.PM = DefaultPMName
		}
		r.Tree = ""
	}
	for i, a := range args {
		if (a == "--session-id" || a == "--resume") && i+1 < len(args) {
			r.SessionID = args[i+1]
		}
	}
	r.Cwd, _ = os.Getwd()
	return r
}

func procPath(pid int) string {
	return filepath.Join(ProcsDir(), strconv.Itoa(pid)+".json")
}

// registerProc writes r. Best effort: an unrecorded agent is one `ps` cannot
// list, which is how every agent was before this.
func registerProc(r ProcRecord) {
	data, err := json.Marshal(r)
	if err != nil {
		return
	}
	if err := os.MkdirAll(ProcsDir(), 0755); err != nil {
		return
	}
	_ = writeFileAtomic(procPath(r.PID), data)
}

func unregisterProc(pid int) {
	_ = os.Remove(procPath(pid))
}

// ListProcs reads every record whose process is still running, and deletes
// the rest — a record outlives its process only when agent-exec was killed
// outright.
func ListProcs() []ProcRecord {
	entries, err := os.ReadDir(ProcsDir())
	if err != nil {
		return nil
	}
	var recs []ProcRecord
	var paths []string
	var pids []int
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(ProcsDir(), e.Name())
		var r ProcRecord
		if err := readJSON(path, &r); err != nil || r.PID <= 0 {
			_ = os.Remove(path)
			continue
		}
		if !pidExists(r.PID) {
			_ = os.Remove(path)
			continue
		}
		recs = append(recs, r)
		paths = append(paths, path)
		pids = append(pids, r.PID)
	}
	// One `ps` for every record, not one each: a registry of a few dozen
	// agents made every PM focus fork a few dozen processes.
	// A ps that could not run (nil) proves nothing about any of them: the
	// records are kept, unverified, rather than every live agent's deleted.
	execs := agentExecs(pids)
	var out []ProcRecord
	for i, r := range recs {
		if execs != nil && !execs[r.PID] {
			_ = os.Remove(paths[i])
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// procAlive reports whether pid is running and is still an agent-exec. A pid
// alone is not enough: once the record's process is gone the number is
// recycled, and signalling it would hit something else entirely.
func procAlive(pid int) bool {
	return pidExists(pid) && agentExecs([]int{pid})[pid]
}

func pidExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// agentExecs reports which of pids are agent-execs, from a single `ps`, or
// nil when ps could not be run at all. Swappable in tests, which cannot start
// an agent-exec.
var agentExecs = func(pids []int) map[int]bool {
	out := map[int]bool{}
	if len(pids) == 0 {
		return out
	}
	list := make([]string, len(pids))
	for i, pid := range pids {
		list[i] = strconv.Itoa(pid)
	}
	// ps exits 1 when some pid has gone in the meantime, and still lists the
	// rest, so its output is read whatever its status.
	raw, err := exec.CommandContext(context.Background(), "ps", "-p", strings.Join(list, ","), "-o", "pid=,command=").Output()
	if err != nil && !errors.As(err, new(*exec.ExitError)) {
		return nil
	}
	for _, line := range strings.Split(string(raw), "\n") {
		pidField, command, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if pid, err := strconv.Atoi(pidField); err == nil && strings.Contains(command, "agent-exec") {
			out[pid] = true
		}
	}
	return out
}

// StopGrace is how long a stopped agent gets to exit before it is killed.
const StopGrace = 5 * time.Second

// StopProc stops the agent r records: a hangup to its agent-exec, which hands
// it on and escalates by itself (AgentExec); then, if the process is still
// there after grace, SIGKILL to nono and agent-exec both.
func StopProc(r ProcRecord, grace time.Duration) error {
	if !procAlive(r.PID) {
		unregisterProc(r.PID)
		return nil
	}
	if err := syscall.Kill(r.PID, syscall.SIGHUP); err != nil {
		return fmt.Errorf("signal %s (pid %d): %w", r.Who(), r.PID, err)
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !procAlive(r.PID) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if r.NonoPID > 0 {
		killTree(r.NonoPID)
	}
	_ = syscall.Kill(r.PID, syscall.SIGKILL)
	unregisterProc(r.PID)
	return nil
}

// Orphans are the records whose process runs with no tab of its name in its
// session — nothing a human can see or close. tabs maps a session to its tab
// names; a session missing from it could not be read, and its records are
// never reported, since "could not tell" is not "has no tab".
func Orphans(procs []ProcRecord, tabs map[string]map[string]bool) []ProcRecord {
	var out []ProcRecord
	for _, r := range procs {
		names, ok := tabs[r.Session]
		if !ok {
			continue
		}
		if !names[r.Tab()] {
			out = append(out, r)
		}
	}
	return out
}

// TreeProcs are the records belonging to tree, members' agents included.
func TreeProcs(procs []ProcRecord, tree string) []ProcRecord {
	var out []ProcRecord
	for _, r := range procs {
		if !r.IsPM() && r.Tree == tree {
			out = append(out, r)
		}
	}
	return out
}

// killTree SIGKILLs pid and every process below it. Killing nono alone is not
// enough: the agent is nono's child, and survives it, reparented, with no pane
// and no supervisor. Descendants are found from one `ps` snapshot and killed
// bottom-up, so none is left to be reparented out of reach.
func killTree(pid int) {
	out, err := exec.CommandContext(context.Background(), "ps", "-A", "-o", "pid=,ppid=").Output()
	var victims []int
	if err == nil {
		victims = descendants(pid, parseProcTable(string(out)))
	}
	for i := len(victims) - 1; i >= 0; i-- {
		_ = syscall.Kill(victims[i], syscall.SIGKILL)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// parseProcTable reads `ps -o pid=,ppid=` into child lists by parent.
func parseProcTable(out string) map[int][]int {
	children := map[int][]int{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			children[ppid] = append(children[ppid], pid)
		}
	}
	return children
}

// descendants lists every process below pid, parents before children.
func descendants(pid int, children map[int][]int) []int {
	var out []int
	queue := []int{pid}
	seen := map[int]bool{pid: true}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range children[p] {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
				queue = append(queue, c)
			}
		}
	}
	return out
}

// SessionTabs lists the tabs of every session procs run in. A session whose
// panes cannot be read is left out, which Orphans takes as "cannot tell".
func SessionTabs(procs []ProcRecord) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, r := range procs {
		if _, done := out[r.Session]; done || r.Session == "" {
			continue
		}
		panes, err := zellij.ListPanes(r.Session)
		if err != nil {
			continue
		}
		tabs := map[string]bool{}
		for _, p := range panes {
			tabs[p.TabName] = true
		}
		out[r.Session] = tabs
	}
	return out
}

// StopTarget selects agent processes to stop: one tree's (or one agent of
// it), and/or every orphan.
type StopTarget struct {
	Tree    string
	Agent   string // within Tree; empty is all of them
	PID     int
	Orphans bool
}

func (t StopTarget) empty() bool { return t.Tree == "" && t.PID == 0 && !t.Orphans }

// Select picks the records t names from procs.
func (t StopTarget) Select(procs []ProcRecord, tabs map[string]map[string]bool) []ProcRecord {
	pick := map[int]ProcRecord{}
	if t.Orphans {
		for _, r := range Orphans(procs, tabs) {
			pick[r.PID] = r
		}
	}
	for _, r := range procs {
		if t.PID != 0 && r.PID == t.PID {
			pick[r.PID] = r
		}
		if t.Tree != "" && !r.IsPM() && r.Tree == t.Tree && (t.Agent == "" || r.Agent == t.Agent || r.Member == t.Agent) {
			pick[r.PID] = r
		}
	}
	out := make([]ProcRecord, 0, len(pick))
	for _, r := range pick {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// StopAgents stops every recorded agent process t selects, and reports what it
// stopped. Only processes in the registry can be stopped — never an arbitrary
// pid.
func StopAgents(t StopTarget) ([]ProcRecord, error) {
	if t.empty() {
		return nil, errors.New("nothing to stop: name a tree, a pid, or orphans")
	}
	procs := ListProcs()
	var tabs map[string]map[string]bool
	if t.Orphans {
		tabs = SessionTabs(procs)
	}
	victims := t.Select(procs, tabs)
	var errs []error
	var stopped []ProcRecord
	for _, r := range victims {
		if err := StopProc(r, StopGrace); err != nil {
			errs = append(errs, err)
			continue
		}
		stopped = append(stopped, r)
	}
	return stopped, errors.Join(errs...)
}

// FormatProcs renders records for a person: who, pid, session, age, and
// whether its tab is there.
func FormatProcs(procs []ProcRecord, tabs map[string]map[string]bool, now time.Time) string {
	if len(procs) == 0 {
		return "no agent processes are recorded"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-7s %-28s %-12s %-8s %s\n", "PID", "AGENT", "SESSION", "AGE", "TAB")
	for _, r := range procs {
		tab := "?"
		if names, ok := tabs[r.Session]; ok {
			tab = "yes"
			if !names[r.Tab()] {
				tab = "NONE (orphan)"
			}
		}
		fmt.Fprintf(&b, "%-7d %-28s %-12s %-8s %s\n", r.PID, r.Who(), r.Session,
			now.Sub(r.Started).Round(time.Minute).String(), tab)
	}
	return b.String()
}
