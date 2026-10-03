package supatree

import (
	"bufio"
	"context"
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
)

// An agent tab runs `nono run --profile … -- claude …` in a pane that closes
// when it exits. When nono cannot load the profile — a nono upgrade that
// renamed a setting a profile in the chain uses, say — it exits at once, the
// tab vanishes, and nothing says why. Two defences:
//
//   - NonoPreflight checks the profile before any agent tab or the PM is
//     opened, and the open fails with nono's own complaint.
//   - The pane runs nono through `supatree agent-exec` (via a `nono` shim
//     first on the pane's PATH — the shared layout names nono as the
//     command). A quick non-zero exit keeps the pane open on nono's output
//     until enter is pressed.

// nonoUpgradeHint is what to check when nono rejects a profile it used to
// accept.
const nonoUpgradeHint = "if nono was upgraded, a profile in the chain may use a setting it no longer accepts — " +
	"check: nono outdated, nono list --installed, nono profile list"

// NonoProfileError is a profile nono refuses to load.
type NonoProfileError struct {
	Profile string
	Output  string // what nono said
}

func (e *NonoProfileError) Error() string {
	msg := fmt.Sprintf("nono cannot load profile %q, so the agent would exit as soon as it started", e.Profile)
	if e.Output != "" {
		msg += ":\n  " + strings.ReplaceAll(e.Output, "\n", "\n  ")
	}
	return msg + "\n" + nonoUpgradeHint
}

// CheckNonoProfile asks nono whether it can load profile, extends chain and
// all. `profile validate` checks the file; `profile show` resolves the chain,
// and is the only one of the two that knows the built-in profiles, which have
// no file to validate.
func CheckNonoProfile(profile string) error {
	if profile == "" {
		return nil
	}
	if _, err := exec.LookPath("nono"); err != nil {
		return errors.New("nono is not installed, or not on PATH — agents run under it: https://nono.sh")
	}
	vOut, vErr := runNono("profile", "validate", profile)
	sOut, sErr := runNono("profile", "show", profile)
	builtin := vErr != nil && strings.Contains(vOut, "profile file not found") && sErr == nil
	switch {
	case vErr != nil && !builtin:
		return &NonoProfileError{Profile: profile, Output: nonoComplaint(vOut)}
	case sErr != nil:
		return &NonoProfileError{Profile: profile, Output: nonoComplaint(sOut)}
	}
	return nil
}

func runNono(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nono", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// nonoComplaint keeps the lines of nono's output that say what is wrong, so
// the error still fits a sidebar.
func nonoComplaint(out string) string {
	var keep []string
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		low := strings.ToLower(l)
		if strings.Contains(low, "err") || strings.Contains(low, "invalid") || strings.Contains(low, "unknown") {
			keep = append(keep, l)
		}
	}
	if len(keep) == 0 {
		lines := strings.Split(out, "\n")
		if len(lines) > 6 {
			lines = lines[len(lines)-6:]
		}
		keep = lines
	}
	if len(keep) > 8 {
		keep = keep[:8]
	}
	return strings.Join(keep, "\n")
}

var (
	preflightMu sync.Mutex
	preflightOK = map[string]bool{}
)

// NonoPreflight is CheckNonoProfile, remembered per process once it passes.
// A failure is not remembered: the sidebar and the watcher live for days, and
// a profile fixed in the meantime must not stay refused until they restart.
func NonoPreflight(profile string) error {
	preflightMu.Lock()
	ok := preflightOK[profile]
	preflightMu.Unlock()
	if ok {
		return nil
	}
	if err := CheckNonoProfile(profile); err != nil {
		return err
	}
	preflightMu.Lock()
	preflightOK[profile] = true
	preflightMu.Unlock()
	return nil
}

// ShimDir holds the `nono` shim agent panes run through. It is in the cache,
// beside the layouts: like them it is run outside any sandbox, so no agent is
// granted it.
func ShimDir() string {
	return filepath.Join(CacheDir(), "shim")
}

