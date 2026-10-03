package supacmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/panamafrancis/supatree/pkg/supatree"
	"github.com/panamafrancis/workbench/pkg/setup"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check that supatree can launch agents: zellij, git, nono and every model's nono profile",
	// Runs on any layout: it is where someone stuck mid-upgrade looks first.
	PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
	RunE: func(cmd *cobra.Command, args []string) error {
		var cfg *supatree.Config
		var results []setup.CheckResult
		if supatree.OldLayout() {
			results = append(results, setup.CheckResult{Name: "layout", Status: setup.StatusFail,
				Message: "still ~/.supatree", Hint: supatree.OldLayoutMessage})
		} else if c, err := supatree.Load(); err != nil {
			results = append(results, setup.CheckResult{Name: "config", Status: setup.StatusFail, Message: err.Error()})
		} else {
			cfg = c
		}
		results = append(results, supatree.Doctor(cfg)...)
		for _, r := range results {
			msg := strings.ReplaceAll(r.Message, "\n", "\n        ")
			fmt.Printf("%-5s %s: %s\n", r.Status, r.Name, msg)
			if r.Hint != "" {
				fmt.Printf("      → %s\n", r.Hint)
			}
		}
		if setup.HasHardFailures(results) {
			return errors.New("doctor found problems")
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}
