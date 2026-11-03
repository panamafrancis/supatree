package supatree

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/panamafrancis/workbench/pkg/git"
)

// Creating a worktree is git run outside any sandbox, in a repository an agent
// can write: the stack (every tree shares its .git) and each member's base
// clone (a tree agent commits there). HardenGit pins the settings that run a
// command and have fixed names — hooks, fsmonitor, the ssh command, the ext
// transport. RequireSafeRepo refuses a repository whose config names the rest,
// filter drivers first; but it checks once, and git re-reads the config when it
// runs, so an agent rewriting .git/config in between would get its filter run
// on checkout (#21).
//
// addWorktree closes that for filters: every filter the commit being checked
// out can name — in its .gitattributes files, or in the repository's
// info/attributes — is pinned on the command line to a pass-through, which
// outranks anything the repository's config says, whenever it says it.

// init turns off git's replace objects for every git this process starts. An
// agent can write refs/replace/ in any repository it commits to, and a replace
// ref makes git substitute an object of the agent's choosing for the one asked
// for — so a pinned commit would check out as whatever the agent liked. No
// supatree workflow uses replace refs, sandboxed or not.
func init() {
	_ = os.Setenv("GIT_NO_REPLACE_OBJECTS", "1")
}

// addWorktree is `git worktree add -b branch path start` in repo, with every
// filter the checkout could run pinned off, run in jail.
func addWorktree(jail gitJail, repo, path, branch, start string) error {
	// One commit, resolved once: a ref is agent-movable, and the filters
	// pinned must be those of the very commit checked out.
	sha, err := gitOutput(repo, "rev-parse", "--verify", "--quiet", start+"^{commit}")
	if err != nil || !commitSHA.MatchString(sha) {
		return fmt.Errorf("no commit %s in %s to check out", start, repo)
	}
	start = sha
	// Checked again here, as late as possible: the keys it reports and nothing
	// can pin (include, url.insteadOf, …) are only as safe as the gap is short.
	if err := git.RequireSafeRepo(repo); err != nil {
		return err
	}
	// Read after that check, for the same reason: info/attributes is the one
	// attribute source the commit does not fix.
	pins, err := filterPins(repo, start)
	if err != nil {
		return err
	}
	// The user's and the system's attribute files name filters too; neither
	// has any business in a checkout supatree makes.
	pins = append(pins, "-c", "core.attributesFile=/dev/null")
	// The jail grants only what exists: the worktree's directory is made
	// here, empty, which git worktree add accepts, and removed again if the
	// checkout fails before filling it.
	if err := os.MkdirAll(path, 0755); err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	cmd, err := jail.command(context.Background(), repo, []string{path},
		append(pins, "worktree", "add", "-b", branch, path, start)...)
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	cmd.Env = append(envOf(cmd), "GIT_ATTR_NOSYSTEM=1")
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		_ = os.Remove(path) // only if still empty
		return fmt.Errorf("git worktree add: %s", strings.TrimSpace(errBuf.String()))
	}
	return nil
}

