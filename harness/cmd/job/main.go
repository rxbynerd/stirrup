// Command job is the K8s job entrypoint for the stirrup coding harness.
// It connects to a control plane via gRPC, waits for a task_assignment
// event containing the RunConfig, then runs the agentic loop with the
// pre-established transport.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/core"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

func main() {
	addr := os.Getenv("CONTROL_PLANE_ADDR")
	if addr == "" {
		fmt.Fprintln(os.Stderr, "Fatal: CONTROL_PLANE_ADDR environment variable is required")
		os.Exit(1)
	}

	// Top-level context with signal handling. The timeout is applied later
	// once we receive the RunConfig (which carries the wall-clock timeout).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nReceived interrupt, shutting down...")
		cancel()
	}()

	// 1. Dial the control plane.
	tp, err := transport.NewGRPCTransport(ctx, addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Fatal: failed to connect to control plane at %s: %v\n", addr, err)
		os.Exit(1)
	}
	defer tp.Close()

	// 2. Send a "ready" event so the control plane knows we are listening.
	if err := tp.Emit(types.HarnessEvent{Type: "ready"}); err != nil {
		fmt.Fprintf(os.Stderr, "Fatal: failed to send ready event: %v\n", err)
		os.Exit(1)
	}

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

	var config *types.RunConfig
	select {
	case config = <-configCh:
		// Got our assignment.
	case <-tp.Done():
		fmt.Fprintln(os.Stderr, "Fatal: gRPC stream closed before receiving task assignment")
		os.Exit(1)
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "Fatal: interrupted before receiving task assignment")
		os.Exit(1)
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
		fmt.Fprintf(os.Stderr, "Error building harness: %v\n", err)
		os.Exit(1)
	}

	runTrace, err := loop.Run(ctx, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error running harness: %v\n", err)
		os.Exit(1)
	}

	// Print summary to stderr (useful for K8s pod logs).
	fmt.Fprintf(os.Stderr, "\n--- Run complete ---\n")
	fmt.Fprintf(os.Stderr, "Outcome: %s\n", runTrace.Outcome)
	fmt.Fprintf(os.Stderr, "Turns: %d\n", runTrace.Turns)
	fmt.Fprintf(os.Stderr, "Tokens: %d in / %d out\n", runTrace.TokenUsage.Input, runTrace.TokenUsage.Output)
	fmt.Fprintf(os.Stderr, "Tool calls: %d\n", len(runTrace.ToolCalls))
	fmt.Fprintf(os.Stderr, "Duration: %s\n", runTrace.CompletedAt.Sub(runTrace.StartedAt).Round(time.Millisecond))
}
