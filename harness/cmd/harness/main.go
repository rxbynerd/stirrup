// Command harness is the CLI entrypoint for the stirrup coding harness.
// It builds a RunConfig from flags/environment variables, constructs the
// component graph via the factory, and runs the agentic loop.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/core"
	"github.com/rxbynerd/stirrup/types"
)

func main() {
	// CLI flags.
	mode := flag.String("mode", "execution", "Run mode: execution, planning, review, research, toil")
	model := flag.String("model", "claude-sonnet-4-6", "Model to use")
	providerType := flag.String("provider", "anthropic", "Provider type: anthropic")
	apiKeyRef := flag.String("api-key-ref", "secret://ANTHROPIC_API_KEY", "Secret reference for API key")
	workspace := flag.String("workspace", "", "Workspace directory (default: current directory)")
	maxTurns := flag.Int("max-turns", 20, "Maximum agentic loop turns")
	timeout := flag.Int("timeout", 600, "Wall-clock timeout in seconds")
	tracePath := flag.String("trace", "", "Path to JSONL trace file (optional)")
	transportType := flag.String("transport", "stdio", "Transport type: stdio, grpc")
	transportAddr := flag.String("transport-addr", "", "gRPC target address (required when transport is grpc)")
	prompt := flag.String("prompt", "", "User prompt (reads from stdin if empty)")
	flag.Parse()

	// Read prompt from remaining args or stdin.
	userPrompt := *prompt
	if userPrompt == "" && flag.NArg() > 0 {
		userPrompt = flag.Arg(0)
	}
	if userPrompt == "" {
		fmt.Fprintln(os.Stderr, "Usage: harness [flags] <prompt>")
		fmt.Fprintln(os.Stderr, "  or:  harness [flags] -prompt 'your prompt here'")
		os.Exit(1)
	}

	// Build RunConfig from flags.
	config := &types.RunConfig{
		RunID: generateRunID(),
		Mode:  *mode,
		Prompt: userPrompt,
		Provider: types.ProviderConfig{
			Type:      *providerType,
			APIKeyRef: *apiKeyRef,
		},
		ModelRouter: types.ModelRouterConfig{
			Type:     "static",
			Provider: *providerType,
			Model:    *model,
		},
		PromptBuilder:   types.PromptBuilderConfig{Type: "default"},
		ContextStrategy: types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
		Executor:        types.ExecutorConfig{Type: "local", Workspace: *workspace},
		EditStrategy:    types.EditStrategyConfig{Type: "whole-file"},
		Verifier:        types.VerifierConfig{Type: "none"},
		GitStrategy:     types.GitStrategyConfig{Type: "none"},
		Transport:       types.TransportConfig{Type: *transportType, Address: *transportAddr},
		TraceEmitter:    types.TraceEmitterConfig{Type: "jsonl", FilePath: *tracePath},
		MaxTurns:        *maxTurns,
		Timeout:         timeout,
	}

	// Set permission policy based on mode.
	readOnlyModes := map[string]bool{
		"planning": true, "review": true, "research": true, "toil": true,
	}
	if readOnlyModes[config.Mode] {
		config.PermissionPolicy = types.PermissionPolicyConfig{Type: "deny-side-effects"}
	} else {
		config.PermissionPolicy = types.PermissionPolicyConfig{Type: "allow-all"}
	}

	// Create context with timeout and signal handling.
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeout)*time.Second)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\nReceived interrupt, shutting down...")
		cancel()
	}()

	// Build and run the agentic loop.
	loop, err := core.BuildLoop(ctx, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error building harness: %v\n", err)
		os.Exit(1)
	}

	runTrace, err := loop.Run(ctx, config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error running harness: %v\n", err)
		os.Exit(1)
	}

	// Print summary to stderr.
	fmt.Fprintf(os.Stderr, "\n--- Run complete ---\n")
	fmt.Fprintf(os.Stderr, "Outcome: %s\n", runTrace.Outcome)
	fmt.Fprintf(os.Stderr, "Turns: %d\n", runTrace.Turns)
	fmt.Fprintf(os.Stderr, "Tokens: %d in / %d out\n", runTrace.TokenUsage.Input, runTrace.TokenUsage.Output)
	fmt.Fprintf(os.Stderr, "Tool calls: %d\n", len(runTrace.ToolCalls))
	fmt.Fprintf(os.Stderr, "Duration: %s\n", runTrace.CompletedAt.Sub(runTrace.StartedAt).Round(time.Millisecond))
}

// generateRunID creates a simple run identifier from the current timestamp.
func generateRunID() string {
	return fmt.Sprintf("run-%d", time.Now().UnixNano())
}
