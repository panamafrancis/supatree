package reviewer

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/panamafrancis/supatree/pkg/supatree"
	"github.com/panamafrancis/workbench/pkg/github"
)

const (
	bot   = "review-bot"
	apiPR = "o/api#1"
	webPR = "o/web#2"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func settings() Settings {
	return Settings{Login: bot, GroupWindow: 5 * time.Minute, MaxConcurrent: 2, MaxAttempts: 3, TreeTTL: 7 * 24 * time.Hour}
}

// pr builds a watched pull request in stack "web": repo is owner/name, and the
// member alias is the name.
func pr(repo string, n int, mods ...func(*PR)) *PR {
	_, alias, _ := strings.Cut(repo, "/")
	p := &PR{
		Key: PRKey(repo, n), Repo: repo, Number: n, URL: "https://github.com/" + repo + "/pull/" + strconv.Itoa(n),
		Author: "alice", HeadRef: "feature-x-" + alias, Head: "abc1234", Status: github.PROpen,
		Requested: true, UpdatedAt: t0, Stack: "web", Alias: alias,
	}
	for _, m := range mods {
		m(p)
	}
	return p
}

func snapOf(now time.Time, prs ...*PR) Snapshot {
	s := Snapshot{Now: now, PRs: map[string]*PR{}, Trees: map[string]*Tree{}, Running: map[string]bool{}}
	for _, p := range prs {
		s.PRs[p.Key] = p
	}
	return s
}

func newState() *State { return &State{PRs: map[string]*PRState{}, Trees: map[string]*TreeState{}} }

func kinds(as []Action) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, string(a.Kind)+":"+a.Tree+":"+strings.Join(a.PRs, ","))
	}
	return out
}

