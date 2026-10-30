// Package reviewer is supatree's automated reviewer: it finds the pull
// requests that request a review from one GitHub account, groups the ones
// that make up one cross-repo change, reviews each group in a review tree of
// its own with a headless agent, and follows up when a re-review is asked for.
//
// The decisions are Go and deterministic (Plan is a pure function over a
// snapshot); an agent runs only for the review itself and, when enabled, for
// grouping pull requests nothing else links. It runs inside the watcher, on
// the watcher's round, from the PR cache the round has just filled — so it
// adds no poller and no GitHub load of its own beyond confirming, once per
// review, what was posted.
package reviewer

import (
	"errors"
	"fmt"
	"time"

	"github.com/panamafrancis/supatree/pkg/supatree"
)

// Defaults for the settings left empty.
const (
	DefaultGroupWindow   = 5 * time.Minute
	DefaultMaxConcurrent = 2
	DefaultMaxAttempts   = 3
	DefaultTreeTTL       = 7 * 24 * time.Hour
)

// AgentName is the reviewer's agent in each of its trees.
const AgentName = "reviewer"

// Settings are the resolved reviewer configuration.
type Settings struct {
	Login  string
	Stacks []string // empty: every registered stack
	Model  string   // empty: the default model
	// GroupWindow is how long a new pull request waits before its tree is
	// made, so the siblings of a cross-repo change — opened a minute or two
	// apart — land in one tree rather than one each.
	GroupWindow   time.Duration
	LLMGrouping   bool
	MaxConcurrent int
	// MaxAttempts is how many runs that post nothing a request gets before the
	// reviewer gives up on it until the pull request changes.
	MaxAttempts int
	Timeout     time.Duration // per run; zero is supatree.DefaultHeadlessTimeout
	// TreeTTL is how long a tree with nothing waiting on it is kept, so a
	// re-review resumes the same session rather than starting over.
	TreeTTL time.Duration
}

// ErrDisabled is a config with the reviewer off or absent.
var ErrDisabled = errors.New("reviewer is not enabled")

// Resolve reads the reviewer settings out of the config.
func Resolve(c *supatree.Config) (Settings, error) {
	if c.Reviewer == nil || !c.Reviewer.Enabled {
		return Settings{}, ErrDisabled
	}
	return ResolveConfigured(c)
}

// ResolveConfigured reads the reviewer settings whether or not the reviewer
// is enabled, for a dry run of one that is not switched on yet.
func ResolveConfigured(c *supatree.Config) (Settings, error) {
	r := c.Reviewer
	if r == nil {
		return Settings{}, errors.New("no reviewer: block in the config")
	}
	if r.Login == "" {
		return Settings{}, errors.New("reviewer.login is not set: name the GitHub account reviews are posted as")
	}
	s := Settings{
		Login:         r.Login,
		Stacks:        r.Stacks,
		Model:         r.Model,
		LLMGrouping:   r.LLMGrouping,
		MaxConcurrent: r.MaxConcurrent,
		MaxAttempts:   r.MaxAttempts,
	}
	var err error
	if s.GroupWindow, err = duration("group_window", r.GroupWindow, DefaultGroupWindow); err != nil {
		return Settings{}, err
	}
	if s.Timeout, err = duration("timeout", r.Timeout, supatree.DefaultHeadlessTimeout); err != nil {
		return Settings{}, err
	}
	if s.TreeTTL, err = duration("tree_ttl", r.TreeTTL, DefaultTreeTTL); err != nil {
		return Settings{}, err
	}
	if s.MaxConcurrent <= 0 {
		s.MaxConcurrent = DefaultMaxConcurrent
	}
	if s.MaxAttempts <= 0 {
		s.MaxAttempts = DefaultMaxAttempts
	}
	for _, st := range s.Stacks {
		if c.FindStack(st) == nil {
			return Settings{}, fmt.Errorf("reviewer.stacks names %q, which is not a registered stack", st)
		}
	}
	return s, nil
}

// WatchedStacks is the stacks the reviewer covers.
func (s Settings) WatchedStacks(c *supatree.Config) []string {
	if len(s.Stacks) > 0 {
		return s.Stacks
	}
	return c.StackAliases()
}

func duration(name, v string, def time.Duration) (time.Duration, error) {
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("reviewer.%s: %q is not a duration", name, v)
	}
	return d, nil
}
