package supatree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/panamafrancis/workbench/pkg/github"
	"github.com/panamafrancis/workbench/pkg/testutil"
)

// repoKeystone is the PR repo used throughout, distinct from the workbench
// alias for the same repository.
const repoKeystone = "fraud-zero/keystone-api"

func TestParsePRRef(t *testing.T) {
	tests := []struct {
		in       string
		wantRepo string
		wantNum  int
	}{
		{"https://github.com/fraud-zero/keystone-api/pull/600", repoKeystone, 600},
		{"https://github.com/fraud-zero/keystone-api/pull/600/", repoKeystone, 600},
		{"http://github.com/o/r/pull/1", "o/r", 1},
		{"fraud-zero/keystone-api#600", repoKeystone, 600},
		{"  fraud-zero/docs#108  ", "fraud-zero/docs", 108},
		// What a browser actually puts on the clipboard.
		{"https://github.com/fraud-zero/keystone-api/pull/600/files", repoKeystone, 600},
		{"https://github.com/fraud-zero/keystone-api/pull/600?w=1", repoKeystone, 600},
		{"https://github.com/fraud-zero/keystone-api/pull/600#discussion_r1", repoKeystone, 600},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			repo, num, err := ParsePRRef(tc.in)
			if err != nil {
				t.Fatalf("ParsePRRef(%q) = %v", tc.in, err)
			}
			if repo != tc.wantRepo || num != tc.wantNum {
				t.Errorf("= %q #%d, want %q #%d", repo, num, tc.wantRepo, tc.wantNum)
			}
		})
	}
}

func TestParsePRRefRejects(t *testing.T) {
	// An issue URL is the one worth rejecting loudly: it parses as a number in a
	// repo, and silently reviewing issue 600 as if it were PR 600 would be a
	// confusing tree rather than an error.
	for _, in := range []string{
		"https://github.com/o/r/issues/600",
		"https://github.com/o/r/pull/notanumber",
		"o/r",
		"#600",
		"",
	} {
		t.Run(in, func(t *testing.T) {
			if _, _, err := ParsePRRef(in); err == nil {
				t.Errorf("ParsePRRef(%q) accepted a non-PR", in)
			}
		})
	}
}

// The branch prefix is what teardown matches on, so it must differ by mode and
// must not be derivable from a slug alone.
func TestMemberBranchByMode(t *testing.T) {
	authoring := &Meta{Slug: "refunds"}
	if got, want := authoring.MemberBranch("keystone"), "st/refunds/keystone"; got != want {
		t.Errorf("authoring MemberBranch = %q, want %q", got, want)
	}
	reviewing := &Meta{Slug: "refunds", Mode: ModeReviewing}
	if got, want := reviewing.MemberBranch("keystone"), "review/refunds/keystone"; got != want {
		t.Errorf("reviewing MemberBranch = %q, want %q", got, want)
	}
}

// The PR cache is one flat map across every repo and tree. Authoring branches
// are unique because st/<slug>/<alias> embeds the alias; review members carry
// the author's branch names, which for a cross-repo change are routinely
// identical across repos — so they must key on the PR instead.
func TestCacheKeyIsPRScopedWhenReviewing(t *testing.T) {
	authoring := Member{Alias: aliasKeystone, Branch: "st/refunds/keystone"}
	if got, want := authoring.CacheKey(), "st/refunds/keystone"; got != want {
		t.Errorf("authoring CacheKey = %q, want %q", got, want)
	}

	// Two members of one review tree whose authors used the same branch name.
	a := Member{Alias: aliasKeystone, Branch: "review/r/keystone",
		Review: &ReviewRef{Repo: repoKeystone, Number: 600, HeadRef: headRefRefunds}}
	b := Member{Alias: aliasAdmin, Branch: "review/r/admin",
		Review: &ReviewRef{Repo: "fraud-zero/admin-frontend", Number: 988, HeadRef: headRefRefunds}}
	if a.CacheKey() == b.CacheKey() {
		t.Fatalf("two repos sharing branch %q collided on cache key %q", a.Review.HeadRef, a.CacheKey())
	}
	if got, want := a.CacheKey(), "pr:fraud-zero/keystone-api#600"; got != want {
		t.Errorf("CacheKey = %q, want %q", got, want)
	}
}

