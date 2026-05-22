package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/harness/internal/core"
	"github.com/rxbynerd/stirrup/types"
)

// maxPromptFileBytes caps the size of a --prompt-file we will read into
// memory. Matches the 10 MiB cap on file reads in
// harness/internal/executor/local.go (maxFileSize): a prompt is a short
// brief, anything in this range is almost certainly a mistake (a symlink
// to /dev/zero, a binary pasted as the path, etc.). The cap prevents OOM
// on a malformed input. Duplicated rather than imported because the
// executor constant is package-private and the coupling is one-shot.
const maxPromptFileBytes int64 = 10 * 1024 * 1024 // matches local.go maxFileSize

// readPromptFile loads a --prompt-file from disk with size + empty
// guards and trailing-newline trimming. Extracted as a tiny helper
// (rather than a full resolvePrompt extraction) because the file I/O
// concerns — size cap, empty check, path sanitisation, error wrapping
// — are non-trivial enough that duplicating them at both runHarness
// call sites would invite drift. The resolution chain that decides
// which source wins stays inlined per issue #165's "minimal-diff,
// house style" direction.
//
// Path is cleaned with filepath.Clean before stat; relative paths are
// resolved by the OS against the CWD (parallel to --config, NOT
// --workspace), so an operator running `stirrup harness
// --prompt-file ./brief.txt` from their checkout gets the file next
// to them, not next to a possibly-remote workspace.
func readPromptFile(path string) (string, error) {
	clean := filepath.Clean(path)
	info, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("reading --prompt-file %q: %w", path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("reading --prompt-file %q: is a directory", path)
	}
	// Reject character devices, named pipes (FIFOs), Unix sockets, and
	// every other non-regular file type. `os.Stat` reports Size()==0
	// for FIFOs and char devices on both Linux and macOS, which would
	// otherwise sail past the size cap below. The concrete failure
	// modes that this guard closes are:
	//   - /dev/zero or an unwritten FIFO: ReadAll blocks forever and
	//     the harness hangs at startup.
	//   - A FIFO pre-loaded with >10 MiB: all of it is read into memory
	//     before the post-read cap check trips.
	// The IsDir() guard above does not cover these; IsDir() is false
	// for devices and FIFOs.
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("reading --prompt-file %q: not a regular file", path)
	}
	if info.Size() > maxPromptFileBytes {
		return "", fmt.Errorf("reading --prompt-file %q: %d bytes exceeds %d byte cap", path, info.Size(), maxPromptFileBytes)
	}
	// Bounded read via io.LimitReader. Belt-and-braces alongside the
	// stat-time size check above: closes the TOCTOU window where the
	// file grows between os.Stat and the open call, and provides a
	// second line of defence if a future refactor accidentally drops
	// the IsRegular() guard. The +1 byte over the cap lets us
	// distinguish "exactly at the cap" from "larger than the cap" so
	// the operator-facing error is accurate.
	f, err := os.Open(clean)
	if err != nil {
		return "", fmt.Errorf("reading --prompt-file %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxPromptFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading --prompt-file %q: %w", path, err)
	}
	if int64(len(data)) > maxPromptFileBytes {
		return "", fmt.Errorf("reading --prompt-file %q: exceeds %d byte cap", path, maxPromptFileBytes)
	}
	// Trim only trailing CR/LF so `echo "prompt" > file` and
	// `printf 'prompt\n' > file` both produce the same string. Leading
	// whitespace is preserved — a prompt that intentionally opens with
	// indentation (e.g. a code block) should round-trip unchanged.
	trimmed := strings.TrimRight(string(data), "\r\n")
	if trimmed == "" {
		return "", fmt.Errorf("--prompt-file %q is empty", path)
	}
	return trimmed, nil
}

// harnessCLIOptions captures every CLI-surfaced setting that influences the
// RunConfig built by buildHarnessRunConfig. Extracted so the construction
// path is testable without booting cobra.
type harnessCLIOptions struct {
	RunID         string
	Mode          string
	SessionName   string
	Prompt        string
	ProviderType  string
	APIKeyRef     string
	BaseURL       string
	APIKeyHeader  string
	QueryParams   map[string]string
	Model         string
	Workspace     string
	MaxTurns      int
	Timeout       int
	TracePath     string
	TransportType string
	TransportAddr string
	FollowUpGrace int
	LogLevel      string

	// Temperature overrides the loop's default sampling temperature.
	// Nil means "do not override" (the harness default applies). A
	// pointer is used so an explicit --temperature=0 (greedy decoding)
	// is distinguishable from the flag being absent — cobra's Float64
	// store is a plain float64, so the disambiguation has to happen at
	// the flags.Changed() check site upstream.
	Temperature *float64

	// Vertex AI Gemini provider fields. Only meaningful when
	// ProviderType == "gemini"; ValidateRunConfig rejects them on
	// every other provider type, so the flag-only path safely
	// passes them through whatever the user typed.
	GCPProject         string
	GCPLocation        string
	GCPCredentialsFile string

	// Anthropic Workload Identity Federation fields (issue #117). Only
	// meaningful when ProviderType == "anthropic" and the operator wants
	// federated auth instead of a static API key. Validation rejects
	// these on every other provider type. When any of the four ID fields
	// is set, the flag-only path infers credential.type=anthropic-wif
	// and the JWT-source flag (or env var) selects the IdP wiring.
	AnthropicFederationRuleID string
	AnthropicOrganizationID   string
	AnthropicServiceAccountID string
	AnthropicWorkspaceID      string
	// AnthropicFromGitHubActions opts the workload-identity flow into
	// the runner-injected ACTIONS_ID_TOKEN_REQUEST_URL/_TOKEN
	// fallback. Implicit selection from env presence is deliberately
	// rejected (issue #117 risk #5: silent IdP selection makes
	// credential bugs unfixable).
	AnthropicFromGitHubActions bool

	// Azure Entra ID Workload Identity Federation fields (issue #118).
	// Only meaningful when --provider is openai-compatible or
	// openai-responses against an Azure OpenAI / Foundry endpoint.
	// Setting --azure-tenant-id implies credential.type=azure-workload-identity
	// (the file/flag is the discriminator, mirroring the
	// --gcp-credentials-file pattern). The TokenSource selection — file,
	// github-actions-oidc, aws-irsa, etc. — must come from --config
	// because flag syntax cannot cleanly express the per-source shape.
	AzureTenantID string
	AzureClientID string
	AzureScope    string

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
	OTelProtocol     string

	// Safety-ring escape hatches (issue #42). These set RunConfig fields
	// on the matching sub-config; an empty string leaves the field unset
	// so ValidateRunConfig's mode-aware defaulting can take over.
	ContainerRuntime     string
	PermissionPolicyFile string
	CodeScannerType      string

	// GuardRail escape hatches (issue #43). When any of these is non-zero
	// the flag-only path constructs a GuardRailConfig; an entirely-zero
	// trio leaves config.GuardRail nil so the factory installs the
	// no-op "none" guard. Composite stages are not surfaced as flags —
	// they require a --config file because flag syntax cannot express
	// per-stage phase restrictions.
	GuardRailType     string
	GuardRailEndpoint string
	GuardRailModel    string
	GuardRailFailOpen bool

	// Observability resource attributes (issue #95). Empty values fall
	// through to env-var fallbacks (OTEL_DEPLOYMENT_ENVIRONMENT,
	// OTEL_SERVICE_NAMESPACE) and finally to defaults at OTel resource
	// construction time, so leaving these unset is a valid choice for
	// local development.
	DeploymentEnvironment string
	ServiceNamespace      string

	// Provider retry policy overrides (issue #197). Zero values leave
	// the corresponding field unset on Provider.Retry so
	// ValidateRunConfig fills in the documented defaults
	// (MaxAttempts=3, InitialDelayMs=500, MaxDelayMs=16000,
	// WallClockBudgetMs=90000). Operators with multi-provider configs
	// must use --config to set per-named-provider retry policies; the
	// flags here apply only to the default provider.
	ProviderRetryMaxAttempts     int
	ProviderRetryInitialDelay    time.Duration
	ProviderRetryMaxDelay        time.Duration
	ProviderRetryWallClockBudget time.Duration

	// Workspace export (issue #164). WorkspaceExportTo is a gs:// URI
	// stored on RunConfig.Executor.WorkspaceExportTo so the export
	// fires from runWithConfig regardless of which code path built
	// the config. WorkspaceExportRequired is *not* persisted on
	// RunConfig — it is a CLI-only behaviour flag that controls
	// whether export failure terminates the run non-zero. The two
	// are decoupled so a config-file-only operator can set the URI
	// once and pass --export-workspace-required from the wrapper
	// script that knows whether the artifact is load-bearing.
	WorkspaceExportTo       string
	WorkspaceExportRequired bool

	// ToolDispatchMaxParallel sets the parallel async-tool dispatch
	// fan-out (issue #184). Zero defers to the library default
	// (DefaultToolDispatchMaxParallel) by leaving config.ToolDispatch
	// nil so the loop reads the effective value via
	// EffectiveToolDispatchMaxParallel.
	ToolDispatchMaxParallel int

	// Batch opts the run into async batch submission (issue #136).
	// The flag carries only the Enabled bit; operators wanting any of
	// the other BatchProviderConfig fields (MaxWaitSeconds,
	// HarnessSidePolling, FallbackOnTimeout, CancelBundleOnRunCancel,
	// AllowInteractiveModes) must use --config. Validation of the
	// batch shape — transport, mode, provider type — runs in
	// ValidateRunConfig and is shared with the --config path.
	Batch bool
}