// fetchGit runs a fetch in repo, in jail: a fetch is where a repository's
// config picks the ssh command, credential helpers and URL rewrites. Bounded
// and never prompting, like netGit; it returns the trimmed combined output.
func fetchGit(jail gitJail, repo string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), stackNetTimeout)
	defer cancel()
	cmd, err := jail.command(ctx, repo, nil, append([]string{"fetch"}, args...)...)
	if err != nil {
		return "", err
	}
	cmd.Stdin = nil
	cmd.Env = append(envOf(cmd), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = netWaitDelay
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// envOf is cmd's environment so far, to add to rather than replace.
func envOf(cmd *exec.Cmd) []string {
	if cmd.Env != nil {
		return cmd.Env
	}
	return os.Environ()
}

// fetchRef fetches ref from repo's origin, in jail, and returns the commit it
// resolved to. It is git.FetchRef, confined.
func fetchRef(jail gitJail, repo, ref string) (string, error) {
	if out, err := fetchGit(jail, repo, "origin", ref); err != nil {
		return "", fmt.Errorf("git fetch %s: %s", ref, out)
	}
	sha, err := gitOutput(repo, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return "", err
	}
	if !commitSHA.MatchString(sha) {
		return "", fmt.Errorf("git fetch %s: FETCH_HEAD is %q, not a commit", ref, sha)
	}
	return sha, nil
}

// createWorktree branches path off repo's default branch, fetching it first:
// from url when one is given (a stack's pinned origin — never a remote the
// repository itself names, which an agent may have rewritten), else from the
// repository's origin. It reports whether it fell back to the last-fetched
// branch because the fetch failed.
//
// With a url, nothing is taken from the repository: the default branch and
// its commit come from the url (ls-remote), the tree starts at that commit,
// and a failed fetch is an error. The repository's own refs are agent-
// writable, and a fallback to origin/<default> would let an agent choose the
// commit — AGENTS.md included — every new tree starts from.
func createWorktree(jail gitJail, repo, path, branch, url string) (offline bool, err error) {
	if err := git.RequireSafeRepo(repo); err != nil {
		return false, err
	}
	if url != "" {
		def, sha, err := remoteHead(url)
		if err != nil {
			return false, err
		}
		if out, err := fetchGit(jail, repo, "-q", "--", url, "refs/heads/"+def); err != nil {
			return false, fmt.Errorf("fetch %s from the stack's pinned origin %s: %s", def, url, out)
		}
		if !gitRefExists(repo, sha) {
			return false, fmt.Errorf("the stack's origin advertised %s for %s, but the fetch did not bring it", sha, def)
		}
		_ = runGit(repo, "update-ref", "refs/remotes/origin/"+def, sha) // for status, not trusted
		return false, addWorktree(jail, repo, path, branch, sha)
	}
	def := git.DefaultBranch(repo)
	base := "origin/" + def
	var fetchErr error
	switch {
	case hasGitRemote(repo, "origin"):
		if out, err := fetchGit(jail, repo, "origin", def); err != nil {
			fetchErr = fmt.Errorf("git fetch: %s", out)
		}
	default:
		base = def
	}
	if !gitRefExists(repo, base) {
		if fetchErr != nil {
			return false, fmt.Errorf("git fetch failed and there is no local %s: %w", base, fetchErr)
		}
		return false, fmt.Errorf("no %s to branch from in %s", base, repo)
	}
	offline = fetchErr != nil || base == def
	return offline, addWorktree(jail, repo, path, branch, base)
}

// filterPins returns -c arguments that turn every filter driver start could
// name into a no-op: smudge and clean pass the content through, there is no
// long-running process, and failing is allowed.
func filterPins(repo, start string) ([]string, error) {
	names, err := filterNames(repo, start)
	if err != nil {
		return nil, err
	}
	var pins []string
	for _, n := range names {
		pins = append(pins,
			"-c", "filter."+n+".smudge=cat",
			"-c", "filter."+n+".clean=cat",
			"-c", "filter."+n+".process=",
			"-c", "filter."+n+".required=false")
	}
	return pins, nil
}

// filterNames lists the filter drivers named by start's .gitattributes files
// and by the repository's info/attributes. The commit is fixed, so what it
// names cannot change after this reads it; info/attributes can, which is why
// it is read here, just before the checkout, and why an attributes line it
// gains afterwards still needs a config definition RequireSafeRepo refuses.
func filterNames(repo, start string) ([]string, error) {
	out, err := gitOutput(repo, "ls-tree", "-r", "--name-only", "-z", start)
	if err != nil {
		return nil, err
	}
	var texts []string
	for _, p := range strings.Split(out, "\x00") {
		// Case-insensitively: on a case-insensitive filesystem .GitAttributes
		// is read as .gitattributes.
		if !strings.EqualFold(filepath.Base(p), ".gitattributes") {
			continue
		}
		body, err := gitOutput(repo, "cat-file", "blob", start+":"+p)
		if err != nil {
			return nil, err
		}
		texts = append(texts, body)
	}
	// Unreadable is not "no filters": a checkout this cannot vouch for is refused.
	common, err := GitCommonDir(repo)
	if err != nil {
		return nil, fmt.Errorf("find %s's git dir: %w", repo, err)
	}
	switch body, err := os.ReadFile(filepath.Join(common, "info", "attributes")); {
	case err == nil:
		texts = append(texts, string(body))
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("read %s's info/attributes: %w", repo, err)
	}
	seen := map[string]bool{}
	for _, t := range texts {
		for _, f := range strings.Fields(t) {
			name, ok := strings.CutPrefix(f, "filter=")
			if !ok || name == "" {
				continue
			}
			// The name is spliced into a -c key: one carrying "=" would set a
			// value of its own choosing, so anything odd refuses the checkout.
			if !filterName.MatchString(name) {
				return nil, fmt.Errorf("refusing to check out %s: its attributes name a filter %q supatree cannot pin", start, name)
			}
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

var filterName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// remoteHead asks url, not any local ref, for its default branch and the
// commit that branch is at.
func remoteHead(url string) (branch, sha string, err error) {
	out, err := netGit("", "ls-remote", "--symref", "--", url, "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("ask the stack's pinned origin %s for its default branch: %s", url, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if ref, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			branch, _, _ = strings.Cut(ref, "\t")
		} else if f := strings.Fields(line); len(f) == 2 && f[1] == "HEAD" {
			sha = f[0]
		}
	}
	if branch == "" || !commitSHA.MatchString(sha) {
		return "", "", fmt.Errorf("the stack's pinned origin %s did not report a default branch", url)
	}
	return branch, sha, nil
}

var commitSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func hasGitRemote(repo, name string) bool {
	_, err := gitOutput(repo, "remote", "get-url", name)
	return err == nil
}

func gitRefExists(repo, ref string) bool {
	_, err := gitOutput(repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}
