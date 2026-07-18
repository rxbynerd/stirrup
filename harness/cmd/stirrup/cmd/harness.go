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
	flag "github.com/spf13/pflag"

	"github.com/rxbynerd/stirrup/harness/internal/core"
	"github.com/rxbynerd/stirrup/types"
)

// maxPromptFileBytes caps --prompt-file reads to avoid OOM on a
// malformed input (matches local.go's maxFileSize; duplicated because
// that constant is package-private).
const maxPromptFileBytes int64 = 10 * 1024 * 1024 // matches local.go maxFileSize

// readPromptFile loads a --prompt-file from disk with a size cap, an
// empty check, and trailing-newline trimming. Relative paths resolve
// against the CWD, parallel to --config rather than --workspace.
func readPromptFile(path string) (string, error) {
	// Every failure here is an I/O error (exit 3); the prompt file is
	// plain text, so none is a JSON parse failure.
	clean := filepath.Clean(path)
	info, err := os.Stat(clean)
	if err != nil {
		return "", ioError(fmt.Errorf("reading --prompt-file %q: %w", path, err))
	}
	if info.IsDir() {
		return "", ioError(fmt.Errorf("reading --prompt-file %q: is a directory", path))
	}
	// Reject FIFOs/char devices/sockets: os.Stat reports Size()==0 for
	// these, which would sail past the size cap below and could hang
	// ReadAll forever (e.g. /dev/zero or an unwritten FIFO).
	if !info.Mode().IsRegular() {
		return "", ioError(fmt.Errorf("reading --prompt-file %q: not a regular file", path))
	}
	if info.Size() > maxPromptFileBytes {
		return "", ioError(fmt.Errorf("reading --prompt-file %q: %d bytes exceeds %d byte cap", path, info.Size(), maxPromptFileBytes))
	}
	// Bounded read closes the TOCTOU window where the file grows
	// between os.Stat and Open; +1 byte distinguishes "at the cap"
	// from "over the cap" for an accurate error.
	f, err := os.Open(clean)
	if err != nil {
		return "", ioError(fmt.Errorf("reading --prompt-file %q: %w", path, err))
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxPromptFileBytes+1))
	if err != nil {
		return "", ioError(fmt.Errorf("reading --prompt-file %q: %w", path, err))
	}
	if int64(len(data)) > maxPromptFileBytes {
		return "", ioError(fmt.Errorf("reading --prompt-file %q: exceeds %d byte cap", path, maxPromptFileBytes))
	}
	// Trim only trailing CR/LF; leading whitespace (e.g. an
	// intentionally-indented code block) round-trips unchanged.
	trimmed := strings.TrimRight(string(data), "\r\n")
	if trimmed == "" {
		return "", ioError(fmt.Errorf("--prompt-file %q is empty", path))
	}
	return trimmed, nil
}

// harnessCLIOptions captures every CLI-surfaced setting that influences the
// RunConfig built by buildHarnessRunConfig. Extracted so the construction
// path is testable without booting cobra.
type harnessCLIOptions struct {
	RunID        string
	Mode         string
	SessionName  string
	Prompt       string
	ProviderType string
	APIKeyRef    string
	BaseURL      string
	APIKeyHeader string
	QueryParams  map[string]string

	// CompatProfile selects an optional provider-quirks compat profile;
	// closed set validated by ValidateRunConfig, empty by default.
	CompatProfile string

	// ToolsProfile selects the model-facing toolset profile; closed set
	// validated by ValidateRunConfig, empty by default (identity
	// presentation).
	ToolsProfile string
	Model        string

	// PromptModel pins the model identity the system prompt templates
	// render against without changing the wire model. Empty derives it
	// from Model.
	PromptModel string

	Workspace     string
	MaxTurns      int
	Timeout       int
	TracePath     string
	TransportType string
	TransportAddr string
	FollowUpGrace int
	LogLevel      string

	// Temperature overrides the loop's default sampling temperature.
	// Nil means "do not override". A pointer disambiguates an explicit
	// --temperature=0 (greedy decoding) from the flag being absent,
	// since cobra's Float64 store cannot represent absence.
	Temperature *float64

	// Vertex AI Gemini provider fields; meaningful only when
	// ProviderType == "gemini" (ValidateRunConfig rejects them otherwise).
	GCPProject         string
	GCPLocation        string
	GCPCredentialsFile string

	// Anthropic Workload Identity Federation fields; meaningful only
	// when ProviderType == "anthropic". Any of the four ID fields being
	// set infers credential.type=anthropic-wif.
	AnthropicFederationRuleID string
	AnthropicOrganizationID   string
	AnthropicServiceAccountID string
	AnthropicWorkspaceID      string
	// AnthropicFromGitHubActions opts into the runner-injected
	// ACTIONS_ID_TOKEN_REQUEST_URL/_TOKEN fallback explicitly; implicit
	// selection from env presence is rejected.
	AnthropicFromGitHubActions bool

	// Azure Entra ID Workload Identity Federation fields; meaningful only
	// against an Azure OpenAI / Foundry endpoint. --azure-tenant-id
	// implies credential.type=azure-workload-identity. TokenSource
	// selection must come from --config.
	AzureTenantID string
	AzureClientID string
	AzureScope    string

	// Component-selection escape hatches. Empty strings fall back to the
	// documented default (local executor, multi edit strategy, none
	// verifier, none git strategy, jsonl trace emitter).
	ExecutorType     string
	EditStrategyType string
	VerifierType     string
	GitStrategyType  string
	TraceEmitterType string
	OTelEndpoint     string
	OTelProtocol     string

	// OTelHeaders carries parsed --otel-header key=value entries.
	// Values may be "secret://" references, resolved at exporter init;
	// RunConfig.Redact() strips them before any trace is persisted.
	OTelHeaders map[string]string

	// OTelMetricsEndpoint sets traceEmitter.metricsEndpoint for runs
	// whose metrics target a different collector than traces. Empty
	// defers to the trace endpoint.
	OTelMetricsEndpoint string

	// OTelCaptureContent opts the otel emitter into GenAI semconv
	// content capture; see types.TraceEmitterConfig.CaptureContent for
	// the PII rationale.
	OTelCaptureContent bool

	// Safety-ring escape hatches. Empty string leaves the field unset so
	// ValidateRunConfig's mode-aware defaulting can take over.
	ContainerRuntime     string
	PermissionPolicyFile string
	CodeScannerType      string

	// K8s executor escape hatches (--executor=k8s or k8s-sandbox).
	// K8sNamespace is required; the rest are optional. The sandbox
	// runtime derives from ContainerRuntime (k8s-sandbox is
	// gVisor-only and forces "gvisor").
	K8sNamespace      string
	K8sKubeconfig     string
	K8sServiceAccount string
	K8sNodeSelector   map[string]string
	K8sEgressProxyURL string

	// GuardRail escape hatches. An entirely-zero trio leaves
	// config.GuardRail nil so the factory installs the no-op "none"
	// guard. Composite stages require a --config file.
	GuardRailType     string
	GuardRailEndpoint string
	GuardRailModel    string
	GuardRailFailOpen bool

	// Observability resource attributes. Empty values fall through to
	// env-var fallbacks (OTEL_DEPLOYMENT_ENVIRONMENT,
	// OTEL_SERVICE_NAMESPACE) and then to defaults.
	DeploymentEnvironment string
	ServiceNamespace      string

	// LogExport opts structured logs into OTLP export alongside the
	// stderr default. "none"/empty keeps stderr-only; "otlp" adds the
	// OTLP/gRPC log exporter. LogExportEndpoint defers to the trace
	// emitter's endpoint when empty.
	LogExport         string
	LogExportEndpoint string

	// Provider retry policy overrides. Zero values leave the
	// corresponding Provider.Retry field unset so ValidateRunConfig
	// fills in its defaults. Applies only to the default provider;
	// multi-provider retry policies require --config.
	ProviderRetryMaxAttempts     int
	ProviderRetryInitialDelay    time.Duration
	ProviderRetryMaxDelay        time.Duration
	ProviderRetryWallClockBudget time.Duration

	// Workspace export. WorkspaceExportTo is a gs:// URI stored on
	// RunConfig.Executor.WorkspaceExportTo. WorkspaceExportRequired is
	// CLI-only (not persisted on RunConfig): it controls whether a
	// failed export terminates the run non-zero.
	WorkspaceExportTo       string
	WorkspaceExportRequired bool

	// ToolDispatchMaxParallel sets the parallel async-tool dispatch
	// fan-out. Zero defers to DefaultToolDispatchMaxParallel by leaving
	// config.ToolDispatch nil.
	ToolDispatchMaxParallel int

	// EscalateToolChoice opts the run into missed-tool recovery; false
	// keeps the loop's escalation path inert. EscalateToolChoiceMaxRetries
	// is ignored unless EscalateToolChoice is set.
	EscalateToolChoice           bool
	EscalateToolChoiceMaxRetries int

	// Batch opts the run into async batch submission, carrying only the
	// Enabled bit; the remaining BatchProviderConfig fields require
	// --config.
	Batch bool
}

