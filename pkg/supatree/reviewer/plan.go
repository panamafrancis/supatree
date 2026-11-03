package reviewer

import (
	"sort"
	"strings"
	"time"

	"github.com/panamafrancis/workbench/pkg/github"
)

// Tree is one of the reviewer's review trees, as it is on disk.
type Tree struct {
	Name  string
	Stack string
	PRs   map[string]string // member alias → PR key
}

// Snapshot is everything Plan decides from.
type Snapshot struct {
	Now     time.Time
	PRs     map[string]*PR   // every watched pull request, by key
	Trees   map[string]*Tree // the reviewer's trees, by name
	Running map[string]bool  // trees with a review run in flight
}

// ActionKind is what an Action does.
type ActionKind string

const (
	// ActCreate makes a review tree for PRs, all in Stack, then reviews Run
	// in it.
	ActCreate ActionKind = "create"
	// ActAdd adds PRs to the existing Tree.
	ActAdd ActionKind = "add"
	// ActRun reviews PRs — the ones waiting on the reviewer — in Tree.
	ActRun ActionKind = "run"
	// ActReap removes Tree.
	ActReap ActionKind = "reap"
)

// Action is one thing Plan decided to do.
type Action struct {
	Kind  ActionKind
	Tree  string
	Stack string
	PRs   []string
	// Run is, for ActCreate, the pull requests to review once the tree is
	// made: those of PRs that are waiting and due.
	Run    []string
	Reason string
}

// Related proposes groups among pull requests the deterministic evidence left
// alone (the LLM pass). inReview maps those of prs already in one of the
// reviewer's trees to it: they are there so a late sibling can join their
// change, not to be regrouped. Plan checks every proposal against the same
// constraint as any other union. Nil means no such pass.
type Related func(prs []*PR, inReview map[string]string) [][]string

// Plan decides what to do this round. It records what it observed in st —
// when a request was first seen, why pull requests were grouped — and returns
// the actions; it performs none of them, so it is pure over its inputs.
func Plan(snap Snapshot, st *State, s Settings, related Related) []Action {
	now := snap.Now
	observe(snap, st, s)

	inTree := treeIndex(snap)
	for name := range st.Trees {
		if snap.Trees[name] == nil {
			delete(st.Trees, name)
		}
	}
	for k := range st.PRs {
		if snap.PRs[k] == nil && inTree[k] == "" {
			delete(st.PRs, k)
		}
	}

	adds, creates := planGroups(snap, st, s, related, inTree)
	actions := adds

	// Runs and new trees share the concurrency cap, oldest-waiting first: a
	// tree is made only when its review can start as soon as it exists, so a
	// backlog does not become a tree per pull request before anything is
	// reviewed. The executor starts each new tree's run right after creating
	// it (Action.Run).
	running := 0
	for _, r := range snap.Running {
		if r {
			running++
		}
	}
	var dues []slotted
	for _, name := range sortedKeys(snap.Trees) {
		if snap.Running[name] {
			continue
		}
		d := slotted{Action: Action{Kind: ActRun, Tree: name}}
		for _, k := range sortedValues(snap.Trees[name].PRs) {
			if snap.PRs[k] != nil && eligible(st.PRs[k], now) {
				d.PRs = append(d.PRs, k)
				if ws := st.PRs[k].WaitingSince; d.since.IsZero() || ws.Before(d.since) {
					d.since = ws
				}
			}
		}
		if len(d.PRs) > 0 {
			dues = append(dues, d)
		}
	}
	dues = append(dues, creates...)
	sort.SliceStable(dues, func(i, j int) bool { return dues[i].since.Before(dues[j].since) })
	var runs, made []Action
	for _, d := range dues {
		if running >= s.MaxConcurrent {
			break
		}
		running++
		if d.Kind == ActRun {
			runs = append(runs, d.Action)
		} else {
			made = append(made, d.Action)
		}
	}
	// Runs first: they start at once, where each create takes a while.
	actions = append(actions, runs...)
	actions = append(actions, made...)

	// Reaping.
	for _, name := range sortedKeys(snap.Trees) {
		if snap.Running[name] {
			continue
		}
		if reason := reapReason(snap, st, s, name, now); reason != "" {
			actions = append(actions, Action{Kind: ActReap, Tree: name, Reason: reason})
		}
	}
	return actions
}

