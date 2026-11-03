package supatree

import (
	"errors"
	"sort"
	"time"

	"github.com/panamafrancis/workbench/pkg/github"
)

// PRStaleAge bounds how old a cached PR status may be before a charged lookup
// is allowed for it. Shared by every supatree surface that talks to GitHub (the
// sidebar, the dashboard, the watcher, the MCP tools): a second poller with its
// own numbers is exactly how the shared quota drains. Repo polls ignore it —
// they are conditional and free when nothing changed.
const PRStaleAge = 10 * time.Minute

// FetchTargets lists every checked-out member branch for a fetch round. Which
// of them actually costs a request is github.Sync's decision, not the caller's:
// it polls each repo once, skips unpushed branches and fresh entries, and backs
// off repos gh cannot see.
//
// A review member is pinned to its PR, and keyed by it: its branch is
// tree-local, so it has no origin ref and no PR of its own *by design*, and
// only the number finds the PR it is reviewing.
func FetchTargets(insts []*Instance) []github.Target {
	var targets []github.Target
	for _, inst := range insts {
		for i := range inst.Members {
			if inst.Members[i].Exists {
				targets = append(targets, MemberTarget(&inst.Members[i]))
			}
		}
	}
	return targets
}

// WatchTargetsFor are the fetch targets that keep login's review requests in
// the cache: one per repository of stacks that supatree has a clone of.
func WatchTargetsFor(c *Config, login string, stacks []string) []github.Target {
	var targets []github.Target
	seen := map[string]bool{}
	for _, stack := range stacks {
		repos, err := c.StackRepos(stack)
		if err != nil {
			continue
		}
		for repo, sr := range repos {
			if sr.Clone == "" || seen[repo] {
				continue
			}
			seen[repo] = true
			targets = append(targets, github.Target{RepoPath: sr.Clone, Watch: login})
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].RepoPath < targets[j].RepoPath })
	return targets
}

// reviewerWatchTargets are the enabled reviewer's watch targets, or none.
func reviewerWatchTargets(c *Config) []github.Target {
	r := c.Reviewer
	if r == nil || !r.Enabled || r.Login == "" {
		return nil
	}
	stacks := r.Stacks
	if len(stacks) == 0 {
		stacks = c.StackAliases()
	}
	return WatchTargetsFor(c, r.Login, stacks)
}

// withReviewerWatch adds the reviewer's watch targets to a round's, unless
// the round has them already. Every process's round carries them, not only
// the watcher's: the round goes to whichever process takes the cache lock
// first, and one that polled a repo without them would consume its change —
// the next poll a 304 — while the watcher, ceding round after round to the
// sidebars, never saw a re-request at all.
func withReviewerWatch(targets []github.Target, c *Config) []github.Target {
	have := map[github.Target]bool{}
	for _, t := range targets {
		if t.Watch != "" {
			have[t] = true
		}
	}
	for _, t := range reviewerWatchTargetsFn(c) {
		if !have[t] {
			targets = append(targets, t)
		}
	}
	return targets
}

var reviewerWatchTargetsFn = reviewerWatchTargets

// FetchOutcome reports what a fetch round did. Skipped means another process
// owned the round (the cache lock was busy) and this one ceded — distinct from
// an error, and from a successful but empty round. Deferred counts lookups held
// back to stay above the shared rate-limit reserve, so a missing status has a
// reason to show.
type FetchOutcome struct {
	Skipped  bool
	Err      error
	Deferred int
}

// FetchPRs brings the cache up to date for targets. The round runs inside
// Cache.TryMutate: with one sidebar per Zellij tab plus a dashboard and a
// watcher all polling independently, only the process that wins the cache lock
// fetches each round while the rest cede and pick up what it writes. Because
// the mutation applies to the cache as it is on disk, a round can neither lose
// a peer's statuses nor disarm a cooldown a peer armed.
//
// force re-polls unconditionally and ignores staleness, for a person waiting
// on an explicit refresh; it still blocks on the lock rather than ceding.
func FetchPRs(targets []github.Target, cache *github.Cache, force bool) FetchOutcome {
	return fetchPRs(targets, cache, force, force)
}

// FetchPRsWaiting is FetchPRs waiting for the cache lock rather than ceding,
// without forcing a re-read: for a process whose own targets have gone
// unfetched too long while others won every round.
func FetchPRsWaiting(targets []github.Target, cache *github.Cache) FetchOutcome {
	return fetchPRs(targets, cache, false, true)
}

func fetchPRs(targets []github.Target, cache *github.Cache, force, wait bool) FetchOutcome {
	if c, err := Load(); err == nil {
		targets = withReviewerWatch(targets, c)
	}
	if len(targets) == 0 {
		return FetchOutcome{}
	}
	var report github.SyncReport
	round := func(w *github.Writable) error {
		report = github.Sync(w, targets, github.SyncOptions{MaxAge: PRStaleAge, Force: force})
		return nil
	}
	var err error
	if wait {
		err = cache.Mutate(round)
	} else {
		err = cache.TryMutate(round)
	}
	if errors.Is(err, github.ErrLockBusy) {
		return FetchOutcome{Skipped: true}
	}
	if err != nil {
		return FetchOutcome{Err: err}
	}
	if report.Paused && report.Err == nil {
		// The core bucket is cooling down; report it the way a fresh rate-limit
		// response would, so every surface shows the same hint.
		return FetchOutcome{Err: github.ErrGHRateLimited, Deferred: report.Deferred}
	}
	return FetchOutcome{Err: report.Err, Deferred: report.Deferred}
}

// MemberTarget is the fetch target for one member: keyed by its cache key, and
// pinned to its PR when it is a review member.
func MemberTarget(m *Member) github.Target {
	t := github.Target{RepoPath: m.Path, Branch: m.Branch, Key: m.CacheKey()}
	if m.Review != nil {
		t.Ref = github.PRRef{Number: m.Review.Number, URL: m.Review.URL}
	}
	return t
}