// buildHarnessRunConfig assembles the RunConfig used by `stirrup harness`,
// encoding defaults such as the per-mode permission policy and read-only
// built-in tool list. Kept pure so tests can exercise every --mode value
// without invoking the agentic loop.
//
// Returns a non-nil error only when a flag fails an up-front sanity
// check that would otherwise silently erase operator intent (e.g. a
// sub-millisecond retry duration truncating to zero); most validation
// happens later in ValidateRunConfig.
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
	// EditStrategyType intentionally not defaulted here: an empty value
	// is filled with "multi" by types.ValidateRunConfig, keeping the
	// default in one place for CLI, gRPC, and direct embedding alike.
	editStrategyType := opts.EditStrategyType
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
		// Empty Protocol falls through to the OTel SDK's grpc default.
		traceEmitter.Protocol = opts.OTelProtocol
		traceEmitter.Headers = opts.OTelHeaders
		traceEmitter.MetricsEndpoint = opts.OTelMetricsEndpoint
		traceEmitter.CaptureContent = opts.OTelCaptureContent
	}

	config := &types.RunConfig{
		RunID:       opts.RunID,
		Mode:        opts.Mode,
		SessionName: opts.SessionName,
		Prompt:      opts.Prompt,
		Provider: types.ProviderConfig{
			Type: opts.ProviderType,
			// Dropped for gemini (Vertex AI uses GCP IAM) and for Azure
			// WIF (bearer comes from OAuth2 exchange); the validator
			// rejects APIKeyRef in both cases, and the cobra default
			// would otherwise trip it with a confusing error.
			APIKeyRef: func() string {
				if opts.ProviderType == "gemini" || opts.AzureTenantID != "" {
					return ""
				}
				return opts.APIKeyRef
			}(),
			BaseURL:       opts.BaseURL,
			APIKeyHeader:  opts.APIKeyHeader,
			QueryParams:   opts.QueryParams,
			CompatProfile: opts.CompatProfile,
		},
		ModelRouter: types.ModelRouterConfig{
			Type:     "static",
			Provider: opts.ProviderType,
			Model:    opts.Model,
		},
		PromptBuilder:   types.PromptBuilderConfig{Type: "default", PromptModel: opts.PromptModel},
		ContextStrategy: types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
		Executor: types.ExecutorConfig{
			Type:              executorType,
			Workspace:         opts.Workspace,
			Runtime:           opts.ContainerRuntime,
			WorkspaceExportTo: opts.WorkspaceExportTo,
			K8sNamespace:      opts.K8sNamespace,
			K8sKubeconfig:     opts.K8sKubeconfig,
			K8sNodeSelector:   opts.K8sNodeSelector,
			K8sServiceAccount: opts.K8sServiceAccount,
			K8sEgressProxyURL: opts.K8sEgressProxyURL,
		},
		EditStrategy: types.EditStrategyConfig{Type: editStrategyType},
		Verifier:     types.VerifierConfig{Type: verifierType},
		GitStrategy:  types.GitStrategyConfig{Type: gitStrategyType},
		// Tools.BuiltIn is left empty (validator treats empty as "all
		// built-ins"); applyModeDefaults fills it for read-only modes.
		Tools:        types.ToolsConfig{Profile: opts.ToolsProfile},
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

	// Scoped to gemini so --gcp-* flags left at their defaults do not
	// leak onto non-gemini runs; the validator rejects them otherwise.
	if opts.ProviderType == "gemini" {
		config.Provider.GCPProject = opts.GCPProject
		config.Provider.GCPLocation = opts.GCPLocation
		config.Provider.GCPCredentialsFile = opts.GCPCredentialsFile
	}

	// --gcp-credentials-file implies credential.type=gcp-service-account
	// when no other credential type is configured (file path is the
	// discriminator, mirroring --permission-policy-file below).
	if opts.ProviderType == "gemini" && opts.GCPCredentialsFile != "" && config.Provider.Credential == nil {
		config.Provider.Credential = &types.CredentialConfig{Type: "gcp-service-account"}
	}

	// Anthropic WIF is populated exclusively by applyAnthropicWIFOverrides
	// at the runHarness call site, which owns flag + env-var precedence
	// for both the --config and flag-only paths.

	// --azure-tenant-id implies credential.type=azure-workload-identity
	// when no other credential type is configured; TokenSource selection
	// still requires --config (too many shapes to express as flags), so
	// a flag-only invocation fails validateRunConfig with a clear
	// "requires tokenSource" error rather than silently dropping the flag.
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
		// Implies type=policy-engine when the caller has not chosen a
		// policy elsewhere.
		config.PermissionPolicy.PolicyFile = opts.PermissionPolicyFile
		if config.PermissionPolicy.Type == "" {
			config.PermissionPolicy.Type = "policy-engine"
		}
	}
	if opts.CodeScannerType != "" {
		config.CodeScanner = &types.CodeScannerConfig{Type: opts.CodeScannerType}
	}

	// Constructed only when the caller touched at least one GuardRail
	// flag; an entirely-empty set leaves config.GuardRail nil so the
	// factory installs the no-op "none" guard.
	if opts.GuardRailType != "" || opts.GuardRailEndpoint != "" || opts.GuardRailModel != "" || opts.GuardRailFailOpen {
		config.GuardRail = &types.GuardRailConfig{
			Type:     opts.GuardRailType,
			Endpoint: opts.GuardRailEndpoint,
			Model:    opts.GuardRailModel,
			FailOpen: opts.GuardRailFailOpen,
		}
	}

	// Constructed only when the caller touched at least one of the
	// relevant flags, matching the GuardRail pattern above. "none" is
	// treated as not-set so a bare --log-export none does not
	// materialise a sub-config; empty/"none" is stderr-only either way.
	logExport := opts.LogExport
	if logExport == "none" {
		logExport = ""
	}
	if opts.DeploymentEnvironment != "" || opts.ServiceNamespace != "" || logExport != "" || opts.LogExportEndpoint != "" {
		config.Observability = types.ObservabilityConfig{
			Environment:      opts.DeploymentEnvironment,
			ServiceNamespace: opts.ServiceNamespace,
			LogsExport: types.LogsExportConfig{
				Type:     logExport,
				Endpoint: opts.LogExportEndpoint,
			},
		}
	}

	// Only allocate Provider.Retry when the caller touched at least one
	// flag; otherwise leave it nil so ValidateRunConfig fills defaults.
	if err := applyProviderRetryOverrides(&config.Provider, opts); err != nil {
		return nil, err
	}

	// Nil ToolDispatch lets the loop reach for
	// DefaultToolDispatchMaxParallel via EffectiveToolDispatchMaxParallel.
	if opts.ToolDispatchMaxParallel > 0 {
		config.ToolDispatch = &types.ToolDispatchConfig{MaxParallel: opts.ToolDispatchMaxParallel}
	}

	if opts.EscalateToolChoice {
		config.ToolChoiceEscalation = &types.ToolChoiceEscalationConfig{
			Enabled:    true,
			MaxRetries: opts.EscalateToolChoiceMaxRetries,
		}
	}

	// --batch carries only the Enabled bit; the remaining
	// BatchProviderConfig fields require --config since flag syntax
	// cannot express their cross-field invariants.
	if opts.Batch {
		config.Provider.Batch = &types.BatchProviderConfig{Enabled: true}
	}

	return config, nil
}

