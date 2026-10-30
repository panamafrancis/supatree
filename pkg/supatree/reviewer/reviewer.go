package reviewer

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/panamafrancis/supatree/pkg/supatree"
	"github.com/panamafrancis/workbench/pkg/git"
	"github.com/panamafrancis/workbench/pkg/github"
)

// Reviewer carries out Plan's actions. One lives in the watcher; Round is
// called once per watcher round, after the PR fetch, and returns at once
// when the previous round is still going.
type Reviewer struct {
	// Lock is held around tree create, add and remove — the watcher's op
	// lock, so the reviewer's tree operations never interleave with the op
	// queue's, nor with a watcher restart.
	Lock sync.Locker
	Logf func(format string, args ...any)

	busy    atomic.Bool
	mu      sync.Mutex
	running map[string]bool // trees with a run in flight in this process
	login   string          // the gh account, once verified
	related map[string][][]string
	wg      sync.WaitGroup
}

// Wait blocks until every run this Reviewer started has finished.
func (r *Reviewer) Wait() { r.wg.Wait() }

func (r *Reviewer) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

// Round runs one reviewer round against the cache the watcher just filled.
func (r *Reviewer) Round(ctx context.Context, cfg *supatree.Config, cache *github.Cache) {
	if !r.busy.CompareAndSwap(false, true) {
		return
	}
	defer r.busy.Store(false)
	s, err := Resolve(cfg)
	if errors.Is(err, ErrDisabled) {
		return
	}
	if err != nil {
		r.logf("reviewer: %v", err)
		return
	}
	if err := r.checkIdentity(s.Login); err != nil {
		r.logf("reviewer: %v", err)
		return
	}
	snap, err := r.snapshot(cfg, s, cache)
	if err != nil {
		r.logf("reviewer: %v", err)
		return
	}
	var related Related
	if s.LLMGrouping {
		related = r.memoRelated(llmGrouper(cfg, s, r.logf))
	}
	var actions []Action
	if err := MutateState(func(st *State) error {
		actions = Plan(snap, st, s, related)
		return nil
	}); err != nil {
		r.logf("reviewer: state: %v", err)
		return
	}
	for _, a := range actions {
		if ctx.Err() != nil {
			return
		}
		switch a.Kind {
		case ActCreate:
			r.create(cfg, s, snap, a)
		case ActAdd:
			r.add(cfg, s, snap, a)
		case ActReap:
			r.reap(cfg, a)
		case ActRun:
			r.start(ctx, cfg, s, snap, a)
		}
	}
}

// SnapshotFor is what Plan would see now, with the reviewer configured but
// not necessarily enabled. For `supatree reviewer status` and `plan`.
func SnapshotFor(cfg *supatree.Config, cache *github.Cache) (Snapshot, Settings, error) {
	s, err := ResolveConfigured(cfg)
	if err != nil {
		return Snapshot{}, s, err
	}
	r := &Reviewer{}
	snap, err := r.snapshot(cfg, s, cache)
	return snap, s, err
}

// CheckLogin reports whether gh is authenticated as login.
func CheckLogin(login string) error {
	return (&Reviewer{}).checkIdentity(login)
}

// RelatedFor is the LLM grouping pass the settings ask for, or nil.
func RelatedFor(cfg *supatree.Config, s Settings, logf func(string, ...any)) Related {
	if !s.LLMGrouping {
		return nil
	}
	return llmGrouper(cfg, s, logf)
}

func (r *Reviewer) snapshot(cfg *supatree.Config, s Settings, cache *github.Cache) (Snapshot, error) {
	snap := Snapshot{
		Now:     time.Now(),
		PRs:     prsFromCache(cache.Watched(s.Login), s.Login, indexRepos(cfg, s)),
		Trees:   map[string]*Tree{},
		Running: map[string]bool{},
	}
	insts, err := supatree.List(cfg)
	if err != nil {
		return snap, err
	}
	for _, inst := range insts {
		if !inst.Reviewing() {
			continue
		}
		meta, err := supatree.LoadMeta(inst.Root)
		if err != nil || meta.Owner != supatree.OwnerReviewer {
			continue
		}
		t := &Tree{Name: inst.Name, Stack: inst.Stack, PRs: map[string]string{}}
		for alias, ref := range meta.Review {
			t.PRs[alias] = PRKey(ref.Repo, ref.Number)
		}
		snap.Trees[inst.Name] = t
	}
	r.mu.Lock()
	for name, on := range r.running {
		snap.Running[name] = on
	}
	r.mu.Unlock()
	// A run a previous watcher started (it restarted mid-run) is still
	// running as far as the trees are concerned.
	for _, p := range supatree.ListProcs() {
		if p.Headless && p.Agent == AgentName && snap.Trees[p.Tree] != nil {
			snap.Running[p.Tree] = true
		}
	}
	return snap, nil
}