// buildHarnessRunConfig assembles the RunConfig used by `stirrup harness`.
// It is the single place that encodes defaults such as the per-mode
// permission policy and the fall-back built-in tool list required by
// read-only modes. Kept pure so tests can exercise every --mode value
// without invoking the agentic loop.
//
// Returns a non-nil error only when an operator-supplied flag fails an
// up-front sanity check (e.g. a sub-millisecond retry duration that
// would silently truncate to zero). Most validation still happens later
// in `ValidateRunConfig`; the checks here exist where the truncation
// would erase operator intent before the validator ever sees it.
//
// Internally delegates to buildHarnessRunConfigCore for the field-by-
// field shape, then runs applyModeDefaults so the returned config is
// ready for the validator. Splitting the two lets BuildRunConfig hand
// run-config's ResolveBase path a config without the mode-default
// mutations applied, matching the spec's "leave the document minimally
// mutated" contract for chained pipeline stages.
func buildHarnessRunConfig(opts harnessCLIOptions) (*types.RunConfig, error) {
	cfg, err := buildHarnessRunConfigCore(opts)
	if err != nil {
		return nil, err
	}
	applyModeDefaults(cfg)
	return cfg, nil
}

// buildHarnessRunConfigCore is buildHarnessRunConfig without the trailing
// applyModeDefaults call. BuildRunConfig uses this directly so the
// run-config subcommand's ResolveBase path can emit a minimally-mutated
// document for downstream pipeline stages.
func buildHarnessRunConfigCore(opts harnessCLIOptions) (*types.RunConfig, error) {
	timeout := opts.Timeout

	executorType := opts.ExecutorType
	if executorType == "" {
		executorType = "local"
	}
	editStrategyType := opts.EditStrategyType
	if editStrategyType == "" {
		// "multi" is the default because the multi-strategy edit tool is the
		// highest-leverage edit configuration for production. Callers asking
		// for write_file/search_replace/apply_diff are aliased to the
		// multi-strategy's edit_file tool by core/factory.go::editToolEnabled.
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
		// Protocol stays empty by default so the exporter falls
		// through to the OTel SDK's grpc default. Operators who
		// want OTLP/HTTP set --otel-protocol=http/protobuf;
		// validation rejects any other value.
		traceEmitter.Protocol = opts.OTelProtocol
	}

	config := &types.RunConfig{
		RunID:       opts.RunID,
		Mode:        opts.Mode,
		SessionName: opts.SessionName,
		Prompt:      opts.Prompt,
		Provider: types.ProviderConfig{
			Type: opts.ProviderType,
			// APIKeyRef is dropped for the gemini provider because Vertex
			// AI uses GCP IAM rather than API keys; the validator rejects
			// APIKeyRef on a gemini run, and forcing the user to type
			// --api-key-ref="" alongside --provider gemini would be
			// hostile UX. Same logic for Azure WIF: the validator rejects
			// APIKeyRef alongside credential.type=azure-workload-identity
			// because the Bearer is fetched via OAuth2 token exchange, so
			// a flag-only Azure WIF run with the cobra default
			// --api-key-ref="secret://ANTHROPIC_API_KEY" would otherwise
			// fail validation with a confusing message about a value the
			// operator never set.
			APIKeyRef: func() string {
				if opts.ProviderType == "gemini" || opts.AzureTenantID != "" {
					return ""
				}
				return opts.APIKeyRef
			}(),
			BaseURL:      opts.BaseURL,
			APIKeyHeader: opts.APIKeyHeader,
			QueryParams:  opts.QueryParams,
		},
		ModelRouter: types.ModelRouterConfig{
			Type:     "static",
			Provider: opts.ProviderType,
			Model:    opts.Model,
		},
		PromptBuilder:   types.PromptBuilderConfig{Type: "default"},
		ContextStrategy: types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
		Executor: types.ExecutorConfig{
			Type:              executorType,
			Workspace:         opts.Workspace,
			Runtime:           opts.ContainerRuntime,
			WorkspaceExportTo: opts.WorkspaceExportTo,
		},
		EditStrategy: types.EditStrategyConfig{Type: editStrategyType},
		Verifier:     types.VerifierConfig{Type: verifierType},
		GitStrategy:  types.GitStrategyConfig{Type: gitStrategyType},
		Transport:    types.TransportConfig{Type: opts.TransportType, Address: opts.TransportAddr},
		TraceEmitter: traceEmitter,
		MaxTurns:     opts.MaxTurns,
		Timeout:      &timeout,
		LogLevel:     opts.LogLevel,
	}
	if opts.FollowUpGrace > 0 {
		grace := opts.FollowUpGrace
		config.FollowUpGrace = &grace
	}
	if opts.Temperature != nil {
		t := *opts.Temperature
		config.Temperature = &t
	}

	// Vertex AI Gemini fields. The validator rejects these on every
	// other provider type, so the flag-only path scopes them to gemini
	// to keep --provider switching ergonomic (you can leave --gcp-*
	// flags at their defaults and they will not leak onto non-gemini
	// runs).
	if opts.ProviderType == "gemini" {
		config.Provider.GCPProject = opts.GCPProject
		config.Provider.GCPLocation = opts.GCPLocation
		config.Provider.GCPCredentialsFile = opts.GCPCredentialsFile
	}

	// --gcp-credentials-file implies credential.type=gcp-service-account
	// when no other credential type is configured. This mirrors how
	// --permission-policy-file implies type=policy-engine: the file
	// path is the discriminator, so requiring the user to set both is
	// redundant. An explicit Credential.Type set elsewhere wins.
	if opts.ProviderType == "gemini" && opts.GCPCredentialsFile != "" && config.Provider.Credential == nil {
		config.Provider.Credential = &types.CredentialConfig{Type: "gcp-service-account"}
	}

	// Anthropic Workload Identity Federation (issue #117) is populated
	// exclusively by applyAnthropicWIFOverrides at the runHarness call
	// site — it owns flag + env-var precedence for both the --config
	// path and the flag-only path, so duplicating the flag path here
	// would only create drift the next time a fifth WIF field lands.
	// Keep this comment as a signpost so future edits do not re-add a
	// parallel population block.

	// --azure-tenant-id implies credential.type=azure-workload-identity
	// when no other credential type is configured. This mirrors the
	// --gcp-credentials-file shortcut above: the flag is the
	// discriminator. The TokenSource still must come from --config
	// (no flag-only path for tokenSource selection — too many shape
	// variants to express cleanly as flags). A flag-only invocation
	// with --azure-tenant-id will therefore fail validateRunConfig
	// with "azure-workload-identity requires tokenSource", which is
	// the correct UX: the validator's error tells the operator to
	// reach for --config to wire the source. The flag is NOT silently
	// dropped, because that would let an Azure WIF run reach the
	// validator with an inconsistent shape and surface a confusing
	// error about credential.type rather than the missing tokenSource.
	if opts.AzureTenantID != "" && config.Provider.Credential == nil {
		config.Provider.Credential = &types.CredentialConfig{
			Type:          "azure-workload-identity",
			AzureTenantID: opts.AzureTenantID,
			AzureClientID: opts.AzureClientID,
			AzureScope:    opts.AzureScope,
		}
	}

	// Safety-ring fields are wired only when the caller supplied them
	// so ValidateRunConfig's mode-aware defaulting (e.g. CodeScanner
	// "patterns" for execution mode) still kicks in for unset values.
	if opts.PermissionPolicyFile != "" {
		// --permission-policy-file implies type=policy-engine when the
		// caller has not explicitly chosen a policy elsewhere; this is
		// the convenience shortcut documented in the flag help.
		config.PermissionPolicy.PolicyFile = opts.PermissionPolicyFile
		if config.PermissionPolicy.Type == "" {
			config.PermissionPolicy.Type = "policy-engine"
		}
	}
	if opts.CodeScannerType != "" {
		config.CodeScanner = &types.CodeScannerConfig{Type: opts.CodeScannerType}
	}

	// GuardRail (issue #43). Only construct the sub-config when the caller
	// touched at least one of the three GuardRail flags; an entirely-empty
	// trio leaves config.GuardRail nil and the factory installs the
	// no-op "none" guard. Composite stages can only be set via --config.
	if opts.GuardRailType != "" || opts.GuardRailEndpoint != "" || opts.GuardRailModel != "" || opts.GuardRailFailOpen {
		config.GuardRail = &types.GuardRailConfig{
			Type:     opts.GuardRailType,
			Endpoint: opts.GuardRailEndpoint,
			Model:    opts.GuardRailModel,
			FailOpen: opts.GuardRailFailOpen,
		}
	}

	// Observability resource attributes (issue #95). Only construct the
	// sub-config when the caller touched at least one of the two flags.
	// An entirely-empty pair leaves config.Observability at the zero value
	// so a future validator or factory branch that distinguishes
	// "operator pinned" from "fall through to env" can do so. Matches the
	// flag-only construction pattern used for GuardRail above.
	if opts.DeploymentEnvironment != "" || opts.ServiceNamespace != "" {
		config.Observability = types.ObservabilityConfig{
			Environment:      opts.DeploymentEnvironment,
			ServiceNamespace: opts.ServiceNamespace,
		}
	}

	// Provider retry policy (issue #197). Only allocate
	// Provider.Retry when the caller touched at least one flag;
	// otherwise leave it nil so ValidateRunConfig fills the documented
	// defaults. Each non-zero field overrides its slot independently,
	// matching the partial-override pattern used by GuardRail above.
	if err := applyProviderRetryOverrides(&config.Provider, opts); err != nil {
		return nil, err
	}

	// Parallel-dispatch knob (issue #184). Only construct the sub-config
	// when the operator opted in; leaving it nil lets the loop reach for
	// types.DefaultToolDispatchMaxParallel via
	// EffectiveToolDispatchMaxParallel without persisting an opinion
	// that the operator did not voice.
	if opts.ToolDispatchMaxParallel > 0 {
		config.ToolDispatch = &types.ToolDispatchConfig{MaxParallel: opts.ToolDispatchMaxParallel}
	}

	// Batch (issue #136). --batch carries only the Enabled bit; every
	// other BatchProviderConfig field stays at its zero value because
	// the flag-only path has no --config to merge against. Operators
	// who need MaxWaitSeconds, HarnessSidePolling, FallbackOnTimeout,
	// CancelBundleOnRunCancel, or AllowInteractiveModes must use
	// --config — flag syntax cannot cleanly express the cross-field
	// invariants the validator enforces. ValidateRunConfig defaults
	// MaxWaitSeconds to 24 h when Enabled and the slot is nil.
	if opts.Batch {
		config.Provider.Batch = &types.BatchProviderConfig{Enabled: true}
	}

	return config, nil
}