// applyProviderRetryOverrides mutates pc.Retry to reflect any of the
// four provider-retry CLI flags the operator set; an unset flag (zero
// value) leaves its slot zero so ValidateRunConfig's per-field
// defaulting fills it in.
//
// Returns a non-nil error if any non-zero duration is below 1ms
// resolution — otherwise the millisecond conversion truncates to zero
// and is indistinguishable from "flag not set".
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
// flag's value iff the operator set it, and nil otherwise. cobra's
// Float64 store cannot represent absence — an unset flag and an
// explicit --foo=0 both read back as 0.0 — so Changed() is the only
// safe disambiguator.
func optionalFloat64Flag(cmd *cobra.Command, name string) *float64 {
	f := cmd.Flags()
	if !f.Changed(name) {
		return nil
	}
	v, _ := f.GetFloat64(name)
	return &v
}

// retryDurationToMs converts a positive Duration to whole milliseconds,
// rejecting any non-zero value below 1ms. Zero (the flag default)
// returns zero with no error, preserving the "flag not set" path.
func retryDurationToMs(flagName string, d time.Duration) (int, error) {
	if d == 0 {
		return 0, nil
	}
	if d < time.Millisecond {
		return 0, fmt.Errorf("%s: minimum resolution is 1ms, got %v", flagName, d)
	}
	return int(d / time.Millisecond), nil
}

// applyModeDefaults fills in PermissionPolicy and the read-only
// Tools.BuiltIn list based on cfg.Mode, but only for fields the caller
// has not set explicitly — an explicit conflicting choice is left for
// ValidateRunConfig to reject rather than silently rewritten. Shared
// between the flag-only and --config paths so both produce consistent
// configs.
func applyModeDefaults(cfg *types.RunConfig) {
	if types.IsReadOnlyMode(cfg.Mode) {
		if cfg.PermissionPolicy.Type == "" {
			cfg.PermissionPolicy = types.PermissionPolicyConfig{Type: "deny-side-effects"}
		}
		// The validator rejects an empty Tools.BuiltIn for read-only
		// modes; the default is executor-aware because executor.type="none"
		// has no filesystem/shell capability at all (see
		// DefaultReadOnlyBuiltInToolsForExecutor's doc comment).
		if len(cfg.Tools.BuiltIn) == 0 {
			cfg.Tools.BuiltIn = types.DefaultReadOnlyBuiltInToolsForExecutor(cfg.Executor.Type)
		}
	} else if cfg.PermissionPolicy.Type == "" {
		cfg.PermissionPolicy = types.PermissionPolicyConfig{Type: "allow-all"}
	}
}

// maxConfigFileBytes caps --config reads to avoid OOM on a malformed
// input; a RunConfig is at most a few KB.
const maxConfigFileBytes int64 = 1 << 20 // 1 MiB