// checkIdentity confirms gh is authenticated as login, once per process:
// reviews are posted through gh, and a reviewer posting as someone else would
// both misattribute them and never see its own requests answered.
func (r *Reviewer) checkIdentity(login string) error {
	if strings.EqualFold(r.login, login) {
		return nil
	}
	got, err := ghLogin()
	if err != nil {
		return fmt.Errorf("cannot tell which account gh posts as: %w", err)
	}
	if !strings.EqualFold(got, login) {
		return fmt.Errorf("gh is authenticated as %s, but reviewer.login is %s — reviews would be posted as the wrong account", got, login)
	}
	r.login = login
	return nil
}

var ghLogin = func() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "api", "user", "--jq", ".login").Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		return "", fmt.Errorf("gh api user: %s", strings.TrimSpace(string(exit.Stderr)))
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// memoRelated caches the LLM's answer per set of pull requests and their
// update times: while a group waits out its window the same question comes
// up every round, and the answer only changes when the pull requests do.
func (r *Reviewer) memoRelated(ask Related) Related {
	return func(prs []*PR) [][]string {
		var b strings.Builder
		for _, p := range prs {
			fmt.Fprintf(&b, "%s@%d;", p.Key, p.UpdatedAt.Unix())
		}
		key := b.String()
		r.mu.Lock()
		got, ok := r.related[key]
		r.mu.Unlock()
		if ok {
			return got
		}
		got = ask(prs)
		r.mu.Lock()
		if r.related == nil || len(r.related) > 256 {
			r.related = map[string][][]string{}
		}
		r.related[key] = got
		r.mu.Unlock()
		return got
	}
}

func (r *Reviewer) create(cfg *supatree.Config, s Settings, snap Snapshot, a Action) {
	refs := refsFor(snap, a.PRs)
	known, _ := supatree.KnownTreeNames(cfg)
	name := treeName(snap, a.PRs, sortedKeys(known))
	r.Lock.Lock()
	inst, _, err := supatree.NewReview(cfg, supatree.ReviewOptions{
		Stack:  a.Stack,
		Name:   name,
		Model:  s.Model,
		Intent: "automated review of " + strings.Join(a.PRs, ", "),
		PRs:    refs,
		Now:    snap.Now,
	})
	if err == nil {
		err = claim(inst.Root)
	}
	r.Lock.Unlock()
	if err != nil {
		r.logf("reviewer: create a tree for %s: %v", strings.Join(a.PRs, ", "), err)
		r.failed(s, a.PRs, "", fmt.Errorf("create review tree: %w", err))
		return
	}
	r.logf("reviewer: created %s for %s", inst.Name, strings.Join(a.PRs, ", "))
	_ = MutateState(func(st *State) error {
		st.Trees[inst.Name] = &TreeState{CreatedAt: snap.Now}
		for _, k := range a.PRs {
			st.pr(k).Tree = inst.Name
		}
		return nil
	})
}

// claim marks a new tree as the reviewer's, and lets it post: posting is
// what it is for, whatever review_outward says for review trees made by hand.
func claim(root string) error {
	meta, err := supatree.LoadMeta(root)
	if err != nil {
		return err
	}
	meta.Owner = supatree.OwnerReviewer
	on := true
	meta.Outward = &on
	return meta.Save(root)
}