// A review member's branch is tree-local and has no origin ref by design. The
// unpushed-branch skip must therefore not apply to it, or a review tree would
// never ask GitHub about its own pull requests — reporting total_prs=0 forever,
// which is the bug review mode exists to fix.
func TestFetchTargetsIncludesReviewMembers(t *testing.T) {
	testutil.IsolateHome(t)
	inst := &Instance{
		Name: treeA,
		Mode: ModeReviewing,
		Members: []Member{{
			Alias:  aliasKeystone,
			Path:   t.TempDir(), // not a git repo: HasRemoteBranch is false
			Branch: "review/canberra/keystone",
			Exists: true,
			Review: &ReviewRef{Repo: repoKeystone, Number: 600, URL: "https://github.com/fraud-zero/keystone-api/pull/600"},
		}},
	}
	targets := FetchTargets([]*Instance{inst})
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1 — a review member was skipped as if it were an unpushed branch", len(targets))
	}
	if targets[0].Ref.Number != 600 {
		t.Errorf("target Ref.Number = %d, want 600 (the lookup must go by number, not by branch head)", targets[0].Ref.Number)
	}
	if targets[0].Key != "pr:fraud-zero/keystone-api#600" {
		t.Errorf("target Key = %q, want the PR-scoped key", targets[0].Key)
	}
}

// An authoring member is not pinned: that is what makes github.Sync apply the
// unpushed-branch skip to it (and match it by head), the quota discipline the
// review-member exception must not weaken.
func TestFetchTargetsLeavesAuthoringBranchesUnpinned(t *testing.T) {
	testutil.IsolateHome(t)
	inst := &Instance{
		Name:    treeA,
		Members: []Member{{Alias: aliasKeystone, Path: t.TempDir(), Branch: "st/canberra/keystone", Exists: true}},
	}
	targets := FetchTargets([]*Instance{inst})
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(targets))
	}
	if targets[0].Ref.Number != 0 {
		t.Errorf("authoring target pinned to #%d, want it matched by head", targets[0].Ref.Number)
	}
	if targets[0].Key != "st/canberra/keystone" {
		t.Errorf("target Key = %q, want the branch", targets[0].Key)
	}
}

// The refusal has to redirect, not just refuse: the agent that reaches it was
// told by its own instructions to rename before opening a PR, so the message is
// read at the one moment it can change what happens next.
func TestRefuseAuthoringRedirects(t *testing.T) {
	authoring := &Instance{Name: treeA}
	if msg := refuseAuthoring(authoring, "Renaming the branches"); msg != "" {
		t.Errorf("authoring tree was refused: %q", msg)
	}

	reviewing := &Instance{Name: treeA, Mode: ModeReviewing, Members: []Member{{
		Alias:  aliasKeystone,
		Review: &ReviewRef{Repo: repoKeystone, Number: 600, HeadRef: headRefRefunds},
	}}}
	msg := refuseAuthoring(reviewing, "Renaming the branches")
	for _, want := range []string{"review tree", "fraud-zero/keystone-api#600", headRefRefunds, "docs"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not mention %q:\n%s", want, msg)
		}
	}
}

// Every topic the tool advertises must resolve, or the discovery path dead-ends
// at the moment it is needed.
func TestDocsTopics(t *testing.T) {
	for _, topic := range []string{"", "overview", "review", "REVIEW"} {
		out, isErr := handleDocsTopic(map[string]any{"topic": topic})
		if isErr || strings.TrimSpace(out) == "" {
			t.Errorf("topic %q did not resolve: %s", topic, out)
		}
	}
	if _, isErr := handleDocsTopic(map[string]any{"topic": "nope"}); !isErr {
		t.Error("unknown topic was not reported as an error")
	}
}

