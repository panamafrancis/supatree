package supatree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/panamafrancis/workbench/pkg/git"
	"github.com/panamafrancis/workbench/pkg/github"
)

// ErrReviewTree is the reason every authoring command gives for refusing.
//
// One sentence, shared, and it names the alternative: the agent that hits this
// is mid-task and needs to know what to do instead, not merely that it may not.
var ErrReviewTree = errors.New(
	"the branches here belong to the pull requests' authors, so renaming, pushing or opening PRs from them is not ours to do. " +
		"To propose changes, branch from the head yourself")

// prURL matches a GitHub pull request URL, which is the form `gh` prints and
// people paste. Owner and repo are deliberately loose — GitHub's own rules have
// changed over time and a wrong reject here is worse than a wrong accept, which
// the lookup catches anyway.
//
// Anything after the number is ignored for the same reason: a URL copied from
// the browser carries the tab (`/files`), the query (`?w=1`) or the anchor
// (`#discussion_r…`) far more often than it is bare, and refusing those makes
// the commonest paste in the world an error the human has to hand-edit.
var prURL = regexp.MustCompile(`^https?://[^/]+/([^/]+/[^/]+)/pull/(\d+)(?:[/?#].*)?$`)

// ParsePRRef reads a pull request reference in either of the two forms people
// have: the URL, and owner/repo#number.
func ParsePRRef(s string) (repo string, number int, err error) {
	s = strings.TrimSpace(s)
	if m := prURL.FindStringSubmatch(s); m != nil {
		n, convErr := strconv.Atoi(m[2])
		if convErr != nil {
			return "", 0, fmt.Errorf("parse %q: %w", s, convErr)
		}
		return m[1], n, nil
	}
	if owner, num, ok := strings.Cut(s, "#"); ok && strings.Count(owner, "/") == 1 {
		n, convErr := strconv.Atoi(num)
		if convErr == nil && n > 0 {
			return owner, n, nil
		}
	}
	return "", 0, fmt.Errorf("%q is not a pull request: expected a URL like https://github.com/owner/repo/pull/123, or owner/repo#123", s)
}

// ghPRView is the subset of `gh pr view` we read.
type ghPRView struct {
	Number         int    `json:"number"`
	URL            string `json:"url"`
	HeadRefName    string `json:"headRefName"`
	HeadRefOid     string `json:"headRefOid"`
	BaseRefName    string `json:"baseRefName"`
	Title          string `json:"title"`
	HeadRepository struct {
		Name string `json:"name"`
	} `json:"headRepository"`
}

// ResolvePRRefs turns pull request references into the facts a review tree
// records. It is split from tree creation so everything downstream of `gh` can
// be tested without a network.
func ResolvePRRefs(refs []string) ([]ReviewRef, error) {
	out := make([]ReviewRef, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, raw := range refs {
		repo, number, err := ParsePRRef(raw)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%s#%d", repo, number)
		if seen[key] {
			continue
		}
		seen[key] = true
		v, err := lookupPR(repo, number)
		if err != nil {
			return nil, err
		}
		out = append(out, ReviewRef{
			Repo:    repo,
			Number:  v.Number,
			URL:     v.URL,
			Head:    v.HeadRefOid,
			HeadRef: v.HeadRefName,
			Base:    v.BaseRefName,
		})
	}
	if len(out) == 0 {
		return nil, errors.New("no pull requests given")
	}
	return out, nil
}

func lookupPR(repo string, number int) (*ghPRView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", strconv.Itoa(number),
		"--repo", repo,
		"--json", "number,url,headRefName,headRefOid,baseRefName,title,headRepository")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("gh pr view %s#%d: %s", repo, number, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("gh pr view %s#%d: %w", repo, number, err)
	}
	var v ghPRView
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("parse gh output for %s#%d: %w", repo, number, err)
	}
	return &v, nil
}

// ReviewOptions parameterizes NewReview.
type ReviewOptions struct {
	Stack  string
	Name   string
	Model  string
	Intent string
	PRs    []ReviewRef
	Now    time.Time
}

