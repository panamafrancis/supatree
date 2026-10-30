package supatree

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/panamafrancis/workbench/pkg/github"
)

// The change block is what makes the pull requests of one supatree findable
// as one change from GitHub alone: every pull request create_pr and
// create_prs open carries it, naming the change by the tree's slug and
// linking the siblings. A reviewer reading one of them sees the others; the
// automated reviewer (pkg/supatree/reviewer) groups on the marker line.
// The markers are HTML comments, so GitHub renders only the list.

const (
	changeOpen  = "<!-- supatree:change id=%s -->"
	changeClose = "<!-- /supatree:change -->"
)

var changeBlockRe = regexp.MustCompile(`(?s)\n*<!--\s*supatree:change\s+id=[^>]*-->.*?<!--\s*/supatree:change\s*-->\n*`)

// ChangeBlock renders the block for a change named slug whose other pull
// requests are siblings (URLs).
func ChangeBlock(slug string, siblings []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, changeOpen+"\n", slug)
	if len(siblings) > 0 {
		b.WriteString("Part of a cross-repo change, together with:\n")
		for _, u := range siblings {
			b.WriteString("- " + u + "\n")
		}
	}
	b.WriteString(changeClose)
	return b.String()
}

// WithChangeBlock puts block into body, replacing a block already there.
func WithChangeBlock(body, block string) string {
	stripped := strings.TrimRight(changeBlockRe.ReplaceAllString(body, "\n"), "\n")
	if strings.TrimSpace(stripped) == "" {
		return block
	}
	return stripped + "\n\n" + block
}

var createdPRURL = regexp.MustCompile(`https://[^\s/]+/[^\s/]+/[^\s/]+/pull/[0-9]+`)

// prURLIn finds the pull request URL in gh pr create's output.
func prURLIn(out string) string {
	return createdPRURL.FindString(out)
}

// siblingURLs lists the cached pull request URLs of every member of inst but
// skip, in member order.
func siblingURLs(inst *Instance, skip string) []string {
	cache := github.NewCache(PRCachePath())
	_ = cache.Load()
	var out []string
	for _, m := range inst.Members {
		if m.Alias == skip || !m.Exists {
			continue
		}
		if info := cache.Get(m.CacheKey()); info != nil && info.URL != "" && info.Status != github.PRNone {
			out = append(out, info.URL)
		}
	}
	return out
}

const crossLinkTimeout = 30 * time.Second

// crossLink rewrites the change block of each pull request in urls to name
// all the others. Best effort: a pull request it cannot edit keeps the block
// it was created with, which still carries the marker. It returns one line
// per failure.
func crossLink(slug string, urls []string) []string {
	var failures []string
	for _, u := range urls {
		var others []string
		for _, o := range urls {
			if o != u {
				others = append(others, o)
			}
		}
		if err := setChangeBlock(u, ChangeBlock(slug, others)); err != nil {
			failures = append(failures, fmt.Sprintf("%s: could not cross-link: %v", u, err))
		}
	}
	return failures
}

// setChangeBlock reads a pull request's body and writes it back with block.
// The body goes to gh on stdin, never on the command line.
func setChangeBlock(url, block string) error {
	ctx, cancel := context.WithTimeout(context.Background(), crossLinkTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "pr", "view", url, "--json", "body", "--jq", ".body").Output()
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	body := strings.TrimRight(string(out), "\n")
	next := WithChangeBlock(body, block)
	if next == body {
		return nil
	}
	cmd := exec.CommandContext(ctx, "gh", "pr", "edit", url, "--body-file", "-")
	cmd.Stdin = strings.NewReader(next)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
