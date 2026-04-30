package cmd

import (
	"bytes"
	"context"
	"encoding/json"
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

	// Component-selection escape hatches. These let the caller steer the
	// non-trivial component choices without having to reach for a full
	// --config file. Empty strings fall back to the documented default
	// (local executor, multi edit strategy, none verifier, none git
	// strategy, jsonl trace emitter).
	ExecutorType     string
	EditStrategyType string
	VerifierType     string
	GitStrategyType  string
	TraceEmitterType string
	OTelEndpoint     string
}

// buildHarnessRunConfig assembles the RunConfig used by `stirrup harness`.
// It is the single place that encodes defaults such as the per-mode
// permission policy and the fall-back built-in tool list required by
// read-only modes. Kept pure so tests can exercise every --mode value
// without invoking the agentic loop.
func buildHarnessRunConfig(opts harnessCLIOptions) *types.RunConfig {
	timeout := opts.Timeout

	executorType := opts.ExecutorType
	if executorType == "" {
		executorType = "local"
	}
	editStrategyType := opts.EditStrategyType
	if editStrategyType == "" {
		// Default changed from "whole-file" to "multi" because the
		// multi-strategy edit tool is the highest-leverage configuration
		// for production use (see VERSION1.md). Existing callers that
		// still ask for write_file/search_replace/apply_diff are preserved
		// by core/factory.go::editToolEnabled, which aliases those names
		// to the multi-strategy's edit_file tool.
		editStrategyType = "multi"
	}
	verifierType := opts.VerifierType
	if verifierType == "" {
		verifierType = "none"
	}
	gitStrategyType := opts.GitStrategyType
	if gitStrategyType == "" {
		gitStrategyType = "none"
	}
	traceEmitterType := opts.TraceEmitterType
	if traceEmitterType == "" {
		traceEmitterType = "jsonl"
	}

	traceEmitter := types.TraceEmitterConfig{Type: traceEmitterType}
	switch traceEmitterType {
	case "jsonl":
		traceEmitter.FilePath = opts.TracePath
	case "otel":
		traceEmitter.Endpoint = opts.OTelEndpoint
	}

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
		Executor:        types.ExecutorConfig{Type: executorType, Workspace: opts.Workspace},
		EditStrategy:    types.EditStrategyConfig{Type: editStrategyType},
		Verifier:        types.VerifierConfig{Type: verifierType},
		GitStrategy:     types.GitStrategyConfig{Type: gitStrategyType},
		Transport:       types.TransportConfig{Type: opts.TransportType, Address: opts.TransportAddr},
		TraceEmitter:    traceEmitter,
		MaxTurns:        opts.MaxTurns,
		Timeout:         &timeout,
		LogLevel:        opts.LogLevel,
	}
	if opts.FollowUpGrace > 0 {
		grace := opts.FollowUpGrace
		config.FollowUpGrace = &grace
	}

	applyModeDefaults(config)
	return config
}

// applyModeDefaults fills in PermissionPolicy and the read-only Tools.BuiltIn
// list based on cfg.Mode, but only for fields the caller has not set
// explicitly. This is shared between the flag-only path (buildHarnessRunConfig)
// and the --config path (runHarness, after applyOverrides) so the two paths
// produce architecturally consistent configs.
//
// The function never strips an explicit configuration — if the caller set
// allow-all on a read-only mode, ValidateRunConfig will reject it with a
// clear error rather than this function silently rewriting the choice.
// That keeps user intent visible: a wrong combination fails loudly.
func applyModeDefaults(cfg *types.RunConfig) {
	if types.IsReadOnlyMode(cfg.Mode) {
		if cfg.PermissionPolicy.Type == "" {
			cfg.PermissionPolicy = types.PermissionPolicyConfig{Type: "deny-side-effects"}
		}
		// Read-only modes need an explicit Tools.BuiltIn list so validation
		// passes: the validator rejects an empty list for read-only modes
		// to force callers to opt specific tools in rather than accidentally
		// inheriting the full set.
		if len(cfg.Tools.BuiltIn) == 0 {
			cfg.Tools.BuiltIn = types.DefaultReadOnlyBuiltInTools()
		}
	} else if cfg.PermissionPolicy.Type == "" {
		cfg.PermissionPolicy = types.PermissionPolicyConfig{Type: "allow-all"}
	}
}

// maxConfigFileBytes caps the size of a --config file we will read into
// memory before parsing. A RunConfig is at most a few KB; anything in the
// MB range is almost certainly a mistake (a symlink to /dev/zero, a binary
// pasted into the path, etc.). The cap prevents OOM on a malformed input.
const maxConfigFileBytes int64 = 1 << 20 // 1 MiB

// loadRunConfigFile reads a JSON file at path and unmarshals it into a
// RunConfig. The file is expected to mirror the proto schema in
// proto/harness/v1/harness.proto (the canonical source of truth). Unknown
// JSON fields are rejected so that typos in the config file surface as
// errors rather than being silently ignored.
func loadRunConfigFile(path string) (*types.RunConfig, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("reading config file %q: is a directory", path)
	}
	if info.Size() > maxConfigFileBytes {
		return nil, fmt.Errorf("reading config file %q: %d bytes exceeds %d byte cap", path, info.Size(), maxConfigFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %q: %w", path, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("parsing config file %q: file is empty", path)
	}
	var cfg types.RunConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %q: %w", path, err)
	}
	return &cfg, nil
}