// The seed exists to make the lookup resolvable, not to report a status nobody
// fetched: it carries the number and URL, and is stale from the moment it is
// written so the first real round replaces it.
func TestSeedPRCacheIsResolvableButNotAuthoritative(t *testing.T) {
	testutil.IsolateHome(t)
	ref := ReviewRef{Repo: repoKeystone, Number: 600, URL: "https://github.com/fraud-zero/keystone-api/pull/600"}
	seedPRCache(map[string]ReviewRef{aliasKeystone: ref})

	cache := github.NewCache(PRCachePath())
	if err := cache.Load(); err != nil {
		t.Fatalf("load cache: %v", err)
	}
	key := "pr:" + repoKeystone + "#600"
	if !cache.KnowsPR(key) {
		t.Fatal("seeded entry does not identify the PR, so the by-number lookup cannot resolve it")
	}
	if got := cache.Ref(key); got.Number != 600 || got.URL != ref.URL {
		t.Errorf("Ref = %+v, want number 600 and the PR URL", got)
	}
	if !cache.IsStale(key, PRStaleAge) {
		t.Error("seeded entry is not stale: a status nobody fetched would be reported as fact")
	}
	if info := cache.Get(key); info == nil || info.Status != github.PRNone {
		t.Errorf("seed asserted a status: %+v", info)
	}
}

// Posting the review is what a review tree is for, so it may post by default —
// but the human can still withhold that, globally or per tree, and the refusal
// must name the switch they would flip.
func TestReviewPostOutwardDefaults(t *testing.T) {
	home := testutil.IsolateHome(t)
	root := filepath.Join(home, "tree")
	inst := &Instance{Name: treeA, Root: root, Mode: ModeReviewing}
	if err := (&Meta{Name: treeA, Mode: ModeReviewing}).Save(root); err != nil {
		t.Fatal(err)
	}
	if msg := outwardDenied(inst, "posting a review"); msg != "" {
		t.Fatalf("a review tree was refused posting by default:\n%s", msg)
	}

	off := false
	if err := (&Config{ReviewOutward: &off}).Save(); err != nil {
		t.Fatal(err)
	}
	msg := outwardDenied(inst, "posting a review")
	if msg == "" {
		t.Fatal("posting was permitted with review_outward off")
	}
	for _, want := range []string{"outward", "review_outward", "let the human post it"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not mention %q:\n%s", want, msg)
		}
	}
}

// An empty review is a mistake, not a verdict.
func TestPostReviewRejectsEmptyAndBadAnchors(t *testing.T) {
	inst := &Instance{Name: treeA, Members: []Member{{
		Alias:  aliasKeystone,
		Review: &ReviewRef{Repo: repoKeystone, Number: 600, Head: "abc123"},
	}}}
	if _, err := PostReview(inst, aliasKeystone, "  ", "COMMENT", nil); err == nil {
		t.Error("an empty review was accepted")
	}
	if _, err := PostReview(inst, aliasKeystone, "x", "LGTM", nil); err == nil {
		t.Error("an invalid event was accepted")
	}
	// A comment with no line number cannot be anchored, and GitHub would either
	// reject it or attach it somewhere arbitrary.
	bad := []ReviewComment{{Path: "pkg/x.go", Body: "hm"}}
	if _, err := PostReview(inst, aliasKeystone, "", "COMMENT", bad); err == nil {
		t.Error("a comment with no line number was accepted")
	}
	if _, err := PostReview(inst, "nosuch", "x", "COMMENT", nil); err == nil {
		t.Error("posting to a non-member was accepted")
	}
}

func TestParseReviewComments(t *testing.T) {
	got, err := ParseReviewComments(`[{"path":"pkg/x.go","line":42,"body":"here"}]`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].Path != "pkg/x.go" || got[0].Line != 42 {
		t.Errorf("got %+v", got)
	}
	if out, err := ParseReviewComments("  "); err != nil || out != nil {
		t.Errorf("empty input should be no comments, got %+v %v", out, err)
	}
	if _, err := ParseReviewComments("not json"); err == nil {
		t.Error("malformed input was accepted")
	}
}

