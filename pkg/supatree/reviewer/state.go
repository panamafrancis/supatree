package reviewer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/panamafrancis/supatree/pkg/supatree"
	"github.com/panamafrancis/workbench/pkg/config"
)

// State is what the reviewer remembers between rounds. It lives in the state
// root, which no sandbox is granted: the reviewer's agents must not be able to
// tell it a review was posted that was not.
type State struct {
	PRs   map[string]*PRState   `json:"prs"`
	Trees map[string]*TreeState `json:"trees"`
}

// PRState is the reviewer's history with one pull request.
type PRState struct {
	// WaitingSince is when the current review request was first seen; zero
	// while none is waiting.
	WaitingSince time.Time `json:"waiting_since,omitzero"`
	// ReviewedAt is when the reviewer's last confirmed review was submitted,
	// as GitHub reports it. A request seen in a listing no newer than this is
	// the one that review answered, read before the review landed.
	ReviewedAt   time.Time `json:"reviewed_at,omitzero"`
	ReviewedHead string    `json:"reviewed_head,omitempty"`
	Verdict      string    `json:"verdict,omitempty"`
	ReviewURL    string    `json:"review_url,omitempty"`
	// Attempts counts runs for the current request that posted nothing.
	Attempts    int       `json:"attempts,omitempty"`
	LastAttempt time.Time `json:"last_attempt,omitzero"`
	// GaveUpAt is when MaxAttempts ran out. Nothing is retried until the pull
	// request changes after it.
	GaveUpAt time.Time `json:"gave_up_at,omitzero"`
	LastErr  string    `json:"last_err,omitempty"`
	// Grouping says why the pull request is in the tree it is in.
	Grouping string `json:"grouping,omitempty"`
	Tree     string `json:"tree,omitempty"`
}

// TreeState is the reviewer's history with one of its trees.
type TreeState struct {
	CreatedAt time.Time `json:"created_at"`
	LastRunAt time.Time `json:"last_run_at,omitzero"`
	Runs      int       `json:"runs,omitempty"`
}

// StatePath is the reviewer's state file.
func StatePath() string {
	return filepath.Join(supatree.StateRoot(), "reviewer", "state.json")
}

func stateLockPath() string { return StatePath() + ".lock" }

// LoadState reads the state, empty when there is none yet.
func LoadState() (*State, error) {
	st := &State{}
	data, err := os.ReadFile(StatePath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(data, st); err != nil {
			return nil, fmt.Errorf("parse %s: %w", StatePath(), err)
		}
	}
	if st.PRs == nil {
		st.PRs = map[string]*PRState{}
	}
	if st.Trees == nil {
		st.Trees = map[string]*TreeState{}
	}
	return st, nil
}

// MutateState applies fn to the state under its lock and writes the result.
// Every change goes through it: a round and several finishing runs write the
// state concurrently, and each must start from what the others wrote.
func MutateState(fn func(*State) error) error {
	if err := os.MkdirAll(filepath.Dir(StatePath()), 0o755); err != nil {
		return err
	}
	return config.WithFileLock(stateLockPath(), func() error {
		st, err := LoadState()
		if err != nil {
			return err
		}
		if err := fn(st); err != nil {
			return err
		}
		data, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			return err
		}
		tmp := StatePath() + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, StatePath())
	})
}

func (st *State) pr(key string) *PRState {
	p := st.PRs[key]
	if p == nil {
		p = &PRState{}
		st.PRs[key] = p
	}
	return p
}
