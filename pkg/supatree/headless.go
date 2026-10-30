package supatree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/panamafrancis/workbench/pkg/sandbox"
)

// A headless run is an agent with no terminal: the same sandbox, grants,
// session and mailbox as an agent tab, but the model's ExecArgs make it work
// through its brief and exit. It is what lets supatree do agent work where no
// zellij session exists — a container, a CI job, the watcher running
// --headless — and what the reviewer (pkg/supatree/reviewer) runs reviews in.
//
// The brief still travels through the mailbox and the first message is still
// the fixed KickoffPrompt: nothing a PR author wrote reaches a command line.

// HeadlessEnv marks an agent-exec process as a headless run, so the process
// registry can tell it from an agent in a tab.
const HeadlessEnv = "SUPATREE_RUN"

// HeadlessOptions parameterizes RunHeadless.
type HeadlessOptions struct {
	Agent string // the agent's name in the tree; empty means MainAgent
	Model string // models key; empty means the tree's model
	// Timeout bounds the run. When it passes, the agent is sent SIGTERM and
	// agent-exec escalates to killing nono's whole process tree. Zero means
	// DefaultHeadlessTimeout.
	Timeout time.Duration
}

// DefaultHeadlessTimeout is how long a headless run may take when the caller
// names no limit. A review of a large cross-repo change, builds and tests
// included, fits comfortably; an agent stuck in a loop does not run all night.
const DefaultHeadlessTimeout = 45 * time.Minute

// HeadlessResult is what a run came to.
type HeadlessResult struct {
	ExitCode int
	TimedOut bool
	Resumed  bool   // the agent's session existed and was continued
	Log      string // the run's combined output
	Duration time.Duration
}

// ErrNoExecArgs is a model that cannot run without a terminal.
var ErrNoExecArgs = errors.New("model has no exec_args, so it cannot run headless")

// RunHeadless runs a tree agent to completion with no terminal. The agent must
// have mail waiting — without a brief there is nothing for it to do, and the
// run would end at once having done nothing. It blocks until the agent exits
// or ctx is done or the timeout passes.
func RunHeadless(ctx context.Context, inst *Instance, c *Config, opts HeadlessOptions) (HeadlessResult, error) {
	name := opts.Agent
	if name == "" {
		name = MainAgent
	}
	if err := ValidateAgentName(name); err != nil {
		return HeadlessResult{}, err
	}
	if !HasMail(inst.Root, name) {
		return HeadlessResult{}, fmt.Errorf("agent %s in %s has no brief waiting", name, inst.Name)
	}
	argv, resumed, err := headlessArgv(inst, c, name, opts.Model)
	if err != nil {
		return HeadlessResult{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return HeadlessResult{}, fmt.Errorf("locate supatree: %w", err)
	}
	nono, err := exec.LookPath("nono")
	if err != nil {
		return HeadlessResult{}, errors.New("nono is not installed, or not on PATH — agents run under it: https://nono.sh")
	}
	logPath, logFile, err := openHeadlessLog(inst.Root, name)
	if err != nil {
		return HeadlessResult{}, err
	}
	defer func() { _ = logFile.Close() }()

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultHeadlessTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Through agent-exec, as a tab's agent is: it registers the process (so
	// `supatree ps`, stop_agent and the launch cap see it) and escalates a
	// SIGTERM to the whole process tree if nono does not go quietly.
	args := append([]string{"agent-exec", "--nono", nono, "--"}, argv...)
	cmd := exec.CommandContext(runCtx, exe, args...)
	cmd.Dir = inst.Root
	cmd.Env = headlessEnv(inst.AgentEnv(name))
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	// agent-exec waits escalateAfter twice before it kills; give it that and
	// a margin before Go kills agent-exec itself.
	cmd.WaitDelay = 4*escalateAfter + 5*time.Second

	start := time.Now()
	runErr := cmd.Run()
	res := HeadlessResult{
		Resumed:  resumed,
		Log:      logPath,
		Duration: time.Since(start),
		TimedOut: errors.Is(runCtx.Err(), context.DeadlineExceeded),
	}
	var exit *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exit):
		res.ExitCode = exit.ExitCode()
		if res.ExitCode < 0 {
			res.ExitCode = 128
		}
	default:
		return res, fmt.Errorf("run agent: %w", runErr)
	}
	logLaunch("headless "+TabName(inst.Name, name), TabName(inst.Name, name),
		fmt.Sprintf("exit %d after %s (timed out: %v)", res.ExitCode, res.Duration.Round(time.Second), res.TimedOut))
	return res, nil
}

