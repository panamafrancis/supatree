package tui

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/panamafrancis/supatree/pkg/supatree"
	"github.com/panamafrancis/workbench/pkg/testutil"
	"github.com/panamafrancis/workbench/pkg/zellij"
)

// The teatest harness runs the sidebar as a real tea.Program at a fixed
// terminal size, over real supatrees in an isolated HOME. The binaries it
// calls out to — zellij, gh, nono — are fakes on PATH, so a test is hermetic
// and can assert on what the sidebar asked zellij to do.
//
// Plan and scenarios: plans/sidebar-ux-testing.md.

// defaultSidebarWidth is the sidebar's width at the default 20% of a
// 140-column terminal, the width the first bugs were found at.
const defaultSidebarWidth = 28

// sidebarOpts configures startSidebar. The zero value is a 28x40 sidebar over
// the standard world, outside zellij.
type sidebarOpts struct {
	width, height int
	// inZellij sets ZELLIJ, which the sidebar checks before opening anything.
	inZellij bool
	// tabs are the tab names the fake zellij reports as open.
	tabs []string
}

// sidebar is a running sidebar under test.
type sidebar struct {
	t         *testing.T
	tm        *teatest.TestModel
	cmds      *trackedModel
	zellijLog string
}

// startSidebar builds the world, installs the fakes and starts the sidebar.
func startSidebar(t *testing.T, o sidebarOpts) *sidebar {
	t.Helper()
	if o.width == 0 {
		o.width = defaultSidebarWidth
	}
	if o.height == 0 {
		o.height = 40
	}
	testutil.IsolateHome(t)
	// The developer's own Claude config may be named here; keep it out.
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(os.Getenv("HOME"), ".claude"))
	zellijLog := installFakes(t, o)
	if o.inZellij {
		t.Setenv("ZELLIJ", "0")
	} else {
		t.Setenv("ZELLIJ", "")
	}
	// Run as the sidebar pane does, not as whatever the developer's shell
	// says: it decides q's confirmation and the cold-start PM.
	t.Setenv("SUPATREE_SIDEBAR", "1")
	t.Setenv("ZELLIJ_SESSION_NAME", "st-test")
	t.Setenv("SUPATREE_ACTIVE_TREE", "")
	// No timer: it would outlive the test. A scenario about the periodic
	// refresh sends tickMsg itself.
	saved := tick
	tick = func(time.Duration, func(time.Time) tea.Msg) tea.Cmd { return nil }
	t.Cleanup(func() { tick = saved })
	cfg := buildWorld(t)

	tracked := &trackedModel{inner: New(cfg, zellij.Workspace{LayoutsDir: supatree.LayoutsDir()}), running: map[int]bool{}}
	tm := teatest.NewTestModel(t, tracked, teatest.WithInitialTermSize(o.width, o.height))
	s := &sidebar{t: t, tm: tm, cmds: tracked, zellijLog: zellijLog}
	// Registered after the temp dirs, so it runs before they are removed.
	// Settle first: once the program has quit, no result can reach Update.
	t.Cleanup(func() {
		tracked.settle(t)
		_ = tm.Quit()
		tm.WaitFinished(t, teatest.WithFinalTimeout(5*time.Second))
	})
	return s
}

