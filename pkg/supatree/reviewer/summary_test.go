package reviewer

import (
	"testing"
	"time"
)

func TestSummarize(t *testing.T) {
	s := Settings{GroupWindow: 5 * time.Minute}
	snap := Snapshot{
		Now:     t0,
		Trees:   map[string]*Tree{"rv-a": {Name: "rv-a", PRs: map[string]string{"site": "o/web#2"}}},
		Running: map[string]bool{"rv-a": true},
	}
	st := &State{PRs: map[string]*PRState{
		"o/api#1": {WaitingSince: t0.Add(-time.Minute)},                             // window ends t0+4m
		"o/web#2": {WaitingSince: t0.Add(-time.Hour)},                               // in a tree: due now
		"o/cli#3": {WaitingSince: t0.Add(-time.Hour), GaveUpAt: t0},                 // given up
		"o/doc#4": {ReviewedAt: t0},                                                 // not waiting
		"o/ops#5": {WaitingSince: t0.Add(-time.Hour), Attempts: 1, LastAttempt: t0}, // backoff ends t0+10m
	}}
	want := "reviewer: 4 pull requests waiting, 1 review running, 1 given up on, next due " +
		t0.Add(4*time.Minute).Local().Format("15:04")
	if got := summarize(snap, st, s); got != want {
		t.Errorf("summary = %q\nwant      %q", got, want)
	}
	if got := summarize(Snapshot{Now: t0}, &State{PRs: map[string]*PRState{}}, s); got != "reviewer: nothing waiting" {
		t.Errorf("idle summary = %q", got)
	}
}

func TestVerdictText(t *testing.T) {
	if got := verdictText("CHANGES_REQUESTED"); got != "changes requested" {
		t.Errorf("verdictText = %q", got)
	}
}
