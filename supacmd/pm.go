package supacmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/panamafrancis/supatree/pkg/supatree"
	"github.com/panamafrancis/workbench/pkg/zellij"
)

var pmCmd = &cobra.Command{
	Use:   "pm [name]",
	Short: "Open a PM agent — a standing agent that manages supatrees",
	Long: "pm opens (or focuses) a PM: an agent rooted at ~/.local/state/supatree/pm that can\n" +
		"see every supatree at once — what is blocked, what reviewers said, who is working\n" +
		"where. With no name it opens the top PM. It is optional — nothing else depends on\n" +
		"one running.\n\n" +
		"There can be several (pm new / pm rm). They share one directory and resume their\n" +
		"own conversations. The top PM reads every request addressed to no PM in\n" +
		"particular; the others read only what is addressed to them.\n\n" +
		"A PM is not rooted in a supatree, because one that manages many cannot live in\n" +
		"one of them. Its sandbox allows its own state, every tree's state and the\n" +
		"stack repos, and nothing inside any tree — it coordinates work rather than\n" +
		"doing it, and asks the watcher to create or remove trees. Press P in the\n" +
		"supatree sidebar for the top PM, or enter on any PM row.",
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !zellij.IsInZellij() {
			return fmt.Errorf("run this inside a supatree session (supatree start)")
		}
		name := ""
		if len(args) > 0 {
			name = args[0]
		}
		// Singleton per PM by tab name, not by file lock: OpenOrFocusTab focuses
		// the live tab rather than opening a second one, and the PM tab names are
		// reserved so no supatree can collide with them. A lock would be theatre
		// — nothing in this process stays alive to hold one on the agent's behalf.
		_, err := supatree.OpenPM(stCfg, supatreeWorkspace(), stCfg.ResolveSidebarWidth(), name)
		return err
	},
}

var pmNewModel string

var pmNewCmd = &cobra.Command{
	Use:   "new <name>",
	Short: "Add a PM below the existing ones, and open it",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		inZellij := zellij.IsInZellij()
		p, err := supatree.NewPM(stCfg, supatreeWorkspace(), stCfg.ResolveSidebarWidth(), args[0], pmNewModel, inZellij)
		if err != nil {
			return err
		}
		if !inZellij {
			fmt.Printf("added PM %s; open it with: supatree pm %s\n", p.Name, p.Name)
		}
		return nil
	},
}

var pmRmCmd = &cobra.Command{
	Use:   "rm <name>",
	Short: "Remove a PM and close its tab",
	Long: "rm drops a PM from the list and closes its tab. Its transcript is kept. The\n" +
		"last PM cannot be removed; removing the top PM makes the next one the top.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := supatree.RemovePM(args[0])
		if err != nil {
			return err
		}
		for _, w := range supatree.ClosePMTab(supatreeWorkspace(), p) {
			fmt.Println("warning:", w)
		}
		fmt.Println("removed PM", p.Name)
		return nil
	},
}

var pmLsCmd = &cobra.Command{
	Use:   "ls",
	Short: "List the PMs, top first",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		pms, err := supatree.LoadPMs()
		if err != nil {
			return err
		}
		for i, p := range pms {
			top := ""
			if i == 0 {
				top = "  (top)"
			}
			pending := ""
			if reqs, _, err := supatree.PendingRequests(p.Name); err == nil && len(reqs) > 0 {
				pending = fmt.Sprintf("  ✉%d", len(reqs))
			}
			fmt.Printf("%-16s %-12s %s%s%s\n", p.Name, p.AgentID(), p.Tab(), pending, top)
		}
		return nil
	},
}

var pmRequestFrom string

var requestCmd = &cobra.Command{
	Use:   "request <text>...",
	Short: "Queue something for a PM agent to look at",
	Long: "request appends to the PM's request queue (~/.local/state/supatree/requests/), which it reads at the top\n" +
		"of each turn. It goes to the top PM unless --to names another. Anything that can\n" +
		"append to a file can raise one — the sidebar,\n" +
		"the watcher, a shell — which is what lets Go processes reach an agent at all.\n\n" +
		"The PM need not be running: the queue is read from a stored offset, so one\n" +
		"that has been down for a day catches up rather than losing the backlog.",
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if pmRequestTo != "" {
			if _, err := supatree.ResolvePM(pmRequestTo); err != nil {
				return err
			}
		}
		req := supatree.Request{From: pmRequestFrom, Tree: pmRequestTree, To: pmRequestTo, Text: strings.Join(args, " ")}
		if err := supatree.AppendRequest(req); err != nil {
			return err
		}
		fmt.Println("queued for the PM")
		return nil
	},
}

var (
	pmRequestTree string
	pmRequestTo   string
)

func init() {
	pmNewCmd.Flags().StringVar(&pmNewModel, "model", "", "model entry to run this PM under (default: the PM model)")
	pmCmd.AddCommand(pmNewCmd, pmRmCmd, pmLsCmd)
	requestCmd.Flags().StringVar(&pmRequestTo, "to", "", "PM the request is for (default: the top PM)")
	requestCmd.Flags().StringVar(&pmRequestFrom, "from", "cli", "who is asking")
	requestCmd.Flags().StringVar(&pmRequestTree, "tree", "", "supatree the request concerns")
}