// applyProviderRetryOverrides mutates pc.Retry to reflect any of the
// four provider-retry CLI flags the operator set. Each flag maps to a
// single ProviderRetryConfig field; an unset flag (zero value) leaves
// its slot zero so ValidateRunConfig's per-field defaulting fills it
// in. Duration flags are converted to integer milliseconds because the
// wire format (ProviderRetryConfig) stores millisecond magnitudes.
//
// Returns a non-nil error if any non-zero duration is below the
// millisecond resolution boundary (e.g. 500µs). Without that guard a
// `int(d / time.Millisecond)` conversion truncates to zero and the
// zero-guard below treats the value as "flag not set", silently
// erasing the operator's expressed intent.
func applyProviderRetryOverrides(pc *types.ProviderConfig, opts harnessCLIOptions) error {
	maxAttempts := opts.ProviderRetryMaxAttempts
	initialMs, err := retryDurationToMs("--provider-retry-initial-delay", opts.ProviderRetryInitialDelay)
	if err != nil {
		return err
	}
	maxMs, err := retryDurationToMs("--provider-retry-max-delay", opts.ProviderRetryMaxDelay)
	if err != nil {
		return err
	}
	wallMs, err := retryDurationToMs("--provider-retry-wall-clock", opts.ProviderRetryWallClockBudget)
	if err != nil {
		return err
	}
	if maxAttempts == 0 && initialMs == 0 && maxMs == 0 && wallMs == 0 {
		return nil
	}
	if pc.Retry == nil {
		pc.Retry = &types.ProviderRetryConfig{}
	}
	if maxAttempts != 0 {
		pc.Retry.MaxAttempts = maxAttempts
	}
	if initialMs != 0 {
		pc.Retry.InitialDelayMs = initialMs
	}
	if maxMs != 0 {
		pc.Retry.MaxDelayMs = maxMs
	}
	if wallMs != 0 {
		pc.Retry.WallClockBudgetMs = wallMs
	}
	return nil
}