// loadRunConfigFile reads a JSON file at path and unmarshals it into a
// RunConfig, mirroring the schema in proto/harness/v1/harness.proto.
// Unknown JSON fields are rejected so config typos surface as errors.
func loadRunConfigFile(path string) (*types.RunConfig, error) {
	// I/O-class errors (exit 3, "reading config file") vs. the decode
	// step's parse-class error (exit 2, "parsing config file") so the
	// operator-facing wording matches the exit code.
	info, err := os.Stat(path)
	if err != nil {
		return nil, ioError(fmt.Errorf("reading config file %q: %w", path, err))
	}
	if info.IsDir() {
		return nil, ioError(fmt.Errorf("reading config file %q: is a directory", path))
	}
	if info.Size() > maxConfigFileBytes {
		return nil, ioError(fmt.Errorf("reading config file %q: %d bytes exceeds %d byte cap", path, info.Size(), maxConfigFileBytes))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, ioError(fmt.Errorf("reading config file %q: %w", path, err))
	}
	if len(data) == 0 {
		// "reading", not "parsing": an empty file never reached the JSON
		// decoder, so the I/O exit class (3) and the wording agree. The
		// parallel readRunConfigFromReader empty-stdin path already says
		// "reading", so this aligns the two.
		return nil, ioError(fmt.Errorf("reading config file %q: file is empty", path))
	}
	var cfg types.RunConfig
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, parseError(fmt.Errorf("parsing config file %q: %w", path, err))
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

	// RunConfig-producing flags live in addRunConfigFlags so the
	// run-config subcommand registers the same set without drifting.
	addRunConfigFlags(harnessCmd)

	f := harnessCmd.Flags()
	f.Bool("export-workspace-required", false, "When true, a failed workspace export exits the run non-zero. When false (default), failures are logged and the run's exit code is unchanged.")
	f.String("output-runconfig", "", "Write the resolved RunConfig as JSON to <path> (use '-' for stdout) and exit without running. Useful for capturing the exact config a flag-only invocation would have used. Validation must pass first; the path is not written on a validator error.")
	f.Bool("dry-run", false, "Run every initialisation step short of the first agentic turn (validate, construct components, resolve credentials, probe provider/MCP/trace/egress reachability), print a per-step preflight report, then exit. No provider tokens are spent. Exit 0 when all steps pass, 1 when any probe fails, 4 for an invalid flag combination. Composes with --output-runconfig (both run) and --output=json (emits the report as JSON).")
	f.Bool("no-probe-provider", false, "With --dry-run, skip the provider reachability probe (for cost-controlled environments that do not want any provider contact). Meaningless without --dry-run (exit 4).")
	f.Bool("no-probe-mcp", false, "With --dry-run, skip the MCP server reachability probe. Meaningless without --dry-run (exit 4).")
	f.Bool("no-probe-trace", false, "With --dry-run, skip the trace-emitter reachability probe. Meaningless without --dry-run (exit 4).")
	f.Bool("no-probe-egress", false, "With --dry-run, skip the egress-allowlist DNS probe. Meaningless without --dry-run (exit 4).")
	f.Bool("no-probe-executor", false, "With --dry-run, skip the container-engine probe (socket ping + image-present). The executor step reports skip; no engine is contacted. Meaningless without --dry-run (exit 4).")
	f.Duration("dry-run-timeout", core.DefaultPreflightTimeout, "With --dry-run, the total wall-clock budget for the preflight. Meaningless without --dry-run (exit 4).")
	f.StringP("output", "o", "text", "Post-run summary format: text (default human-readable summary on stderr), json (structured RunResult JSON on stdout, suppresses stderr summary), none (suppresses both). When json is set together with resultSink.type=stdout-json the line is emitted once (the flag wins); pair json with a trace emitter that does not target stdout (the default jsonl file path is fine).")

	// --output-runconfig accepts a path or "-" for stdout. The .json
	// hint nudges the shell toward the conventional extension; "-" is
	// a literal one-character argument no completion engine needs to
	// suggest, so the file-name completion alone is sufficient.
	_ = harnessCmd.MarkFlagFilename("output-runconfig", "json")

	// --output is a closed three-value set; pin the completion list so
	// shells surface the same values the validator enforces.
	_ = harnessCmd.RegisterFlagCompletionFunc("output", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return []string{"text", "json", "none"}, cobra.ShellCompDirectiveNoFileComp
	})
}

// validateOutputMode rejects any --output value outside the closed
// three-value set, before the loop builds, so a typo surfaces as a clear
// error rather than a silently missing summary at end-of-run.
func validateOutputMode(mode string) error {
	switch mode {
	case "text", "json", "none":
		return nil
	default:
		return fmt.Errorf("--output: invalid value %q (expected one of: text, json, none)", mode)
	}
}

