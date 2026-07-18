package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/harness/internal/core"
	"github.com/rxbynerd/stirrup/harness/internal/health"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
	"github.com/rxbynerd/stirrup/types/version"
)

var jobCmd = &cobra.Command{
	Use:   "job",
	Short: "Run as a Kubernetes job connected to a control plane",
	Long: `Run the stirrup harness as a Kubernetes job. Connects to a control plane
via gRPC, waits for a task_assignment event containing the RunConfig, then
runs the agentic loop with the pre-established transport.

Required environment variables:
  CONTROL_PLANE_ADDR          gRPC address of the control plane
  CONTROL_PLANE_SESSION_ID    Session ID for correlation (optional)
  STIRRUP_FOLLOWUP_GRACE      Follow-up grace period in seconds (optional)`,
	Args: cobra.NoArgs,
	RunE: runJob,
}

func init() {
	rootCmd.AddCommand(jobCmd)
}

func runJob(cmd *cobra.Command, args []string) error {
	addr := os.Getenv("CONTROL_PLANE_ADDR")
	if addr == "" {
		return fmt.Errorf("CONTROL_PLANE_ADDR environment variable is required")
	}

	// shutdownCtx carries only the process-level shutdown signal, independent
	// of ctx below, so the detached postRun hook phase still observes it.
	// See docs/cloud-run-jobs.md.
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	defer shutdownCancel()

	// Timeout is applied later once the RunConfig (which carries it) arrives.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setupSignalHandler(func() {
		shutdownCancel()
		cancel()
	})

	tp, err := transport.NewGRPCTransport(ctx, addr)
	if err != nil {
		return fmt.Errorf("failed to connect to control plane at %s: %w", addr, err)
	}
	defer func() { _ = tp.Close() }()

	// Session ID (if set) lets the control plane correlate this stream with
	// the session that launched the subprocess.
	sessionID := os.Getenv("CONTROL_PLANE_SESSION_ID")
	if err := tp.Emit(types.HarnessEvent{Type: "ready", ID: sessionID, HarnessVersion: version.Version()}); err != nil {
		return fmt.Errorf("failed to send ready event: %w", err)
	}

	if err := health.WriteProbe("/tmp/healthy"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to write health probe: %v\n", err)
	}
	defer func() { _ = health.RemoveProbe("/tmp/healthy") }()

	// A cancel event before any task_assignment exits cleanly — the control
	// plane may want to abort a pod that hasn't been dispatched yet.
	configCh := make(chan *types.RunConfig, 1)
	preTaskCancelCh := make(chan struct{}, 1)
	tp.OnControl(func(event types.ControlEvent) {
		switch event.Type {
		case "task_assignment":
			if event.Task != nil {
				select {
				case configCh <- event.Task:
				default:
					// Already received a task; ignore duplicates.
				}
			}
		case "cancel":
			select {
			case preTaskCancelCh <- struct{}{}:
			default:
			}
		}
	})

	assignTimer := time.NewTimer(5 * time.Minute)
	defer assignTimer.Stop()

	var config *types.RunConfig
	select {
	case config = <-configCh:
		// Assignment received.
	case <-preTaskCancelCh:
		fmt.Fprintln(os.Stderr, "cancel received before task assignment; exiting")
		return nil
	case <-assignTimer.C:
		return fmt.Errorf("no task assignment received within 5 minutes")
	case <-tp.Done():
		return fmt.Errorf("gRPC stream closed before receiving task assignment")
	case <-ctx.Done():
		return fmt.Errorf("interrupted before receiving task assignment")
	}

	if config.Timeout != nil && *config.Timeout > 0 {
		var timeoutCancel context.CancelFunc
		ctx, timeoutCancel = context.WithTimeout(ctx, time.Duration(*config.Timeout)*time.Second)
		defer timeoutCancel()
	}

	loop, err := core.BuildLoopWithTransport(ctx, config, tp)
	if err != nil {
		return fmt.Errorf("building harness: %w", err)
	}
	defer func() { _ = loop.Close() }()

	loop.Shutdown = shutdownCtx
	stopShutdownWatchdog := armShutdownWatchdog(shutdownCtx, loop, shutdownCloseGrace)
	defer stopShutdownWatchdog()

	runTrace, runErr := loop.Run(ctx, config)
	if runTrace == nil {
		// No trace was produced at all (e.g. the trace emitter itself failed).
		return fmt.Errorf("running harness: %w", runErr)
	}
	printRunSummary(runTrace)
	// Fresh context: ctx may already be cancelled by a SIGTERM here, and a
	// remote sink would otherwise silently drop the result on every
	// cancelled run.
	emitCtx, emitCancel := context.WithTimeout(context.Background(), postRunEmitTimeout)
	defer emitCancel()
	emitRunResult(emitCtx, config, runTrace)

	if runErr != nil {
		// A trace can exist alongside a fatal runErr (e.g. setup or a fatal
		// preRun hook failed); the emission above must still happen, but
		// there is nothing further to do for a failed run.
		return fmt.Errorf("running harness: %w", runErr)
	}

	// The control plane decides the export URI via
	// RunConfig.Executor.WorkspaceExportTo; upload failure is non-fatal so
	// an exit-failing job doesn't lose the trace/resultSink before an
	// operator can correlate it.
	exportCtx, exportCancel := context.WithTimeout(context.Background(), postRunExportTimeout)
	defer exportCancel()
	if err := exportWorkspace(exportCtx, config, false); err != nil {
		// Unreachable in the non-required path; guards the signature.
		return err
	}

	graceSecs := 0
	if config.FollowUpGrace != nil && *config.FollowUpGrace > 0 {
		graceSecs = *config.FollowUpGrace
	} else if v := os.Getenv("STIRRUP_FOLLOWUP_GRACE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			graceSecs = n
		}
	}
	if graceSecs > 0 {
		core.RunFollowUpLoop(ctx, loop, config, graceSecs)
	}

	// A non-success outcome (runErr == nil) must still fail the process so
	// the job orchestrator can decide whether to retry or alert.
	return runOutcomeError(runTrace)
}