// observe updates each pull request's waiting state from the snapshot.
func observe(snap Snapshot, st *State, s Settings) {
	for _, k := range sortedKeys(snap.PRs) {
		p := snap.PRs[k]
		ps := st.PRs[k]
		if !waiting(p, ps, s) {
			if ps != nil {
				ps.WaitingSince = time.Time{}
				ps.Attempts = 0
				ps.GaveUpAt = time.Time{}
			}
			continue
		}
		ps = st.pr(k)
		if ps.WaitingSince.IsZero() {
			ps.WaitingSince = snap.Now
			ps.Attempts = 0
		}
		// The pull request moved since the reviewer gave up on it: try again.
		if !ps.GaveUpAt.IsZero() && p.UpdatedAt.After(ps.GaveUpAt) {
			ps.GaveUpAt = time.Time{}
			ps.Attempts = 0
		}
	}
}

// waiting reports whether p is waiting on a review from s.Login. GitHub
// removes a reviewer from requested_reviewers when their review lands and
// puts them back on a re-request, so "requested" is the whole signal — except
// that a listing read before the last review landed still shows the request
// that review answered. A listing no newer than that review is therefore not
// news. An ignored author's, or one older than s.Since, never waits.
func waiting(p *PR, ps *PRState, s Settings) bool {
	if !p.Open() || !p.Requested || strings.EqualFold(p.Author, s.Login) || s.ignores(p.Author) {
		return false
	}
	if !s.Since.IsZero() && p.UpdatedAt.Before(s.Since) {
		return false
	}
	return ps == nil || p.UpdatedAt.After(ps.ReviewedAt)
}

// eligible reports whether a waiting pull request may be run now: not given
// up on, and past the backoff its failed attempts earned.
func eligible(ps *PRState, now time.Time) bool {
	if ps == nil || ps.WaitingSince.IsZero() || !ps.GaveUpAt.IsZero() {
		return false
	}
	return !now.Before(ps.LastAttempt.Add(backoff(ps.Attempts)))
}

// backoff is the wait after attempts runs that posted nothing: 10m, 20m, 40m…
// up to two hours.
func backoff(attempts int) time.Duration {
	if attempts <= 0 {
		return 0
	}
	d := 10 * time.Minute << (attempts - 1)
	return min(d, 2*time.Hour)
}

// slotted is a run, or a create with its run, waiting for a slot under the
// concurrency cap; since is when its oldest pull request started waiting.
type slotted struct {
	Action
	since time.Time
}

// planGroups finds the waiting pull requests no tree holds yet, groups them
// with their siblings, and decides whether each group joins an existing tree
// or gets one of its own. Adds are returned to do now; creates, to do when a
// slot is free.
func planGroups(snap Snapshot, st *State, s Settings, related Related, inTree map[string]string) (adds []Action, creates []slotted) {
	g, open := buildGroups(snap, st, related, inTree)
	// A tree is made for, or joined by, a pull request that may run now: one
	// whose last create failed waits out the same backoff a failed run does.
	isWaiting := func(k string) bool { return eligible(st.PRs[k], snap.Now) }

	for _, comp := range g.components() {
		var unassigned, waitingNew []string
		trees := map[string]bool{}
		for _, k := range comp {
			if t := inTree[k]; t != "" {
				trees[t] = true
				continue
			}
			unassigned = append(unassigned, k)
			if isWaiting(k) {
				waitingNew = append(waitingNew, k)
			}
		}
		if len(waitingNew) == 0 {
			continue
		}
		for _, k := range unassigned {
			if r := g.reason[k]; r != "" {
				st.pr(k).Grouping = r
			}
		}
		if len(trees) == 1 {
			var name string
			for t := range trees {
				name = t
			}
			fit, rest := fitTree(snap.Trees[name], waitingNew, open)
			if len(fit) > 0 {
				adds = append(adds, Action{Kind: ActAdd, Tree: name, PRs: fit, Reason: "sibling of a change already in review"})
			}
			if len(rest) == 0 {
				continue
			}
			unassigned, waitingNew = rest, rest
		}
		// A group of its own. Wait out the window from the newest request, so
		// siblings opened a minute later are in the tree from the start.
		var newest time.Time
		for _, k := range unassigned {
			if ps := st.PRs[k]; ps != nil && ps.WaitingSince.After(newest) {
				newest = ps.WaitingSince
			}
		}
		if snap.Now.Before(newest.Add(s.GroupWindow)) {
			continue
		}
		c := slotted{Action: Action{Kind: ActCreate, Stack: open[unassigned[0]].Stack, PRs: unassigned, Run: waitingNew}}
		for _, k := range waitingNew {
			if ws := st.PRs[k].WaitingSince; c.since.IsZero() || ws.Before(c.since) {
				c.since = ws
			}
		}
		creates = append(creates, c)
	}
	return adds, creates
}