// optionalFloat64Flag returns a heap-allocated copy of the named Float64
// flag's value iff the operator set it on the command line, and nil
// otherwise. cobra's Float64 store cannot represent absence — both an
// unset flag and an explicit --foo=0 read back as 0.0 — so the
// Changed() bit is the only safe way to preserve "use the default"
// versus "the operator chose 0". Used by --temperature on both the
// --config-path (applyOverrides) and the flag-only (runHarness) entry
// points; centralising the pattern keeps the two paths from drifting
// when a future env-var fallback (e.g. STIRRUP_TEMPERATURE) lands.
func optionalFloat64Flag(cmd *cobra.Command, name string) *float64 {
	f := cmd.Flags()
	if !f.Changed(name) {
		return nil
	}
	v, _ := f.GetFloat64(name)
	return &v
}

// retryDurationToMs converts a positive Duration to whole milliseconds
// for the provider-retry CLI flags, rejecting any non-zero value below
// 1ms. A zero input (the flag's default sentinel) returns zero with no
// error, preserving the "flag not set" path. Errors include the flag
// name so the operator sees which value they need to raise.
func retryDurationToMs(flagName string, d time.Duration) (int, error) {
	if d == 0 {
		return 0, nil
	}
	if d < time.Millisecond {
		return 0, fmt.Errorf("%s: minimum resolution is 1ms, got %v", flagName, d)
	}
	return int(d / time.Millisecond), nil
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

// parseQueryParam splits a "key=value" --query-param entry. Empty keys and
// missing "=" are rejected so a typo at the CLI surfaces immediately rather
// than silently dropping. Validation of the resulting key/value (charset
// limits, CRLF, total encoded size) is left to ValidateRunConfig — this
// helper only handles the syntactic split.
func parseQueryParam(entry string) (string, string, error) {
	idx := bytes.IndexByte([]byte(entry), '=')
	if idx < 0 {
		return "", "", fmt.Errorf("expected key=value, got %q", entry)
	}
	key := entry[:idx]
	val := entry[idx+1:]
	if key == "" {
		return "", "", fmt.Errorf("empty key (entry: %q)", entry)
	}
	return key, val, nil
}

var harnessCmd = &cobra.Command{
	Use:   "harness [flags] [prompt]",
	Short: "Run the coding agent harness",
	Long: `Run the stirrup coding agent harness. The prompt can be provided as a
positional argument, via the --prompt flag, via --prompt-file (read from
CWD; trailing newlines trimmed; 10 MiB cap), or via the STIRRUP_PROMPT
environment variable. Resolution order: --prompt > positional > --prompt-file
> STIRRUP_PROMPT > prompt field in --config.

Configuration precedence: a --config JSON file (if provided) populates the
full RunConfig; explicitly-set flags then override individual fields; flags
left at their default value do NOT override the file. When --config is not
provided, flags + defaults build the RunConfig directly.

Workspace export (--export-workspace-to gs://...): at end-of-run the
executor's workspace is tarred, gzipped, and uploaded to the named GCS
URI. The flag overrides executor.workspaceExportTo from --config when
explicitly set. Default error semantics: upload failures are logged and
the run's exit code is unchanged. Pass --export-workspace-required to
flip that — a failed upload then exits the run non-zero, suitable for
Cloud Run jobs whose downstream automation depends on the artifact.

The default --mode is "planning" (read-only: no write_file / edit_file /
run_command, deny-side-effects permission policy). Pass --mode execution
to enable editing and shell access; the read-only modes (planning, review,
research, toil) differ only in prompt template and ship as the safe-by-
default first-touch posture.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHarness,
}

func init() {
	rootCmd.AddCommand(harnessCmd)

	// All RunConfig-producing flags live in addRunConfigFlags so the
	// run-config subcommand can register the same set without drifting.
	// CLI-behaviour flags that do not round-trip through RunConfig
	// (--export-workspace-required, --output-runconfig) remain on the
	// harness command directly.
	addRunConfigFlags(harnessCmd)

	f := harnessCmd.Flags()
	f.Bool("export-workspace-required", false, "When true, a failed workspace export exits the run non-zero. When false (default), failures are logged and the run's exit code is unchanged.")
	f.String("output-runconfig", "", "Write the resolved RunConfig as JSON to <path> (use '-' for stdout) and exit without running. Useful for capturing the exact config a flag-only invocation would have used. Validation must pass first; the path is not written on a validator error.")
}

// applyOverrides mutates cfg in place, replacing fields whose corresponding
// flag was explicitly set on the command line. Defaults (i.e. flags the
// user did not touch) deliberately do NOT override the file. The list of
// flags handled here mirrors the documented override surface in the
// CLI help text.
//
// Returns a non-nil error when an override is structurally invalid (today,
// only a malformed --query-param entry triggers this). The flag-only path
// in runHarness already fails hard for the same input, so propagating the
// error here keeps the two paths aligned: a typo at the CLI must never be
// silently dropped, because that would let a request reach the provider
// missing a required parameter (e.g. Azure's `api-version`) and surface as
// an opaque HTTP 400 instead of a clear operator error.
func applyOverrides(cmd *cobra.Command, cfg *types.RunConfig, args []string) error {
	f := cmd.Flags()
	changed := func(name string) bool { return f.Changed(name) }

	if changed("mode") {
		cfg.Mode, _ = f.GetString("mode")
	}
	if changed("name") {
		cfg.SessionName, _ = f.GetString("name")
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
	// optionalFloat64Flag distinguishes an explicit --temperature=0
	// (greedy decoding) from the flag being absent: cobra's Float64
	// store coerces both to 0.0, so without the Changed() check every
	// run that omitted --temperature would silently rewrite a
	// file-provided non-zero value to greedy decoding.
	if t := optionalFloat64Flag(cmd, "temperature"); t != nil {
		cfg.Temperature = t
	}
	if changed("log-level") {
		cfg.LogLevel, _ = f.GetString("log-level")
	}
	if changed("provider") {
		cfg.Provider.Type, _ = f.GetString("provider")
		// Vertex AI uses GCP IAM, not API keys. When the operator
		// switches an existing config to gemini via --provider, the old
		// provider's APIKeyRef would otherwise linger and trip
		// validateGeminiProviderFields with a confusing "apiKeyRef must
		// not be set" error about a value the operator did not
		// intentionally set on this run. Mirror buildHarnessRunConfig's
		// flag-only behaviour by clearing APIKeyRef unless the operator
		// explicitly passed --api-key-ref alongside --provider gemini.
		if cfg.Provider.Type == "gemini" && !changed("api-key-ref") {
			cfg.Provider.APIKeyRef = ""
		}
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
	if changed("base-url") {
		cfg.Provider.BaseURL, _ = f.GetString("base-url")
	}
	if changed("api-key-header") {
		cfg.Provider.APIKeyHeader, _ = f.GetString("api-key-header")
	}
	if changed("gcp-project") {
		cfg.Provider.GCPProject, _ = f.GetString("gcp-project")
	}
	if changed("gcp-location") {
		cfg.Provider.GCPLocation, _ = f.GetString("gcp-location")
	}
	// When a config file omits gcpLocation and the operator has not
	// explicitly passed --gcp-location, the validator otherwise rejects
	// with "gcpLocation is required" even though the flag carries a
	// documented default of "global". Apply the default explicitly on
	// the gemini path so the same default reaches both flag-only and
	// --config users. (The flag-only buildHarnessRunConfig path already
	// gets this for free because cobra populates GCPLocation with the
	// flag default before harnessCLIOptions is read.)
	if cfg.Provider.Type == "gemini" && cfg.Provider.GCPLocation == "" {
		cfg.Provider.GCPLocation = "global"
	}
	if changed("gcp-credentials-file") {
		path, _ := f.GetString("gcp-credentials-file")
		cfg.Provider.GCPCredentialsFile = path
		// Setting --gcp-credentials-file implies the explicit
		// gcp-service-account credential type so the credential layer
		// reads the file rather than falling through to ADC. Mirrors
		// the convenience shortcut buildHarnessRunConfig applies for
		// the flag-only path. An existing Credential.Type from the
		// config file wins (this only fills in unset).
		if path != "" && cfg.Provider.Credential == nil {
			cfg.Provider.Credential = &types.CredentialConfig{Type: "gcp-service-account"}
		}
	}

	// Anthropic WIF folding lives in BuildRunConfig as a single
	// post-override step (see runconfigbuilder.go). Calling it again
	// here would double-invoke its slog.Warn diagnostics and silently
	// double-count any future non-idempotent additions.

	// Azure Entra ID Workload Identity Federation (issue #118). The three
	// --azure-* flags compose: --azure-tenant-id alone is enough to imply
	// credential.type=azure-workload-identity (mirroring the
	// --gcp-credentials-file pattern); --azure-client-id and --azure-scope
	// fill in fields on whichever Credential block the operator ends up
	// with (file-loaded or flag-implied). An explicit Credential block of
	// any other type in the file wins — operators who have set
	// credential.type=static deliberately should not have it silently
	// upgraded to WIF by a stray --azure-tenant-id.
	if changed("azure-tenant-id") {
		tenantID, _ := f.GetString("azure-tenant-id")
		if tenantID != "" && cfg.Provider.Credential == nil {
			cfg.Provider.Credential = &types.CredentialConfig{Type: "azure-workload-identity"}
		}
		if cfg.Provider.Credential != nil {
			cfg.Provider.Credential.AzureTenantID = tenantID
		}
		// Azure WIF resolves the bearer dynamically via OAuth2 token
		// exchange; the validator rejects APIKeyRef alongside
		// credential.type=azure-workload-identity. Mirror the gemini
		// clear above so an operator who switches an existing config to
		// Azure WIF via --azure-tenant-id does not have to also pass
		// --api-key-ref="" to clear a stale value the file kept around.
		// An explicit --api-key-ref on the same command line wins.
		if tenantID != "" && !changed("api-key-ref") {
			cfg.Provider.APIKeyRef = ""
		}
	}
	// --azure-client-id and --azure-scope amend an EXISTING Credential
	// block but never create one on their own. Only --azure-tenant-id is
	// the discriminator that materialises a Credential block (mirroring
	// --gcp-credentials-file). Without this restriction, a user passing
	// --azure-client-id alone would silently produce an
	// azure-workload-identity Credential missing tenantID, which the
	// validator would then reject with a "requires azureTenantId" error
	// the operator never asked for.
	if changed("azure-client-id") {
		clientID, _ := f.GetString("azure-client-id")
		if cfg.Provider.Credential != nil {
			cfg.Provider.Credential.AzureClientID = clientID
		}
	}
	if changed("azure-scope") {
		scope, _ := f.GetString("azure-scope")
		if cfg.Provider.Credential != nil {
			cfg.Provider.Credential.AzureScope = scope
		}
	}
	if changed("query-param") {
		// Replace rather than merge: explicit --query-param flags clear any
		// QueryParams set in the --config file, mirroring how a single
		// --base-url flag wholesale replaces the file's BaseURL. Mixing
		// would surprise users who set --query-param to override a stale
		// file entry.
		raw, _ := f.GetStringArray("query-param")
		cfg.Provider.QueryParams = nil
		for _, entry := range raw {
			k, v, err := parseQueryParam(entry)
			if err != nil {
				// Hard-fail rather than dropping the malformed entry. The
				// flag-only path in runHarness returns the same shape of
				// error for the same input; warning-and-continue here would
				// leave Path 1 (--config) and Path 2 (flags only) inconsistent
				// and let an Azure request proceed without a required
				// parameter (e.g. api-version), surfacing later as an opaque
				// HTTP 400 from the provider.
				return fmt.Errorf("--query-param %q: %w", entry, err)
			}
			if cfg.Provider.QueryParams == nil {
				cfg.Provider.QueryParams = map[string]string{}
			}
			cfg.Provider.QueryParams[k] = v
		}
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
	if changed("otel-protocol") {
		cfg.TraceEmitter.Protocol, _ = f.GetString("otel-protocol")
	}
	if changed("container-runtime") {
		cfg.Executor.Runtime, _ = f.GetString("container-runtime")
	}
	if changed("permission-policy-file") {
		path, _ := f.GetString("permission-policy-file")
		cfg.PermissionPolicy.PolicyFile = path
		// If the file already names a non-policy-engine type, leave it
		// alone — the user is fine-tuning a config that ships its own
		// policy choice. Only flip to policy-engine when the type was
		// not set by the file. This mirrors the buildHarnessRunConfig
		// convenience shortcut.
		if path != "" && cfg.PermissionPolicy.Type == "" {
			cfg.PermissionPolicy.Type = "policy-engine"
		}
	}
	if changed("code-scanner") {
		typ, _ := f.GetString("code-scanner")
		if typ == "" {
			// Empty flag means "fall back to the mode default";
			// represent that by clearing the field so applyCodeScannerDefault
			// can re-fill it during validation.
			cfg.CodeScanner = nil
		} else {
			cfg.CodeScanner = &types.CodeScannerConfig{Type: typ}
		}
	}
	// GuardRail (issue #43). Each flag is independently overrideable so
	// callers can fine-tune one field (e.g. swap the endpoint) without
	// having to restate the rest of the file's GuardRail block. The "set
	// type to empty string" convention clears the GuardRail entirely,
	// matching the --code-scanner pattern above.
	if changed("guardrail") {
		typ, _ := f.GetString("guardrail")
		if typ == "" {
			cfg.GuardRail = nil
		} else {
			if cfg.GuardRail == nil {
				cfg.GuardRail = &types.GuardRailConfig{}
			}
			cfg.GuardRail.Type = typ
		}
	}
	if changed("guardrail-endpoint") {
		endpoint, _ := f.GetString("guardrail-endpoint")
		if cfg.GuardRail == nil {
			cfg.GuardRail = &types.GuardRailConfig{}
		}
		cfg.GuardRail.Endpoint = endpoint
	}
	if changed("guardrail-model") {
		model, _ := f.GetString("guardrail-model")
		if cfg.GuardRail == nil {
			cfg.GuardRail = &types.GuardRailConfig{}
		}
		cfg.GuardRail.Model = model
	}
	if changed("guardrail-fail-open") {
		failOpen, _ := f.GetBool("guardrail-fail-open")
		if cfg.GuardRail == nil {
			cfg.GuardRail = &types.GuardRailConfig{}
		}
		cfg.GuardRail.FailOpen = failOpen
	}
	// Observability resource attributes (issue #95). Each flag overrides
	// the corresponding RunConfig field independently so an operator can
	// fine-tune one (e.g. swap the environment label) without restating
	// the rest of the file's observability block.
	if changed("deployment-environment") {
		cfg.Observability.Environment, _ = f.GetString("deployment-environment")
	}
	if changed("service-namespace") {
		cfg.Observability.ServiceNamespace, _ = f.GetString("service-namespace")
	}

	// Workspace export (issue #164). The flag explicitly overrides
	// whatever the file set so a deployment can flip the destination
	// URI without re-templating the JSON. An empty flag value with
	// "changed" status clears the field entirely — the mirror of
	// "set to empty to clear" applied elsewhere in this file.
	if changed("export-workspace-to") {
		cfg.Executor.WorkspaceExportTo, _ = f.GetString("export-workspace-to")
	}

	// Parallel async-tool dispatch (issue #184). Explicit zero clears
	// the field so the loop falls back to DefaultToolDispatchMaxParallel;
	// any positive value pins MaxParallel on cfg.ToolDispatch without
	// disturbing other fields a future revision might introduce.
	if changed("max-tool-parallel") {
		mp, _ := f.GetInt("max-tool-parallel")
		if mp > 0 {
			if cfg.ToolDispatch == nil {
				cfg.ToolDispatch = &types.ToolDispatchConfig{}
			}
			cfg.ToolDispatch.MaxParallel = mp
		} else {
			cfg.ToolDispatch = nil
		}
	}

	// Provider retry policy (issue #197). Each flag overrides its slot
	// on cfg.Provider.Retry independently so an operator can pin a
	// single value (e.g. just --provider-retry-max-attempts=5) without
	// having to restate the rest of the file's retry block. A flag left
	// at its zero default does NOT override the file, matching the
	// general "explicit flag wins; defaults don't" rule documented in
	// the precedence section of docs/configuration.md.
	if err := applyProviderRetryFlagOverrides(cmd, &cfg.Provider); err != nil {
		return err
	}

	// Batch (issue #136). --batch only flips the Enabled bit; every
	// other Batch field (MaxWaitSeconds, HarnessSidePolling,
	// FallbackOnTimeout, CancelBundleOnRunCancel,
	// AllowInteractiveModes) must come from --config because flag
	// syntax cannot express their cross-field invariants. When the
	// file already supplied a Batch block, preserve its other fields
	// so an operator can keep e.g. harnessSidePolling=true from the
	// file and only flip enabled=true at the CLI. When the file
	// omitted Batch entirely, allocate a fresh block with only
	// Enabled set.
	if changed("batch") {
		enabled, _ := f.GetBool("batch")
		if enabled {
			if cfg.Provider.Batch == nil {
				cfg.Provider.Batch = &types.BatchProviderConfig{Enabled: true}
			} else {
				cfg.Provider.Batch.Enabled = true
			}
		} else if cfg.Provider.Batch != nil {
			// Explicit --batch=false clears Enabled but preserves the
			// surrounding struct. The divergence from --code-scanner ""
			// and --guardrail "" (which nil the entire sub-config) is
			// deliberate: keeping HarnessSidePolling and other fields
			// intact means a follow-up --batch=true re-enables without
			// the operator having to re-supply --config, which matches
			// the mode-toggle workflow the flag is built for.
			cfg.Provider.Batch.Enabled = false
		}
	}
	return nil
}

// applyProviderRetryFlagOverrides mutates pc.Retry to reflect any of
// the --provider-retry-* CLI flags the operator explicitly set on top
// of an existing --config file. Mirrors applyProviderRetryOverrides for
// the flag-only path but uses cmd.Flags().Changed() so a flag left at
// its zero default does not clobber a file-supplied value.
//
// Returns a non-nil error if any operator-supplied duration is below
// the 1ms resolution boundary (see retryDurationToMs).
func applyProviderRetryFlagOverrides(cmd *cobra.Command, pc *types.ProviderConfig) error {
	f := cmd.Flags()
	changed := func(name string) bool { return f.Changed(name) }
	if !changed("provider-retry-max-attempts") &&
		!changed("provider-retry-initial-delay") &&
		!changed("provider-retry-max-delay") &&
		!changed("provider-retry-wall-clock") {
		return nil
	}
	if pc.Retry == nil {
		pc.Retry = &types.ProviderRetryConfig{}
	}
	if changed("provider-retry-max-attempts") {
		v, _ := f.GetInt("provider-retry-max-attempts")
		pc.Retry.MaxAttempts = v
	}
	if changed("provider-retry-initial-delay") {
		d, _ := f.GetDuration("provider-retry-initial-delay")
		ms, err := retryDurationToMs("--provider-retry-initial-delay", d)
		if err != nil {
			return err
		}
		pc.Retry.InitialDelayMs = ms
	}
	if changed("provider-retry-max-delay") {
		d, _ := f.GetDuration("provider-retry-max-delay")
		ms, err := retryDurationToMs("--provider-retry-max-delay", d)
		if err != nil {
			return err
		}
		pc.Retry.MaxDelayMs = ms
	}
	if changed("provider-retry-wall-clock") {
		d, _ := f.GetDuration("provider-retry-wall-clock")
		ms, err := retryDurationToMs("--provider-retry-wall-clock", d)
		if err != nil {
			return err
		}
		pc.Retry.WallClockBudgetMs = ms
	}
	return nil
}

func runHarness(cmd *cobra.Command, args []string) error {
	f := cmd.Flags()
	configPath, _ := f.GetString("config")

	cfg, err := BuildRunConfig(RunConfigSources{
		Stdin:      os.Stdin,
		ConfigPath: configPath,
		Cmd:        cmd,
		Args:       args,
		Resolve:    ResolveAll,
	})
	if err != nil {
		// Intercept the bare-invocation "prompt is required" path
		// (#249 section B). An operator who typed `stirrup harness`
		// with no prompt at an interactive terminal wants a
		// grouped, scannable example block rather than a one-line
		// nudge to re-run with --help. Two gates protect the
		// scripted path:
		//
		//   - Only the "prompt is required" error from
		//     resolvePromptForRun: every other validation/IO
		//     failure (bad --config path, malformed JSON, invalid
		//     mode, missing API key, etc.) keeps its current loud
		//     error so a CI run still exits non-zero.
		//   - Only when stderr is a tty. The hint is emitted on
		//     stderr, so stderr's fd is the correct channel to
		//     gate on: `stirrup harness 2>/dev/null` (stderr
		//     discarded) and `stirrup harness 2>&1 | cat` (stderr
		//     piped) are both scripted shapes that must keep the
		//     opaque error so CI exits non-zero. Stdin can be
		//     redirected independently and is not a reliable
		//     proxy for "operator is watching".
		//
		// Whether the help itself contains ANSI is a separate
		// decision (honouring NO_COLOR, writer identity) handled
		// inside writeBareHarnessHint.
		if isPromptRequiredErr(err) && stderrIsTTY() {
			writeBareHarnessHint(bareHintStderr)
			return nil
		}
		return err
	}

	// --output-runconfig is a dry-run capture: write the validated
	// RunConfig to the named path (or stdout) and exit without
	// invoking the loop. The validator has already run inside
	// BuildRunConfig, so reaching this branch with an invalid config
	// is structurally impossible — that is what the spec means by
	// "never writes on validation failure".
	if outPath, _ := f.GetString("output-runconfig"); outPath != "" {
		return writeOutputRunConfig(outPath, cfg)
	}

	exportRequired, _ := f.GetBool("export-workspace-required")
	return runWithConfig(cfg, runOptions{exportWorkspaceRequired: exportRequired})
}

// bareHintStderr is the writer the bare-`stirrup harness` grouped help
// is emitted to. Indirection so tests can capture without rewriting
// the os.Stderr fd. Production code keeps the default.
var bareHintStderr io.Writer = os.Stderr

// isPromptRequiredErr reports whether err is the typed sentinel
// returned by resolvePromptForRun when no prompt source resolved.
// errors.Is — not a strings.Contains match on the message — keeps
// the detector immune to future ValidateRunConfig errors that
// happen to mention "prompt is required" in unrelated contexts.
func isPromptRequiredErr(err error) bool {
	return errors.Is(err, errPromptRequired)
}

// writeBareHarnessHint emits the grouped #249-B example block to w.
// Formatting (ANSI vs plain) is decided per-writer via colorEnabled —
// a non-os.Stderr writer or NO_COLOR=1 forces plain output. The
// template is a static literal rather than something derived from
// cobra's flag list because layout control matters more than schema
// coupling here: the operator scanning this for the first time wants
// curated groups, not the full 40-flag table that --help already
// serves.
func writeBareHarnessHint(w io.Writer) {
	// A write error here is unrecoverable — stderr going away
	// means the operator never sees the hint. Discard the return
	// rather than surfacing it, matching runRootHint's contract:
	// the bare-invocation path is a help surface, not a failure.
	_, _ = fmt.Fprint(w, bareHarnessHintText(colorEnabled(w)))
}

// bareHarnessHintText assembles the grouped help body. Exported as a
// pure function so tests can drive both colour modes without having
// to thread a writer through. References to #240 features
// (`--config -`, `--output-runconfig`, the `run-config` subcommand)
// are intentional: this branch is rebased on top of the #240 work
// and the parent branch carries the underlying functionality.
func bareHarnessHintText(color bool) string {
	var b strings.Builder
	b.WriteString(bold(color, "stirrup harness"))
	b.WriteString(" — run the agentic loop\n\n")

	b.WriteString(bold(color, "USAGE:"))
	b.WriteString("\n  stirrup harness [flags] [prompt]\n\n")

	b.WriteString(bold(color, "REQUIRED:"))
	b.WriteString("\n  --prompt ")
	b.WriteString(dim(color, `"<task>"`))
	b.WriteString("        Or pass as positional, --prompt-file, or STIRRUP_PROMPT env var.\n\n")

	b.WriteString(bold(color, "RUN SHAPE:"))
	b.WriteString("\n  --mode ")
	b.WriteString(dim(color, "planning|execution|review|research|toil"))
	b.WriteString("   Default: planning (read-only).\n  --max-turns ")
	b.WriteString(dim(color, "20"))
	b.WriteString("           Agentic loop turn cap (max 100).\n  --timeout ")
	b.WriteString(dim(color, "600"))
	b.WriteString("             Wall-clock seconds (max 3600).\n\n")

	b.WriteString(bold(color, "PROVIDER:"))
	b.WriteString("\n  --provider ")
	b.WriteString(dim(color, "anthropic"))
	b.WriteString("     anthropic | bedrock | gemini | openai-compatible | openai-responses\n  --model ")
	b.WriteString(dim(color, "claude-sonnet-4-6"))
	b.WriteString("\n  --api-key-ref ")
	b.WriteString(dim(color, "secret://ANTHROPIC_API_KEY"))
	b.WriteString("\n\n")

	b.WriteString(bold(color, "CONFIGURATION:"))
	b.WriteString("\n  --config ")
	b.WriteString(dim(color, "path.json"))
	b.WriteString("       Base config from a file (or `-` for stdin).\n  --output-runconfig ")
	b.WriteString(dim(color, "out.json"))
	b.WriteString("    Dry-run: write resolved config and exit.\n\n")

	b.WriteString(bold(color, "EXAMPLES:"))
	b.WriteString("\n  stirrup harness --prompt ")
	b.WriteString(dim(color, `"refactor module X"`))
	b.WriteString("\n  stirrup harness --mode execution --prompt ")
	b.WriteString(dim(color, `"fix the failing test"`))
	b.WriteString("\n  stirrup harness --config ")
	b.WriteString(dim(color, "production.json"))
	b.WriteString(" --prompt ")
	b.WriteString(dim(color, `"ship feature Y"`))
	b.WriteString("\n\n  ")
	b.WriteString(dim(color, "# Pipeline composition (see #240):"))
	b.WriteString("\n  stirrup run-config --max-turns 100 | stirrup harness --prompt ")
	b.WriteString(dim(color, `"Z"`))
	b.WriteString("\n\n")

	b.WriteString("For the full flag list: stirrup harness --help\n")
	b.WriteString("Documentation:          https://github.com/rxbynerd/stirrup/tree/main/docs\n")
	return b.String()
}

// writeOutputRunConfig emits the resolved RunConfig as pretty-printed
// JSON to the named path. The special value "-" writes to stdout so
// `--output-runconfig=-` composes with `stirrup run-config` for
// pipeline replay. Non-stdout paths open with 0600 — a captured
// RunConfig may carry secret:// references whose names are operationally
// sensitive even though the references themselves are not secrets, and
// the conservative mode matches how the rest of the harness writes
// operator-facing artifacts (trace files, prompt files).
func writeOutputRunConfig(path string, cfg *types.RunConfig) error {
	if path == "-" {
		return writeRunConfigJSON(os.Stdout, cfg, false)
	}
	// O_TRUNC because a captured config replaces any previous one at
	// the same path; the alternative ("append") would silently corrupt
	// a replay file.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("opening --output-runconfig %q: %w", path, err)
	}
	return writeAndCloseRunConfig(f, path, cfg)
}

// writeAndCloseRunConfig is the testable seam writeOutputRunConfig
// delegates to once a writer is available. Tests inject a synthetic
// io.WriteCloser whose Close returns an error to pin the deferred-
// flush diagnostic path.
//
// Linux's buffered file I/O can defer kernel page flushes until
// Close. A successful writeRunConfigJSON but a failed Close (ENOSPC,
// EIO, NFS commit failure) would otherwise hand the operator a
// zero-exit run with a corrupt or empty capture file and no
// diagnostic. Surface the close error when the prior write
// succeeded.
func writeAndCloseRunConfig(wc io.WriteCloser, path string, cfg *types.RunConfig) error {
	writeErr := writeRunConfigJSON(wc, cfg, false)
	if cerr := wc.Close(); cerr != nil && writeErr == nil {
		return fmt.Errorf("closing --output-runconfig %q: %w", path, cerr)
	}
	return writeErr
}

// applyAnthropicWIFOverrides folds the Anthropic-WIF flag surface and
// the documented env-var fallbacks into the RunConfig. Called from
// both paths so --config users and flag-only users see the same
// resolution chain:
//
//  1. Federation IDs: explicit flag > ANTHROPIC_*_ID env var > file value.
//     Setting any ID without a Credential block infers
//     credential.type=anthropic-wif.
//  2. Token source inference (only when Credential.TokenSource is nil
//     so a config-file source always wins):
//     - --anthropic-from-github-actions → github-actions-oidc with
//     Anthropic OAuth audience
//     - ANTHROPIC_IDENTITY_TOKEN_FILE → file source pointing at it
//     - ANTHROPIC_IDENTITY_TOKEN → env source
//     - Naked ACTIONS_ID_TOKEN_REQUEST_URL is NOT auto-selected
//     (issue #117 risk #5: silent IdP selection makes credential
//     bugs unfixable; require explicit opt-in).
//  3. apiKeyRef mutual exclusion: anthropic + anthropic-wif must not
//     also carry a static API key (issue #117 risk #4 — leftover
//     ANTHROPIC_API_KEY silently shadows federation in the SDK).
//     Explicit --api-key-ref is a hard error; the default
//     "secret://ANTHROPIC_API_KEY" is cleared silently because no
//     intent was expressed.
//
// Returns a non-nil error only on the apiKeyRef guard; everything
// else is best-effort folding that ValidateRunConfig will reject if
// the resulting shape is invalid.
func applyAnthropicWIFOverrides(cmd *cobra.Command, cfg *types.RunConfig) error {
	f := cmd.Flags()
	changed := func(name string) bool { return f.Changed(name) }

	// Step 1 — federation IDs from flags + env. Local helper keeps the
	// dispatch table compact. The middle "registered-default" branch
	// from an earlier draft has been collapsed: all four
	// --anthropic-* WIF flags register an empty-string default, so a
	// non-changed flag is always "", and falling through to env-var
	// lookup is the correct behaviour. Mirrors the gcp-credentials-file
	// shape elsewhere in this file.
	resolveID := func(flagName, envName string) string {
		if changed(flagName) {
			v, _ := f.GetString(flagName)
			return v
		}
		return os.Getenv(envName)
	}

	ruleID := resolveID("anthropic-federation-rule-id", "ANTHROPIC_FEDERATION_RULE_ID")
	orgID := resolveID("anthropic-organization-id", "ANTHROPIC_ORGANIZATION_ID")
	saID := resolveID("anthropic-service-account-id", "ANTHROPIC_SERVICE_ACCOUNT_ID")
	wsID := resolveID("anthropic-workspace-id", "ANTHROPIC_WORKSPACE_ID")
	fromGHA, _ := f.GetBool("anthropic-from-github-actions")

	anyIDSet := ruleID != "" || orgID != "" || saID != "" || wsID != ""

	// Step 2 — type inference. Only fire when the operator has
	// signalled WIF intent (any ID set, the GHA opt-in, or an existing
	// type=anthropic-wif config). A config that already names a
	// non-anthropic-wif credential type plus a federation ID is
	// inconsistent — surface it loudly rather than silently rewriting
	// the operator's choice.
	if !anyIDSet && !fromGHA &&
		(cfg.Provider.Credential == nil || cfg.Provider.Credential.Type != "anthropic-wif") {
		return nil
	}

	if cfg.Provider.Credential == nil {
		cfg.Provider.Credential = &types.CredentialConfig{Type: "anthropic-wif"}
	} else if cfg.Provider.Credential.Type == "" || cfg.Provider.Credential.Type == "static" {
		cfg.Provider.Credential.Type = "anthropic-wif"
	} else if cfg.Provider.Credential.Type != "anthropic-wif" && anyIDSet {
		return fmt.Errorf(
			"--anthropic-* federation flags imply credential.type=anthropic-wif, "+
				"but credential.type is already %q; remove the conflicting type or "+
				"the federation flags",
			cfg.Provider.Credential.Type)
	}

	// Apply IDs after the type is settled; an explicit/env value
	// overrides an existing value from --config. (changed() above is
	// the precedence gate — env-only fills in unset values.)
	if ruleID != "" {
		cfg.Provider.Credential.FederationRuleID = ruleID
	}
	if orgID != "" {
		cfg.Provider.Credential.OrganizationID = orgID
	}
	if saID != "" {
		cfg.Provider.Credential.ServiceAccountID = saID
	}
	if wsID != "" {
		cfg.Provider.Credential.WorkspaceID = wsID
	}

	// Step 3 — token-source inference. A config-file token source
	// always wins; we only fill in the slot when it is nil. Order
	// follows the issue's documented precedence: explicit GHA opt-in
	// first, then the two ANTHROPIC_IDENTITY_TOKEN_* env vars.
	//
	// If the operator passed --anthropic-from-github-actions but a
	// --config file already set credential.tokenSource, the flag has
	// no effect. Surface this as a warning so it is not silently
	// dropped — the config file wins, but the operator should know
	// their flag was discarded so they can fix the redundancy.
	if fromGHA && cfg.Provider.Credential.TokenSource != nil {
		slog.Warn("--anthropic-from-github-actions ignored: config file already specifies credential.tokenSource",
			"existing_type", cfg.Provider.Credential.TokenSource.Type)
	}
	if cfg.Provider.Credential.TokenSource == nil {
		switch {
		case fromGHA:
			// Audience defaults to the Anthropic OAuth host; operators
			// who need a different audience claim must use --config.
			cfg.Provider.Credential.TokenSource = &types.TokenSourceConfig{
				Type:     "github-actions-oidc",
				Audience: "https://api.anthropic.com",
			}
		case os.Getenv("ANTHROPIC_IDENTITY_TOKEN_FILE") != "":
			cfg.Provider.Credential.TokenSource = &types.TokenSourceConfig{
				Type: "file",
				Path: os.Getenv("ANTHROPIC_IDENTITY_TOKEN_FILE"),
			}
		case os.Getenv("ANTHROPIC_IDENTITY_TOKEN") != "":
			cfg.Provider.Credential.TokenSource = &types.TokenSourceConfig{
				Type:   "env",
				EnvVar: "ANTHROPIC_IDENTITY_TOKEN",
			}
			// Bare ACTIONS_ID_TOKEN_REQUEST_URL is intentionally NOT
			// handled here. Silent IdP selection from env presence is
			// rejected per issue #117 risk #5 — operators must opt in
			// via --anthropic-from-github-actions.
		}
	}

	// Step 4 — apiKeyRef mutual exclusion. Only enforce on the
	// anthropic provider with anthropic-wif credentials; other
	// combinations are validated separately (validateAnthropicProviderFields
	// catches a leftover --config value, but it does not know about the
	// per-provider default flag value being "secret://ANTHROPIC_API_KEY",
	// so we have to reconcile the default-vs-explicit case here before
	// validation runs).
	if cfg.Provider.Type == "anthropic" &&
		cfg.Provider.Credential != nil &&
		cfg.Provider.Credential.Type == "anthropic-wif" &&
		cfg.Provider.APIKeyRef != "" {
		if changed("api-key-ref") {
			return fmt.Errorf(
				"--api-key-ref must not be set with --anthropic-federation-rule-id " +
					"(or any other --anthropic-* federation flag): WIF authenticates " +
					"via OAuth bearer tokens and a static API key would silently " +
					"shadow the federated credential (issue #117 risk #4)")
		}
		// Default flag value carries no operator intent under WIF —
		// clear it silently so validateAnthropicProviderFields does not
		// reject the otherwise-valid config.
		cfg.Provider.APIKeyRef = ""
	}

	return nil
}

// runOptions carries CLI-only behaviour that doesn't fit on RunConfig.
// Today this is just exportWorkspaceRequired — a flag that controls
// whether a failed workspace export propagates a non-zero exit code.
// Threading it through here (rather than embedding it on RunConfig)
// keeps the wire schema free of CLI-shaped knobs.
type runOptions struct {
	exportWorkspaceRequired bool
}

// runWithConfig is the shared run path for both --config and flag-only
// invocations. Both code paths converge here once they have a validated
// RunConfig — ValidateRunConfig rejects nil Timeout, so the dereference
// below is safe.
func runWithConfig(config *types.RunConfig, opts runOptions) error {
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
	// resultSink emission is the last thing on stdout for the run, so
	// a Cloud Logging grep / shell pipeline can extract the
	// "STIRRUP_RESULT <json>" line deterministically. Failures are
	// logged inside emitRunResult and never fatal — the trace and the
	// stderr summary already reflect outcome.
	emitRunResult(ctx, config, runTrace)

	// Workspace export (issue #164). Called after the trace and
	// resultSink so a failed upload's slog warning lands after the
	// run's structured outcome — easier to correlate during
	// post-mortem. When required, the error here propagates and
	// becomes the process exit status.
	if err := exportWorkspace(ctx, config, opts.exportWorkspaceRequired); err != nil {
		return err
	}

	if config.FollowUpGrace != nil && *config.FollowUpGrace > 0 {
		core.RunFollowUpLoop(ctx, loop, config, *config.FollowUpGrace)
	}
	return nil
}
