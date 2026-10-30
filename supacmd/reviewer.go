package supacmd

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/panamafrancis/supatree/pkg/supatree"
	"github.com/panamafrancis/supatree/pkg/supatree/reviewer"
	"github.com/panamafrancis/workbench/pkg/github"
)

var reviewerCmd = &cobra.Command{
	Use:   "reviewer",
	Short: "The automated reviewer the watcher runs",
	Long: "The automated reviewer picks up pull requests that request a review from\n" +
		"`reviewer.login`, groups the ones that make up one cross-repo change, reviews\n" +
		"each group in a review tree of its own with a headless agent, and re-reviews\n" +
		"when asked. It runs inside `supatree watch` (use --headless where there is no\n" +
		"zellij); these commands only inspect and correct what it remembers.",
}

var reviewerStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the pull requests the reviewer sees and what it did with them",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cache := github.NewCache(supatree.PRCachePath())
		_ = cache.Load()
		snap, s, err := reviewer.SnapshotFor(stCfg, cache)
		if err != nil {
			return err
		}
		st, err := reviewer.LoadState()
		if err != nil {
			return err
		}
		fmt.Printf("reviewing as @%s; %d pull request(s) watched, %d tree(s)\n\n", s.Login, len(snap.PRs), len(snap.Trees))
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "PULL REQUEST\tSTATE\tREQUESTED\tTREE\tLAST VERDICT\tNOTE")
		keys := make([]string, 0, len(snap.PRs))
		for k := range snap.PRs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := snap.PRs[k]
			ps := st.PRs[k]
			if ps == nil {
				ps = &reviewer.PRState{}
			}
			note := ps.Grouping
			switch {
			case !ps.GaveUpAt.IsZero():
				note = "gave up: " + ps.LastErr
			case ps.Attempts > 0:
				note = fmt.Sprintf("%d failed attempt(s): %s", ps.Attempts, ps.LastErr)
			case !ps.WaitingSince.IsZero():
				note = "waiting since " + ps.WaitingSince.Local().Format(time.DateTime)
			}
			verdict := strings.ToLower(ps.Verdict)
			if verdict == "" {
				verdict = "-"
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%v\t%s\t%s\t%s\n", k, p.Status, p.Requested, orDash(ps.Tree), verdict, note)
		}
		return tw.Flush()
	},
}

var reviewerForgetCmd = &cobra.Command{
	Use:   "forget <owner/repo#n>…",
	Short: "Drop what the reviewer remembers about pull requests",
	Long: "forget clears the reviewer's record of each pull request: failed attempts, a\n" +
		"give-up, and the last review it confirmed. A pull request still requesting a\n" +
		"review is then picked up again on the next round.",
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var keys []string
		for _, a := range args {
			repo, number, err := supatree.ParsePRRef(a)
			if err != nil {
				return err
			}
			keys = append(keys, reviewer.PRKey(repo, number))
		}
		return reviewer.MutateState(func(st *reviewer.State) error {
			for _, k := range keys {
				if _, ok := st.PRs[k]; !ok {
					fmt.Printf("%s: nothing remembered\n", k)
					continue
				}
				delete(st.PRs, k)
				fmt.Printf("%s: forgotten\n", k)
			}
			return nil
		})
	},
}

var (
	planNoFetch bool
	planNoLLM   bool
)