func (r *Reviewer) add(cfg *supatree.Config, s Settings, snap Snapshot, a Action) {
	r.Lock.Lock()
	// vet: the tree's own spec is its agent's to write, and the watcher is
	// about to clone and check out what it names.
	res, err := supatree.AddReview(cfg, a.Tree, refsFor(snap, a.PRs), AgentName, true)
	r.Lock.Unlock()
	if err != nil {
		r.logf("reviewer: add %s to %s: %v", strings.Join(a.PRs, ", "), a.Tree, err)
		r.failed(s, a.PRs, a.Tree, fmt.Errorf("add to review tree: %w", err))
		return
	}
	var skipped []string
	for _, ar := range res {
		if ar.Skipped != "" {
			skipped = append(skipped, ar.PR+": "+ar.Skipped)
		}
	}
	if len(skipped) > 0 {
		r.logf("reviewer: %s: not added: %s", a.Tree, strings.Join(skipped, "; "))
	}
	_ = MutateState(func(st *State) error {
		for _, k := range a.PRs {
			st.pr(k).Tree = a.Tree
		}
		return nil
	})
}

func (r *Reviewer) reap(cfg *supatree.Config, a Action) {
	r.Lock.Lock()
	_, err := supatree.Remove(cfg, a.Tree, supatree.RemoveOptions{Force: true})
	r.Lock.Unlock()
	if err != nil {
		r.logf("reviewer: remove %s: %v", a.Tree, err)
		return
	}
	r.logf("reviewer: removed %s: %s", a.Tree, a.Reason)
	_ = MutateState(func(st *State) error {
		delete(st.Trees, a.Tree)
		return nil
	})
}

func (r *Reviewer) start(ctx context.Context, cfg *supatree.Config, s Settings, snap Snapshot, a Action) {
	r.mu.Lock()
	if r.running == nil {
		r.running = map[string]bool{}
	}
	if r.running[a.Tree] {
		r.mu.Unlock()
		return
	}
	r.running[a.Tree] = true
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			delete(r.running, a.Tree)
			r.mu.Unlock()
		}()
		r.run(ctx, cfg, s, snap, a)
	}()
}

// run reviews a.PRs in a.Tree: move the tree onto the current heads, brief
// the agent, run it, and confirm on GitHub what it posted.
func (r *Reviewer) run(ctx context.Context, cfg *supatree.Config, s Settings, snap Snapshot, a Action) {
	started := time.Now()
	r.logf("reviewer: reviewing %s in %s", strings.Join(a.PRs, ", "), a.Tree)
	if _, err := supatree.RefreshReview(cfg, a.Tree); err != nil {
		r.failed(s, a.PRs, a.Tree, fmt.Errorf("refresh review tree: %w", err))
		return
	}
	inst, err := supatree.Get(cfg, a.Tree)
	if err != nil {
		r.failed(s, a.PRs, a.Tree, err)
		return
	}
	st, err := LoadState()
	if err != nil {
		r.failed(s, a.PRs, a.Tree, err)
		return
	}
	// Whatever is left in the mailbox is from a run that did not finish, or
	// AddReview's note; this brief supersedes all of it.
	_, _ = supatree.Drain(inst.Root, AgentName)
	if err := supatree.Deliver(inst.Root, AgentName, AgentName, briefFor(inst, s.Login, a.PRs, st)); err != nil {
		r.failed(s, a.PRs, a.Tree, err)
		return
	}
	res, err := supatree.RunHeadless(ctx, inst, cfg, supatree.HeadlessOptions{Agent: AgentName, Model: s.Model, Timeout: s.Timeout})
	if ctx.Err() != nil {
		// The watcher is stopping; the run did not fail, it was cut short.
		// The request is still waiting and the next watcher picks it up.
		return
	}
	if err != nil {
		r.failed(s, a.PRs, a.Tree, fmt.Errorf("run agent: %w", err))
		return
	}
	r.logf("reviewer: %s: agent exited %d after %s (log %s)", a.Tree, res.ExitCode, res.Duration.Round(time.Second), res.Log)

	// What the agent says it did is not evidence; what GitHub has is.
	posted := map[string]github.PostedReview{}
	var missing []string
	for _, k := range a.PRs {
		p := snap.PRs[k]
		rev, ok, err := latestReview(p, s.Login, started)
		switch {
		case err != nil:
			r.logf("reviewer: %s: confirm review of %s: %v", a.Tree, k, err)
			missing = append(missing, k)
		case !ok:
			missing = append(missing, k)
		default:
			posted[k] = rev
		}
	}
	now := time.Now()
	_ = MutateState(func(st *State) error {
		ts := st.Trees[a.Tree]
		if ts == nil {
			ts = &TreeState{CreatedAt: started}
			st.Trees[a.Tree] = ts
		}
		ts.LastRunAt, ts.Runs = now, ts.Runs+1
		for k, rev := range posted {
			ps := st.pr(k)
			ps.ReviewedAt, ps.Verdict, ps.ReviewURL = rev.SubmittedAt, rev.State, rev.URL
			if validSHA.MatchString(rev.CommitID) {
				ps.ReviewedHead = rev.CommitID
			}
			ps.WaitingSince, ps.Attempts, ps.LastErr, ps.GaveUpAt = time.Time{}, 0, "", time.Time{}
		}
		return nil
	})
	for k, rev := range posted {
		p := snap.PRs[k]
		_ = supatree.AppendNotification(supatree.Event{
			Kind: supatree.EventReviewPosted, Tree: a.Tree, Mode: supatree.ModeReviewing,
			Member: p.Alias, PR: p.Number,
			Text: fmt.Sprintf("@%s %s %s#%d", s.Login, strings.ToLower(strings.ReplaceAll(rev.State, "_", " ")), p.Repo, p.Number),
		})
	}
	if len(missing) > 0 {
		why := fmt.Errorf("the run ended without a review posted (exit %d, timed out: %v, log %s)", res.ExitCode, res.TimedOut, res.Log)
		r.failed(s, missing, a.Tree, why)
	}
}