// headlessArgv is the nono argv for a headless run of agent name, and whether
// it resumes the agent's session. It is the tab launch's argv
// (openRootAgentLocked) with the model's ExecArgs added before the prompt.
func headlessArgv(inst *Instance, c *Config, name, modelKey string) ([]string, bool, error) {
	if err := EnsureLayout(); err != nil {
		return nil, false, err
	}
	_ = PublishPMRoster()
	key := inst.Model
	if modelKey != "" {
		key = modelKey
	}
	agent, _, err := EnsureAgent(inst.Root, inst.Name, name, key, time.Now())
	if err != nil {
		return nil, false, err
	}
	if err := checkAgentModel(agent, modelKey); err != nil {
		return nil, false, err
	}
	model, err := c.Model(agent.Model)
	if err != nil {
		return nil, false, err
	}
	if len(model.ExecArgs) == 0 {
		return nil, false, fmt.Errorf("%s: %w", agent.Model, ErrNoExecArgs)
	}
	// A CLI that picks its own session ID could only be resumed by finding
	// its transcript after the fact, by start time — which several headless
	// runs starting together would make ambiguous.
	if sandbox.PicksOwnSessionID(model) {
		return nil, false, fmt.Errorf("%s picks its own session IDs; headless runs need a model that takes one", agent.Model)
	}
	// Seeded as for a tab (see sandbox.TrustDir), so an interactive open of
	// this agent later finds the folder trusted. Best effort: print mode does
	// not ask, and a host where claude has never run has no config to seed.
	_ = sandbox.TrustDir(inst.Root)
	if err := NonoPreflight(model.NonoProfile); err != nil {
		return nil, false, err
	}
	resume := sandbox.SessionExistsFor(model, inst.Root, agent.SessionID)
	argv := sandbox.BuildGrantedAgentNonoArgs(model, TreeGrants(c, inst), inst.Root, agent.SessionID, agent.Address, resume)
	argv = append(argv, model.ExecArgs...)
	argv = sandbox.AppendPrompt(argv, model, KickoffPrompt)
	return argv, resume, nil
}

// headlessEnv is the process environment for a headless run: the caller's,
// with the agent's variables laid over it and every zellij variable removed —
// an agent that believed it had a session would try to reach it.
func headlessEnv(agentEnv map[string]string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "ZELLIJ") {
			continue
		}
		if _, override := agentEnv[k]; override || k == HeadlessEnv {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range agentEnv {
		env = append(env, k+"="+v)
	}
	return append(env, HeadlessEnv+"=headless")
}

// HeadlessLogDir holds the output of a tree's headless runs, one file each.
// In the tree's state, so it goes when the tree does.
func HeadlessLogDir(root string) string {
	return filepath.Join(StateDir(root), "logs")
}

func openHeadlessLog(root, agent string) (string, *os.File, error) {
	dir := HeadlessLogDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, agent+"-"+time.Now().UTC().Format("20060102T150405Z")+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", nil, err
	}
	return path, f, nil
}

// HeadlessRunning counts the headless runs the process registry shows alive.
func HeadlessRunning() int {
	n := 0
	for _, r := range ListProcs() {
		if r.Headless {
			n++
		}
	}
	return n
}
