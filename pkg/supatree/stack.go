package supatree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/panamafrancis/workbench/pkg/git"
)

// Editing a stack. Each change is one commit to the stack repo's supatree.yml:
// the stack is shared, so its history is how a teammate sees why a repo joined
// or left. Only supatree.yml is staged — whatever else is uncommitted in the
// stack stays as it was — and a supatree.yml with uncommitted edits of its own
// is refused rather than folded into a commit that says something else.

// StackAdd adds a member to a stack. The repository is cloned into the cache
// first, when clone is set, so the first tree does not pay for it.
func (c *Config) StackAdd(stackAlias, alias, url string, clone bool, mode StackEditMode) (StackEditResult, error) {
	stack := c.FindStack(stackAlias)
	if stack == nil {
		return StackEditResult{}, fmt.Errorf("stack %q not found", stackAlias)
	}
	if err := git.ValidateName(alias, nil); err != nil {
		return StackEditResult{}, fmt.Errorf("invalid member alias %q: %w", alias, err)
	}
	if _, err := CacheKey(url); err != nil {
		return StackEditResult{}, err
	}
	return c.editSpec(stack, mode, "supatree: add "+alias, func(s *Spec) error {
		if prev, ok := s.Members[alias]; ok {
			if prev == url {
				return errNoChange
			}
			return fmt.Errorf("%q is already a member (%s) — pick another alias with --as", alias, prev)
		}
		for a, u := range s.Members {
			if sameRepo(u, url) {
				return fmt.Errorf("%s is already a member, as %q", url, a)
			}
		}
		// s is the pinned origin's spec, read in the watcher's private clone,
		// so the owners it vouches for are ones a human put there.
		if mode == ByWatcher && !ownerVouchedFor(url, stack.Origin, s) {
			return fmt.Errorf("%s is not under an owner this stack already uses — the watcher clones with your credentials, "+
				"so a PM may only add repositories alongside the stack's existing ones; add it from a terminal: supatree stack add %s %s",
				url, stack.Alias, url)
		}
		if clone {
			if _, err := c.ResolveMember(alias, url); err != nil {
				return err
			}
		}
		if s.Members == nil {
			s.Members = map[string]string{}
		}
		s.Members[alias] = url
		return nil
	})
}

// ownerVouchedFor reports whether url's host and owner are already in the
// stack: a member's, or the pinned origin's own. A PM-requested member is
// cloned — now or by the first tree to need it — with the human's
// credentials, which reach every private repository they can; this keeps a
// PM to the organisations the stack already works in.
func ownerVouchedFor(url, origin string, s *Spec) bool {
	want, ok := resolveRemote(url)
	if !ok {
		return false
	}
	same := func(u string) bool {
		r, ok := resolveRemote(u)
		return ok && strings.EqualFold(r.Host, want.Host) && strings.EqualFold(r.Owner, want.Owner)
	}
	if same(origin) {
		return true
	}
	for _, u := range s.Members {
		if same(u) {
			return true
		}
	}
	return false
}

// StackRm removes a member from a stack, and every dependency edge that names
// it. Existing trees keep the member until they sync with prune.
func (c *Config) StackRm(stackAlias, alias string, mode StackEditMode) (StackEditResult, error) {
	stack := c.FindStack(stackAlias)
	if stack == nil {
		return StackEditResult{}, fmt.Errorf("stack %q not found", stackAlias)
	}
	return c.editSpec(stack, mode, "supatree: remove "+alias, func(s *Spec) error {
		if _, ok := s.Members[alias]; !ok {
			return fmt.Errorf("%q is not a member of %s (members: %s)", alias, stackAlias, strings.Join(s.Aliases(), ", "))
		}
		delete(s.Members, alias)
		delete(s.Deps, alias)
		for from, tos := range s.Deps {
			kept := tos[:0]
			for _, to := range tos {
				if to != alias {
					kept = append(kept, to)
				}
			}
			if len(kept) == 0 {
				delete(s.Deps, from)
			} else {
				s.Deps[from] = kept
			}
		}
		return nil
	})
}