// A review tree holds every member of its stack, not only the repos the change
// touched. A bystander left on the review rung would pin the rollup below
// `reviewed` forever, so the tree could never be reaped.
func TestUntrackedReviewMemberIsIdle(t *testing.T) {
	const branch = "review/refunds/docs"
	local := MemberLocal{Exists: true, CheckedOut: branch}
	if got := memberState(local, nil, branch, ModeReviewing, false); got != MemberIdle {
		t.Errorf("untracked review member = %q, want %q", got, MemberIdle)
	}
	if got := memberState(local, nil, branch, ModeReviewing, true); got != MemberInReview {
		t.Errorf("tracked review member = %q, want %q", got, MemberInReview)
	}
}

// Every reviewed PR has landed and the repos the change did not touch are idle:
// the tree is finished, and finished under its own name rather than as `done`.
func TestReviewTreeReachesReviewedWithBystanders(t *testing.T) {
	merged := github.PRInfo{Number: 600, Status: github.PRMerged}
	tree := TreeStatus{Mode: ModeReviewing, Members: []MemberStatus{
		{Alias: aliasKeystone, State: MemberMerged, PR: &merged},
		{Alias: "docs", State: MemberIdle},
	}}
	rollup(&tree, time.Now(), DefaultStaleAfter)
	if tree.State != TreeReviewed {
		t.Errorf("state = %q, want %q", tree.State, TreeReviewed)
	}
	if !tree.Reap() {
		t.Error("a finished review tree should be reapable")
	}
}

// repoOA and repoOB are the GitHub repos the review fixtures map members a and b to.
const (
	repoOA = "o/a"
	repoOB = "o/b"
)

// reviewStackFixture is a stack with an origin — trees are built from origin's
// default branch, which is what lets the local checkout run ahead of it — and
// member repos that resolve to o/<alias> on GitHub, each with a PR #1 head.
func reviewStackFixture(t *testing.T) (c *Config, a, b string) {
	t.Helper()
	c, a, b = stackFixture(t)
	repos := map[string]string{a: repoOA, b: repoOB}
	orig := resolveRemote
	t.Cleanup(func() { resolveRemote = orig })
	resolveRemote = func(raw string) (github.RemoteRef, bool) {
		if repo, ok := repos[raw]; ok {
			return github.ParseRemote("git@github.com:" + repo + ".git")
		}
		return orig(raw)
	}
	for _, r := range []string{a, b} {
		run(t, r, "update-ref", "refs/pull/1/head", "HEAD")
	}
	stack := c.FindStack("s").Path
	origin := filepath.Join(t.TempDir(), "stack.git")
	run(t, stack, "clone", "-q", "--bare", stack, origin)
	run(t, stack, "remote", "add", "origin", origin)
	run(t, stack, "fetch", "-q", "origin")
	return c, a, b
}