// NewReview creates a review tree: a supatree whose members are checked out at
// foreign pull request heads and whose authoring commands are refused.
//
// Members are mapped to the stack by their repository. A pull request whose
// repository is not a member of the stack is an error naming both, rather than
// a tree silently missing the repo the reviewer asked for.
func NewReview(c *Config, opts ReviewOptions) (*Instance, *SyncReport, error) {
	if len(opts.PRs) == 0 {
		return nil, nil, errors.New("no pull requests given")
	}
	if opts.Stack == "" && len(c.Stacks) > 1 {
		alias, err := inferReviewStack(c, opts.PRs)
		if err != nil {
			return nil, nil, err
		}
		opts.Stack = alias
	}
	inst, report, err := New(c, CreateOptions{
		Stack:     opts.Stack,
		Name:      opts.Name,
		Model:     opts.Model,
		Now:       opts.Now,
		ReviewPRs: opts.PRs,
		Intent:    opts.Intent,
	})
	if err != nil {
		return nil, report, err
	}
	// Seed the PR cache from what we already know, so the tree shows its PRs
	// by number before the first round has run.
	byAlias := make(map[string]ReviewRef, len(opts.PRs))
	for _, m := range inst.Members {
		if m.Review != nil {
			byAlias[m.Alias] = *m.Review
		}
	}
	seedPRCache(byAlias)
	return inst, report, nil
}

// inferReviewStack picks the stack for a review whose caller named none: the
// one registered stack that has a member for every pull request's repository.
// With several stacks registered, asking for the alias failed the PM's review
// requests until it guessed one, when the pull requests already say which.
//
// The specs read are the local checkouts', which agents can write; that only
// chooses among stacks the human registered, and the tree is still checked
// against the spec it is built from (finishCreate).
func inferReviewStack(c *Config, prs []ReviewRef) (string, error) {
	var fits, unread []string
	for _, st := range c.Stacks {
		spec, err := LoadSpec(st.Path)
		if err != nil {
			unread = append(unread, fmt.Sprintf("%s: %v", st.Alias, err))
			continue
		}
		if _, err := mapPRsToMembers(spec, prs); err == nil {
			fits = append(fits, st.Alias)
		}
	}
	repos := make([]string, len(prs))
	for i, pr := range prs {
		repos[i] = pr.Repo
	}
	switch len(fits) {
	case 1:
		return fits[0], nil
	case 0:
		var skipped string
		if len(unread) > 0 {
			skipped = "; unreadable: " + strings.Join(unread, "; ")
		}
		return "", fmt.Errorf("no registered stack has a member for every one of %s (stacks: %s%s) — add the repos to a stack, or name one",
			strings.Join(repos, ", "), strings.Join(c.StackAliases(), ", "), skipped)
	}
	return "", fmt.Errorf("stacks %s each have %s: name one — --stack on the command line, `stack` to new_tree",
		strings.Join(fits, ", "), strings.Join(repos, ", "))
}

// mapPRsToMembers keys the review set by member alias, matching each pull
// request's repository against the spec's members.
func mapPRsToMembers(spec *Spec, prs []ReviewRef) (map[string]ReviewRef, error) {
	byAlias := make(map[string]ReviewRef, len(prs))
	for _, pr := range prs {
		alias, err := aliasForRepo(pr.Repo, spec.Members)
		if err != nil {
			return nil, err
		}
		if prev, dup := byAlias[alias]; dup {
			return nil, fmt.Errorf("%s and %s are both in repo %q — a review tree holds one pull request per repo",
				prev.URL, pr.URL, alias)
		}
		byAlias[alias] = pr
	}
	return byAlias, nil
}

// unpublishedMember explains a mapping failure. The tree is built from the
// stack's origin, but `stack add` commits to the local checkout first — so a
// repo the local spec has and the tree's does not is a member that was added
// and never pushed, and saying only "no member" sends the caller to add it
// again.
func unpublishedMember(stack *Stack, tree *Spec, err error, prs []ReviewRef) error {
	local, lerr := LoadSpec(stack.Path)
	if lerr != nil {
		return err
	}
	for _, pr := range prs {
		if _, inTree := aliasForRepo(pr.Repo, tree.Members); inTree == nil {
			continue
		}
		if _, inLocal := aliasForRepo(pr.Repo, local.Members); inLocal == nil {
			return fmt.Errorf("%w\nstack %s's local checkout (%s) has %s, but trees are built from its origin's default branch, which does not — push the stack (or merge its pending PR) and try again",
				err, stack.Alias, stack.Path, pr.Repo)
		}
	}
	return err
}

