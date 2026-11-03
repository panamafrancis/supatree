package reviewer

import (
	"encoding/json"
	"time"
)

// Preview is what a reviewer round would do, worked out without doing any
// of it: `supatree reviewer plan`.
type Preview struct {
	// Actions are what Plan decides now.
	Actions []Action
	// Groups are the changes the reviewer sees among the open pull requests,
	// whether or not anything is due for them yet.
	Groups []PreviewGroup
}

// PreviewGroup is one change as the reviewer has grouped it.
type PreviewGroup struct {
	PRs     []string
	Reasons map[string]string // why each joined, for those that joined one
	Tree    string            // the reviewer's tree holding any of them
	// Waiting are the pull requests in it that request a review now.
	Waiting []string
	// ReadyAt is when a tree would be made for it, for a group no tree holds
	// with a request waiting; zero otherwise.
	ReadyAt time.Time
}

// DryRun plans a round against a copy of st, so nothing it observes — not
// even when a request was first seen — is recorded. A request seen for the
// first time is therefore always "waiting out the window" in a dry run; its
// group's ReadyAt says when that would end.
func DryRun(snap Snapshot, st *State, s Settings, related Related) (Preview, error) {
	cp, err := copyState(st)
	if err != nil {
		return Preview{}, err
	}
	if related != nil {
		related = memoize(related) // Plan and the grouping below ask the same
	}
	p := Preview{Actions: Plan(snap, cp, s, related)}
	inTree := treeIndex(snap)
	g, _ := buildGroups(snap, cp, related, inTree)
	for _, comp := range g.components() {
		pg := PreviewGroup{PRs: comp, Reasons: map[string]string{}}
		var newest time.Time
		for _, k := range comp {
			if r := g.reason[k]; r != "" {
				pg.Reasons[k] = r
			}
			if t := inTree[k]; t != "" {
				pg.Tree = t
			}
			if ps := cp.PRs[k]; ps != nil && !ps.WaitingSince.IsZero() {
				pg.Waiting = append(pg.Waiting, k)
				if ps.WaitingSince.After(newest) {
					newest = ps.WaitingSince
				}
			}
		}
		if pg.Tree == "" && len(pg.Waiting) > 0 {
			pg.ReadyAt = newest.Add(s.GroupWindow)
		}
		p.Groups = append(p.Groups, pg)
	}
	return p, nil
}

func memoize(ask Related) Related {
	seen := map[string][][]string{}
	return func(prs []*PR, inReview map[string]string) [][]string {
		key := ""
		for _, p := range prs {
			key += p.Key + ":" + inReview[p.Key] + ";"
		}
		if got, ok := seen[key]; ok {
			return got
		}
		got := ask(prs, inReview)
		seen[key] = got
		return got
	}
}

func copyState(st *State) (*State, error) {
	data, err := json.Marshal(st)
	if err != nil {
		return nil, err
	}
	cp := &State{}
	if err := json.Unmarshal(data, cp); err != nil {
		return nil, err
	}
	if cp.PRs == nil {
		cp.PRs = map[string]*PRState{}
	}
	if cp.Trees == nil {
		cp.Trees = map[string]*TreeState{}
	}
	return cp, nil
}
