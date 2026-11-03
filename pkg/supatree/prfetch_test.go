package supatree

import (
	"testing"

	"github.com/panamafrancis/workbench/pkg/github"
)

// Whichever process wins a fetch round polls the reviewer's repositories with
// its watch on: one polling them without would consume the change it is
// waiting for.
func TestWithReviewerWatch(t *testing.T) {
	branch := github.Target{RepoPath: "/r/api", Branch: "feature"}
	watch := github.Target{RepoPath: "/r/api", Watch: "bot"}
	c := &Config{}
	if got := withReviewerWatch([]github.Target{branch}, c); len(got) != 1 {
		t.Errorf("no reviewer: %v", got)
	}
	c.Reviewer = &ReviewerSettings{Enabled: true, Login: "bot"}
	stub := func(*Config) []github.Target { return []github.Target{watch} }
	old := reviewerWatchTargetsFn
	reviewerWatchTargetsFn = stub
	defer func() { reviewerWatchTargetsFn = old }()
	got := withReviewerWatch([]github.Target{branch}, c)
	if len(got) != 2 || got[1] != watch {
		t.Errorf("with reviewer: %v", got)
	}
	if got := withReviewerWatch([]github.Target{branch, watch}, c); len(got) != 2 {
		t.Errorf("watch target added twice: %v", got)
	}
}
