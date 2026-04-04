package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/version"
	"github.com/rxbynerd/stirrup/types"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:     "stirrup",
	Short:   "A coding agent harness",
	Long:    "Stirrup is a coding agent harness with swappable components that can be composed via RunConfig.",
	Version: version.Version(),
}

// Execute runs the root command. Called from main().
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// generateRunID creates a simple run identifier from the current timestamp.
func generateRunID() string {
	return fmt.Sprintf("run-%d", time.Now().UnixNano())
}

// printRunSummary writes a brief run summary to stderr.
func printRunSummary(runTrace *types.RunTrace) {
	fmt.Fprintf(os.Stderr, "\n--- Run complete ---\n")
	fmt.Fprintf(os.Stderr, "Outcome: %s\n", runTrace.Outcome)
	fmt.Fprintf(os.Stderr, "Turns: %d\n", runTrace.Turns)
	fmt.Fprintf(os.Stderr, "Tokens: %d in / %d out\n", runTrace.TokenUsage.Input, runTrace.TokenUsage.Output)
	fmt.Fprintf(os.Stderr, "Tool calls: %d\n", len(runTrace.ToolCalls))
	fmt.Fprintf(os.Stderr, "Duration: %s\n", runTrace.CompletedAt.Sub(runTrace.StartedAt).Round(time.Millisecond))
}

// setupSignalHandler installs SIGINT/SIGTERM handlers that cancel the given context.
func setupSignalHandler(cancel context.CancelFunc) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nReceived interrupt, shutting down...")
		cancel()
	}()
}
