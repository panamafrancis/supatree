package supatree

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/panamafrancis/workbench/pkg/config"
	"github.com/panamafrancis/workbench/pkg/setup"
	"github.com/panamafrancis/workbench/pkg/testutil"
	"github.com/panamafrancis/workbench/pkg/zellij"
)

// fakeNono puts a nono on PATH that accepts every profile but those in broken,
// for which it fails the way nono 0.79 failed on a profile using `undo`. A
// name in builtin has no file to validate, as nono's built-ins do not.
func fakeNono(t *testing.T, broken, builtin []string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"[ \"$1\" = --version ] && { echo 'nono 0.79.0'; exit 0; }\n" +
		"case \" " + strings.Join(broken, " ") + " \" in *\" $3 \"*)\n" +
		"  echo \"nono profile: validating $3\"\n" +
		"  echo '  [err]  Profile parse error: unknown field `undo`, expected one of `rollback`'\n" +
		"  echo 'nono: Profile parse error: validation failed' >&2\n" +
		"  exit 1;;\n" +
		"esac\n" +
		"case \" " + strings.Join(builtin, " ") + " \" in *\" $3 \"*)\n" +
		"  [ \"$2\" = validate ] && { echo '  [err]  File read error: profile file not found'; exit 1; };;\n" +
		"esac\n" +
		"echo '  Result: valid'\n"
	if err := os.WriteFile(filepath.Join(dir, "nono"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	resetPreflight(t)
}

func resetPreflight(t *testing.T) {
	t.Helper()
	preflightMu.Lock()
	preflightOK = map[string]bool{}
	preflightMu.Unlock()
	t.Cleanup(func() {
		preflightMu.Lock()
		preflightOK = map[string]bool{}
		preflightMu.Unlock()
	})
}

// brokenProfile is a profile the fake nono refuses.
const brokenProfile = "pre-rollback"

func TestNonoPreflight(t *testing.T) {
	testutil.IsolateHome(t)
	fakeNono(t, []string{brokenProfile}, []string{"default"})

	err := NonoPreflight(brokenProfile)
	var pe *NonoProfileError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a NonoProfileError", err)
	}
	for _, want := range []string{`"` + brokenProfile + `"`, "unknown field `undo`", "nono outdated", "nono list --installed", "nono profile list"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
	if err := NonoPreflight("supatree-agent"); err != nil {
		t.Errorf("a valid profile failed: %v", err)
	}
	if err := NonoPreflight("default"); err != nil {
		t.Errorf("a built-in profile, which has no file to validate, failed: %v", err)
	}

	// A pass is remembered; a failure is not, so a fixed profile is not
	// refused until the sidebar restarts.
	fakeNono(t, nil, nil)
	if err := NonoPreflight(brokenProfile); err != nil {
		t.Errorf("a profile fixed since the last check is still refused: %v", err)
	}
	fakeNono(t, []string{brokenProfile}, nil)
	preflightMu.Lock()
	preflightOK[brokenProfile] = true
	preflightMu.Unlock()
	if err := NonoPreflight(brokenProfile); err != nil {
		t.Errorf("a passed check was not cached: %v", err)
	}
}

func TestNonoPreflightWithoutNono(t *testing.T) {
	testutil.IsolateHome(t)
	resetPreflight(t)
	t.Setenv("PATH", t.TempDir())
	if err := NonoPreflight("supatree-agent"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("err = %v, want nono reported missing", err)
	}
}

// The PM's open fails, with nono's complaint, before any tab is opened.
func TestOpenPMPreflight(t *testing.T) {
	testutil.IsolateHome(t)
	fakeNono(t, []string{brokenProfile}, nil)
	cfg := &Config{Models: map[string]config.Model{defaultModelKey: {NonoProfile: brokenProfile, Binary: defaultModelKey}}}
	ws := zellij.Workspace{LayoutsDir: LayoutsDir()}
	_, err := OpenPM(cfg, ws, "20%", "")
	if err == nil || !strings.Contains(err.Error(), "undo") {
		t.Fatalf("err = %v, want nono's complaint", err)
	}
	if entries, _ := os.ReadDir(LayoutsDir()); len(entries) > 0 {
		t.Error("a layout was written for an agent that cannot start")
	}
}

func TestDoctorNamesBrokenParent(t *testing.T) {
	testutil.IsolateHome(t)
	fakeNono(t, []string{"team-base"}, []string{"default"})
	writeFile(t, setup.NonoProfilePath("supatree-agent"), `{"extends": ["team-base"]}`)
	writeFile(t, setup.NonoProfilePath("team-base"), `{"extends": "default"}`)

	var failed []string
	for _, r := range Doctor(&Config{}) {
		if r.Status == setup.StatusFail && strings.HasPrefix(r.Name, "nono profile") {
			failed = append(failed, r.Name)
		}
	}
	if len(failed) != 1 || !strings.Contains(failed[0], "team-base, extended by supatree-agent") {
		t.Errorf("failed checks = %q, want just the broken parent", failed)
	}
}

func TestAgentExec(t *testing.T) {
	testutil.IsolateHome(t)
	run := func(quick time.Duration, script string) (int, string) {
		var out bytes.Buffer
		code := AgentExec(AgentExecOptions{
			Nono: "/bin/sh", Args: []string{"-c", script}, Quick: quick,
			Stdin: strings.NewReader("\n"), Stdout: &out, Stderr: &out,
		})
		return code, out.String()
	}
	code, out := run(0, "echo 'profile parse error' >&2; exit 2")
	if code != 2 || !strings.Contains(out, "profile parse error") || !strings.Contains(out, "Press enter") {
		t.Errorf("quick failure: code %d, output %q — want the pane held open on nono's output", code, out)
	}
	if code, out := run(0, "exit 0"); code != 0 || strings.Contains(out, "Press enter") {
		t.Errorf("clean exit: code %d, output %q", code, out)
	}
	if code, out := run(time.Nanosecond, "sleep 0.01; exit 1"); code != 1 || strings.Contains(out, "Press enter") {
		t.Errorf("an agent that ran and then failed held the pane: code %d, output %q", code, out)
	}
	if data, err := os.ReadFile(filepath.Join(LogsDir(), "agent-exec.log")); err != nil || strings.Count(string(data), "\n") != 3 {
		t.Errorf("agent-exec.log = %q, %v", data, err)
	}
}

func TestAgentExecShim(t *testing.T) {
	testutil.IsolateHome(t)
	fakeNono(t, nil, nil)
	env := withAgentExec(map[string]string{"SUPATREE": "lima"})
	if env["SUPATREE"] != "lima" || !strings.HasPrefix(env["PATH"], ShimDir()+string(os.PathListSeparator)) {
		t.Fatalf("env = %v, want the shim first on PATH", env)
	}
	data, err := os.ReadFile(filepath.Join(ShimDir(), "nono"))
	if err != nil || !strings.Contains(string(data), "agent-exec --nono") {
		t.Fatalf("shim = %q, %v", data, err)
	}
	got := withoutPathEntry([]string{"A=1", "PATH=" + ShimDir() + ":/usr/bin"}, ShimDir())
	if got[1] != "PATH=/usr/bin" {
		t.Errorf("PATH below the shim = %q, want the shim dropped", got[1])
	}
}