// applyOverrides mutates cfg in place, replacing fields whose
// corresponding flag was explicitly set on the command line; flags left
// at their default deliberately do NOT override the file.
//
// Returns a non-nil error when an override is structurally invalid
// (today, only a malformed --query-param entry): silently dropping it
// would let a request reach the provider missing a required parameter
// and surface as an opaque HTTP 400 instead of a clear operator error.
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
		// --trace is the JSONL path; coerce the emitter type to jsonl
		// unless --trace-emitter was also set explicitly.
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
	// See optionalFloat64Flag: without the Changed() check, omitting
	// --temperature would silently rewrite a file-provided value to 0.
	if t := optionalFloat64Flag(cmd, "temperature"); t != nil {
		cfg.Temperature = t
	}
	if changed("log-level") {
		cfg.LogLevel, _ = f.GetString("log-level")
	}
	if changed("provider") {
		cfg.Provider.Type, _ = f.GetString("provider")
		// Vertex AI uses GCP IAM, not API keys; clear a lingering
		// file-provided APIKeyRef on switching to gemini, mirroring
		// buildHarnessRunConfig's flag-only behaviour.
		if cfg.Provider.Type == "gemini" && !changed("api-key-ref") {
			cfg.Provider.APIKeyRef = ""
		}
	}
	if changed("model") {
		// For static routers this is where the active model lives.
		cfg.ModelRouter.Model, _ = f.GetString("model")
	}
	if changed("prompt-model") {
		cfg.PromptBuilder.PromptModel, _ = f.GetString("prompt-model")
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
	if changed("provider-compat-profile") {
		cfg.Provider.CompatProfile, _ = f.GetString("provider-compat-profile")
	}
	if changed("tools-profile") {
		cfg.Tools.Profile, _ = f.GetString("tools-profile")
	}
	if changed("gcp-project") {
		cfg.Provider.GCPProject, _ = f.GetString("gcp-project")
	}
	if changed("gcp-location") {
		cfg.Provider.GCPLocation, _ = f.GetString("gcp-location")
	}
	// Apply the documented "global" default explicitly on the --config
	// path; the flag-only path gets it for free from the cobra default.
	if cfg.Provider.Type == "gemini" && cfg.Provider.GCPLocation == "" {
		cfg.Provider.GCPLocation = "global"
	}
	if changed("gcp-credentials-file") {
		path, _ := f.GetString("gcp-credentials-file")
		cfg.Provider.GCPCredentialsFile = path
		// Implies gcp-service-account credential type so the file is
		// read rather than falling through to ADC; an existing
		// Credential.Type from the file wins.
		if path != "" && cfg.Provider.Credential == nil {
			cfg.Provider.Credential = &types.CredentialConfig{Type: "gcp-service-account"}
		}
	}

	// Anthropic WIF folding lives in BuildRunConfig as a single
	// post-override step (see runconfigbuilder.go); calling it again
	// here would double-invoke its slog.Warn diagnostics.

	// --azure-tenant-id alone implies credential.type=azure-workload-identity
	// (mirroring --gcp-credentials-file); --azure-client-id/--azure-scope
	// fill in fields on whichever Credential block results. An explicit
	// Credential block of a different type in the file wins.
	if changed("azure-tenant-id") {
		tenantID, _ := f.GetString("azure-tenant-id")
		if tenantID != "" && cfg.Provider.Credential == nil {
			cfg.Provider.Credential = &types.CredentialConfig{Type: "azure-workload-identity"}
		}
		if cfg.Provider.Credential != nil {
			cfg.Provider.Credential.AzureTenantID = tenantID
		}
		// Azure WIF resolves the bearer via OAuth2 exchange; the
		// validator rejects APIKeyRef alongside it. Explicit
		// --api-key-ref on the same command line wins.
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
		// Replace rather than merge: explicit --query-param flags clear
		// any QueryParams from the --config file, mirroring --base-url.
		raw, _ := f.GetStringArray("query-param")
		cfg.Provider.QueryParams = nil
		for _, entry := range raw {
			k, v, err := parseQueryParam(entry)
			if err != nil {
				// Hard-fail rather than dropping the malformed entry.
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
	if changed("otel-header") {
		// Replace rather than merge, mirroring --query-param above.
		raw, _ := f.GetStringArray("otel-header")
		cfg.TraceEmitter.Headers = nil
		for _, entry := range raw {
			k, v, err := parseQueryParam(entry)
			if err != nil {
				// Hard-fail rather than dropping the malformed entry.
				return fmt.Errorf("--otel-header %q: %w", entry, err)
			}
			if cfg.TraceEmitter.Headers == nil {
				cfg.TraceEmitter.Headers = map[string]string{}
			}
			cfg.TraceEmitter.Headers[k] = v
		}
	}
	if changed("otel-metrics-endpoint") {
		cfg.TraceEmitter.MetricsEndpoint, _ = f.GetString("otel-metrics-endpoint")
	}
	if changed("otel-capture-content") {
		cfg.TraceEmitter.CaptureContent, _ = f.GetBool("otel-capture-content")
	}
	if changed("container-runtime") {
		cfg.Executor.Runtime, _ = f.GetString("container-runtime")
	}
	if changed("k8s-namespace") {
		cfg.Executor.K8sNamespace, _ = f.GetString("k8s-namespace")
	}
	if changed("k8s-kubeconfig") {
		cfg.Executor.K8sKubeconfig, _ = f.GetString("k8s-kubeconfig")
	}
	if changed("k8s-service-account") {
		cfg.Executor.K8sServiceAccount, _ = f.GetString("k8s-service-account")
	}
	if changed("k8s-egress-proxy-url") {
		cfg.Executor.K8sEgressProxyURL, _ = f.GetString("k8s-egress-proxy-url")
	}
	if changed("k8s-node-selector") {
		raw, _ := f.GetStringArray("k8s-node-selector")
		var selector map[string]string
		for _, entry := range raw {
			k, v, err := parseQueryParam(entry)
			if err != nil {
				return fmt.Errorf("--k8s-node-selector %q: %w", entry, err)
			}
			if selector == nil {
				selector = map[string]string{}
			}
			selector[k] = v
		}
		cfg.Executor.K8sNodeSelector = selector
	}
	if changed("permission-policy-file") {
		path, _ := f.GetString("permission-policy-file")
		cfg.PermissionPolicy.PolicyFile = path
		// Only flip to policy-engine when the file didn't already name a
		// type; mirrors the buildHarnessRunConfig shortcut.
		if path != "" && cfg.PermissionPolicy.Type == "" {
			cfg.PermissionPolicy.Type = "policy-engine"
		}
	}
	if changed("code-scanner") {
		typ, _ := f.GetString("code-scanner")
		if typ == "" {
			// Empty flag falls back to the mode default via
			// applyCodeScannerDefault during validation.
			cfg.CodeScanner = nil
		} else {
			cfg.CodeScanner = &types.CodeScannerConfig{Type: typ}
		}
	}
	// Each GuardRail flag is independently overrideable; empty type
	// clears the GuardRail entirely, matching --code-scanner above.
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
	// Each flag independently overrides the corresponding field without
	// restating the rest of the file's observability block.
	if changed("deployment-environment") {
		cfg.Observability.Environment, _ = f.GetString("deployment-environment")
	}
	if changed("service-namespace") {
		cfg.Observability.ServiceNamespace, _ = f.GetString("service-namespace")
	}
	// "none" maps to the empty (stderr-only) form so --log-export none
	// flips a file's "otlp" back off. OTEL_EXPORTER_OTLP_LOGS_ENDPOINT,
	// when set, pins the log endpoint regardless of the file value.
	if changed("log-export") {
		v, _ := f.GetString("log-export")
		if v == "none" {
			v = ""
		}
		cfg.Observability.LogsExport.Type = v
	}
	if endpoint := os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"); endpoint != "" {
		cfg.Observability.LogsExport.Endpoint = endpoint
	}

	if changed("export-workspace-to") {
		cfg.Executor.WorkspaceExportTo, _ = f.GetString("export-workspace-to")
	}

	// Explicit zero clears the field so the loop falls back to
	// DefaultToolDispatchMaxParallel.
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

	if err := applyProviderRetryFlagOverrides(cmd, &cfg.Provider); err != nil {
		return err
	}

	// --batch only flips the Enabled bit; other Batch fields must come
	// from --config. Preserve the rest of an existing Batch block (e.g.
	// harnessSidePolling) rather than replacing it.
	if changed("batch") {
		enabled, _ := f.GetBool("batch")
		if enabled {
			if cfg.Provider.Batch == nil {
				cfg.Provider.Batch = &types.BatchProviderConfig{Enabled: true}
			} else {
				cfg.Provider.Batch.Enabled = true
			}
		} else if cfg.Provider.Batch != nil {
			// Preserve the struct (unlike --code-scanner ""/--guardrail
			// "" which nil the sub-config) so a follow-up --batch=true
			// re-enables without re-supplying --config.
			cfg.Provider.Batch.Enabled = false
		}
	}

	// --escalate-tool-choice flips Enabled, preserving any file-supplied
	// MaxRetries (same toggle rationale as --batch).
	if changed("escalate-tool-choice") {
		enabled, _ := f.GetBool("escalate-tool-choice")
		if enabled {
			if cfg.ToolChoiceEscalation == nil {
				cfg.ToolChoiceEscalation = &types.ToolChoiceEscalationConfig{}
			}
			cfg.ToolChoiceEscalation.Enabled = true
		} else if cfg.ToolChoiceEscalation != nil {
			cfg.ToolChoiceEscalation.Enabled = false
		}
	}
	if changed("escalate-tool-choice-max-retries") {
		mr, _ := f.GetInt("escalate-tool-choice-max-retries")
		if cfg.ToolChoiceEscalation == nil {
			cfg.ToolChoiceEscalation = &types.ToolChoiceEscalationConfig{}
		}
		cfg.ToolChoiceEscalation.MaxRetries = mr
	}
	return nil
}

// applyProviderRetryFlagOverrides mirrors applyProviderRetryOverrides
// for the --config path, using cmd.Flags().Changed() so a flag left at
// its zero default does not clobber a file-supplied value.
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

	// Validate --output before any side effects (file reads, credential
	// resolution) in BuildRunConfig below.
	outputMode, _ := f.GetString("output")
	if err := validateOutputMode(outputMode); err != nil {
		return err
	}

	cfg, err := BuildRunConfig(RunConfigSources{
		Stdin:      os.Stdin,
		ConfigPath: configPath,
		Cmd:        cmd,
		Args:       args,
		Resolve:    ResolveAll,
	})
	if err != nil {
		// A bare `stirrup harness` on an interactive terminal reaches the
		// prompt-required gate with this sentinel. Print the hint to
		// stderr and exit 0 (returning nil so Cobra appends neither its
		// error line nor usage block). Colour is auto-detected against
		// the SAME writer the hint is written to, to avoid leaking ANSI
		// into a redirected non-TTY stderr. Non-TTY callers never
		// produce this sentinel, so scripted use keeps its terse,
		// non-zero failure.
		if errors.Is(err, errPromptHintRequested) {
			w := cmd.ErrOrStderr()
			printHarnessUsageHint(w, shouldColor(colorAuto, w))
			return nil
		}
		return err
	}

	// Probe gates and timeout only make sense alongside --dry-run; runs
	// after BuildRunConfig so a bad config surfaces its own exit class
	// first.
	dryRun, _ := f.GetBool("dry-run")
	if err := validateDryRunFlags(f, dryRun); err != nil {
		return err
	}
	if dryRun {
		outPath, _ := f.GetString("output-runconfig")
		return runDryRun(cmd, cfg, dryRunOptionsFromFlags(f), outputMode, outPath)
	}

	// Config capture: write the validated RunConfig and exit without
	// invoking the loop. BuildRunConfig has already validated, so this
	// never writes on a validation failure.
	if outPath, _ := f.GetString("output-runconfig"); outPath != "" {
		return writeOutputRunConfig(outPath, cfg)
	}

	exportRequired, _ := f.GetBool("export-workspace-required")
	return runWithConfig(cfg, runOptions{
		exportWorkspaceRequired: exportRequired,
		outputMode:              outputMode,
	})
}

// writeOutputRunConfig emits the resolved RunConfig as pretty-printed
// JSON to the named path, or to stdout for path "-". Non-stdout paths
// open with 0600 since a captured RunConfig may carry secret://
// reference names that are operationally sensitive.
func writeOutputRunConfig(path string, cfg *types.RunConfig) error {
	if path == "-" {
		return writeRunConfigJSON(os.Stdout, cfg, false)
	}
	// O_TRUNC: a captured config replaces any previous one at the path.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return ioError(fmt.Errorf("opening --output-runconfig %q: %w", path, err))
	}
	return writeAndCloseRunConfig(f, path, cfg)
}

