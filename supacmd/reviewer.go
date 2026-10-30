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

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func init() {
	reviewerCmd.AddCommand(reviewerStatusCmd, reviewerForgetCmd)
	rootCmd.AddCommand(reviewerCmd)
}