// StackDep records that from depends on to: to's pull request merges first.
func (c *Config) StackDep(stackAlias, from, to string, mode StackEditMode) (StackEditResult, error) {
	stack := c.FindStack(stackAlias)
	if stack == nil {
		return StackEditResult{}, fmt.Errorf("stack %q not found", stackAlias)
	}
	return c.editSpec(stack, mode, fmt.Sprintf("supatree: %s depends on %s", from, to), func(s *Spec) error {
		for _, a := range []string{from, to} {
			if _, ok := s.Members[a]; !ok {
				return fmt.Errorf("%q is not a member of %s", a, stackAlias)
			}
		}
		if from == to {
			return fmt.Errorf("a member cannot depend on itself")
		}
		for _, d := range s.Deps[from] {
			if d == to {
				return errNoChange
			}
		}
		if s.Deps == nil {
			s.Deps = map[string][]string{}
		}
		s.Deps[from] = append(s.Deps[from], to)
		sort.Strings(s.Deps[from])
		if _, err := s.OrderedMembers(); err != nil {
			return err
		}
		return nil
	})
}

// errNoChange makes an edit that would change nothing succeed quietly.
var errNoChange = errors.New("no change")

// StackEditMode is who is making a stack edit, which decides what it may trust.
//
// The stack checkout is writable by the PM (it keeps notes there) and by every
// tree agent (a tree is a worktree of it, sharing its .git). So its remotes,
// its hooks and its unpushed commits are all things a sandboxed agent may have
// written. An edit made outside any sandbox must not run git there, push to a
// remote named there, or publish a commit found there.
type StackEditMode int

const (
	// ByHuman is the CLI: someone at the keyboard. It publishes to the origin
	// the human pinned (supatree stack pin, or stack clone), and otherwise
	// commits to the checkout only.
	ByHuman StackEditMode = iota
	// ByWatcher is the watcher acting for the PM. It trusts only the pinned
	// origin, and works in a private clone of it: the checkout is never read.
	ByWatcher
	// InSandbox is the PM itself, for a stack with no pinned origin: the edit
	// is committed to the checkout and goes no further.
	InSandbox
)

// StackEditResult is where a stack edit ended up. New builds every tree from
// the stack's origin, so an edit that is only committed locally is invisible to
// the next tree — the gap #17 fell into. Publishing is part of the edit.
type StackEditResult struct {
	Commit string // the edit's commit; "" when nothing changed
	// Pushed is set when the edit is on origin's default branch, where New
	// reads it.
	Pushed bool
	// Branch and PRURL are set when the default branch refused the push and
	// the edit went to a branch, with a pull request when one could be opened.
	Branch string
	PRURL  string
	// NoOrigin: the stack has no remote, so its local default branch is what
	// trees are built from and the commit is all there is to do.
	NoOrigin bool
	// NotPushed says why the edit is not on the default branch, when it is not.
	NotPushed string
	// CheckoutNote tells the human their stack checkout is behind the edit.
	CheckoutNote string
}

// Live reports whether the next tree made from the stack will see the edit.
func (r StackEditResult) Live() bool { return r.Commit == "" || r.Pushed || r.NoOrigin }

// Summary says where the edit went and who will see it.
func (r StackEditResult) Summary() string {
	const existing = " Existing trees keep their own copy of the stack."
	switch {
	case r.Commit == "":
		return "Nothing changed."
	case r.NoOrigin:
		return "Committed to the stack (it has no remote, so new trees are built from this checkout)." + existing
	case r.Pushed:
		return "Pushed to the stack's default branch; new trees get it." + existing + r.CheckoutNote
	case r.PRURL != "":
		return fmt.Sprintf("The default branch refused a direct push, so it is in a pull request: %s. New trees get it once that merges.", r.PRURL)
	case r.Branch != "":
		return fmt.Sprintf("The default branch refused a direct push; pushed branch %s, but could not open a pull request (%s). New trees get it once it is merged.", r.Branch, r.NotPushed)
	}
	return fmt.Sprintf("Committed (%s) but NOT published: %s. New trees are built from the stack's origin and will not see it until it is.", shortSHA(r.Commit), r.NotPushed)
}

// editSpec makes one edit to a stack's supatree.yml, as one commit, and
// publishes it as far as mode allows.
func (c *Config) editSpec(stack *Stack, mode StackEditMode, message string, edit func(*Spec) error) (StackEditResult, error) {
	return c.editStackFile(stack, mode, SpecName, message, func(dir string) error {
		spec, err := LoadSpec(dir)
		if err != nil {
			return err
		}
		if err := edit(spec); err != nil {
			return err
		}
		return SaveSpec(dir, spec)
	})
}