func TestNewReviewChecksOutThePR(t *testing.T) {
	c, _, _ := reviewStackFixture(t)
	inst, _, err := NewReview(c, ReviewOptions{Stack: "s", Name: "rev",
		PRs: []ReviewRef{{Repo: repoOA, Number: 1, URL: "https://github.com/o/a/pull/1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if m := inst.FindMember("a"); m == nil || !m.Exists || m.Review == nil || m.Review.Number != 1 {
		t.Fatalf("member a = %+v, want it checked out at o/a#1", m)
	}
}

// #17: a member committed to the stack's checkout but never pushed — what
// `stack add` did before it published — is missing from origin, which a review
// tree is built from. That must be refused, saying why, not create a tree with
// nothing to review.
func TestNewReviewRefusesAMemberThatWasNeverPushed(t *testing.T) {
	c, _, b := reviewStackFixture(t)
	stack := c.FindStack("s").Path
	spec, err := LoadSpec(stack)
	if err != nil {
		t.Fatal(err)
	}
	spec.Members["b"] = b
	if err := SaveSpec(stack, spec); err != nil {
		t.Fatal(err)
	}
	run(t, stack, "commit", "-q", "-am", "supatree: add b (local only)")
	_, _, err = NewReview(c, ReviewOptions{Stack: "s", Name: "rev",
		PRs: []ReviewRef{{Repo: repoOB, Number: 1, URL: "https://github.com/o/b/pull/1"}}})
	if err == nil {
		t.Fatal("a review of a repo missing from origin's spec was accepted")
	}
	if !strings.Contains(err.Error(), "local checkout") || !strings.Contains(err.Error(), "push the stack") {
		t.Errorf("err = %v, want it to name the unpushed stack edit", err)
	}
	if insts, _ := List(c); len(insts) != 0 {
		t.Errorf("a refused review left %d tree(s) behind", len(insts))
	}

	// Once pushed, the same review works.
	run(t, c.FindStack("s").Path, "push", "-q", "origin", "HEAD:main")
	inst, _, err := NewReview(c, ReviewOptions{Stack: "s", Name: "rev",
		PRs: []ReviewRef{{Repo: repoOB, Number: 1, URL: "https://github.com/o/b/pull/1"}}})
	if err != nil {
		t.Fatalf("after pushing: %v", err)
	}
	if m := inst.FindMember("b"); m == nil || m.Review == nil {
		t.Errorf("member b = %+v, want it under review", m)
	}
}

// #12: a PR added to an existing review tree moves the idle member onto its
// head, keeps what was there, refuses over work, and tells the agent.
// addReviewFixture is a review tree for o/a#1 in a stack that also has b,
// whose PR #1 head is a commit b's default branch does not have.
func addReviewFixture(t *testing.T) (c *Config, inst *Instance, prHead string) {
	t.Helper()
	c, _, b := reviewStackFixture(t)
	stack := c.FindStack("s").Path
	spec, _ := LoadSpec(stack)
	spec.Members["b"] = b
	if err := SaveSpec(stack, spec); err != nil {
		t.Fatal(err)
	}
	run(t, stack, "commit", "-q", "-am", "add b")
	run(t, stack, "push", "-q", "origin", "HEAD:main")
	// A commit on b's PR head that its default branch does not have, so the
	// move is visible.
	run(t, b, "commit", "-q", "--allow-empty", "-m", "pr work")
	run(t, b, "update-ref", "refs/pull/1/head", "HEAD")
	run(t, b, "reset", "-q", "--hard", "HEAD~1")
	prHead, _ = gitOutput(b, "rev-parse", "refs/pull/1/head")

	var err error
	inst, _, err = NewReview(c, ReviewOptions{Stack: "s", Name: "rev",
		PRs: []ReviewRef{{Repo: repoOA, Number: 1, URL: "https://github.com/o/a/pull/1"}}})
	if err != nil {
		t.Fatal(err)
	}
	return c, inst, prHead
}

func TestAddReview(t *testing.T) {
	c, inst, prHead := addReviewFixture(t)
	writeFile(t, filepath.Join(StateDir(inst.Root), "review.md"), "notes so far\n")
	memberB := MemberPath(inst.Root, "b")

	// Uncommitted work on the bystander: reported, not reset.
	writeFile(t, filepath.Join(memberB, "scratch.txt"), "mine\n")
	run(t, memberB, "add", "scratch.txt")
	prB := ReviewRef{Repo: repoOB, Number: 1, URL: "https://github.com/o/b/pull/1"}
	res, err := AddReview(c, "rev", []ReviewRef{prB}, PMAgentName, false)
	if err != nil || len(res) != 1 || !strings.Contains(res[0].Skipped, "uncommitted") {
		t.Fatalf("add over uncommitted work: %+v, %v", res, err)
	}
	run(t, memberB, "reset", "-q", "--hard")

	res, err = AddReview(c, "rev", []ReviewRef{
		prB,
		{Repo: repoOA, Number: 1, URL: "https://github.com/o/a/pull/1"},
		{Repo: repoOA, Number: 2, URL: "https://github.com/o/a/pull/2"},
		{Repo: "o/nope", Number: 3, URL: "https://github.com/o/nope/pull/3"},
	}, PMAgentName, false)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Skipped != "" || res[0].Alias != "b" || res[0].Head != prHead {
		t.Errorf("o/b#1: %+v, want it added at %s", res[0], prHead)
	}
	for i, want := range []string{"already in this tree", "one pull request per repo", "no member of this stack"} {
		if !strings.Contains(res[i+1].Skipped, want) {
			t.Errorf("result %d = %+v, want skipped: %s", i+1, res[i+1], want)
		}
	}
	if head, _ := gitOutput(memberB, "rev-parse", "HEAD"); head != prHead {
		t.Errorf("member b is at %s, want the PR head %s", head, prHead)
	}
	after, _ := LoadInstance(inst.Root)
	if m := after.FindMember("b"); m == nil || m.Review == nil || m.Review.Number != 1 {
		t.Errorf("b is not recorded as under review: %+v", m)
	}
	if body, _ := os.ReadFile(filepath.Join(StateDir(inst.Root), "review.md")); string(body) != "notes so far\n" {
		t.Errorf("review.md = %q, want it kept", body)
	}
	msgs, _ := Mail(inst.Root, MainAgent)
	if len(msgs) == 0 || !strings.Contains(msgs[len(msgs)-1].Text, "https://github.com/o/b/pull/1") {
		t.Errorf("the agent was not told about the added PR: %+v", msgs)
	}
}

// A review tree's PR that appears is one added for review, not opened.
func TestAddedReviewPREventText(t *testing.T) {
	prev := MemberStatus{Alias: "b", State: MemberIdle}
	cur := MemberStatus{Alias: "b", State: MemberOpen, PR: &github.PRInfo{Number: 7, Status: github.PROpen}}
	evs := memberEvents("rev", prev, cur, time.Now(), ModeReviewing)
	if len(evs) != 1 || !strings.Contains(evs[0].Text, "added for review") {
		t.Errorf("events = %+v", evs)
	}
}

// The watcher acts on repos/<alias> only when it is the worktree supatree
// made: an agent swapping it for a link to elsewhere gets it left alone.
func TestAddReviewRefusesARedirectedMember(t *testing.T) {
	c, inst, _ := addReviewFixture(t)
	member := MemberPath(inst.Root, "b")
	elsewhere := t.TempDir()
	if err := os.RemoveAll(member); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, member); err != nil {
		t.Fatal(err)
	}
	res, err := AddReview(c, "rev", []ReviewRef{{Repo: repoOB, Number: 1, URL: "https://github.com/o/b/pull/1"}}, PMAgentName, false)
	if err != nil || len(res) != 1 || !strings.Contains(res[0].Skipped, "not this tree's worktree") {
		t.Errorf("add through a symlinked member: %+v, %v", res, err)
	}
}

// The clone is shared by every tree: repos/ linked into another tree must not
// get that tree's worktree removed.
func TestAddReviewRefusesAnotherTreesWorktree(t *testing.T) {
	c, inst, _ := addReviewFixture(t)
	other, _, err := NewReview(c, ReviewOptions{Stack: "s", Name: "other",
		PRs: []ReviewRef{{Repo: repoOA, Number: 1, URL: "https://github.com/o/a/pull/1"}}})
	if err != nil {
		t.Fatal(err)
	}
	repos := filepath.Join(inst.Root, ReposDirName)
	if err := os.RemoveAll(repos); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other.Root, ReposDirName), repos); err != nil {
		t.Fatal(err)
	}
	res, err := AddReview(c, "rev", []ReviewRef{{Repo: repoOB, Number: 1, URL: "https://github.com/o/b/pull/1"}}, PMAgentName, false)
	if err != nil || len(res) != 1 || res[0].Skipped == "" {
		t.Fatalf("add through a repos/ linked into another tree: %+v, %v; want it refused", res, err)
	}
	if _, err := os.Stat(filepath.Join(other.Root, ReposDirName, "b", ".git")); err != nil {
		t.Error("the other tree's worktree was removed")
	}
}