// withAgentExec routes an agent pane's nono through `supatree agent-exec`, by
// putting a `nono` shim first on the pane's PATH. Best effort: if the shim
// cannot be written, the pane runs nono directly, as it always has.
func withAgentExec(env map[string]string) map[string]string {
	exe, err := os.Executable()
	if err != nil {
		return env
	}
	nono, err := exec.LookPath("nono")
	if err != nil {
		return env
	}
	dir := ShimDir()
	if err := writeShim(dir, exe, nono); err != nil {
		return env
	}
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		out[k] = v
	}
	out["PATH"] = dir + string(os.PathListSeparator) + os.Getenv("PATH")
	return out
}

func writeShim(dir, exe, nono string) error {
	body := "#!/bin/sh\n" +
		"# Written by supatree. An agent pane's nono runs through supatree agent-exec,\n" +
		"# which keeps the pane open if nono fails at once instead of letting it vanish.\n" +
		"exec " + shQuote(exe) + " agent-exec --nono " + shQuote(nono) + " --shim-dir " + shQuote(dir) + ` -- "$@"` + "\n"
	path := filepath.Join(dir, "nono")
	if have, err := os.ReadFile(path); err == nil && string(have) == body {
		return nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".nono-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// AgentExecOptions parameterizes AgentExec.
type AgentExecOptions struct {
	Nono    string   // the real nono
	ShimDir string   // dropped from PATH, so nothing below sees the shim
	Args    []string // nono's arguments
	// Quick is how soon a failing exit counts as a launch failure, which keeps
	// the pane open. Zero means 5s.
	Quick  time.Duration
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// AgentExec runs nono for an agent pane and returns the exit code the pane
// should end with. If nono fails within Quick — it never got as far as the
// agent — it says so and waits for enter, so whatever nono printed stays on
// screen. Every run is logged to LogsDir()/agent-exec.log.
func AgentExec(opts AgentExecOptions) int {
	if opts.Quick == 0 {
		opts.Quick = 5 * time.Second
	}
	cmd := exec.CommandContext(context.Background(), opts.Nono, opts.Args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = opts.Stdin, opts.Stdout, opts.Stderr
	cmd.Env = withoutPathEntry(os.Environ(), opts.ShimDir)

	// The terminal delivers ^C to nono itself; hangups and terminations sent
	// to this process are passed on.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)

	start := time.Now()
	code := 0
	if err := cmd.Start(); err != nil {
		code = 127
		_, _ = fmt.Fprintf(opts.Stderr, "supatree: could not start %s: %v\n", opts.Nono, err)
	} else {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		var err error
	wait:
		for {
			select {
			case s := <-sigs:
				if s != syscall.SIGINT {
					_ = cmd.Process.Signal(s)
				}
			case err = <-done:
				break wait
			}
		}
		var exit *exec.ExitError
		switch {
		case errors.As(err, &exit):
			code = exit.ExitCode()
			if code < 0 {
				code = 128 // killed by a signal
			}
		case err != nil:
			code = 1
		}
	}
	elapsed := time.Since(start)
	logAgentExec(opts.Args, code, elapsed)
	if code == 0 || elapsed >= opts.Quick {
		return code
	}
	_, _ = fmt.Fprintf(opts.Stderr, "\nsupatree: the agent exited with status %d after %s, before it started — "+
		"nono's output is above.\n%s\nPress enter to close this pane. ", code, elapsed.Round(100*time.Millisecond), nonoUpgradeHint)
	_, _ = bufio.NewReader(opts.Stdin).ReadString('\n')
	return code
}

func withoutPathEntry(env []string, dir string) []string {
	if dir == "" {
		return env
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			var keep []string
			for _, p := range filepath.SplitList(v) {
				if filepath.Clean(p) != filepath.Clean(dir) {
					keep = append(keep, p)
				}
			}
			kv = "PATH=" + strings.Join(keep, string(os.PathListSeparator))
		}
		out = append(out, kv)
	}
	return out
}

func logAgentExec(args []string, code int, elapsed time.Duration) {
	if err := os.MkdirAll(LogsDir(), 0755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(LogsDir(), "agent-exec.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	cwd, _ := os.Getwd()
	profile := ""
	for i, a := range args {
		if a == "--profile" && i+1 < len(args) {
			profile = args[i+1]
		}
	}
	_, _ = fmt.Fprintf(f, "%s cwd=%s profile=%s exit=%d after=%s\n",
		time.Now().Format(time.RFC3339), cwd, profile, code, elapsed.Round(time.Millisecond))
}
