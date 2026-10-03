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
	// brokenProfiles are nono profiles the fake nono refuses.
	brokenProfiles []string
}

// sidebar is a running sidebar under test.
type sidebar struct {
	t         *testing.T
	tm        *teatest.TestModel
	cmds      *trackedModel
	zellijLog string
	world     world
}

// world is the fixture the sidebar runs over — the same trees
// scripts/ux-env.sh seeds, so the two layers describe one world.
type world struct {
	cfg *supatree.Config
	// berlin is fresh; paris has a commit on api, an uncommitted file in web,
	// and a named agent, reviewer, beside main.
	berlin, paris *supatree.Instance
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
	w := buildWorld(t)

	tracked := &trackedModel{inner: New(w.cfg, zellij.Workspace{LayoutsDir: supatree.LayoutsDir()}), running: map[int]time.Time{}}
	tm := teatest.NewTestModel(t, tracked, teatest.WithInitialTermSize(o.width, o.height))
	s := &sidebar{t: t, tm: tm, cmds: tracked, zellijLog: zellijLog, world: w}
	// Registered after the temp dirs, so it runs before they are removed.
	t.Cleanup(func() {
		_ = tm.Quit()
		tm.WaitFinished(t, teatest.WithFinalTimeout(5*time.Second))
		tracked.settle(t)
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
	// nono: accepts every profile but the broken ones, failing those the way a
	// profile nono cannot parse does.
	fakeBin(t, bin, "nono", `case " `+strings.Join(o.brokenProfiles, " ")+` " in *" $3 "*)
  echo "  [err]  Profile parse error: unknown field"; exit 1;;
esac
echo "  Result: valid"`)
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

// buildWorld creates the fixture repos, a stack over them, and the trees.
func buildWorld(t *testing.T) world {
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
	w := world{cfg: cfg, berlin: newTree(berlin), paris: newTree("paris")}

	api := w.paris.FindMember("api").Path
	writeTestFile(t, filepath.Join(api, "README.md"), "# api\nchanged\n")
	runGit(t, api, "commit", "-qam", "api: a change")
	writeTestFile(t, filepath.Join(w.paris.FindMember("web").Path, "draft.txt"), "draft\n")
	if _, _, err := supatree.EnsureAgent(w.paris.Root, "paris", "reviewer", "claude", now); err != nil {
		t.Fatal(err)
	}
	return w
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
// of anything else, one key at a time as a terminal would.
func (s *sidebar) press(keys ...string) {
	for _, k := range keys {
		switch k {
		case "enter":
			s.tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
		case "esc":
			s.tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
		case "down":
			s.tm.Send(tea.KeyMsg{Type: tea.KeyDown})
		case "up":
			s.tm.Send(tea.KeyMsg{Type: tea.KeyUp})
		default:
			for _, r := range k {
				s.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
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
	inner   *Model
	mu      sync.Mutex
	next    int
	running map[int]time.Time
}

func (w *trackedModel) Init() tea.Cmd { return w.track(w.inner.Init()) }

func (w *trackedModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// A command counts as running until its result has been handled, not
	// just returned: a test waiting for the sidebar to settle wants the
	// screen that result produces.
	if env, ok := msg.(trackedMsg); ok {
		defer w.done(env.id)
		msg = env.msg
	}
	_, cmd := w.inner.Update(msg)
	return w, w.track(cmd)
}

func (w *trackedModel) View() string { return w.inner.View() }

// trackedMsg carries a tracked command's result to Update.
type trackedMsg struct {
	id  int
	msg tea.Msg
}

// track wraps cmd to record it from the moment it starts until Update has
// handled its result. A batch is not handled by Update but run by the program,
// so the batch itself is done once it returns, and each of its commands is
// tracked in turn.
func (w *trackedModel) track(cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		w.mu.Lock()
		id := w.next
		w.next++
		w.running[id] = time.Now()
		w.mu.Unlock()
		msg := cmd()
		switch msg := msg.(type) {
		case tea.BatchMsg:
			w.done(id)
			for i := range msg {
				msg[i] = w.track(msg[i])
			}
			return msg
		case nil:
			w.done(id)
			return nil
		default:
			return trackedMsg{id: id, msg: msg}
		}
	}
}

func (w *trackedModel) done(id int) {
	w.mu.Lock()
	delete(w.running, id)
	w.mu.Unlock()
}

// timerAge is how long a command must have been running to count as a timer —
// the sidebar's tick sleeps for half a minute and then only returns a message,
// so it is not waited for. Everything else finishes in milliseconds.
const timerAge = 250 * time.Millisecond

// settle waits until no command but a timer is running or awaiting Update.
func (w *trackedModel) settle(t *testing.T) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		w.mu.Lock()
		busy := 0
		for _, started := range w.running {
			if time.Since(started) < timerAge {
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
