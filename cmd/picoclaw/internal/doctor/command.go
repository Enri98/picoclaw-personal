package doctor

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// NewDoctorCommand returns the cobra command for preflight validation.
func NewDoctorCommand() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Validate deployment configuration and environment",
		Long: `doctor runs a series of read-only preflight checks against the
deployment configuration and the host environment. Each check reports
[ PASS ], [ FAIL ], or [ SKIP ]. The command exits with status 1 if any
check fails; 0 otherwise.`,
		Example: `picoclaw doctor --config deploy/config.yaml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if configPath == "" {
				// Default: look for config.yaml next to the binary, then in the
				// working directory.
				wd, _ := os.Getwd()
				candidates := []string{
					filepath.Join(wd, "config.yaml"),
					filepath.Join(wd, "deploy", "config.yaml"),
				}
				for _, c := range candidates {
					if _, err := os.Stat(c); err == nil {
						configPath = c
						break
					}
				}
				if configPath == "" {
					return fmt.Errorf("config file not found; use --config to specify the path")
				}
			}

			results := RunChecks(configPath)
			code := PrintResults(cmd.OutOrStdout(), results)
			if code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&configPath, "config", "", "path to the YAML deployment config (default: config.yaml or deploy/config.yaml in cwd)")

	return cmd
}