// installFakes puts zellij, gh and nono on PATH, returning the file the fake
// zellij logs its calls to.
func installFakes(t *testing.T, o sidebarOpts) string {
	t.Helper()
	bin := t.TempDir()
	logPath := filepath.Join(bin, "zellij.log")
	tabsPath := filepath.Join(bin, "zellij.tabs")
	if err := os.WriteFile(tabsPath, []byte(strings.Join(o.tabs, "\n")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// zellij: logs every call; answers the tab query from the tabs file.
	fakeBin(t, bin, "zellij", `echo "$*" >> '`+logPath+`'
case "$*" in *query-tab-names*) cat '`+tabsPath+`';; esac
exit 0`)
	// gh: unauthenticated, so nothing reaches GitHub. PR scenarios seed the
	// cache instead.
	fakeBin(t, bin, "gh", `echo "gh: not logged in to any hosts" >&2
exit 4`)
	// nono: accepts every profile. (A refusing variant needs the supatree
	// package's preflight cache reset between tests; it comes with the
	// failed-launch scenarios.)
	fakeBin(t, bin, "nono", `echo "  Result: valid"`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// fakeBin writes an executable shell script named name into dir.
func fakeBin(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
}

// buildWorld creates the fixture repos, a stack over them, and the trees the
// sidebar runs over — the world scripts/ux-env.sh seeds, so the two layers
// describe the same thing: berlin is fresh; paris has a commit on api, an
// uncommitted file in web, and a named agent, reviewer, beside main.
func buildWorld(t *testing.T) *supatree.Config {
	t.Helper()
	for _, k := range []string{"GIT_AUTHOR", "GIT_COMMITTER"} {
		t.Setenv(k+"_NAME", "ux")
		t.Setenv(k+"_EMAIL", "ux@test.local")
	}
	src := t.TempDir()
	members := map[string]string{}
	for _, r := range []string{"api", "web"} {
		dir := filepath.Join(src, r)
		runGit(t, "", "init", "-q", "-b", "main", dir)
		writeTestFile(t, filepath.Join(dir, "README.md"), "# "+r+"\n")
		runGit(t, dir, "add", "README.md")
		runGit(t, dir, "commit", "-qm", "initial")
		members[r] = dir
	}
	if _, err := supatree.Scaffold(&supatree.Config{}, "demo", "", members); err != nil {
		t.Fatal(err)
	}
	cfg, err := supatree.Load()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	newTree := func(name string) *supatree.Instance {
		inst, _, err := supatree.New(cfg, supatree.CreateOptions{Stack: "demo", Name: name, Now: now})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return inst
	}
	newTree(berlin)
	p := newTree(paris)

	api := p.FindMember("api").Path
	writeTestFile(t, filepath.Join(api, "README.md"), "# api\nchanged\n")
	runGit(t, api, "commit", "-qam", "api: a change")
	writeTestFile(t, filepath.Join(p.FindMember("web").Path, "draft.txt"), "draft\n")
	if _, _, err := supatree.EnsureAgent(p.Root, paris, "reviewer", "claude", now); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// runGit runs git in dir (the test's working directory when dir is "").
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// press sends keys: a named key ("enter", "esc", "down", "up") or the runes
// of anything else, one key at a time as a terminal would. Each is tracked
// until handled, so settling after press waits for what the keys set off.
func (s *sidebar) press(keys ...string) {
	send := func(k tea.KeyMsg) { s.tm.Send(trackedMsg{id: s.cmds.register(), msg: k}) }
	for _, k := range keys {
		switch k {
		case "enter":
			send(tea.KeyMsg{Type: tea.KeyEnter})
		case "esc":
			send(tea.KeyMsg{Type: tea.KeyEsc})
		case "down":
			send(tea.KeyMsg{Type: tea.KeyDown})
		case "up":
			send(tea.KeyMsg{Type: tea.KeyUp})
		default:
			for _, r := range k {
				send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
			}
		}
	}
}

// waitFor blocks until the screen has shown want, failing the test after a
// few seconds. It reads the program's output stream, so each call only sees
// output after the previous one's match.
func (s *sidebar) waitFor(want string) {
	s.t.Helper()
	teatest.WaitFor(s.t, s.tm.Output(), func(b []byte) bool {
		return bytes.Contains(b, []byte(want))
	}, teatest.WithDuration(5*time.Second), teatest.WithCheckInterval(20*time.Millisecond))
}

// waitForZellij blocks until the fake zellij has been called with args
// containing want.
func (s *sidebar) waitForZellij(want string) {
	s.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, call := range s.zellijCalls() {
			if strings.Contains(call, want) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.t.Fatalf("zellij was never called with %q; calls:\n%s", want, strings.Join(s.zellijCalls(), "\n"))
}

// zellijCalls is every call the fake zellij has had, one per line.
func (s *sidebar) zellijCalls() []string {
	data, _ := os.ReadFile(s.zellijLog)
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// finish quits the sidebar and returns its final model, whose View is the
// last screen without the program's escape sequences.
func (s *sidebar) finish() *Model {
	s.t.Helper()
	// Let every result already on its way land first, or the screen depends
	// on how quickly the fakes answered.
	s.cmds.settle(s.t)
	if err := s.tm.Quit(); err != nil {
		s.t.Fatal(err)
	}
	final, ok := s.tm.FinalModel(s.t, teatest.WithFinalTimeout(5*time.Second)).(*trackedModel)
	if !ok {
		s.t.Fatal("final model is not the tracked sidebar")
	}
	return final.inner
}

// trackedModel wraps the sidebar to know which of its commands are still
// running. A test must not end with one in flight: it would go on calling
// zellij, gh or git after the test's fakes and temp dirs are gone — writing
// into a directory being deleted, or failing against the real zellij often
// enough to open the zellij package's process-wide circuit breaker, which then
// skips every zellij call for a minute, in whatever tests run next.
type trackedModel struct {
	inner *Model
	mu    sync.Mutex
	next  int
	// running maps each tracked id to whether it is still executing (true)
	// or has returned a result Update has yet to handle (false).
	running map[int]bool
	// quit is set once a quit has gone by: results still to be handled after
	// it never will be, so only what is executing is worth waiting for.
	quit bool
}

func (w *trackedModel) Init() tea.Cmd { return w.track(w.inner.Init()) }

func (w *trackedModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// A command counts as running until its result has been handled, not
	// just returned: a test waiting for the sidebar to settle wants the
	// screen that result produces. Any command handling it returns is
	// registered (by track) before this one is let go, so there is no moment
	// in between when nothing seems to be running.
	if env, ok := msg.(trackedMsg); ok {
		defer w.done(env.id)
		msg = env.msg
	}
	_, cmd := w.inner.Update(msg)
	return w, w.track(cmd)
}

func (w *trackedModel) View() string { return w.inner.View() }

// trackedMsg carries a tracked command's result, or a test's key, to Update.
type trackedMsg struct {
	id  int
	msg tea.Msg
}

// track registers cmd as running from now until Update has handled its
// result. Results the program loop handles itself never reach Update, so they
// pass through unwrapped and the command is done when it returns: quitting,
// and a batch, whose commands are each tracked in turn.
func (w *trackedModel) track(cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	id := w.register()
	return func() tea.Msg {
		msg := cmd()
		switch msg := msg.(type) {
		case tea.BatchMsg:
			for i := range msg {
				msg[i] = w.track(msg[i])
			}
			w.done(id)
			return msg
		case tea.QuitMsg:
			w.mu.Lock()
			w.quit = true
			w.mu.Unlock()
			w.done(id)
			return msg
		case nil:
			w.done(id)
			return msg
		default:
			w.mu.Lock()
			w.running[id] = false
			w.mu.Unlock()
			return trackedMsg{id: id, msg: msg}
		}
	}
}

// register records something as running — a command, or a key on its way —
// and returns its id for done.
func (w *trackedModel) register() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	id := w.next
	w.next++
	w.running[id] = true
	return id
}

func (w *trackedModel) done(id int) {
	w.mu.Lock()
	delete(w.running, id)
	w.mu.Unlock()
}

// settle waits until every command has run and its result been handled — or,
// after a quit, until nothing is executing.
func (w *trackedModel) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		w.mu.Lock()
		busy := 0
		for _, executing := range w.running {
			if executing || !w.quit {
				busy++
			}
		}
		w.mu.Unlock()
		if busy == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("%d sidebar commands still running at the end of the test", busy)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