func TestWaiting(t *testing.T) {
	reviewed := &PRState{ReviewedAt: t0}
	tests := []struct {
		name string
		p    *PR
		ps   *PRState
		want bool
	}{
		{"requested", pr("o/api", 1), nil, true},
		{"not requested", pr("o/api", 1, func(p *PR) { p.Requested = false }), nil, false},
		{"draft", pr("o/api", 1, func(p *PR) { p.Status = github.PRDraft }), nil, false},
		{"merged", pr("o/api", 1, func(p *PR) { p.Status = github.PRMerged }), nil, false},
		{"own pull request", pr("o/api", 1, func(p *PR) { p.Author = "Review-Bot" }), nil, false},
		// A listing read before the review landed still shows the request it answered.
		{"stale listing after a review", pr("o/api", 1, func(p *PR) { p.UpdatedAt = t0.Add(-time.Second) }), reviewed, false},
		{"re-requested after a review", pr("o/api", 1, func(p *PR) { p.UpdatedAt = t0.Add(time.Hour) }), reviewed, true},
	}
	for _, tc := range tests {
		if got := waiting(tc.p, tc.ps, bot); got != tc.want {
			t.Errorf("%s: waiting = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestGroupDeterministic(t *testing.T) {
	marker := "<!-- supatree:change id=fix-login -->"
	tests := []struct {
		name string
		prs  []*PR
		want [][]string
	}{
		{
			"change marker",
			[]*PR{pr("o/api", 1, func(p *PR) { p.Body = marker; p.HeadRef = "a" }), pr("o/web", 2, func(p *PR) { p.Body = "x\n" + marker; p.HeadRef = "b" })},
			[][]string{{apiPR, webPR}},
		},
		{
			"linked from the description",
			[]*PR{pr("o/api", 1, func(p *PR) { p.Body = "needs o/web#2"; p.HeadRef = "a" }), pr("o/web", 2, func(p *PR) { p.HeadRef = "b" })},
			[][]string{{apiPR, webPR}},
		},
		{
			"same supatree slug",
			[]*PR{pr("o/api", 1, func(p *PR) { p.HeadRef = "st/fix-login/api" }), pr("o/web", 2, func(p *PR) { p.HeadRef = "st/fix-login/web" })},
			[][]string{{apiPR, webPR}},
		},
		{
			"same branch, different authors",
			[]*PR{pr("o/api", 1, func(p *PR) { p.HeadRef = "fix" }), pr("o/web", 2, func(p *PR) { p.HeadRef = "fix"; p.Author = "bob" })},
			[][]string{{apiPR}, {webPR}},
		},
		{
			"a link to someone else's pull request",
			[]*PR{pr("o/api", 1, func(p *PR) { p.Body = "see https://github.com/o/web/pull/2"; p.HeadRef = "a" }), pr("o/web", 2, func(p *PR) { p.Author = "bob"; p.HeadRef = "b" })},
			[][]string{{apiPR}, {webPR}},
		},
		{
			// A review tree holds one pull request per repo: two in one
			// repo split rather than merge, the first union winning.
			"two in one repo",
			[]*PR{
				pr("o/api", 1, func(p *PR) { p.HeadRef = "st/x/api" }),
				pr("o/api", 3, func(p *PR) { p.HeadRef = "st/x/api2" }),
				pr("o/web", 2, func(p *PR) { p.HeadRef = "st/x/web" }),
			},
			[][]string{{apiPR, webPR}, {"o/api#3"}},
		},
		{
			"different stacks",
			[]*PR{pr("o/api", 1, func(p *PR) { p.HeadRef = "fix" }), pr("o/web", 2, func(p *PR) { p.HeadRef = "fix"; p.Stack = "other" })},
			[][]string{{apiPR}, {webPR}},
		},
		{
			"a default branch says nothing",
			[]*PR{pr("o/api", 1, func(p *PR) { p.HeadRef = "main" }), pr("o/web", 2, func(p *PR) { p.HeadRef = "main" })},
			[][]string{{apiPR}, {webPR}},
		},
	}
	for _, tc := range tests {
		m := map[string]*PR{}
		for _, p := range tc.prs {
			m[p.Key] = p
		}
		got := groupDeterministic(m).components()
		if !slices.EqualFunc(got, tc.want, slices.Equal[[]string]) {
			t.Errorf("%s: groups = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Siblings opened a few minutes apart land in one tree: a new group waits out
// the window from its newest request.
func TestPlanWaitsOutTheGroupWindow(t *testing.T) {
	st := newState()
	api := pr("o/api", 1, func(p *PR) { p.HeadRef = "st/x/api" })
	if as := Plan(snapOf(t0, api), st, settings(), nil); len(as) != 0 {
		t.Fatalf("first sight: %v, want nothing until the window passes", kinds(as))
	}
	web := pr("o/web", 2, func(p *PR) { p.HeadRef = "st/x/web" })
	if as := Plan(snapOf(t0.Add(4*time.Minute), api, web), st, settings(), nil); len(as) != 0 {
		t.Fatalf("sibling arrived: %v, want the window restarted", kinds(as))
	}
	as := Plan(snapOf(t0.Add(10*time.Minute), api, web), st, settings(), nil)
	if got := kinds(as); !slices.Equal(got, []string{"create::o/api#1,o/web#2"}) {
		t.Fatalf("after the window: %v, want one tree for both", got)
	}
	if st.PRs[webPR].Grouping != ReasonBranch {
		t.Errorf("grouping reason = %q", st.PRs[webPR].Grouping)
	}
}

func TestPlanRunsAddsAndReaps(t *testing.T) {
	now := t0.Add(time.Hour)
	tree := &Tree{Name: "rv-api-1", Stack: "web", PRs: map[string]string{"api": apiPR}}

	t.Run("a waiting pull request in a tree is run", func(t *testing.T) {
		st := newState()
		snap := snapOf(now, pr("o/api", 1))
		snap.Trees[tree.Name] = tree
		if got := kinds(Plan(snap, st, settings(), nil)); !slices.Equal(got, []string{"run:rv-api-1:o/api#1"}) {
			t.Errorf("actions = %v", got)
		}
	})
	t.Run("not while a run is in flight", func(t *testing.T) {
		st := newState()
		snap := snapOf(now, pr("o/api", 1))
		snap.Trees[tree.Name] = tree
		snap.Running[tree.Name] = true
		if got := kinds(Plan(snap, st, settings(), nil)); len(got) != 0 {
			t.Errorf("actions = %v", got)
		}
	})
	t.Run("a late sibling joins the tree at once", func(t *testing.T) {
		st := newState()
		snap := snapOf(now,
			pr("o/api", 1, func(p *PR) { p.HeadRef = "st/x/api"; p.Requested = false }),
			pr("o/web", 2, func(p *PR) { p.HeadRef = "st/x/web" }))
		snap.Trees[tree.Name] = tree
		got := kinds(Plan(snap, st, settings(), nil))
		if !slices.Equal(got, []string{"add:rv-api-1:o/web#2"}) {
			t.Errorf("actions = %v", got)
		}
	})
	t.Run("a second change in a held repo gets its own tree", func(t *testing.T) {
		st := newState()
		st.PRs["o/api#3"] = &PRState{WaitingSince: t0}
		snap := snapOf(now,
			pr("o/api", 1, func(p *PR) { p.HeadRef = "st/x/api"; p.Requested = false }),
			pr("o/api", 3, func(p *PR) { p.HeadRef = "st/x/api" }))
		snap.Trees[tree.Name] = tree
		got := kinds(Plan(snap, st, settings(), nil))
		if !slices.Equal(got, []string{"create::o/api#3"}) {
			t.Errorf("actions = %v", got)
		}
	})
	t.Run("merged everywhere is reaped", func(t *testing.T) {
		st := newState()
		snap := snapOf(now, pr("o/api", 1, func(p *PR) { p.Status = github.PRMerged }))
		snap.Trees[tree.Name] = tree
		if got := kinds(Plan(snap, st, settings(), nil)); !slices.Equal(got, []string{"reap:rv-api-1:"}) {
			t.Errorf("actions = %v", got)
		}
	})
	t.Run("a pull request the cache lost is not proof it merged", func(t *testing.T) {
		st := newState()
		st.Trees[tree.Name] = &TreeState{CreatedAt: now.Add(-time.Hour)}
		snap := snapOf(now)
		snap.Trees[tree.Name] = tree
		if got := kinds(Plan(snap, st, settings(), nil)); len(got) != 0 {
			t.Errorf("actions = %v", got)
		}
	})
	t.Run("idle past the TTL is reaped", func(t *testing.T) {
		st := newState()
		st.Trees[tree.Name] = &TreeState{CreatedAt: now.Add(-30 * 24 * time.Hour), LastRunAt: now.Add(-8 * 24 * time.Hour)}
		snap := snapOf(now, pr("o/api", 1, func(p *PR) { p.Requested = false }))
		snap.Trees[tree.Name] = tree
		if got := kinds(Plan(snap, st, settings(), nil)); !slices.Equal(got, []string{"reap:rv-api-1:"}) {
			t.Errorf("actions = %v", got)
		}
	})
}

func TestPlanBacksOffAndGivesUp(t *testing.T) {
	tree := &Tree{Name: "rv-api-1", Stack: "web", PRs: map[string]string{"api": apiPR}}
	plan := func(now time.Time, st *State, p *PR) []string {
		snap := snapOf(now, p)
		snap.Trees[tree.Name] = tree
		return kinds(Plan(snap, st, settings(), nil))
	}
	st := newState()
	st.PRs[apiPR] = &PRState{WaitingSince: t0, Attempts: 1, LastAttempt: t0}
	if got := plan(t0.Add(5*time.Minute), st, pr("o/api", 1)); len(got) != 0 {
		t.Errorf("inside the backoff: %v", got)
	}
	if got := plan(t0.Add(11*time.Minute), st, pr("o/api", 1)); len(got) != 1 {
		t.Errorf("past the backoff: %v", got)
	}
	st.PRs[apiPR].GaveUpAt = t0
	if got := plan(t0.Add(3*time.Hour), st, pr("o/api", 1)); len(got) != 0 {
		t.Errorf("given up: %v", got)
	}
	// The author pushed (or re-requested): the pull request moved after the
	// give-up, so it is tried afresh.
	moved := pr("o/api", 1, func(p *PR) { p.UpdatedAt = t0.Add(time.Hour) })
	if got := plan(t0.Add(3*time.Hour), st, moved); len(got) != 1 {
		t.Errorf("moved after the give-up: %v", got)
	}
	if st.PRs[apiPR].Attempts != 0 {
		t.Errorf("attempts = %d, want reset", st.PRs[apiPR].Attempts)
	}
}

func TestPlanCapsConcurrentRuns(t *testing.T) {
	snap := snapOf(t0.Add(time.Hour), pr("o/api", 1), pr("o/api", 2), pr("o/api", 3))
	st := newState()
	for i, n := range []string{"a", "b", "c"} {
		snap.Trees[n] = &Tree{Name: n, PRs: map[string]string{"api": PRKey("o/api", i+1)}}
	}
	snap.Running["a"] = true
	s := settings()
	got := kinds(Plan(snap, st, s, nil))
	if len(got) != s.MaxConcurrent-1 {
		t.Errorf("runs = %v, want %d with one already running", got, s.MaxConcurrent-1)
	}
}

// The LLM pass only ever sees one author's lone pull requests across
// repositories, and what it proposes is held to the same constraint.
func TestPlanAppliesRelatedUnderTheConstraint(t *testing.T) {
	api := pr("o/api", 1, func(p *PR) { p.HeadRef = "a" })
	web := pr("o/web", 2, func(p *PR) { p.HeadRef = "b" })
	api2 := pr("o/api", 3, func(p *PR) { p.HeadRef = "c" })
	st := newState()
	var asked []string
	related := func(prs []*PR) [][]string {
		for _, p := range prs {
			asked = append(asked, p.Key)
		}
		return [][]string{{apiPR, webPR, "o/api#3"}}
	}
	Plan(snapOf(t0, api, web, api2), st, settings(), related) // first sight starts the window
	got := kinds(Plan(snapOf(t0.Add(time.Hour), api, web, api2), st, settings(), related))
	if !slices.Equal(got, []string{"create::o/api#1,o/web#2", "create::o/api#3"}) {
		t.Errorf("actions = %v", got)
	}
	if st.PRs[webPR].Grouping != ReasonLLM {
		t.Errorf("grouping = %q", st.PRs[webPR].Grouping)
	}
	if len(asked) == 0 {
		t.Error("related was never asked")
	}
}

func TestParseWatchKey(t *testing.T) {
	repo, n, ok := parseWatchKey(github.WatchKey("Bot", github.RepoRef{Owner: "acme", Name: "api"}, 12))
	if !ok || repo != "acme/api" || n != 12 {
		t.Errorf("parsed %q %d %v", repo, n, ok)
	}
	for _, bad := range []string{"pr:acme/api#1", "watch:bot", "watch:bot:acme/api#x", "watch:bot:acme/api"} {
		if _, _, ok := parseWatchKey(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestBriefCarriesNoAuthorText(t *testing.T) {
	b := Brief(bot,
		[]briefPR{{Alias: "api", URL: "https://github.com/o/api/pull/1", LastHead: "0123456789abcdef"}},
		[]briefPR{{Alias: "web", URL: "https://github.com/o/web/pull/2"}})
	for _, want := range []string{"@" + bot, "re-review", "git -C repos/api diff 0123456789abcdef..HEAD", "post nothing on them", "o/web/pull/2"} {
		if !strings.Contains(b, want) {
			t.Errorf("brief lacks %q:\n%s", want, b)
		}
	}
}

func TestParseLLMOutput(t *testing.T) {
	prs := []*PR{pr("o/api", 1), pr("o/web", 2)}
	structured := `{"type":"result","is_error":false,"result":"","structured_output":{"groups":[{"prs":["o/api#1","o/web#2","o/evil#9"],"reason":"r"}]}}`
	got, err := parseLLMOutput([]byte(structured), prs)
	if err != nil || len(got) != 1 || !slices.Equal(got[0], []string{apiPR, webPR}) {
		t.Errorf("structured: %v %v", got, err)
	}
	text := `{"type":"result","result":"{\"groups\":[{\"prs\":[\"o/api#1\"]}]}"}`
	if got, err := parseLLMOutput([]byte(text), prs); err != nil || len(got) != 0 {
		t.Errorf("a one-PR group is no group: %v %v", got, err)
	}
}

// The marker the reviewer groups on is the one create_prs writes.
func TestGroupsOnTheBlockCreatePRsWrites(t *testing.T) {
	body := supatree.WithChangeBlock("desc", supatree.ChangeBlock("fix-login", nil))
	prs := map[string]*PR{
		apiPR: pr("o/api", 1, func(p *PR) { p.Body = body; p.HeadRef = "a" }),
		webPR: pr("o/web", 2, func(p *PR) { p.Body = body; p.HeadRef = "b" }),
	}
	g := groupDeterministic(prs)
	if got := g.components(); len(got) != 1 || g.reason[webPR] != ReasonMarker {
		t.Errorf("groups = %v (reason %q), want one by the marker", got, g.reason[webPR])
	}
}