// writeAndCloseRunConfig is the testable seam writeOutputRunConfig
// delegates to. Surfaces a Close error (ENOSPC, EIO, NFS commit
// failure) when the prior write succeeded, since buffered file I/O can
// defer the actual flush until Close.
func writeAndCloseRunConfig(wc io.WriteCloser, path string, cfg *types.RunConfig) error {
	writeErr := writeRunConfigJSON(wc, cfg, false)
	if cerr := wc.Close(); cerr != nil && writeErr == nil {
		return ioError(fmt.Errorf("closing --output-runconfig %q: %w", path, cerr))
	}
	return writeErr
}

// dryRunProbeGates lists flags meaningful only alongside --dry-run.
var dryRunProbeGates = []string{
	"no-probe-provider",
	"no-probe-mcp",
	"no-probe-trace",
	"no-probe-egress",
	"no-probe-executor",
	"dry-run-timeout",
}

// validateDryRunFlags rejects a --dry-run probe gate or
// --dry-run-timeout supplied without --dry-run (exit 4), rather than
// silently ignoring a typo that would otherwise contact the provider
// on a real run.
func validateDryRunFlags(f *flag.FlagSet, dryRun bool) error {
	if dryRun {
		return nil
	}
	for _, name := range dryRunProbeGates {
		if f.Changed(name) {
			return usageError(fmt.Errorf("--%s has no effect without --dry-run", name))
		}
	}
	return nil
}

// dryRunOptionsFromFlags maps the --no-probe-* gates and --dry-run-timeout
// onto core.PreflightOptions.
func dryRunOptionsFromFlags(f *flag.FlagSet) core.PreflightOptions {
	skipProvider, _ := f.GetBool("no-probe-provider")
	skipMCP, _ := f.GetBool("no-probe-mcp")
	skipTrace, _ := f.GetBool("no-probe-trace")
	skipEgress, _ := f.GetBool("no-probe-egress")
	skipExecutor, _ := f.GetBool("no-probe-executor")
	timeout, _ := f.GetDuration("dry-run-timeout")
	return core.PreflightOptions{
		SkipProvider: skipProvider,
		SkipMCP:      skipMCP,
		SkipTrace:    skipTrace,
		SkipEgress:   skipEgress,
		SkipExecutor: skipExecutor,
		Timeout:      timeout,
	}
}

