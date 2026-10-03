package supacmd

import (
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/panamafrancis/supatree/pkg/supatree"
)

var (
	agentExecNono    string
	agentExecShimDir string
)

// agentExecCmd is what an agent pane's `nono` shim runs. It needs no config and
// must work on any layout: it only stands between the pane and nono.
var agentExecCmd = &cobra.Command{
	Use:               "agent-exec --nono <path> [--shim-dir <dir>] -- <nono args>",
	Short:             "Run an agent pane's nono, keeping the pane open if it fails at once",
	Hidden:            true,
	PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
	Args:              cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if agentExecNono == "" {
			return errors.New("agent-exec: --nono is required")
		}
		os.Exit(supatree.AgentExec(supatree.AgentExecOptions{
			Nono:    agentExecNono,
			ShimDir: agentExecShimDir,
			Args:    args,
			Stdin:   os.Stdin,
			Stdout:  os.Stdout,
			Stderr:  os.Stderr,
		}))
		return nil
	},
}

func init() {
	agentExecCmd.Flags().StringVar(&agentExecNono, "nono", "", "the real nono binary")
	agentExecCmd.Flags().StringVar(&agentExecShimDir, "shim-dir", "", "the shim's directory, dropped from PATH")
	rootCmd.AddCommand(agentExecCmd)
}