// editStackFile makes one edit to one file of a stack, as one commit, and
// publishes it as far as mode allows. edit rewrites the file in the checkout
// or clone it is given, or returns errNoChange.
func (c *Config) editStackFile(stack *Stack, mode StackEditMode, file, message string, edit func(dir string) error) (StackEditResult, error) {
	switch mode {
	case ByWatcher:
		if stack.Origin == "" {
			return StackEditResult{}, fmt.Errorf("stack %s has no pinned origin, so the watcher will not publish edits to it — pin it once from a terminal: supatree stack pin %s", stack.Alias, stack.Alias)
		}
		return publishStackEdit(stack.Origin, file, message, edit)
	case InSandbox:
		res, err := commitStackEdit(stack.Path, file, message, edit)
		if err == nil && res.Commit != "" && !res.NoOrigin {
			res.NotPushed = fmt.Sprintf("stack %s has no pinned origin, so its edits cannot be published for you — pin it once from a terminal (supatree stack pin %s), or push the checkout", stack.Alias, stack.Alias)
		}
		return res, err
	case ByHuman:
		return editStackByHuman(stack, file, message, edit)
	}
	return StackEditResult{}, fmt.Errorf("unknown stack edit mode %d", mode)
}

// editStackByHuman publishes the edit to the stack's pinned origin, or — for a
// stack nobody has pinned — commits it to the checkout and says how to pin.
// It never takes the origin from the checkout itself: the checkout's remotes
// are agent-writable, and adopting one here would hand an agent the very
// trust pinning exists to withhold.
func editStackByHuman(stack *Stack, file, message string, edit func(dir string) error) (StackEditResult, error) {
	if stack.Origin == "" {
		res, err := commitStackEdit(stack.Path, file, message, edit)
		if err == nil && res.Commit != "" && !res.NoOrigin {
			res.NotPushed = fmt.Sprintf("stack %s has no pinned origin, so the edit was only committed to %s — check the remote (git -C %s remote -v), then pin it: supatree stack pin %s <url>",
				stack.Alias, stack.Path, stack.Path, stack.Alias)
		}
		return res, err
	}
	res, err := publishStackEdit(stack.Origin, file, message, edit)
	if err == nil && res.Pushed {
		// Not fast-forwarded for you: that would run git in a checkout an
		// agent can write.
		res.CheckoutNote = fmt.Sprintf(" Your checkout %s is now behind its origin; pull it.", stack.Path)
	}
	return res, err
}

// commitStackEdit edits and commits one file in a checkout, staging only that
// file and refusing one with uncommitted edits of its own.
func commitStackEdit(stackPath, file, message string, edit func(dir string) error) (StackEditResult, error) {
	var res StackEditResult
	if err := git.RequireSafeRepo(stackPath); err != nil {
		return res, err
	}
	if out, err := gitOutput(stackPath, "status", "--porcelain", "--", file); err != nil {
		return res, err
	} else if out != "" {
		return res, fmt.Errorf("%s has uncommitted changes in %s — commit or discard them first", file, stackPath)
	}
	_, originErr := gitOutput(stackPath, "remote", "get-url", "origin")
	res.NoOrigin = originErr != nil
	changed, err := applyStackEdit(stackPath, file, message, edit)
	if err != nil || !changed {
		return res, err
	}
	res.Commit, _ = gitOutput(stackPath, "rev-parse", "HEAD")
	return res, nil
}

// applyStackEdit runs edit in dir and commits file, reporting whether
// anything changed.
func applyStackEdit(dir, file, message string, edit func(dir string) error) (bool, error) {
	if err := edit(dir); err != nil {
		if errors.Is(err, errNoChange) {
			return false, nil
		}
		return false, err
	}
	if err := runGit(dir, "add", "--", file); err != nil {
		return false, err
	}
	return true, runGit(dir, "commit", "-q", "-m", message, "--", file)
}

// StackEditsDir holds the private clones stack edits are made in. Under the
// cache, beside the layouts, and like them granted to no sandbox.
func StackEditsDir() string {
	return filepath.Join(CacheDir(), "stack-edits")
}

