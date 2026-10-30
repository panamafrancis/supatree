package reviewer

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Grouping decides which pull requests are one change. It is a union-find
// over evidence, strongest first, under one constraint that every union must
// keep: a group holds at most one pull request per repository, all in one
// stack — exactly what a review tree can hold (mapPRsToMembers). A union that
// would break it is refused, so a wrong guess splits a change in two, which
// costs a second review, rather than merging two changes, which reviews one
// in the light of the other.

// ChangeMarker is the block `create_prs` writes into every pull request of a
// change, naming the change. It is HTML comments, so GitHub renders nothing.
var changeMarker = regexp.MustCompile(`<!--\s*supatree:change\s+id=([A-Za-z0-9._/-]{1,80})\s*-->`)

// prURLRef and shortRef find references to other pull requests in a body.
var (
	prURLRef = regexp.MustCompile(`https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)/pull/([0-9]+)`)
	shortRef = regexp.MustCompile(`(?:^|[\s(\[])([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#([0-9]+)`)
)

// Reasons a union was made, recorded so `supatree reviewer status` can say
// why two pull requests were reviewed together.
const (
	ReasonMarker = "change marker"
	ReasonLink   = "linked from the description"
	ReasonBranch = "same author and branch"
	ReasonLLM    = "judged related"
)

type groups struct {
	parent map[string]string
	repos  map[string]map[string]bool // root → repositories in its group
	stack  map[string]string          // root → stack
	reason map[string]string          // key → why it joined its group
}

func newGroups(prs map[string]*PR) *groups {
	g := &groups{parent: map[string]string{}, repos: map[string]map[string]bool{}, stack: map[string]string{}, reason: map[string]string{}}
	for k, p := range prs {
		g.parent[k] = k
		g.repos[k] = map[string]bool{strings.ToLower(p.Repo): true}
		g.stack[k] = p.Stack
	}
	return g
}

func (g *groups) find(k string) string {
	for g.parent[k] != k {
		g.parent[k] = g.parent[g.parent[k]]
		k = g.parent[k]
	}
	return k
}

// union joins a's and b's groups, unless that would put two pull requests of
// one repository, or two stacks, in one group.
func (g *groups) union(a, b, reason string) {
	ra, rb := g.find(a), g.find(b)
	if ra == rb || g.stack[ra] != g.stack[rb] {
		return
	}
	for repo := range g.repos[rb] {
		if g.repos[ra][repo] {
			return
		}
	}
	// Deterministic root: the smaller key, so the same evidence always gives
	// the same groups whatever order the map iterates in.
	if rb < ra {
		ra, rb = rb, ra
	}
	g.parent[rb] = ra
	for repo := range g.repos[rb] {
		g.repos[ra][repo] = true
	}
	delete(g.repos, rb)
	for _, k := range []string{a, b} {
		if g.reason[k] == "" {
			g.reason[k] = reason
		}
	}
}

// components lists the groups, each sorted, ordered by their first key.
func (g *groups) components() [][]string {
	by := map[string][]string{}
	for k := range g.parent {
		r := g.find(k)
		by[r] = append(by[r], k)
	}
	out := make([][]string, 0, len(by))
	for _, ks := range by {
		sort.Strings(ks)
		out = append(out, ks)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// groupDeterministic unions prs on the evidence that needs no judgement: the
// change marker, a description linking another pull request, and the same
// author pushing the same branch name (or the same st/<slug>/ prefix) to
// several repositories. Only an author's own pull requests are ever joined.
func groupDeterministic(prs map[string]*PR) *groups {
	g := newGroups(prs)
	keys := sortedKeys(prs)

	// 1. The change marker.
	byMarker := map[string][]string{}
	for _, k := range keys {
		if m := changeMarker.FindStringSubmatch(prs[k].Body); m != nil {
			id := strings.ToLower(prs[k].Author) + "\x00" + m[1]
			byMarker[id] = append(byMarker[id], k)
		}
	}
	unionRuns(g, byMarker, ReasonMarker)

	// 2. A description naming another watched pull request by the same author.
	for _, k := range keys {
		for _, ref := range bodyRefs(prs[k].Body) {
			if other, ok := prs[ref]; ok && ref != k && sameAuthor(prs[k], other) {
				g.union(k, ref, ReasonLink)
			}
		}
	}

	// 3. The same author's same branch, or the same supatree slug.
	byBranch := map[string][]string{}
	for _, k := range keys {
		if b := branchKey(prs[k].HeadRef); b != "" {
			id := strings.ToLower(prs[k].Author) + "\x00" + b
			byBranch[id] = append(byBranch[id], k)
		}
	}
	unionRuns(g, byBranch, ReasonBranch)
	return g
}

func unionRuns(g *groups, runs map[string][]string, reason string) {
	ids := make([]string, 0, len(runs))
	for id := range runs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ks := runs[id]
		for _, k := range ks[1:] {
			g.union(ks[0], k, reason)
		}
	}
}

// branchKey is what two pull requests' head branches must share to count as
// one change: a supatree branch's slug, or any other branch's whole name. A
// default-ish branch name says nothing.
func branchKey(ref string) string {
	if rest, ok := strings.CutPrefix(ref, "st/"); ok {
		if slug, _, ok := strings.Cut(rest, "/"); ok && slug != "" {
			return "st/" + slug
		}
	}
	switch strings.ToLower(ref) {
	case "", "main", "master", "develop", "dev", "patch-1", "patch-2":
		return ""
	}
	return ref
}

// bodyRefs lists the pull request keys a description names.
func bodyRefs(body string) []string {
	var out []string
	for _, m := range prURLRef.FindAllStringSubmatch(body, -1) {
		if n, err := strconv.Atoi(m[2]); err == nil {
			out = append(out, PRKey(m[1], n))
		}
	}
	for _, m := range shortRef.FindAllStringSubmatch(body, -1) {
		if n, err := strconv.Atoi(m[2]); err == nil {
			out = append(out, PRKey(m[1], n))
		}
	}
	return out
}

func sameAuthor(a, b *PR) bool {
	return a.Author != "" && strings.EqualFold(a.Author, b.Author)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