var harnessCmd = &cobra.Command{
	Use:   "harness [flags] [prompt]",
	Short: "Run the coding agent harness",
	Long: `Run the stirrup coding agent harness. The prompt can be provided as a
positional argument or via the --prompt flag.

Configuration precedence: a --config JSON file (if provided) populates the
full RunConfig; explicitly-set flags then override individual fields; flags
left at their default value do NOT override the file. When --config is not
provided, flags + defaults build the RunConfig directly.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHarness,
}

func init() {
	rootCmd.AddCommand(harnessCmd)

	f := harnessCmd.Flags()
	f.String("config", "", "Path to a JSON RunConfig file (mirrors proto/harness/v1/harness.proto). Explicit flags still override individual fields; unset flags do not.")
	f.StringP("mode", "m", "execution", "Run mode: execution, planning, review, research, toil")
	f.String("model", "claude-sonnet-4-6", "Model to use (sets ModelRouter.Model; for dynamic/per-mode routers in --config files this only sets the default-model field, not the cheap/expensive override)")
	f.String("provider", "anthropic", "Provider type: anthropic, bedrock, openai-compatible (Chat Completions), openai-responses (Responses API). The two OpenAI variants speak different wire formats and must be selected explicitly.")
	f.String("api-key-ref", "secret://ANTHROPIC_API_KEY", "Secret reference for API key")
	f.StringP("workspace", "w", "", "Workspace directory (default: current directory)")
	f.Int("max-turns", 20, "Maximum agentic loop turns")
	f.Int("timeout", 600, "Wall-clock timeout in seconds")
	f.String("trace", "", "Path to JSONL trace file (sets --trace-emitter to jsonl unless --trace-emitter is explicitly set)")
	f.String("transport", "stdio", "Transport type: stdio, grpc")
	f.String("transport-addr", "", "gRPC target address (required when transport is grpc)")
	f.Int("followup-grace", 0, "Seconds to keep gRPC transport open for follow-up requests (0 = disabled; env: STIRRUP_FOLLOWUP_GRACE)")
	f.String("log-level", "info", "Log level: debug, info, warn, error")
	f.String("prompt", "", "User prompt (can also be passed as a positional argument)")

	// Component-selection flags. Escape hatches for callers who don't want
	// a full --config file but need to switch a single component. These
	// are still honoured (as overrides) when --config is set.
	f.String("executor", "local", "Executor: local, container, api")
	f.String("edit-strategy", "multi", "Edit strategy: whole-file, search-replace, udiff, multi (composite available only via --config)")
	f.String("verifier", "none", "Verifier: none, test-runner, llm-judge (composite available only via --config)")
	f.String("git-strategy", "none", "Git strategy: none, deterministic")
	f.String("trace-emitter", "jsonl", "Trace emitter: jsonl, otel")
	f.String("otel-endpoint", "", "OTLP endpoint for the otel trace emitter (default: localhost:4317)")
}

// applyOverrides mutates cfg in place, replacing fields whose corresponding
// flag was explicitly set on the command line. Defaults (i.e. flags the
// user did not touch) deliberately do NOT override the file. The list of
// flags handled here mirrors the documented override surface in the
// CLI help text.
func applyOverrides(cmd *cobra.Command, cfg *types.RunConfig, args []string) {
	f := cmd.Flags()
	changed := func(name string) bool { return f.Changed(name) }

	if changed("mode") {
		cfg.Mode, _ = f.GetString("mode")
	}
	if changed("prompt") {
		cfg.Prompt, _ = f.GetString("prompt")
	} else if cfg.Prompt == "" && len(args) > 0 {
		// Positional prompt fills in only when neither the file nor the
		// flag has set one.
		cfg.Prompt = args[0]
	}
	if changed("max-turns") {
		cfg.MaxTurns, _ = f.GetInt("max-turns")
	}
	if changed("timeout") {
		t, _ := f.GetInt("timeout")
		cfg.Timeout = &t
	}
	if changed("trace") {
		path, _ := f.GetString("trace")
		cfg.TraceEmitter.FilePath = path
		// --trace is the JSONL trace path; if the file uses the otel emitter
		// FilePath would be ignored. To make the user's intent stand, coerce
		// the emitter type to jsonl unless the user also set --trace-emitter
		// explicitly (in which case their explicit choice wins).
		if !changed("trace-emitter") {
			cfg.TraceEmitter.Type = "jsonl"
		}
	}
	if changed("workspace") {
		cfg.Executor.Workspace, _ = f.GetString("workspace")
	}
	if changed("transport") {
		cfg.Transport.Type, _ = f.GetString("transport")
	}
	if changed("transport-addr") {
		cfg.Transport.Address, _ = f.GetString("transport-addr")
	}
	if changed("followup-grace") {
		g, _ := f.GetInt("followup-grace")
		if g > 0 {
			cfg.FollowUpGrace = &g
		} else {
			cfg.FollowUpGrace = nil
		}
	}
	if changed("log-level") {
		cfg.LogLevel, _ = f.GetString("log-level")
	}
	if changed("provider") {
		cfg.Provider.Type, _ = f.GetString("provider")
	}
	if changed("model") {
		// Override the router's model. The config file may set the model
		// on the router (per-mode/dynamic) — for static routers this is
		// where the active model lives.
		cfg.ModelRouter.Model, _ = f.GetString("model")
	}
	if changed("api-key-ref") {
		cfg.Provider.APIKeyRef, _ = f.GetString("api-key-ref")
	}
	if changed("executor") {
		cfg.Executor.Type, _ = f.GetString("executor")
	}
	if changed("edit-strategy") {
		cfg.EditStrategy.Type, _ = f.GetString("edit-strategy")
	}
	if changed("verifier") {
		cfg.Verifier.Type, _ = f.GetString("verifier")
	}
	if changed("git-strategy") {
		cfg.GitStrategy.Type, _ = f.GetString("git-strategy")
	}
	if changed("trace-emitter") {
		cfg.TraceEmitter.Type, _ = f.GetString("trace-emitter")
	}
	if changed("otel-endpoint") {
		cfg.TraceEmitter.Endpoint, _ = f.GetString("otel-endpoint")
	}
}

func runHarness(cmd *cobra.Command, args []string) error {
	f := cmd.Flags()
	configPath, _ := f.GetString("config")

	// Path 1: --config file is the base; explicitly-set flags override it.
	if configPath != "" {
		cfg, err := loadRunConfigFile(configPath)
		if err != nil {
			return err
		}
		applyOverrides(cmd, cfg, args)

		// After overrides, derive any unset mode-driven defaults
		// (PermissionPolicy, read-only Tools.BuiltIn). Mirrors what
		// buildHarnessRunConfig does in the flag-only path so the two
		// code paths produce architecturally consistent configs.
		applyModeDefaults(cfg)

		// Generate a RunID if the file omitted one. We never let the file
		// dictate identity, but we do honour an explicit RunID for replay
		// scenarios.
		if cfg.RunID == "" {
			cfg.RunID = generateRunID()
		}
		// Resolve env var for follow-up grace if neither flag nor file set it.
		if cfg.FollowUpGrace == nil {
			if v := os.Getenv("STIRRUP_FOLLOWUP_GRACE"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					cfg.FollowUpGrace = &n
				}
			}
		}
		if cfg.Prompt == "" {
			return fmt.Errorf("prompt is required: set in --config file, pass as argument, or use --prompt flag")
		}
		if err := types.ValidateRunConfig(cfg); err != nil {
			return fmt.Errorf("invalid config from %q: %w", configPath, err)
		}
		return runWithConfig(cfg)
	}

	// Path 2: no --config file. Build the RunConfig from flags + defaults.
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
	executorType, _ := f.GetString("executor")
	editStrategyType, _ := f.GetString("edit-strategy")
	verifierType, _ := f.GetString("verifier")
	gitStrategyType, _ := f.GetString("git-strategy")
	traceEmitterType, _ := f.GetString("trace-emitter")
	otelEndpoint, _ := f.GetString("otel-endpoint")

	// Allow the env var to set the follow-up grace when the flag is not provided.
	if followUpGrace == 0 {
		if v := os.Getenv("STIRRUP_FOLLOWUP_GRACE"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				followUpGrace = n
			}
		}
	}

	config := buildHarnessRunConfig(harnessCLIOptions{
		RunID:            generateRunID(),
		Mode:             mode,
		Prompt:           prompt,
		ProviderType:     providerType,
		APIKeyRef:        apiKeyRef,
		Model:            model,
		Workspace:        workspace,
		MaxTurns:         maxTurns,
		Timeout:          timeout,
		TracePath:        tracePath,
		TransportType:    transportType,
		TransportAddr:    transportAddr,
		FollowUpGrace:    followUpGrace,
		LogLevel:         logLevel,
		ExecutorType:     executorType,
		EditStrategyType: editStrategyType,
		VerifierType:     verifierType,
		GitStrategyType:  gitStrategyType,
		TraceEmitterType: traceEmitterType,
		OTelEndpoint:     otelEndpoint,
	})

	if err := types.ValidateRunConfig(config); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	return runWithConfig(config)
}

// runWithConfig is the shared run path for both --config and flag-only
// invocations. Both code paths converge here once they have a validated
// RunConfig — ValidateRunConfig rejects nil Timeout, so the dereference
// below is safe.
func runWithConfig(config *types.RunConfig) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*config.Timeout)*time.Second)
	defer cancel()
	setupSignalHandler(cancel)

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

	if config.FollowUpGrace != nil && *config.FollowUpGrace > 0 {
		core.RunFollowUpLoop(ctx, loop, config, *config.FollowUpGrace)
	}
	return nil
}
