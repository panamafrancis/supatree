package reviewer

import (
	"fmt"
	"strings"
	"time"
)

// summarize is a one-line account of where the reviewer stands, for the
// watcher's log. The watcher logs it only when it changes, so a quiet log
// means nothing changed — not that the reviewer stopped.
func summarize(snap Snapshot, st *State, s Settings) string {
	inTree := treeIndex(snap)
	var waiting, gaveUp int
	var next time.Time
	for k, ps := range st.PRs {
		if ps.WaitingSince.IsZero() {
			continue
		}
		waiting++
		if !ps.GaveUpAt.IsZero() {
			gaveUp++
			continue
		}
		due := ps.LastAttempt.Add(backoff(ps.Attempts))
		if inTree[k] == "" {
			if w := ps.WaitingSince.Add(s.GroupWindow); w.After(due) {
				due = w
			}
		}
		if due.After(snap.Now) && (next.IsZero() || due.Before(next)) {
			next = due
		}
	}
	running := 0
	for _, on := range snap.Running {
		if on {
			running++
		}
	}
	if waiting == 0 && running == 0 {
		return "reviewer: nothing waiting"
	}
	parts := []string{plural(waiting, "pull request") + " waiting"}
	if running > 0 {
		parts = append(parts, plural(running, "review")+" running")
	}
	if gaveUp > 0 {
		parts = append(parts, fmt.Sprintf("%d given up on", gaveUp))
	}
	line := "reviewer: " + strings.Join(parts, ", ")
	if !next.IsZero() {
		line += ", next due " + next.Local().Format("15:04")
	}
	return line
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// verdictText is a review state as a log or notification says it:
// CHANGES_REQUESTED → "changes requested".
func verdictText(state string) string {
	return strings.ToLower(strings.ReplaceAll(state, "_", " "))
}