var reviewerPlanCmd = &cobra.Command{
	Use:   "plan",
	Short: "Dry run: show what the reviewer would do now, doing none of it",
	Long: "plan refreshes the watched repositories' pull request listings (unless\n" +
		"--no-fetch), then works out what a reviewer round would do — the trees it\n" +
		"would create or extend, the reviews it would run, the trees it would remove —\n" +
		"and how it groups the open pull requests into changes. It creates nothing,\n" +
		"posts nothing and records nothing, and it works with the reviewer disabled,\n" +
		"so a configuration can be tried before it is switched on.\n\n" +
		"Because nothing is recorded, a request plan sees for the first time is always\n" +
		"still waiting out group_window; the change it belongs to says when its tree\n" +
		"would be made.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cache := github.NewCache(supatree.PRCachePath())
		_ = cache.Load()
		s, err := reviewer.ResolveConfigured(stCfg)
		if err != nil {
			return err
		}
		state := "enabled"
		if stCfg.Reviewer != nil && !stCfg.Reviewer.Enabled {
			state = "disabled — showing what it would do if enabled"
		}
		fmt.Printf("reviewer as @%s (%s)\n", s.Login, state)
		if err := reviewer.CheckLogin(s.Login); err != nil {
			fmt.Printf("warning: %v\n", err)
		}
		if !planNoFetch {
			targets := reviewer.WatchTargets(stCfg, s)
			if len(targets) == 0 {
				fmt.Println("warning: no watched repository has a clone yet — register a stack (supatree stack clone) first")
			}
			if out := supatree.FetchPRs(targets, cache, false); out.Err != nil {
				fmt.Printf("warning: fetch incomplete: %v\n", out.Err)
			} else if out.Skipped {
				fmt.Println("note: another process held the PR cache; showing what it last fetched")
			}
		}
		snap, _, err := reviewer.SnapshotFor(stCfg, cache)
		if err != nil {
			return err
		}
		st, err := reviewer.LoadState()
		if err != nil {
			return err
		}
		var related reviewer.Related
		if !planNoLLM {
			related = reviewer.RelatedFor(stCfg, s, func(f string, a ...any) { fmt.Printf("warning: "+f+"\n", a...) })
		}
		p, err := reviewer.DryRun(snap, st, s, related)
		if err != nil {
			return err
		}
		fmt.Printf("%d pull request(s) watched, %d tree(s)\n\nWould do now:\n", len(snap.PRs), len(snap.Trees))
		if len(p.Actions) == 0 {
			fmt.Println("  nothing")
		}
		for _, a := range p.Actions {
			fmt.Println("  " + describeAction(a))
		}
		fmt.Println("\nChanges among the open pull requests:")
		if len(p.Groups) == 0 {
			fmt.Println("  none")
		}
		now := time.Now()
		for _, g := range p.Groups {
			var parts []string
			if g.Tree != "" {
				parts = append(parts, "in tree "+g.Tree)
			}
			if len(g.Waiting) > 0 {
				parts = append(parts, "waiting on a review: "+strings.Join(g.Waiting, ", "))
			} else {
				parts = append(parts, "no review requested")
			}
			if !g.ReadyAt.IsZero() {
				if g.ReadyAt.After(now) {
					parts = append(parts, fmt.Sprintf("tree would be made at %s (in %s)", g.ReadyAt.Local().Format(time.TimeOnly), g.ReadyAt.Sub(now).Round(time.Second)))
				} else {
					parts = append(parts, "tree is due")
				}
			}
			fmt.Printf("  %s\n      %s\n", strings.Join(g.PRs, " + "), strings.Join(parts, "; "))
			for _, k := range g.PRs {
				if r := g.Reasons[k]; r != "" {
					fmt.Printf("      %s: %s\n", k, r)
				}
			}
		}
		return nil
	},
}

func describeAction(a reviewer.Action) string {
	prs := strings.Join(a.PRs, ", ")
	switch a.Kind {
	case reviewer.ActCreate:
		return fmt.Sprintf("create a review tree in stack %s for %s", a.Stack, prs)
	case reviewer.ActAdd:
		return fmt.Sprintf("add %s to %s (%s)", prs, a.Tree, a.Reason)
	case reviewer.ActRun:
		return fmt.Sprintf("review %s in %s", prs, a.Tree)
	case reviewer.ActReap:
		return fmt.Sprintf("remove %s: %s", a.Tree, a.Reason)
	}
	return string(a.Kind)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func init() {
	reviewerPlanCmd.Flags().BoolVar(&planNoFetch, "no-fetch", false, "use the PR cache as it is, without refreshing it")
	reviewerPlanCmd.Flags().BoolVar(&planNoLLM, "no-llm", false, "skip the LLM grouping pass even when llm_grouping is on")
	reviewerCmd.AddCommand(reviewerStatusCmd, reviewerForgetCmd, reviewerPlanCmd)
	rootCmd.AddCommand(reviewerCmd)
}