// runDryRun executes the preflight, renders the report, optionally
// writes the captured RunConfig, and maps the aggregate to an exit
// code (nil for all steps ok/skip, else a plain error). The config
// write happens only after a successfully-produced report, so a
// dry-run that surfaces a misconfiguration still captures the config.
//
// preflightFn is the seam through which runDryRun invokes the
// preflight; tests substitute a stub returning a canned report.
var preflightFn = core.Preflight

func runDryRun(cmd *cobra.Command, cfg *types.RunConfig, opts core.PreflightOptions, outputMode, outputRunConfigPath string) error {
	// Rooted at context.Background(), matching runWithConfig, rather
	// than cmd.Context() which is nil outside Cobra's Execute path.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	setupSignalHandler(cancel)

	report, err := preflightFn(ctx, cfg, opts)
	if err != nil {
		return err
	}
	return renderAndDispatchPreflight(cmd, report, cfg, outputMode, outputRunConfigPath)
}

// renderAndDispatchPreflight writes the report (JSON to stdout for
// --output=json, human-readable to stderr otherwise), composes with
// --output-runconfig when requested, and maps the aggregate to an exit
// code: a failed --output-runconfig write (exit 3) takes precedence
// over a probe failure (exit 1) so it is not masked.
func renderAndDispatchPreflight(cmd *cobra.Command, report *core.PreflightReport, cfg *types.RunConfig, outputMode, outputRunConfigPath string) error {
	if outputMode == "json" {
		if err := writePreflightJSON(cmd.OutOrStdout(), report); err != nil {
			return ioError(err)
		}
	} else if outputMode != "none" {
		writePreflightText(cmd.ErrOrStderr(), report)
	}

	if outputRunConfigPath != "" {
		if werr := writeOutputRunConfig(outputRunConfigPath, cfg); werr != nil {
			return werr
		}
	}

	if !report.OK {
		return fmt.Errorf("dry-run preflight failed: %d of %d step(s) did not pass", failedStepCount(report), len(report.Steps))
	}
	return nil
}

// failedStepCount counts the failed steps in a report.
func failedStepCount(report *core.PreflightReport) int {
	n := 0
	for _, s := range report.Steps {
		if s.Status == core.PreflightFail {
			n++
		}
	}
	return n
}

// writePreflightJSON emits the report as indented JSON. The report is
// already secret-free: it carries step names, statuses, and error text
// (provider/MCP diagnostics), never resolved credentials.
func writePreflightJSON(w io.Writer, report *core.PreflightReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("encode preflight report: %w", err)
	}
	return nil
}

// writePreflightText renders a human-readable per-step report to w,
// each line leading with the status; a remediation hint (when present)
// is indented under a failing step.
func writePreflightText(w io.Writer, report *core.PreflightReport) {
	var b strings.Builder
	b.WriteString("Dry-run preflight:\n")
	for _, s := range report.Steps {
		label := strings.ToUpper(string(s.Status))
		if s.Detail != "" {
			fmt.Fprintf(&b, "  [%-4s] %s: %s\n", label, s.Name, s.Detail)
		} else {
			fmt.Fprintf(&b, "  [%-4s] %s\n", label, s.Name)
		}
		if s.Status == core.PreflightFail && s.Hint != "" {
			fmt.Fprintf(&b, "         hint: %s\n", s.Hint)
		}
	}
	if report.OK {
		b.WriteString("Result: OK (all steps ok or skipped)\n")
	} else {
		fmt.Fprintf(&b, "Result: FAIL (%d of %d step(s) did not pass)\n", failedStepCount(report), len(report.Steps))
	}
	_, _ = io.WriteString(w, b.String())
}