// publishStackEdit makes the edit in a fresh clone of origin and pushes exactly
// that one commit to its default branch — or, when the branch refuses it, to a
// branch with a pull request. Nothing the stack checkout holds is read, so no
// agent-written hook, remote or commit comes along.
func publishStackEdit(origin, file, message string, edit func(dir string) error) (StackEditResult, error) {
	var res StackEditResult
	if err := os.MkdirAll(StackEditsDir(), 0755); err != nil {
		return res, err
	}
	dir, err := os.MkdirTemp(StackEditsDir(), "edit-")
	if err != nil {
		return res, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if out, err := netGit("", "clone", "-q", "--", origin, dir); err != nil {
		return res, fmt.Errorf("clone the stack's origin %s: %s", origin, out)
	}
	def := git.DefaultBranch(dir)
	changed, err := applyStackEdit(dir, file, message, edit)
	if err != nil || !changed {
		return res, err
	}
	res.Commit, _ = gitOutput(dir, "rev-parse", "HEAD")
	out, pushErr := netGit(dir, "push", "-q", "origin", "HEAD:refs/heads/"+def)
	if pushErr == nil {
		res.Pushed = true
		return res, nil
	}
	res.NotPushed = "git push: " + out
	branch := "supatree/stack-edit-" + time.Now().UTC().Format("20060102-150405")
	if _, err := netGit(dir, "push", "-q", "origin", "HEAD:refs/heads/"+branch); err != nil {
		return res, nil // unreachable, not protected: reported as not published
	}
	res.Branch = branch
	url, err := openStackPR(dir, branch, def, message)
	if err != nil {
		res.NotPushed = err.Error()
		return res, nil
	}
	res.PRURL = url
	return res, nil
}

// stackNetTimeout bounds each network step of publishing a stack edit. The
// watcher answers ops one at a time, so a hung push would stall every other
// request behind it.
const stackNetTimeout = 5 * time.Minute

// netWaitDelay bounds how long a timed-out network git command's output is
// still waited for. The timeout kills git (or the nono wrapping it), but not
// what it started — git-remote-https, or git itself under nono — and those
// hold the output pipe open for as long as their stalled connection lasts:
// without it a fetch's timeout ends nothing, and the op holding the
// watcher's op lock never returns.
const netWaitDelay = 10 * time.Second

// netGit runs a git command that talks to a remote: bounded, and never
// stopping to ask for credentials. It returns the trimmed combined output.
func netGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), stackNetTimeout)
	defer cancel()
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = netWaitDelay
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// openStackPR is swappable in tests, which have no GitHub.
var openStackPR = func(dir, branch, base, title string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), stackNetTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "create",
		"--head", branch, "--base", base, "--title", title,
		"--body", "Opened by supatree: the stack's default branch refused a direct push of this edit.")
	cmd.Dir = dir
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return "", fmt.Errorf("gh pr create: %s", strings.TrimSpace(string(exit.Stderr)))
		}
		return "", fmt.Errorf("gh pr create: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// sameRepo reports whether two URLs name one repository.
func sameRepo(a, b string) bool {
	ka, errA := CacheKey(a)
	kb, errB := CacheKey(b)
	return errA == nil && errB == nil && ka == kb
}

// StackCloneResult reports what StackClone did.
type StackCloneResult struct {
	Alias string
	Path  string
	// Setup is the stack's scripts/setup, if it has one. It runs for every
	// tree made from this stack, so whoever adopts a stack should read it.
	Setup    string
	Warnings []string
}

// StackClone is teammate onboarding: clone a shared stack repo, register it,
// and fill the repo cache with its members.
func (c *Config) StackClone(url, alias, pathOverride string) (*StackCloneResult, error) {
	if alias == "" {
		alias = repoName(url)
	}
	if err := git.ValidateName(alias, nil); err != nil {
		return nil, fmt.Errorf("invalid stack name %q (pass --as): %w", alias, err)
	}
	if c.FindStack(alias) != nil {
		return nil, fmt.Errorf("a stack named %q is already registered", alias)
	}
	dest := DefaultStackPath(alias)
	if pathOverride != "" {
		abs, err := filepath.Abs(pathOverride)
		if err != nil {
			return nil, err
		}
		dest = abs
	}
	if _, err := os.Stat(dest); err == nil {
		return nil, fmt.Errorf("%s already exists", dest)
	}
	if err := cloneInto(url, dest); err != nil {
		return nil, fmt.Errorf("clone stack: %w", err)
	}
	spec, err := LoadSpec(dest)
	if err != nil {
		_ = os.RemoveAll(dest)
		return nil, err
	}
	if err := AddStack(alias, dest); err != nil {
		return nil, err
	}
	// The URL the human gave is the stack's origin; pin it before any agent
	// has had a chance to write the checkout's remotes.
	if err := PinStackOrigin(alias, AbsGitURL(url, "")); err != nil {
		return nil, err
	}
	res := &StackCloneResult{Alias: alias, Path: dest}
	for _, a := range spec.Aliases() {
		if _, err := c.ResolveMember(a, spec.Members[a]); err != nil {
			res.Warnings = append(res.Warnings, err.Error())
		}
	}
	if data, err := os.ReadFile(filepath.Join(dest, SetupScript)); err == nil {
		res.Setup = string(data)
	}
	return res, nil
}

// ScanOrigins reads the origin of every git repository directly under dir. It
// only reads them: the scanned checkouts are never used, only their URLs.
func ScanOrigins(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if !isClone(path) {
			continue
		}
		url, err := gitOutput(path, "remote", "get-url", "origin")
		if err != nil || url == "" {
			continue
		}
		out[repoName(url)] = url
	}
	return out, nil
}