// requireReviewMembers checks every pull request ended up checked out. A
// review tree missing one is worse than no tree: it reports nothing to review,
// and its agent reviews an empty set.
func requireReviewMembers(inst *Instance, review map[string]ReviewRef) error {
	for alias, ref := range review {
		m := inst.FindMember(alias)
		if m == nil || !m.Exists || m.Review == nil {
			return fmt.Errorf("%s was not checked out as member %q — the review tree would not contain it", ref.URL, alias)
		}
	}
	return nil
}

// aliasForRepo finds the stack member whose URL is repo. The spec carries the
// URLs, so this reads them rather than asking each clone for its origin.
func aliasForRepo(repo string, members map[string]string) (string, error) {
	aliases := make([]string, 0, len(members))
	for alias := range members {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		if r, ok := resolveRemote(members[alias]); ok && strings.EqualFold(r.Repo().String(), repo) {
			return alias, nil
		}
	}
	return "", fmt.Errorf("no member of this stack is %s — add its repo to supatree.yml, or review it in a stack that has it (members: %s)",
		repo, strings.Join(aliases, ", "))
}

// seedPRCache writes what the review set already knows into the shared cache,
// so the first status round resolves by number instead of finding nothing.
func seedPRCache(byAlias map[string]ReviewRef) {
	_ = github.NewCache(PRCachePath()).Mutate(func(w *github.Writable) error {
		for _, ref := range byAlias {
			key := fmt.Sprintf("pr:%s#%d", ref.Repo, ref.Number)
			if w.Get(key) != nil {
				continue
			}
			// The number and URL, and deliberately no status: what the seed is
			// for is making the lookup resolvable, not reporting a state nobody
			// has fetched. Writable.Rename takes the same line when a branch is
			// renamed — it carries the number across and zeroes FetchedAt — and
			// an entry that was never verified is looked up for real by the
			// first round rather than confirmed by a poll.
			w.Set(key, &github.PRInfo{Number: ref.Number, URL: ref.URL, Status: github.PRNone})
		}
		return nil
	})
}

// RefreshResult reports what a refresh did to one member.
type RefreshResult struct {
	Alias string
	PR    string // owner/repo#number
	Was   string // the head recorded before
	Now   string // the head the PR has now
	Moved bool
	// Skipped says the worktree was left alone, and why. A member with
	// uncommitted work is never reset: review notes are not disposable, and the
	// reviewer can commit or discard them and run refresh again.
	Skipped string
}

// RefreshReview re-fetches every tracked pull request head and moves the member
// worktrees onto it.
//
// This exists because authors push during review. Without it you read a tree
// that quietly stops matching the pull request you are commenting on, and
// GitHub marks the inline comments outdated the moment they land.
func RefreshReview(c *Config, name string) ([]RefreshResult, error) {
	inst, err := Get(c, name)
	if err != nil {
		return nil, err
	}
	meta, err := LoadMeta(inst.Root)
	if err != nil {
		return nil, err
	}
	if !meta.Reviewing() {
		return nil, fmt.Errorf("%s is not a review tree", name)
	}

	var out []RefreshResult
	changed := false
	for _, m := range inst.Members {
		if m.Review == nil {
			continue
		}
		res := RefreshResult{Alias: m.Alias, PR: fmt.Sprintf("%s#%d", m.Review.Repo, m.Review.Number), Was: m.Review.Head}
		if !m.Exists {
			res.Skipped = "worktree is not checked out — run sync"
			out = append(out, res)
			continue
		}
		// Fetched from the worktree: it shares the clone's refs and objects,
		// and it is where this agent's sandbox can reach.
		sha, err := git.FetchRef(m.Path, fmt.Sprintf("refs/pull/%d/head", m.Review.Number))
		if err != nil {
			res.Skipped = err.Error()
			out = append(out, res)
			continue
		}
		res.Now = sha
		res.Moved = sha != m.Review.Head
		if !res.Moved {
			out = append(out, res)
			continue
		}
		if git.IsDirty(m.Path) {
			res.Skipped = "uncommitted changes here — commit or discard them, then refresh again"
			out = append(out, res)
			continue
		}
		if err := resetTo(m.Path, sha); err != nil {
			res.Skipped = err.Error()
			out = append(out, res)
			continue
		}
		ref := meta.Review[m.Alias]
		ref.Head = sha
		meta.Review[m.Alias] = ref
		changed = true
		out = append(out, res)
	}

	if changed {
		if err := meta.Save(inst.Root); err != nil {
			return out, fmt.Errorf("save meta: %w", err)
		}
		// The recorded heads are what "the author pushed" is measured against,
		// so a stale info.md would describe commits nobody is looking at.
		if updated, err := LoadInstance(inst.Root); err == nil {
			_ = WriteInfo(updated)
		}
	}
	return out, nil
}

