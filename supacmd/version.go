package supacmd

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"

	"github.com/panamafrancis/workbench/pkg/version"
)

var versionCmd = &cobra.Command{
	Use:               "version",
	Short:             "Print the supatree version",
	PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
	RunE: func(*cobra.Command, []string) error {
		fmt.Printf("supatree %s\n", version.Version)
		if wb := workbenchVersion(); wb != "" {
			fmt.Printf("built against workbench %s\n", wb)
		}
		return nil
	},
}

// workbenchVersion is the version of the workbench module this binary was
// built against — what its shared packages are — from the build info.
func workbenchVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, d := range info.Deps {
		if d.Path == "github.com/panamafrancis/workbench" {
			if d.Version == "(devel)" {
				return "from a local checkout (go.work)"
			}
			if d.Replace != nil {
				return d.Version + " (replaced by " + d.Replace.Path + ")"
			}
			return d.Version
		}
	}
	return ""
}
