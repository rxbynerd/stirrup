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

// livenessMarkerPath and readinessMarkerPath are the health.WriteProbe
// targets runJob uses. Tests override these to a t.TempDir()-scoped path
// so they don't race a real /tmp/healthy or /tmp/ready on the host.
var (
	livenessMarkerPath  = health.LivenessMarker
	readinessMarkerPath = health.ReadinessMarker
)

// buildJobLoop constructs the job's loop; tests replace it to land a
// control stream loss during construction.
var buildJobLoop = core.BuildLoopWithTransport

func runJob(cmd *cobra.Command, args []string) error {
	started := time.Now()

	addr := os.Getenv("CONTROL_PLANE_ADDR")
	if addr == "" {
		return fmt.Errorf("CONTROL_PLANE_ADDR environment variable is required")
	}

	// The first SIGINT/SIGTERM cancels ctx and so the run; detached
	// post-run work keeps its bounded budget until a second signal
	// cancels shutdownCtx, or until the shutdown watchdog armed on the
	// first signal closes the loop ahead of the orchestrator's SIGKILL.
	// See docs/cloud-run-jobs.md.
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	defer shutdownCancel()

	// Timeout is applied later once the RunConfig (which carries it) arrives.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setupSignalHandler(cancel, shutdownCancel)

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

	if err := health.WriteProbe(livenessMarkerPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to write liveness probe: %v\n", err)
	}
	defer func() { _ = health.RemoveProbe(livenessMarkerPath) }()

	// Readiness spans only the assignment wait below: it signals the pod
	// is idle and assignable, not merely alive. It is dropped the moment
	// a task arrives, since the pod then runs one task to completion
	// rather than accepting more work.
	if err := health.WriteProbe(readinessMarkerPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to write readiness probe: %v\n", err)
	}
	defer func() { _ = health.RemoveProbe(readinessMarkerPath) }()

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
	var assigned time.Duration
	select {
	case config = <-configCh:
		assigned = time.Since(started)
		// Assignment received; the pod is no longer idle-and-assignable.
		_ = health.RemoveProbe(readinessMarkerPath)
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

	// The wall-clock budget covers component construction plus the
	// primary run. Follow-up runs derive their own budget from the
	// cancel-only ctx, so a long primary run cannot starve them.
	runTimeout := runTimeoutFor(config)
	runCtx, runCancel := withRunTimeout(ctx, runTimeout)
	defer runCancel()

	// A control stream lost during construction aborts it, and the job
	// fails rather than starting a run nobody is listening for.
	buildCtx, cancelBuild := context.WithCancelCause(runCtx)
	defer cancelBuild(nil)
	stopBuildWatch := watchControlStream(tp, cancelBuild)
	loop, err := buildJobLoop(buildCtx, config, tp)
	stopBuildWatch()
	if lossErr := controlStreamErr(tp); lossErr != nil {
		if loop != nil {
			_ = loop.Close()
		}
		lostErr := fmt.Errorf("building harness: %w: %w", core.ErrControlStreamLost, lossErr)
		emitTerminalFailure(tp, lostErr, "control_stream_lost")
		return lostErr
	}
	if err != nil {
		// No loop exists to run its own error/done emission, so without
		// this the control plane cannot tell a rejected config from a
		// crashed pod or a dropped connection.
		buildErr := fmt.Errorf("building harness: %w", err)
		buildFailed := time.Since(started)
		emitTerminalFailure(tp, buildErr, "error")
		// Phase timings for the terminal-failure path, so a control plane
		// reporting a missing "done" can name the phase that stalled.
		// emit_returned marks the handoff to the transport, not delivery;
		// the transport reports the close-time drain separately.
		fmt.Fprintf(os.Stderr,
			"job: terminal failure signalled; entry=%s assigned=+%s build_failed=+%s emit_returned=+%s\n",
			started.UTC().Format(time.RFC3339Nano),
			assigned.Round(time.Microsecond),
			buildFailed.Round(time.Microsecond),
			time.Since(started).Round(time.Microsecond))
		return buildErr
	}
	defer func() { _ = loop.Close() }()

	loop.Shutdown = shutdownCtx
	loop.Handoff = workspaceHandoff{primaryRunID: config.RunID}
	stopShutdownWatchdog := armShutdownWatchdog(ctx, loop, shutdownCloseGrace)
	defer stopShutdownWatchdog()

	policy := &postRunPolicy{
		emit: func(ctx context.Context, cfg *types.RunConfig, rt *types.RunTrace) {
			printRunSummary(rt)
			emitRunResult(ctx, cfg, rt)
		},
	}

	stopStreamWatch := watchControlStream(loop.Transport, loop.ControlStreamLost)
	defer stopStreamWatch()

	runTrace, runErr := loop.Run(runCtx, config)
	if err := policy.finalise(config, runTrace, runErr); err != nil {
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
	// Called even with no grace window so user_response events that
	// arrived after the run's last turn boundary are rejected with a
	// warning rather than lost at exit.
	core.RunFollowUpLoop(ctx, loop, config, graceSecs, core.FollowUpOptions{
		RunTimeout:    runTimeout,
		OnRunComplete: policy.finaliseFollowUp,
	})
	// A required export that fails on a follow-up fails the process the
	// same way it would for the primary run.
	if err := policy.followUpErr(); err != nil {
		return err
	}

	// A non-success outcome (runErr == nil) must still fail the process so
	// the job orchestrator can decide whether to retry or alert.
	return runOutcomeError(runTrace)
}

// emitTerminalFailure sends the "error" then "done" pair that the
// agentic loop emits on its own fatal paths, for failures that occur
// after a task assignment but before the loop exists to emit them.
// "done" is the control plane's terminal signal, so it must follow
// "error" even here; stopReason is its stop_reason.
//
// Best-effort: emit failures are reported on stderr and never replace
// the failure being signalled.
func emitTerminalFailure(tp transport.Transport, cause error, stopReason string) {
	if tp == nil {
		return
	}
	if err := tp.Emit(types.HarnessEvent{Type: "error", Message: cause.Error()}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to send error event: %v\n", err)
	}
	if err := tp.Emit(types.HarnessEvent{Type: "done", StopReason: stopReason}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to send done event: %v\n", err)
	}
}