// applyAnthropicWIFOverrides folds the Anthropic-WIF flag surface and
// documented env-var fallbacks into the RunConfig; see docs/anthropic-wif.md
// for the full precedence chain and risk rationale. Called from both
// paths so --config and flag-only users see the same resolution.
//
// Returns a non-nil error only on the apiKeyRef guard; everything else
// is best-effort folding that ValidateRunConfig rejects if invalid.
func applyAnthropicWIFOverrides(cmd *cobra.Command, cfg *types.RunConfig) error {
	f := cmd.Flags()
	changed := func(name string) bool { return f.Changed(name) }

	// Federation IDs from flags + env; all four --anthropic-* WIF flags
	// register an empty-string default, so a non-changed flag always
	// falls through to the env-var lookup.
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

	// Only fire when the operator has signalled WIF intent (any ID set,
	// the GHA opt-in, or an existing type=anthropic-wif config).
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
	// overrides an existing value from --config.
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

	// Token-source inference: a config-file source always wins; the
	// warning tells the operator their flag was discarded rather than
	// silently dropping it.
	if fromGHA && cfg.Provider.Credential.TokenSource != nil {
		slog.Warn("--anthropic-from-github-actions ignored: config file already specifies credential.tokenSource",
			"existing_type", cfg.Provider.Credential.TokenSource.Type)
	}
	if cfg.Provider.Credential.TokenSource == nil {
		switch {
		case fromGHA:
			// Audience defaults to the Anthropic OAuth host; a custom
			// audience claim requires --config.
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
			// handled here — operators must opt in explicitly via
			// --anthropic-from-github-actions.
		}
	}

	// Only enforce on the anthropic provider with anthropic-wif
	// credentials; reconciles the cobra default flag value against WIF
	// before validateAnthropicProviderFields runs.
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

// applyOpenAIWIFOverrides folds the OpenAI-WIF flag surface and the
// OPENAI_* env fallbacks into cfg, mirroring applyAnthropicWIFOverrides;
// see docs/openai-wif.md for the full precedence chain.
//
// Returns a non-nil error only on the conflicting-type and apiKeyRef
// guards; everything else is best-effort folding that ValidateRunConfig
// rejects if invalid.
func applyOpenAIWIFOverrides(cmd *cobra.Command, cfg *types.RunConfig) error {
	f := cmd.Flags()
	changed := func(name string) bool { return f.Changed(name) }

	resolveID := func(flagName, envName string) string {
		if changed(flagName) {
			v, _ := f.GetString(flagName)
			return v
		}
		return os.Getenv(envName)
	}

	idpID := resolveID("openai-identity-provider-id", "OPENAI_IDENTITY_PROVIDER_ID")
	saID := resolveID("openai-service-account-id", "OPENAI_SERVICE_ACCOUNT_ID")
	subjectTokenType := resolveID("openai-subject-token-type", "OPENAI_SUBJECT_TOKEN_TYPE")
	fromGHA, _ := f.GetBool("openai-from-github-actions")

	// Only the two required identifiers discriminate WIF intent;
	// subjectTokenType has its own env fallback, so including it here
	// would let a stray CI env var flip a plain run onto the WIF path.
	anyIDSet := idpID != "" || saID != ""

	if !anyIDSet && !fromGHA &&
		(cfg.Provider.Credential == nil || cfg.Provider.Credential.Type != "openai-wif") {
		return nil
	}

	if cfg.Provider.Credential == nil {
		cfg.Provider.Credential = &types.CredentialConfig{Type: "openai-wif"}
	} else if cfg.Provider.Credential.Type == "" || cfg.Provider.Credential.Type == "static" {
		cfg.Provider.Credential.Type = "openai-wif"
	} else if cfg.Provider.Credential.Type != "openai-wif" && anyIDSet {
		return fmt.Errorf(
			"--openai-* federation flags imply credential.type=openai-wif, "+
				"but credential.type is already %q; remove the conflicting type or "+
				"the federation flags",
			cfg.Provider.Credential.Type)
	}

	// Apply IDs after the type is settled; an explicit/env value
	// overrides an existing value from --config.
	if idpID != "" {
		cfg.Provider.Credential.OpenAIIdentityProviderID = idpID
	}
	if saID != "" {
		cfg.Provider.Credential.OpenAIServiceAccountID = saID
	}
	if subjectTokenType != "" {
		cfg.Provider.Credential.OpenAISubjectTokenType = subjectTokenType
	}

	// A config-file token source always wins; fill the slot only when nil.
	if fromGHA && cfg.Provider.Credential.TokenSource != nil {
		slog.Warn("--openai-from-github-actions ignored: config file already specifies credential.tokenSource",
			"existing_type", cfg.Provider.Credential.TokenSource.Type)
	}
	if cfg.Provider.Credential.TokenSource == nil {
		switch {
		case fromGHA:
			// Canonical audience is the OpenAI API root; a custom
			// audience claim requires --config.
			cfg.Provider.Credential.TokenSource = &types.TokenSourceConfig{
				Type:     "github-actions-oidc",
				Audience: "https://api.openai.com/v1",
			}
		case os.Getenv("OPENAI_IDENTITY_TOKEN_FILE") != "":
			cfg.Provider.Credential.TokenSource = &types.TokenSourceConfig{
				Type: "file",
				Path: os.Getenv("OPENAI_IDENTITY_TOKEN_FILE"),
			}
		case os.Getenv("OPENAI_IDENTITY_TOKEN") != "":
			cfg.Provider.Credential.TokenSource = &types.TokenSourceConfig{
				Type:   "env",
				EnvVar: "OPENAI_IDENTITY_TOKEN",
			}
		}
	}

	// Reconciles the cobra default flag value (shared "secret://ANTHROPIC_API_KEY"
	// across providers) against WIF before validateOpenAIWIFCrossField runs.
	if (cfg.Provider.Type == "openai-compatible" || cfg.Provider.Type == "openai-responses") &&
		cfg.Provider.Credential != nil &&
		cfg.Provider.Credential.Type == "openai-wif" &&
		cfg.Provider.APIKeyRef != "" {
		if changed("api-key-ref") {
			return fmt.Errorf(
				"--api-key-ref must not be set with --openai-identity-provider-id " +
					"(or any other --openai-* federation flag): WIF authenticates " +
					"via OAuth bearer tokens and a static API key would conflict with " +
					"the federated credential")
		}
		// Default flag value carries no operator intent under WIF — clear it
		// silently so validateOpenAIWIFCrossField does not reject the
		// otherwise-valid config.
		cfg.Provider.APIKeyRef = ""
	}

	return nil
}

// runOptions carries CLI-only behaviour that doesn't fit on RunConfig,
// keeping the wire schema free of CLI-shaped knobs. outputMode selects
// the post-run summary surface: "text" (stderr), "json" (a single
// STIRRUP_RESULT line on stdout), or "none".
type runOptions struct {
	exportWorkspaceRequired bool
	outputMode              string
}

// runWithConfig is the shared run path for both --config and flag-only
// invocations, once both have a validated RunConfig (ValidateRunConfig
// rejects nil Timeout, so the dereference below is safe).
func runWithConfig(config *types.RunConfig, opts runOptions) error {
	// shutdownCtx carries only the process-level shutdown signal,
	// independent of ctx's run-deadline cancellation below: the
	// detached postRun hook phase must survive ctx's own deadline but
	// still observe a genuine shutdown — see AgenticLoop.Shutdown and
	// docs/cloud-run-jobs.md.
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	defer shutdownCancel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*config.Timeout)*time.Second)
	defer cancel()
	setupSignalHandler(func() {
		shutdownCancel()
		cancel()
	})

	loop, err := core.BuildLoop(ctx, config)
	if err != nil {
		return fmt.Errorf("building harness: %w", err)
	}
	defer func() { _ = loop.Close() }()

	loop.Shutdown = shutdownCtx
	stopShutdownWatchdog := armShutdownWatchdog(shutdownCtx, loop, shutdownCloseGrace)
	defer stopShutdownWatchdog()

	runTrace, runErr := loop.Run(ctx, config)
	if runTrace == nil {
		// No trace was produced at all (e.g. the trace emitter itself
		// failed) — nothing to emit.
		return fmt.Errorf("running harness: %w", runErr)
	}

	// A fresh, short-deadline context so a signal-cancelled/timed-out
	// ctx does not eat the run's answer (mirrors bestEffortCancel in
	// batchpoll.go).
	emitCtx, emitCancel := context.WithTimeout(context.Background(), postRunEmitTimeout)
	defer emitCancel()
	emitRunOutput(emitCtx, config, runTrace, opts.outputMode)

	if runErr != nil {
		// finishWithOutcome's early-return paths (build-system-prompt
		// failure, git setup failure, fatal preRun hook failure) return
		// a valid trace alongside a non-nil error; the RunResult
		// emission above must still run for these, but a failed run has
		// nothing further to do.
		return fmt.Errorf("running harness: %w", runErr)
	}

	// Independent context, same reason as emitRunOutput, with a longer
	// deadline for a possibly multi-MB GCS PUT.
	exportCtx, exportCancel := context.WithTimeout(context.Background(), postRunExportTimeout)
	defer exportCancel()
	if err := exportWorkspace(exportCtx, config, opts.exportWorkspaceRequired); err != nil {
		return err
	}

	if config.FollowUpGrace != nil && *config.FollowUpGrace > 0 {
		core.RunFollowUpLoop(ctx, loop, config, *config.FollowUpGrace)
	}
	// A non-success outcome reached here (runErr == nil but e.g.
	// Outcome == "error" or "hook_failed") must still fail the process.
	return runOutcomeError(runTrace)
}
