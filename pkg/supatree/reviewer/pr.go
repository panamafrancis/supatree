package reviewer

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/panamafrancis/supatree/pkg/supatree"
	"github.com/panamafrancis/workbench/pkg/github"
)

// PR is one watched pull request, as the cache last saw it.
type PR struct {
	Key       string // owner/name#number, the repository lowercased
	Repo      string // owner/name
	Number    int
	URL       string
	Title     string
	Author    string
	HeadRef   string
	BaseRef   string
	Head      string // head commit
	Body      string
	Status    github.PRStatus
	Requested bool // the reviewer is a requested reviewer right now
	UpdatedAt time.Time
	Stack     string // the watched stack whose member it is
	Alias     string // that member's alias
}

// Open reports whether the pull request can still be reviewed. A draft is
// open but not ready: it is picked up once it is marked ready.
func (p *PR) Open() bool { return p.Status == github.PROpen }

// Ref is the review tree reference for the pull request.
func (p *PR) Ref() supatree.ReviewRef {
	return supatree.ReviewRef{Repo: p.Repo, Number: p.Number, URL: p.URL, Head: p.Head, HeadRef: p.HeadRef, Base: p.BaseRef}
}

// PRKey is the key of a pull request: owner/name#number, the repository
// lowercased, since GitHub's names are case-insensitive.
func PRKey(repo string, number int) string {
	return strings.ToLower(repo) + "#" + strconv.Itoa(number)
}

// WatchTargets are the fetch targets that keep the reviewer's pull requests
// in the cache: one per watched repository that supatree has a clone of.
func WatchTargets(c *supatree.Config, s Settings) []github.Target {
	var targets []github.Target
	seen := map[string]bool{}
	for _, stack := range s.WatchedStacks(c) {
		repos, err := c.StackRepos(stack)
		if err != nil {
			continue
		}
		for repo, sr := range repos {
			if sr.Clone == "" || seen[repo] {
				continue
			}
			seen[repo] = true
			targets = append(targets, github.Target{RepoPath: sr.Clone, Watch: s.Login})
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].RepoPath < targets[j].RepoPath })
	return targets
}

// stackRepo is a watched repository with the stack it belongs to. A
// repository in several watched stacks belongs to the first, in the order the
// settings list them.
type stackRepo struct {
	stack string
	supatree.StackRepo
}

func indexRepos(c *supatree.Config, s Settings) map[string]stackRepo {
	idx := map[string]stackRepo{}
	for _, stack := range s.WatchedStacks(c) {
		repos, err := c.StackRepos(stack)
		if err != nil {
			continue
		}
		for repo, sr := range repos {
			if _, taken := idx[repo]; !taken {
				idx[repo] = stackRepo{stack: stack, StackRepo: sr}
			}
		}
	}
	return idx
}

// prsFromCache turns the cache's watched entries into PRs, keeping only those
// in a watched repository.
func prsFromCache(watched map[string]*github.PRInfo, login string, idx map[string]stackRepo) map[string]*PR {
	out := map[string]*PR{}
	for key, info := range watched {
		repo, number, ok := parseWatchKey(key)
		if !ok || info == nil {
			continue
		}
		sr, ok := idx[strings.ToLower(repo)]
		if !ok {
			continue
		}
		p := &PR{
			Key:       PRKey(repo, number),
			Repo:      sr.Repo,
			Number:    number,
			URL:       info.URL,
			Title:     info.Title,
			Author:    info.Author,
			HeadRef:   info.HeadRef,
			BaseRef:   info.BaseRef,
			Head:      info.HeadOID,
			Body:      info.Body,
			Status:    info.Status,
			Requested: info.Requests(login),
			UpdatedAt: info.UpdatedAt,
			Stack:     sr.stack,
			Alias:     sr.Alias,
		}
		out[p.Key] = p
	}
	return out
}

// parseWatchKey reads owner/name and the number back out of a
// github.WatchKey.
func parseWatchKey(key string) (string, int, bool) {
	rest, ok := strings.CutPrefix(key, github.WatchPrefix)
	if !ok {
		return "", 0, false
	}
	_, rest, ok = strings.Cut(rest, ":")
	if !ok {
		return "", 0, false
	}
	i := strings.LastIndexByte(rest, '#')
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(rest[i+1:])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return rest[:i], n, true
}