// resetTo moves a clean worktree onto sha, keeping it on its own branch.
func resetTo(path, sha string) error {
	return runGit(path, "reset", "--hard", sha)
}

// ForkResult reports one member's conversion from review to authoring.
type ForkResult struct {
	Alias string
	From  string // the review branch it was on
	To    string // the authoring branch it is on now
	Base  string // the branch its pull request will target
}

// ForkReview converts a review tree into an authoring one.
//
// Reviews turn into patches often enough to be worth a command rather than a
// procedure. Each member keeps the commits it is sitting on and moves from
// review/<slug>/<alias> to st/<slug>/<alias>, and each pull request's head ref
// is recorded as the new base — so the eventual PR is opened *against the
// author's branch*, arriving as a proposal on their work rather than as a rival
// pull request against main.
//
// Like RenameBranchSlug this is phased: every branch is renamed first, and a
// failure part-way rolls the earlier ones back, because the mode in meta is what
// derives every member's branch name and a half-converted tree would strand
// members outside their own tree.
func ForkReview(c *Config, name string) ([]ForkResult, error) {
	inst, err := Get(c, name)
	if err != nil {
		return nil, err
	}
	meta, err := LoadMeta(inst.Root)
	if err != nil {
		return nil, err
	}
	if !meta.Reviewing() {
		return nil, fmt.Errorf("%s is not a review tree", name)
	}

	// The names the members will carry once the mode flips.
	authoring := &Meta{Slug: meta.Slug}

	var done []renamedMember
	var out []ForkResult
	for _, m := range inst.Members {
		if !m.Exists {
			continue
		}
		to := authoring.MemberBranch(m.Alias)
		if err := git.RenameBranch(m.Path, to); err != nil {
			return nil, undoRenames(fmt.Errorf("fork %s: %w", m.Alias, err), done)
		}
		done = append(done, renamedMember{alias: m.Alias, path: m.Path, oldBranch: m.Branch, newBranch: to})
		res := ForkResult{Alias: m.Alias, From: m.Branch, To: to}
		if m.Review != nil {
			res.Base = m.Review.HeadRef
		}
		out = append(out, res)
	}

	base := make(map[string]string, len(out))
	for _, r := range out {
		if r.Base != "" {
			base[r.Alias] = r.Base
		}
	}
	meta.Mode = ModeAuthoring
	meta.Base = base
	// Review is kept: it is where this work came from, and the PR numbers are
	// what a description should cross-link back to.
	if err := meta.Save(inst.Root); err != nil {
		return nil, undoRenames(fmt.Errorf("save meta: %w", err), done)
	}

	// The PR cache entries were keyed on the pull requests being reviewed. The
	// members now key on their own branches, which have no PR yet, so nothing
	// needs migrating — but info.md describes a tree that no longer exists.
	if updated, err := LoadInstance(inst.Root); err == nil {
		_ = WriteInfo(updated)
	}
	return out, nil
}

// AddResult reports what adding one pull request to a review tree did.
type AddResult struct {
	PR    string // owner/repo#number
	Alias string
	Head  string
	// Skipped says the pull request was not added, and why.
	Skipped string
}

