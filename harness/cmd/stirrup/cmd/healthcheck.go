package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/harness/internal/health"
)

var healthcheckFile string

var healthcheckCmd = &cobra.Command{
	Use:   "healthcheck",
	Short: "Check a stirrup job health probe marker file",
	Long: `Check whether a health probe marker written by "stirrup job" is present
and readable, exiting 0 if so and non-zero otherwise. It exists because the
published harness image is distroless: it has no shell and no "test" binary,
so a Kubernetes exec probe cannot run "test -f /tmp/healthy" against it, but
it can exec the stirrup binary itself.

"stirrup job" maintains two markers with different lifetimes:

  /tmp/healthy  liveness  — present from just after the control-plane
                            connection until process exit (assignment
                            wait and task execution).
  /tmp/ready    readiness — present only while idle and waiting for a
                            task_assignment; removed once one arrives.

Example Pod probe config:

  livenessProbe:
    exec:
      command: ["/stirrup", "healthcheck", "--file=/tmp/healthy"]
  readinessProbe:
    exec:
      command: ["/stirrup", "healthcheck", "--file=/tmp/ready"]

See docs/deployment.md for the full recipe.`,
	Args: cobra.NoArgs,
	RunE: runHealthcheck,
}

func init() {
	healthcheckCmd.Flags().StringVar(&healthcheckFile, "file", health.LivenessMarker, "path to the health probe marker file to check")
	rootCmd.AddCommand(healthcheckCmd)
}

func runHealthcheck(_ *cobra.Command, _ []string) error {
	if err := health.CheckProbe(healthcheckFile); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("health probe marker %s not present", healthcheckFile)
		}
		return ioError(fmt.Errorf("health probe marker %s: %w", healthcheckFile, err))
	}
	return nil
}
