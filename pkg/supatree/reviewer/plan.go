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
	// ActCreate makes a review tree for PRs, all in Stack.
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
	Kind   ActionKind
	Tree   string
	Stack  string
	PRs    []string
	Reason string
}

// Related proposes groups among pull requests the deterministic evidence left
// alone (the LLM pass). Plan checks every proposal against the same
// constraint as any other union. Nil means no such pass.
type Related func(prs []*PR) [][]string

// Plan decides what to do this round. It records what it observed in st —
// when a request was first seen, why pull requests were grouped — and returns
// the actions; it performs none of them, so it is pure over its inputs.
func Plan(snap Snapshot, st *State, s Settings, related Related) []Action {
	now := snap.Now
	observe(snap, st, s.Login)

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

	var actions []Action
	actions = append(actions, planGroups(snap, st, s, related, inTree)...)

	// Runs, oldest-waiting tree first, within the concurrency cap.
	running := 0
	for _, r := range snap.Running {
		if r {
			running++
		}
	}
	type due struct {
		tree  string
		prs   []string
		since time.Time
	}
	var dues []due
	for _, name := range sortedKeys(snap.Trees) {
		if snap.Running[name] {
			continue
		}
		var d due
		for _, k := range sortedValues(snap.Trees[name].PRs) {
			if snap.PRs[k] != nil && eligible(st.PRs[k], now) {
				d.prs = append(d.prs, k)
				if ws := st.PRs[k].WaitingSince; d.since.IsZero() || ws.Before(d.since) {
					d.since = ws
				}
			}
		}
		if len(d.prs) > 0 {
			d.tree = name
			dues = append(dues, d)
		}
	}
	sort.SliceStable(dues, func(i, j int) bool { return dues[i].since.Before(dues[j].since) })
	for _, d := range dues {
		if running >= s.MaxConcurrent {
			break
		}
		running++
		actions = append(actions, Action{Kind: ActRun, Tree: d.tree, PRs: d.prs})
	}

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
func observe(snap Snapshot, st *State, login string) {
	for _, k := range sortedKeys(snap.PRs) {
		p := snap.PRs[k]
		ps := st.PRs[k]
		if !waiting(p, ps, login) {
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

// waiting reports whether p is waiting on a review from login. GitHub removes
// a reviewer from requested_reviewers when their review lands and puts them
// back on a re-request, so "requested" is the whole signal — except that a
// listing read before the last review landed still shows the request that
// review answered. A listing no newer than that review is therefore not news.
func waiting(p *PR, ps *PRState, login string) bool {
	if !p.Open() || !p.Requested || strings.EqualFold(p.Author, login) {
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

// planGroups finds the waiting pull requests no tree holds yet, groups them
// with their siblings, and decides whether each group joins an existing tree
// or gets one of its own.
func planGroups(snap Snapshot, st *State, s Settings, related Related, inTree map[string]string) []Action {
	g, open := buildGroups(snap, st, related, inTree)
	// A tree is made for, or joined by, a pull request that may run now: one
	// whose last create failed waits out the same backoff a failed run does.
	isWaiting := func(k string) bool { return eligible(st.PRs[k], snap.Now) }

	var actions []Action
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
			adds, rest := fitTree(snap.Trees[name], waitingNew, open)
			if len(adds) > 0 {
				actions = append(actions, Action{Kind: ActAdd, Tree: name, PRs: adds, Reason: "sibling of a change already in review"})
			}
			if len(rest) == 0 {
				continue
			}
			unassigned = rest
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
		actions = append(actions, Action{Kind: ActCreate, Stack: open[unassigned[0]].Stack, PRs: unassigned})
	}
	return actions
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
		applyRelated(g, open, related, func(k string) bool { return eligible(st.PRs[k], snap.Now) && inTree[k] == "" })
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
// left alone — waiting, in no tree, and the only one in their group — when
// one author has several of them in different repositories, and unions what
// it proposes, under the usual constraint.
func applyRelated(g *groups, open map[string]*PR, related Related, candidate func(string) bool) {
	sizes := map[string]int{}
	for k := range open {
		sizes[g.find(k)]++
	}
	byAuthor := map[string][]*PR{}
	for _, k := range sortedKeys(open) {
		if candidate(k) && sizes[g.find(k)] == 1 && open[k].Author != "" {
			a := strings.ToLower(open[k].Author)
			byAuthor[a] = append(byAuthor[a], open[k])
		}
	}
	for _, a := range sortedKeys(byAuthor) {
		prs := byAuthor[a]
		repos := map[string]bool{}
		for _, p := range prs {
			repos[strings.ToLower(p.Repo)] = true
		}
		if len(repos) < 2 {
			continue
		}
		for _, grp := range related(prs) {
			for _, k := range grp[1:] {
				if open[k] != nil && open[grp[0]] != nil && sameAuthor(open[k], open[grp[0]]) {
					g.union(grp[0], k, ReasonLLM)
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
