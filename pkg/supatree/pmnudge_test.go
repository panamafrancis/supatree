package supatree

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/panamafrancis/workbench/pkg/testutil"
	"github.com/panamafrancis/workbench/pkg/zellij"
)

func TestNudgeDue(t *testing.T) {
	for _, tc := range []struct {
		name      string
		st        readOffset
		end, size int64
		want      bool
	}{
		{"first mail", readOffset{}, 100, 100, true},
		{"already nudged", readOffset{Offset: 100, Size: 100}, 100, 150, false},
		{"newer mail", readOffset{Offset: 100, Size: 100}, 150, 150, true},
		{"queue truncated", readOffset{Offset: 100, Size: 100}, 40, 40, true},
		{"truncated, regrown past offset only", readOffset{Offset: 100, Size: 300}, 120, 120, true},
	} {
		if got := nudgeDue(tc.st, tc.end, tc.size); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// nudgeStub replaces the process registry and the zellij calls. By default
// every PM in pms runs in session "s" with an agent pane in its tab.
type nudgeStub struct {
	procs     []ProcRecord
	listed    int
	dead      bool
	noPane    map[string]bool // tab → no agent pane there
	submitErr error
	submitted []string // tabs typed into
}

func stubNudge(t *testing.T, pms ...string) *nudgeStub {
	t.Helper()
	s := &nudgeStub{noPane: map[string]bool{}}
	for _, name := range pms {
		s.procs = append(s.procs, ProcRecord{PID: 1, PM: name, Session: "s"})
	}
	paneTab := map[int]string{}
	oldList, oldAlive, oldPane, oldSubmit := listProcs, sessionAlive, agentPane, submitToPane
	listProcs = func() []ProcRecord { s.listed++; return s.procs }
	sessionAlive = func(string) (bool, error) { return !s.dead, nil }
	agentPane = func(_, tab string) (zellij.Pane, bool, error) {
		if s.noPane[tab] {
			return zellij.Pane{}, false, nil
		}
		id := len(paneTab) + 1
		paneTab[id] = tab
		return zellij.Pane{ID: id, TabName: tab}, true, nil
	}
	submitToPane = func(session string, id int, text string) error {
		if session != "s" || text != PMNudgeText {
			t.Errorf("submitToPane(%q, %d, %q)", session, id, text)
		}
		s.submitted = append(s.submitted, paneTab[id])
		return s.submitErr
	}
	t.Cleanup(func() {
		listProcs, sessionAlive, agentPane, submitToPane = oldList, oldAlive, oldPane, oldSubmit
	})
	return s
}

func appendReq(t *testing.T, r Request) {
	t.Helper()
	if r.Text == "" {
		r.Text = "hi"
	}
	if err := AppendRequest(r); err != nil {
		t.Fatal(err)
	}
}

// nudge runs NudgePMs and returns the tabs it typed into this call.
func nudge(t *testing.T, s *nudgeStub) []string {
	t.Helper()
	s.submitted = nil
	if _, err := NudgePMs(); err != nil {
		t.Fatal(err)
	}
	return s.submitted
}

func TestNudgePMsOncePerMail(t *testing.T) {
	testutil.IsolateHome(t)
	s := stubNudge(t, DefaultPMName)

	appendReq(t, Request{From: fromCLI})
	if got := nudge(t, s); !reflect.DeepEqual(got, []string{PMTab}) {
		t.Fatalf("first mail typed into %v, want [%s]", got, PMTab)
	}
	if got := nudge(t, s); len(got) != 0 {
		t.Fatalf("second call for the same mail typed into %v", got)
	}
	appendReq(t, Request{From: fromCLI, Text: "more"})
	if got := nudge(t, s); len(got) != 1 {
		t.Fatalf("new mail typed into %v, want one nudge", got)
	}
}

// A forwarded reply keeps the time its agent wrote it, which may be before the
// last nudge, or — written by an agent — anywhere at all. Neither may decide.
func TestNudgePMsIgnoresRequestTimes(t *testing.T) {
	testutil.IsolateHome(t)
	s := stubNudge(t, DefaultPMName)
	now := time.Now().UTC()

	appendReq(t, Request{From: fromCLI, At: now})
	nudge(t, s)
	appendReq(t, Request{From: "st-x-main", At: now.Add(-time.Hour)})
	if got := nudge(t, s); len(got) != 1 {
		t.Fatalf("older-stamped mail typed into %v, want a nudge", got)
	}
	appendReq(t, Request{From: "st-x-main", At: now.AddDate(70, 0, 0)})
	nudge(t, s)
	appendReq(t, Request{From: fromCLI, At: now})
	if got := nudge(t, s); len(got) != 1 {
		t.Fatalf("mail after a future-stamped one typed into %v, want a nudge", got)
	}
}

func TestNudgePMsOnlyTheAddressee(t *testing.T) {
	testutil.IsolateHome(t)
	ops, err := AddPM("ops", "")
	if err != nil {
		t.Fatal(err)
	}
	s := stubNudge(t, DefaultPMName, ops.Name)

	appendReq(t, Request{From: fromCLI}) // unaddressed: the top PM's
	if got := nudge(t, s); !reflect.DeepEqual(got, []string{PMTab}) {
		t.Fatalf("typed into %v, want only [%s]", got, PMTab)
	}
	// Mail for ops moves the queue on; the top PM, still holding unread mail,
	// must not be nudged again for it.
	appendReq(t, Request{From: fromCLI, To: ops.Name})
	if got := nudge(t, s); !reflect.DeepEqual(got, []string{ops.Tab()}) {
		t.Fatalf("typed into %v, want only [%s]", got, ops.Tab())
	}
}

// Every reason not to type leaves the state where it was, so the nudge comes
// once the reason clears.
func TestNudgePMsSkipsAndRetries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block func(*nudgeStub)
		clear func(*nudgeStub)
	}{
		{"no live process",
			func(s *nudgeStub) { s.procs[0].Session = "" },
			func(s *nudgeStub) { s.procs[0].Session = "s" }},
		{"session gone",
			func(s *nudgeStub) { s.dead = true },
			func(s *nudgeStub) { s.dead = false }},
		{"no agent pane",
			func(s *nudgeStub) { s.noPane[PMTab] = true },
			func(s *nudgeStub) { delete(s.noPane, PMTab) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testutil.IsolateHome(t)
			s := stubNudge(t, DefaultPMName)
			appendReq(t, Request{From: fromCLI})
			tc.block(s)
			if got := nudge(t, s); len(got) != 0 {
				t.Fatalf("typed into %v while blocked", got)
			}
			tc.clear(s)
			if got := nudge(t, s); len(got) != 1 {
				t.Fatalf("typed into %v once clear, want a nudge", got)
			}
		})
	}
}

// A failed submit is reported but not retried: whatever part of it reached the
// prompt would be typed a second time.
func TestNudgePMsSubmitErrorAdvances(t *testing.T) {
	testutil.IsolateHome(t)
	s := stubNudge(t, DefaultPMName)
	s.submitErr = errors.New("zellij write: boom")
	appendReq(t, Request{From: fromCLI})

	nudged, err := NudgePMs()
	if err == nil || !strings.Contains(err.Error(), "boom") || len(nudged) != 0 {
		t.Fatalf("NudgePMs = %v, %v; want the submit error and nobody nudged", nudged, err)
	}
	s.submitErr = nil
	if got := nudge(t, s); len(got) != 0 {
		t.Fatalf("retried a failed nudge into %v", got)
	}
}

// With nothing due there is no reason to run ps once per process every tick.
func TestNudgePMsNothingDueListsNoProcs(t *testing.T) {
	testutil.IsolateHome(t)
	s := stubNudge(t, DefaultPMName)
	nudge(t, s)
	if s.listed != 0 {
		t.Fatalf("listed processes %d times with no mail", s.listed)
	}
}
