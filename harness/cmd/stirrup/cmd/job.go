package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/core"
	"github.com/rxbynerd/stirrup/harness/internal/health"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
	"github.com/spf13/cobra"
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

	// Top-level context with signal handling. The timeout is applied later
	// once we receive the RunConfig (which carries the wall-clock timeout).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setupSignalHandler(cancel)

	// 1. Dial the control plane.
	tp, err := transport.NewGRPCTransport(ctx, addr)
	if err != nil {
		return fmt.Errorf("failed to connect to control plane at %s: %w", addr, err)
	}
	defer tp.Close()

	// 2. Send a "ready" event so the control plane knows we are listening.
	//    Include the session ID (if set) so the control plane can correlate
	//    this gRPC stream back to the session that launched the subprocess.
	sessionID := os.Getenv("CONTROL_PLANE_SESSION_ID")
	if err := tp.Emit(types.HarnessEvent{Type: "ready", ID: sessionID}); err != nil {
		return fmt.Errorf("failed to send ready event: %w", err)
	}

	// Write the liveness probe file so K8s knows we are healthy.
	if err := health.WriteProbe("/tmp/healthy"); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to write health probe: %v\n", err)
	}
	defer health.RemoveProbe("/tmp/healthy")

	// 3. Register OnControl and block until a task_assignment arrives.
	configCh := make(chan *types.RunConfig, 1)
	tp.OnControl(func(event types.ControlEvent) {
		if event.Type == "task_assignment" && event.Task != nil {
			select {
			case configCh <- event.Task:
			default:
				// Already received a task; ignore duplicates.
			}
		}
	})

	assignTimer := time.NewTimer(5 * time.Minute)
	defer assignTimer.Stop()

	var config *types.RunConfig
	select {
	case config = <-configCh:
		// Got our assignment.
	case <-assignTimer.C:
		return fmt.Errorf("no task assignment received within 5 minutes")
	case <-tp.Done():
		return fmt.Errorf("gRPC stream closed before receiving task assignment")
	case <-ctx.Done():
		return fmt.Errorf("interrupted before receiving task assignment")
	}

	// 4. Apply wall-clock timeout from the RunConfig.
	if config.Timeout != nil && *config.Timeout > 0 {
		var timeoutCancel context.CancelFunc
		ctx, timeoutCancel = context.WithTimeout(ctx, time.Duration(*config.Timeout)*time.Second)
		defer timeoutCancel()
	}

	// 5. Build and run the agentic loop, reusing the existing gRPC transport.
	loop, err := core.BuildLoopWithTransport(ctx, config, tp)
	if err != nil {
		return fmt.Errorf("building harness: %w", err)
	}
	defer loop.Close()

	runTrace, err := loop.Run(ctx, config)
	if err != nil {
		return fmt.Errorf("running harness: %w", err)
	}
	printRunSummary(runTrace)

	// Honour follow-up grace from the RunConfig (set by the control plane) or
	// fall back to the STIRRUP_FOLLOWUP_GRACE environment variable.
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

	return nil
}
