package supatree

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/panamafrancis/workbench/pkg/config"
	"github.com/panamafrancis/workbench/pkg/testutil"
)

func headlessFixture(t *testing.T, exec []string) (*Instance, *Config) {
	t.Helper()
	testutil.IsolateHome(t)
	fakeNono(t, nil, nil)
	cfg := &Config{Models: map[string]config.Model{defaultModelKey: {
		NonoProfile:       "supatree-agent",
		Binary:            defaultModelKey,
		NewSessionArgs:    []string{"--session-id", "{session_id}"},
		ResumeSessionArgs: []string{"--resume", "{session_id}"},
		PromptArgs:        []string{"{prompt}"},
		ExecArgs:          exec,
	}}}
	return &Instance{Name: treeLima, Root: t.TempDir(), Model: defaultModelKey}, cfg
}

// The exec args must come after the session args and before the prompt, so a
// headless run starts the agent's own session and is handed the fixed
// kickoff, never anything taken from the brief.
func TestHeadlessArgv(t *testing.T) {
	inst, cfg := headlessFixture(t, []string{"-p", "--output-format", "stream-json"})
	argv, resumed, err := headlessArgv(inst, cfg, "reviewer", "")
	if err != nil {
		t.Fatal(err)
	}
	if resumed {
		t.Error("a fresh agent has no session to resume")
	}
	sep := slices.Index(argv, "--")
	if sep < 0 || argv[sep+1] != defaultModelKey {
		t.Fatalf("argv = %q, want nono run … -- %s …", argv, defaultModelKey)
	}
	tail := argv[sep+1:]
	if !slices.Contains(tail, "--session-id") {
		t.Errorf("argv %q starts no session", tail)
	}
	want := []string{"-p", "--output-format", "stream-json", KickoffPrompt}
	if got := tail[len(tail)-len(want):]; !slices.Equal(got, want) {
		t.Errorf("argv ends %q, want %q", got, want)
	}
	if agents, _ := LoadAgents(inst.Root); FindAgent(agents, "reviewer") == nil {
		t.Error("the run's agent must be registered, so it resumes by a known session")
	}
}

func TestHeadlessRefusesInteractiveOnlyModel(t *testing.T) {
	inst, cfg := headlessFixture(t, nil)
	if _, _, err := headlessArgv(inst, cfg, MainAgent, ""); !errors.Is(err, ErrNoExecArgs) {
		t.Fatalf("err = %v, want ErrNoExecArgs", err)
	}
}

func TestRunHeadlessNeedsABrief(t *testing.T) {
	inst, cfg := headlessFixture(t, []string{"-p"})
	_, err := RunHeadless(context.Background(), inst, cfg, HeadlessOptions{Agent: "reviewer"})
	if err == nil || !strings.Contains(err.Error(), "no brief") {
		t.Fatalf("err = %v, want a refusal for want of mail", err)
	}
}

func TestHeadlessEnv(t *testing.T) {
	t.Setenv("ZELLIJ_SESSION_NAME", "st-main")
	t.Setenv("ZELLIJ", "0")
	t.Setenv("SUPATREE_AGENT", "outer")
	env := headlessEnv(map[string]string{"SUPATREE_AGENT": "reviewer"})
	var agent []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "ZELLIJ") {
			t.Errorf("zellij variable %q reached a headless run", kv)
		}
		if strings.HasPrefix(kv, "SUPATREE_AGENT=") {
			agent = append(agent, kv)
		}
	}
	if !slices.Equal(agent, []string{"SUPATREE_AGENT=reviewer"}) {
		t.Errorf("SUPATREE_AGENT = %q, want only the run's own", agent)
	}
	if !slices.Contains(env, HeadlessEnv+"=headless") {
		t.Error("the run is not marked headless for the process registry")
	}
}