var validSHA = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// clockSkew allows for GitHub's clock and ours disagreeing about when the
// run started.
const clockSkew = 2 * time.Minute

// latestReview finds the newest review login submitted on p since the run
// started.
func latestReview(p *PR, login string, since time.Time) (github.PostedReview, bool, error) {
	owner, name, ok := strings.Cut(p.Repo, "/")
	if !ok {
		return github.PostedReview{}, false, fmt.Errorf("bad repository %q", p.Repo)
	}
	revs, err := reviewsBy(github.RepoRef{Owner: owner, Name: name}, p.Number, login)
	if err != nil {
		return github.PostedReview{}, false, err
	}
	var best github.PostedReview
	found := false
	for _, rev := range revs {
		if rev.SubmittedAt.Before(since.Add(-clockSkew)) {
			continue
		}
		if !found || rev.SubmittedAt.After(best.SubmittedAt) {
			best, found = rev, true
		}
	}
	return best, found, nil
}

var reviewsBy = github.ReviewsBy

// failed records a failed attempt on each of keys, giving up on those that
// have run out of attempts.
func (r *Reviewer) failed(s Settings, keys []string, tree string, cause error) {
	now := time.Now()
	var gaveUp []string
	_ = MutateState(func(st *State) error {
		for _, k := range keys {
			ps := st.pr(k)
			ps.Attempts++
			ps.LastAttempt = now
			ps.LastErr = cause.Error()
			if ps.Attempts >= s.MaxAttempts && ps.GaveUpAt.IsZero() {
				ps.GaveUpAt = now
				gaveUp = append(gaveUp, k)
			}
		}
		return nil
	})
	r.logf("reviewer: %s: %v", strings.Join(keys, ", "), cause)
	for _, k := range gaveUp {
		_ = supatree.AppendNotification(supatree.Event{
			Kind: supatree.EventReviewFailed, Tree: tree, Mode: supatree.ModeReviewing,
			Text: fmt.Sprintf("gave up reviewing %s after %d attempts: %v", k, s.MaxAttempts, cause),
		})
	}
}

func refsFor(snap Snapshot, keys []string) []supatree.ReviewRef {
	refs := make([]supatree.ReviewRef, 0, len(keys))
	for _, k := range keys {
		if p := snap.PRs[k]; p != nil {
			refs = append(refs, p.Ref())
		}
	}
	return refs
}

// treeName names a new tree after its pull requests — rv-<alias>-<n>, from
// the first in alias order — so `supatree ls` says what each one is
// reviewing. A name already taken, or one git refuses, falls back to a city.
func treeName(snap Snapshot, keys []string, taken []string) string {
	ps := make([]*PR, 0, len(keys))
	for _, k := range keys {
		if p := snap.PRs[k]; p != nil {
			ps = append(ps, p)
		}
	}
	if len(ps) == 0 {
		return ""
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Alias < ps[j].Alias })
	name := fmt.Sprintf("rv-%s-%d", strings.ToLower(ps[0].Alias), ps[0].Number)
	if git.ValidateName(name, taken) != nil {
		return ""
	}
	return name
}
