package cmd

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/harness/internal/core"
	"github.com/rxbynerd/stirrup/types"
)

// harnessCLIOptions captures every CLI-surfaced setting that influences the
// RunConfig built by buildHarnessRunConfig. Extracted so the construction
// path is testable without booting cobra.
type harnessCLIOptions struct {
	RunID         string
	Mode          string
	Prompt        string
	ProviderType  string
	APIKeyRef     string
	Model         string
	Workspace     string
	MaxTurns      int
	Timeout       int
	TracePath     string
	TransportType string
	TransportAddr string
	FollowUpGrace int
	LogLevel      string
}

// buildHarnessRunConfig assembles the RunConfig used by `stirrup harness`.
// It is the single place that encodes defaults such as the per-mode
// permission policy and the fall-back built-in tool list required by
// read-only modes. Kept pure so tests can exercise every --mode value
// without invoking the agentic loop.
func buildHarnessRunConfig(opts harnessCLIOptions) *types.RunConfig {
	timeout := opts.Timeout
	config := &types.RunConfig{
		RunID:  opts.RunID,
		Mode:   opts.Mode,
		Prompt: opts.Prompt,
		Provider: types.ProviderConfig{
			Type:      opts.ProviderType,
			APIKeyRef: opts.APIKeyRef,
		},
		ModelRouter: types.ModelRouterConfig{
			Type:     "static",
			Provider: opts.ProviderType,
			Model:    opts.Model,
		},
		PromptBuilder:   types.PromptBuilderConfig{Type: "default"},
		ContextStrategy: types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
		Executor:        types.ExecutorConfig{Type: "local", Workspace: opts.Workspace},
		EditStrategy:    types.EditStrategyConfig{Type: "whole-file"},
		Verifier:        types.VerifierConfig{Type: "none"},
		GitStrategy:     types.GitStrategyConfig{Type: "none"},
		Transport:       types.TransportConfig{Type: opts.TransportType, Address: opts.TransportAddr},
		TraceEmitter:    types.TraceEmitterConfig{Type: "jsonl", FilePath: opts.TracePath},
		MaxTurns:        opts.MaxTurns,
		Timeout:         &timeout,
		LogLevel:        opts.LogLevel,
	}
	if opts.FollowUpGrace > 0 {
		grace := opts.FollowUpGrace
		config.FollowUpGrace = &grace
	}

	// Set permission policy based on mode. Read-only modes additionally
	// need an explicit Tools.BuiltIn list so validation passes: the
	// validator rejects an empty list for read-only modes to force callers
	// to opt specific tools in rather than accidentally inheriting the
	// full set.
	if types.IsReadOnlyMode(config.Mode) {
		config.PermissionPolicy = types.PermissionPolicyConfig{Type: "deny-side-effects"}
		if len(config.Tools.BuiltIn) == 0 {
			config.Tools.BuiltIn = types.DefaultReadOnlyBuiltInTools()
		}
	} else {
		config.PermissionPolicy = types.PermissionPolicyConfig{Type: "allow-all"}
	}
	return config
}

var harnessCmd = &cobra.Command{
	Use:   "harness [flags] [prompt]",
	Short: "Run the coding agent harness",
	Long: `Run the stirrup coding agent harness. The prompt can be provided as a
positional argument or via the --prompt flag.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHarness,
}

func init() {
	rootCmd.AddCommand(harnessCmd)

	f := harnessCmd.Flags()
	f.StringP("mode", "m", "execution", "Run mode: execution, planning, review, research, toil")
	f.String("model", "claude-sonnet-4-6", "Model to use")
	f.String("provider", "anthropic", "Provider type: anthropic, bedrock, openai")
	f.String("api-key-ref", "secret://ANTHROPIC_API_KEY", "Secret reference for API key")
	f.StringP("workspace", "w", "", "Workspace directory (default: current directory)")
	f.Int("max-turns", 20, "Maximum agentic loop turns")
	f.Int("timeout", 600, "Wall-clock timeout in seconds")
	f.String("trace", "", "Path to JSONL trace file (optional)")
	f.String("transport", "stdio", "Transport type: stdio, grpc")
	f.String("transport-addr", "", "gRPC target address (required when transport is grpc)")
	f.Int("followup-grace", 0, "Seconds to keep gRPC transport open for follow-up requests (0 = disabled; env: STIRRUP_FOLLOWUP_GRACE)")
	f.String("log-level", "info", "Log level: debug, info, warn, error")
	f.String("prompt", "", "User prompt (can also be passed as a positional argument)")
}

func runHarness(cmd *cobra.Command, args []string) error {
	f := cmd.Flags()

	// Resolve prompt: --prompt flag takes priority, then positional arg.
	prompt, _ := f.GetString("prompt")
	if prompt == "" && len(args) > 0 {
		prompt = args[0]
	}
	if prompt == "" {
		return fmt.Errorf("prompt is required: pass as argument or use --prompt flag")
	}

	mode, _ := f.GetString("mode")
	model, _ := f.GetString("model")
	providerType, _ := f.GetString("provider")
	apiKeyRef, _ := f.GetString("api-key-ref")
	workspace, _ := f.GetString("workspace")
	maxTurns, _ := f.GetInt("max-turns")
	timeout, _ := f.GetInt("timeout")
	tracePath, _ := f.GetString("trace")
	transportType, _ := f.GetString("transport")
	transportAddr, _ := f.GetString("transport-addr")
	followUpGrace, _ := f.GetInt("followup-grace")
	logLevel, _ := f.GetString("log-level")

	// Allow the env var to set the follow-up grace when the flag is not provided.
	if followUpGrace == 0 {
		if v := os.Getenv("STIRRUP_FOLLOWUP_GRACE"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				followUpGrace = n
			}
		}
	}

	config := buildHarnessRunConfig(harnessCLIOptions{
		RunID:         generateRunID(),
		Mode:          mode,
		Prompt:        prompt,
		ProviderType:  providerType,
		APIKeyRef:     apiKeyRef,
		Model:         model,
		Workspace:     workspace,
		MaxTurns:      maxTurns,
		Timeout:       timeout,
		TracePath:     tracePath,
		TransportType: transportType,
		TransportAddr: transportAddr,
		FollowUpGrace: followUpGrace,
		LogLevel:      logLevel,
	})

	// Create context with timeout and signal handling.
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()
	setupSignalHandler(cancel)

	// Build and run the agentic loop.
	loop, err := core.BuildLoop(ctx, config)
	if err != nil {
		return fmt.Errorf("building harness: %w", err)
	}
	defer func() { _ = loop.Close() }()

	runTrace, err := loop.Run(ctx, config)
	if err != nil {
		return fmt.Errorf("running harness: %w", err)
	}
	printRunSummary(runTrace)

	// If a follow-up grace period is configured, keep the transport open and
	// re-run the loop for each user_response control event received within the
	// window.
	if config.FollowUpGrace != nil && *config.FollowUpGrace > 0 {
		core.RunFollowUpLoop(ctx, loop, config, *config.FollowUpGrace)
	}

	return nil
}
