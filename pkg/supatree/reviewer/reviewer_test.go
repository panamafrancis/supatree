package reviewer

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/panamafrancis/supatree/pkg/supatree"
	"github.com/panamafrancis/workbench/pkg/github"
	"github.com/panamafrancis/workbench/pkg/testutil"
)

func TestLatestReviewOnlyCountsThisRun(t *testing.T) {
	started := t0.Add(time.Hour)
	orig := reviewsBy
	t.Cleanup(func() { reviewsBy = orig })
	reviewsBy = func(repo github.RepoRef, n int, login string) ([]github.PostedReview, error) {
		if repo.String() != "o/api" || n != 1 || login != bot {
			t.Errorf("asked for %s#%d by %s", repo, n, login)
		}
		return []github.PostedReview{
			{ID: 1, State: "CHANGES_REQUESTED", SubmittedAt: t0},                     // an earlier run's
			{ID: 2, State: "APPROVED", SubmittedAt: started.Add(-time.Minute)},       // clock skew
			{ID: 3, State: "COMMENTED", SubmittedAt: started.Add(-30 * time.Minute)}, // too old
		}, nil
	}
	rev, ok, err := latestReview(pr("o/api", 1), bot, started)
	if err != nil || !ok || rev.ID != 2 {
		t.Fatalf("latest = %+v %v %v, want review 2", rev, ok, err)
	}
	reviewsBy = func(github.RepoRef, int, string) ([]github.PostedReview, error) {
		return []github.PostedReview{{ID: 1, SubmittedAt: t0}}, nil
	}
	if _, ok, _ := latestReview(pr("o/api", 1), bot, started); ok {
		t.Error("a review from before the run counted as this run's")
	}
}

func TestFailedGivesUpAndNotifies(t *testing.T) {
	testutil.IsolateHome(t)
	if err := supatree.EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	r := &Reviewer{}
	s := settings()
	for i := 0; i < s.MaxAttempts; i++ {
		r.failed(s, []string{apiPR}, "rv-api-1", errors.New("boom"))
	}
	st, err := LoadState()
	if err != nil {
		t.Fatal(err)
	}
	ps := st.PRs[apiPR]
	if ps == nil || ps.Attempts != s.MaxAttempts || ps.GaveUpAt.IsZero() || ps.LastErr != "boom" {
		t.Fatalf("state = %+v, want given up after %d attempts", ps, s.MaxAttempts)
	}
	// One more failure must not notify twice.
	r.failed(s, []string{apiPR}, "rv-api-1", errors.New("boom"))
	data, err := os.ReadFile(supatree.NotifyPath())
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), string(supatree.EventReviewFailed)); n != 1 {
		t.Errorf("%d review_failed notifications, want exactly one:\n%s", n, data)
	}
}