// AddReview adds pull requests to an existing review tree (#12). A cross-repo
// change grows after its review tree exists — another PR turns up — and the
// alternatives were both poor: recreating the tree throws away review.md and
// the reviewer's context, and a second tree loses the side-by-side check that
// is the point of having one.
//
// Each PR is mapped against the tree's own spec. A member already in the tree
// but tracking nothing is moved onto the PR head — refused if it has
// uncommitted work or commits of its own, as refresh refuses; a member not yet
// checked out is created at the head. The tree's agent is told by mail, and
// its state returns to reviewing on the next round while any added PR is open.
func AddReview(c *Config, name string, prs []ReviewRef, from string, vet bool) ([]AddResult, error) {
	inst, err := Get(c, name)
	if err != nil {
		return nil, err
	}
	meta, err := LoadMeta(inst.Root)
	if err != nil {
		return nil, err
	}
	if !meta.Reviewing() {
		return nil, fmt.Errorf("%s is not a review tree — create one with: supatree review <pr>…", name)
	}
	// The tree's spec is the tree agent's to write: when the agent asked, the
	// very spec mapped against is the one vetted, so it cannot change between.
	spec, err := LoadSpec(inst.Root)
	if err != nil {
		return nil, err
	}
	if vet {
		if err := c.vetSpec(inst.Stack, spec); err != nil {
			return nil, err
		}
	}
	if meta.Review == nil {
		meta.Review = map[string]ReviewRef{}
	}

	var out []AddResult
	added := map[string]ReviewRef{}
	for _, pr := range prs {
		res := AddResult{PR: fmt.Sprintf("%s#%d", pr.Repo, pr.Number)}
		alias, err := aliasForRepo(pr.Repo, spec.Members)
		if err != nil {
			res.Skipped = err.Error()
			out = append(out, res)
			continue
		}
		res.Alias = alias
		if prev, tracked := meta.Review[alias]; tracked {
			if prev.Number == pr.Number {
				res.Skipped = "already in this tree — review_refresh moves it to the latest head"
			} else {
				res.Skipped = fmt.Sprintf("repo %q already tracks #%d — a review tree holds one pull request per repo", alias, prev.Number)
			}
			out = append(out, res)
			continue
		}
		path := MemberPath(inst.Root, alias)
		var sha string
		if _, statErr := os.Lstat(path); statErr == nil {
			sha, err = moveBystanderToPR(c, inst.Root, meta, alias, spec.Members[alias], pr)
		} else {
			sha, err = createReviewMember(c, inst.Root, meta, alias, spec.Members[alias], pr)
		}
		if err != nil {
			res.Skipped = err.Error()
			out = append(out, res)
			continue
		}
		pr.Head = sha
		res.Head = sha
		meta.Review[alias] = pr
		added[alias] = pr
		// Saved per PR: a later one failing must not lose the ones that
		// already moved a worktree.
		if err := meta.Save(inst.Root); err != nil {
			return out, fmt.Errorf("save meta: %w", err)
		}
		out = append(out, res)
	}
	if len(added) == 0 {
		return out, nil
	}
	seedPRCache(added)
	if updated, err := LoadInstance(inst.Root); err == nil {
		_ = WriteInfo(updated)
	}
	_ = Deliver(inst.Root, MainAgent, from, addedBrief(added))
	return out, nil
}

// moveBystanderToPR moves a member that tracks no PR onto the PR head — never
// over work: uncommitted changes, or commits of its own on top of the default
// branch, are the reviewer's, and are reported instead.
//
// It runs in the watcher, on a worktree an agent can write, so it trusts the
// worktree for nothing. The member must be a real directory (not a link to
// somewhere else) that is a worktree of the cached clone for its URL; the PR
// is fetched in that clone; and rather than resetting the worktree in place —
// a checkout with whatever filters the repository names — the member is
// removed and re-created at the PR head by addWorktree, with them pinned off.
func moveBystanderToPR(c *Config, root string, meta *Meta, alias, url string, pr ReviewRef) (string, error) {
	repo, err := c.ResolveMember(alias, url)
	if err != nil {
		return "", err
	}
	branch := meta.MemberBranch(alias)
	path, err := ownMemberWorktree(root, alias, repo.Clone, branch)
	if err != nil {
		return "", err
	}
	if git.IsDirty(path) {
		return "", errors.New("uncommitted changes here — commit or discard them, then add it again")
	}
	if n, err := git.CommitsAhead(path); err == nil && n > 0 {
		return "", fmt.Errorf("%d commit(s) of its own on this member — it is not idle, so it is not moved", n)
	}
	jail, err := c.gitJail()
	if err != nil {
		return "", err
	}
	sha, err := fetchRef(jail.reading(repo.URL), repo.Clone, fmt.Sprintf("refs/pull/%d/head", pr.Number))
	if err != nil {
		return "", err
	}
	// Again, immediately before anything is removed: the check above was a
	// fetch ago, and the path is the agent's to swap.
	if _, err := ownMemberWorktree(root, alias, repo.Clone, branch); err != nil {
		return "", err
	}
	if err := git.RemoveWorktree(repo.Clone, path); err != nil {
		return "", err
	}
	_ = git.DeleteBranch(repo.Clone, branch)
	if err := addWorktree(jail, repo.Clone, path, branch, sha); err != nil {
		return "", fmt.Errorf("re-create repos/%s at the PR head: %w", alias, err)
	}
	return sha, nil
}

