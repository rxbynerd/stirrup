package cmd

import (
	"context"
	"io"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/types"
	tracereader "github.com/rxbynerd/stirrup/types/trace"
)

var traceTailCmd = &cobra.Command{
	Use:   "tail <file>",
	Short: "Stream JSONL trace records as they are written",
	Long: `Stream JSONL trace records from a file in chronological order.

Without -f, tail prints every record currently in the file and exits
(equivalent to ` + "`stirrup trace show`" + ` minus the per-run framing).
With -f, tail keeps polling the file at --interval, printing records
as they are appended. SIGINT terminates the follow loop cleanly.

Pass ` + "`-`" + ` to read from stdin; the -f flag is ignored on stdin
because stdin already blocks until more bytes are written.`,
	Args: cobra.ExactArgs(1),
	RunE: runTraceTail,
}

func init() {
	traceCmd.AddCommand(traceTailCmd)
	addColorFlag(traceTailCmd)
	f := traceTailCmd.Flags()
	f.BoolP("follow", "f", false, "Keep tailing the file, printing records as they are appended (tail -f semantics).")
	f.Duration("interval", 100*time.Millisecond, "Polling interval when --follow is set. Defaults to 100ms; raise it to reduce CPU on a very quiet file.")
}

func runTraceTail(cmd *cobra.Command, args []string) error {
	mode, err := resolveColorMode(cmd)
	if err != nil {
		return err
	}
	follow, _ := cmd.Flags().GetBool("follow")
	interval, _ := cmd.Flags().GetDuration("interval")

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return runTraceTailWith(ctx, args[0], cmd.OutOrStdout(), mode, follow, interval)
}

func runTraceTailWith(ctx context.Context, path string, out io.Writer, mode colorMode, follow bool, interval time.Duration) error {
	color := shouldColor(mode, out)
	return tracereader.Tail(ctx, path, tracereader.TailOptions{
		Follow:       follow,
		PollInterval: interval,
	}, func(t *types.RunTrace) error {
		return renderTailRecord(out, t, color)
	})
}

// renderTailRecord emits a single record per RunTrace using the same
// shape as `trace show`. Unlike show, tail does not error on an empty
// stream — an empty follow-tail is normal while the run is still
// warming up.
func renderTailRecord(out io.Writer, t *types.RunTrace, color bool) error {
	return renderRunTrace(out, t, color)
}