// OrgRepos lists an organization's repositories through gh, skipping archived
// repositories and forks: neither is somewhere new work goes.
func OrgRepos(org string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "repo", "list", org, "--limit", "1000",
		"--json", "name,sshUrl,isArchived,isFork").Output()
	if err != nil {
		return nil, fmt.Errorf("gh repo list %s: %w", org, err)
	}
	var repos []struct {
		Name       string `json:"name"`
		SSHURL     string `json:"sshUrl"`
		IsArchived bool   `json:"isArchived"`
		IsFork     bool   `json:"isFork"`
	}
	if err := json.Unmarshal(out, &repos); err != nil {
		return nil, fmt.Errorf("parse gh repo list: %w", err)
	}
	res := map[string]string{}
	for _, r := range repos {
		if r.IsArchived || r.IsFork {
			continue
		}
		res[strings.ToLower(r.Name)] = r.SSHURL
	}
	return res, nil
}

// RepoUse is a tree whose member hangs off a cached clone.
type RepoUse struct {
	Tree  string
	Alias string
}

// CacheUsage maps each cache clone to the trees with a worktree of it, keyed
// by the clone's resolved path (see UsageOf).
func CacheUsage(insts []*Instance) map[string][]RepoUse {
	out := map[string][]RepoUse{}
	for _, inst := range insts {
		for _, m := range inst.Members {
			if !m.Exists {
				continue
			}
			if clone, ok := cloneOf(m.Path); ok {
				key := realPath(clone)
				out[key] = append(out[key], RepoUse{Tree: inst.Name, Alias: m.Alias})
			}
		}
	}
	return out
}

// FindCachedRepo resolves what a person calls a cached repository — its cache
// key, owner/repo, or a URL — to the clone.
func FindCachedRepo(name string) (*CachedRepo, error) {
	repos, err := ListCachedRepos()
	if err != nil {
		return nil, err
	}
	want := strings.ToLower(strings.TrimSuffix(name, ".git"))
	if key, err := CacheKey(name); err == nil {
		want = strings.ToLower(key)
	}
	var matches []CachedRepo
	for _, r := range repos {
		k := strings.ToLower(r.Key)
		if k == want || strings.HasSuffix(k, "/"+want) {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("%q is not in the repo cache — see: supatree repo ls", name)
	case 1:
		return &matches[0], nil
	default:
		keys := make([]string, len(matches))
		for i, m := range matches {
			keys[i] = m.Key
		}
		return nil, fmt.Errorf("%q matches several cached repos: %s", name, strings.Join(keys, ", "))
	}
}

// RemoveCachedRepo deletes a clone from the cache. It refuses while any tree
// has a worktree of it: that worktree's .git points into the clone, and its
// branch and unpushed commits live in the clone. The clone's own worktree list
// is the authority — a member that has left its tree's spec, or a tree List
// cannot parse, still has one there.
func RemoveCachedRepo(r *CachedRepo, insts []*Instance) error {
	_ = runGit(r.Path, "worktree", "prune")
	out, err := gitOutput(r.Path, "worktree", "list", "--porcelain")
	if err != nil {
		return fmt.Errorf("list %s's worktrees: %w", r.Key, err)
	}
	var others []string
	for _, line := range strings.Split(out, "\n") {
		if wt, ok := strings.CutPrefix(line, "worktree "); ok && realPath(wt) != realPath(r.Path) {
			others = append(others, wt)
		}
	}
	if len(others) > 0 {
		return fmt.Errorf("%s still has worktrees: %s — remove the trees using it first", r.Key, strings.Join(others, ", "))
	}
	if uses := UsageOf(CacheUsage(insts), r.Path); len(uses) > 0 {
		trees := make([]string, 0, len(uses))
		for _, u := range uses {
			trees = append(trees, u.Tree)
		}
		return fmt.Errorf("%s is in use by %s — remove those trees first", r.Key, strings.Join(trees, ", "))
	}
	return os.RemoveAll(r.Path)
}

// UsageOf looks a clone up in a CacheUsage map. git reports paths with
// symlinks resolved (/private/var for /var on macOS), so both sides are.
func UsageOf(usage map[string][]RepoUse, clone string) []RepoUse {
	return usage[realPath(clone)]
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}