// buildGroups groups the open pull requests: the deterministic evidence,
// then related's proposals for the waiting ones no tree holds.
func buildGroups(snap Snapshot, st *State, related Related, inTree map[string]string) (*groups, map[string]*PR) {
	open := map[string]*PR{}
	for k, p := range snap.PRs {
		if p.Open() {
			open[k] = p
		}
	}
	g := groupDeterministic(open)
	if related != nil {
		applyRelated(g, open, related, inTree, func(k string) bool { return eligible(st.PRs[k], snap.Now) && inTree[k] == "" })
	}
	return g, open
}

// treeIndex maps each pull request in one of the snapshot's trees to it.
func treeIndex(snap Snapshot) map[string]string {
	inTree := map[string]string{}
	for _, name := range sortedKeys(snap.Trees) {
		for _, k := range snap.Trees[name].PRs {
			inTree[k] = name
		}
	}
	return inTree
}

// fitTree splits ks into those tree has room for — its member for their
// repository is free — and the rest.
func fitTree(tree *Tree, ks []string, prs map[string]*PR) (fit, rest []string) {
	for _, k := range ks {
		if held, taken := tree.PRs[prs[k].Alias]; taken && held != k {
			rest = append(rest, k)
			continue
		}
		fit = append(fit, k)
	}
	return fit, rest
}

// applyRelated asks related about the pull requests the deterministic pass
// left alone — waiting, in no tree, and the only one in their group — and
// unions what it proposes, under the usual constraint. It asks per author,
// when that author has such a pull request and something in another
// repository it could belong with: another lone one, or one of theirs
// already under review, so a sibling opened after its change's tree was made
// can still join it. Pull requests under review are only something to join:
// a proposal is used only for the lone ones in it, and joins at most one
// pull request under review.
func applyRelated(g *groups, open map[string]*PR, related Related, inTree map[string]string, candidate func(string) bool) {
	sizes := map[string]int{}
	for k := range open {
		sizes[g.find(k)]++
	}
	lone := map[string]bool{}
	byAuthor := map[string][]*PR{}
	for _, k := range sortedKeys(open) {
		if open[k].Author == "" || isBot(open[k].Author) {
			continue
		}
		if candidate(k) && sizes[g.find(k)] == 1 {
			lone[k] = true
		} else if inTree[k] == "" {
			continue
		}
		a := strings.ToLower(open[k].Author)
		byAuthor[a] = append(byAuthor[a], open[k])
	}
	for _, a := range sortedKeys(byAuthor) {
		prs := byAuthor[a]
		repos := map[string]bool{}
		inReview := map[string]string{}
		asks := false
		for _, p := range prs {
			repos[strings.ToLower(p.Repo)] = true
			if lone[p.Key] {
				asks = true
			} else {
				inReview[p.Key] = inTree[p.Key]
			}
		}
		if !asks || len(repos) < 2 {
			continue
		}
		for _, grp := range related(prs, inReview) {
			var join []string
			anchor := ""
			for _, k := range grp {
				switch {
				case open[k] == nil:
				case lone[k]:
					join = append(join, k)
				case inReview[k] != "" && anchor == "":
					anchor = k
				}
			}
			if anchor != "" {
				join = append(join, anchor)
			}
			for _, k := range join[min(1, len(join)):] {
				if sameAuthor(open[k], open[join[0]]) {
					g.union(join[0], k, ReasonLLM)
				}
			}
		}
	}
}

// reapReason says why tree should go, or "" when it stays: every pull request
// in it has merged or closed, or nothing has waited on it for TreeTTL.
func reapReason(snap Snapshot, st *State, s Settings, name string, now time.Time) string {
	tree := snap.Trees[name]
	done := true
	for _, k := range tree.PRs {
		// One the cache no longer holds is not proof of anything — a cache
		// rebuilt from scratch holds none — so it keeps the tree until TTL.
		if p := snap.PRs[k]; p == nil || !terminal(p) {
			done = false
		}
		if ps := st.PRs[k]; ps != nil && !ps.WaitingSince.IsZero() {
			return ""
		}
	}
	if done {
		return "every pull request in it has merged or closed"
	}
	last := time.Time{}
	if ts := st.Trees[name]; ts != nil {
		last = ts.CreatedAt
		if ts.LastRunAt.After(last) {
			last = ts.LastRunAt
		}
	}
	if !last.IsZero() && now.Sub(last) > s.TreeTTL {
		return "nothing has asked for a review in it for " + s.TreeTTL.String()
	}
	return ""
}

func terminal(p *PR) bool {
	return p.Status == github.PRMerged || p.Status == github.PRClosed
}

func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