// With several stacks registered and none named, a review goes to the one
// stack holding every pull request's repo, and says so when none or several do.
func TestInferReviewStack(t *testing.T) {
	c, a, b := reviewStackFixture(t)
	addStack := func(alias string, members map[string]string) {
		dir := t.TempDir()
		if err := SaveSpec(dir, &Spec{Members: members}); err != nil {
			t.Fatal(err)
		}
		c.Stacks = append(c.Stacks, Stack{Alias: alias, Path: dir})
	}
	addStack("t", map[string]string{"b": b})
	prA := ReviewRef{Repo: repoOA, Number: 1}
	prB := ReviewRef{Repo: repoOB, Number: 1}

	if got, err := inferReviewStack(c, []ReviewRef{prA}); err != nil || got != "s" {
		t.Errorf("o/a: got %q, %v; want s", got, err)
	}
	if got, err := inferReviewStack(c, []ReviewRef{prB}); err != nil || got != "t" {
		t.Errorf("o/b: got %q, %v; want t", got, err)
	}
	if _, err := inferReviewStack(c, []ReviewRef{prA, prB}); err == nil || !strings.Contains(err.Error(), "no registered stack") {
		t.Errorf("o/a with o/b, in no one stack: err = %v", err)
	}
	addStack("u", map[string]string{"a": a})
	if _, err := inferReviewStack(c, []ReviewRef{prA}); err == nil || !strings.Contains(err.Error(), "s, u") {
		t.Errorf("o/a in two stacks: err = %v, want both named", err)
	}

	// NewReview uses it when no stack is named.
	c.Stacks = c.Stacks[:2]
	inst, _, err := NewReview(c, ReviewOptions{Name: "rev", PRs: []ReviewRef{{Repo: repoOA, Number: 1, URL: "https://github.com/o/a/pull/1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if inst.Stack != "s" {
		t.Errorf("review tree built from stack %q, want s", inst.Stack)
	}
}

// A reviewer that may not approve posts COMMENT whatever its agent asks for,
// and every review it posts carries the signature.
func TestReviewerPost(t *testing.T) {
	off := false
	const lgtm = "lgtm"
	sig := "_Automated review by supatree._"
	cases := []struct {
		name            string
		r               *ReviewerSettings
		event, body     string
		wantEv, wantBod string
		noted           bool
	}{
		{"no reviewer block", nil, "APPROVE", lgtm, "APPROVE", lgtm, false},
		{"approve unset", &ReviewerSettings{}, "APPROVE", lgtm, "APPROVE", lgtm, false},
		{"approve off", &ReviewerSettings{Approve: &off}, "approve", lgtm, "COMMENT", lgtm, true},
		{"approve off, changes", &ReviewerSettings{Approve: &off}, "REQUEST_CHANGES", "fix", "REQUEST_CHANGES", "fix", false},
		{"signature", &ReviewerSettings{Signature: sig}, "COMMENT", "body\n", "COMMENT", "body\n\n---\n" + sig, false},
		{"signature, empty body", &ReviewerSettings{Signature: " " + sig + "\n"}, "COMMENT", "", "COMMENT", sig, false},
	}
	for _, c := range cases {
		ev, body, note := reviewerPost(c.r, c.event, c.body)
		if ev != c.wantEv || body != c.wantBod || (note != "") != c.noted {
			t.Errorf("%s: got (%q, %q, %q), want (%q, %q, noted %v)", c.name, ev, body, note, c.wantEv, c.wantBod, c.noted)
		}
	}
}