// ownMemberWorktree finds repos/<alias> of the tree at root as the clone
// itself records it, and returns its real path — or refuses. The clone is
// shared by every tree, so "a worktree of this clone" is not enough: repos/ or
// the member could be a link into another tree, whose worktree would then be
// the one removed. The resolved path must sit exactly at root/repos/<alias>
// with no link on the way, and the clone must list it on this tree's own
// branch — whose name carries the tree's slug.
func ownMemberWorktree(root, alias, clone, branch string) (string, error) {
	refuse := fmt.Errorf("repos/%s is not this tree's worktree of its cached clone — leaving it alone", alias)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", refuse
	}
	want := filepath.Join(realRoot, ReposDirName, alias)
	for _, p := range []string{filepath.Join(root, ReposDirName), MemberPath(root, alias)} {
		if fi, err := os.Lstat(p); err != nil || !fi.IsDir() {
			return "", refuse // missing, or a link
		}
	}
	got, err := filepath.EvalSymlinks(MemberPath(root, alias))
	if err != nil || got != want {
		return "", refuse
	}
	out, err := gitOutput(clone, "worktree", "list", "--porcelain")
	if err != nil {
		return "", refuse
	}
	for _, block := range strings.Split(out, "\n\n") {
		var wt, br string
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "worktree "); ok {
				wt = v
			} else if v, ok := strings.CutPrefix(line, "branch "); ok {
				br = strings.TrimPrefix(v, "refs/heads/")
			}
		}
		if r, err := filepath.EvalSymlinks(wt); err == nil && r == want && br == branch {
			return want, nil
		}
	}
	return "", refuse
}

// createReviewMember checks a member out at the PR head, as creating the
// review tree would have.
func createReviewMember(c *Config, root string, meta *Meta, alias, url string, pr ReviewRef) (string, error) {
	repo, err := c.ResolveMember(alias, url)
	if err != nil {
		return "", err
	}
	meta.Review[alias] = pr
	if err := createMember(c, repo, MemberPath(root, alias), meta, alias, &SyncReport{}); err != nil {
		delete(meta.Review, alias)
		return "", err
	}
	return gitOutput(MemberPath(root, alias), "rev-parse", "HEAD")
}

// addedBrief tells the tree's agent what it now also has to review.
func addedBrief(added map[string]ReviewRef) string {
	aliases := make([]string, 0, len(added))
	for a := range added {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	var b strings.Builder
	b.WriteString("More pull requests were added to this review tree — the same change, grown:\n")
	for _, a := range aliases {
		pr := added[a]
		fmt.Fprintf(&b, "  repos/%s — %s at %s\n", a, pr.URL, shortSHA(pr.Head))
	}
	b.WriteString("\nReview them as part of the change you already reviewed: check them against the other repos " +
		"side by side, and note whether they change any verdict you posted. .supatree/info.md lists every PR.")
	return b.String()
}

// StackRepo is one member of a stack as the reviewer needs it: which
// repository it is and where supatree's clone of it lives.
type StackRepo struct {
	Alias string
	Repo  string // owner/name
	// Clone is supatree's cached clone, or "" when there is none yet. Only a
	// repository supatree has already cloned is watched: the spec is read from
	// the stack's checkout, which agents can write, and nothing here clones
	// what only that checkout names.
	Clone string
}

// StackRepos lists the members of a stack that resolve to a GitHub
// repository, keyed by owner/name (lowercased).
func (c *Config) StackRepos(stack string) (map[string]StackRepo, error) {
	st := c.FindStack(stack)
	if st == nil {
		return nil, fmt.Errorf("no stack %q", stack)
	}
	spec, err := LoadSpec(st.Path)
	if err != nil {
		return nil, err
	}
	out := make(map[string]StackRepo, len(spec.Members))
	for _, alias := range spec.Aliases() {
		url := spec.Members[alias]
		r, ok := resolveRemote(url)
		if !ok || !r.IsGitHub() {
			continue
		}
		sr := StackRepo{Alias: alias, Repo: r.Repo().String()}
		if bc, err := c.cachedClone(alias, url); err == nil && isClone(bc.Clone) {
			sr.Clone = bc.Clone
		}
		out[strings.ToLower(sr.Repo)] = sr
	}
	return out, nil
}
