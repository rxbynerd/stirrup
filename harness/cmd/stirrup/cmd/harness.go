package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
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
	defer f.Close()
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
		Executor:        types.ExecutorConfig{Type: executorType, Workspace: opts.Workspace, Runtime: opts.ContainerRuntime},
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
	f.String("provider", "anthropic", "Provider type: anthropic, bedrock, gemini (Vertex AI), openai-compatible (Chat Completions), openai-responses (Responses API). The two OpenAI variants speak different wire formats and must be selected explicitly.")
	f.String("api-key-ref", "secret://ANTHROPIC_API_KEY", "Secret reference for API key")
	f.String("base-url", "", "API base URL for openai-compatible / openai-responses providers (e.g. https://<resource>.openai.azure.com/openai/v1)")
	f.String("api-key-header", "", "Header name for sending the API key. Empty = Authorization: Bearer (default). Set to \"api-key\" for Azure OpenAI key auth.")
	f.StringArray("query-param", nil, "Repeatable key=value query parameter appended to every provider request URL (e.g. api-version=preview). Used by the openai-* adapters.")
	f.String("gcp-project", "", "GCP project ID hosting the Vertex AI usage. Required when --provider=gemini.")
	f.String("gcp-location", "global", "Vertex AI location: \"global\" or a region (e.g. us-central1). Determines the URL host and project location segment.")
	f.String("gcp-credentials-file", "", "Path to a Google service account JSON key file. When set, implies credential.type=gcp-service-account.")

	// Anthropic Workload Identity Federation flags (issue #117). Setting
	// any of the four ID flags implies credential.type=anthropic-wif when
	// the credential type is otherwise unset; the JWT source is selected
	// by --anthropic-from-github-actions or the ANTHROPIC_IDENTITY_TOKEN*
	// env vars (precedence documented in the operator walkthrough).
	f.String("anthropic-federation-rule-id", "", "Anthropic federation rule ID (`fdrl_...`). Implies `credential.type=anthropic-wif` when set. Env fallback: ANTHROPIC_FEDERATION_RULE_ID.")
	f.String("anthropic-organization-id", "", "Anthropic organization UUID. Required with WIF. Env fallback: ANTHROPIC_ORGANIZATION_ID.")
	f.String("anthropic-service-account-id", "", "Anthropic service account ID (`svac_...`). Required with WIF. Env fallback: ANTHROPIC_SERVICE_ACCOUNT_ID.")
	f.String("anthropic-workspace-id", "", "Anthropic workspace ID (`wrkspc_...`) or `default`. Conditional. Env fallback: ANTHROPIC_WORKSPACE_ID.")
	f.Bool("anthropic-from-github-actions", false, "Enable GitHub Actions OIDC token source for Anthropic WIF. Reads ACTIONS_ID_TOKEN_REQUEST_URL and ACTIONS_ID_TOKEN_REQUEST_TOKEN from the runner environment. Ignored (with a warning) if `--config` already sets `credential.tokenSource`.")

	// Azure Entra ID Workload Identity Federation flags (issue #118).
	f.String("azure-tenant-id", "", "Azure AD tenant UUID hosting the App Registration. When set, implies credential.type=azure-workload-identity. Use with --provider=openai-compatible or openai-responses against Azure OpenAI / Foundry.")
	f.String("azure-client-id", "", "App Registration / federated identity credential client ID (UUID). Required with --azure-tenant-id.")
	f.String("azure-scope", "https://cognitiveservices.azure.com/.default", "OAuth2 scope for the Entra access token. Override only for non-default Azure audiences (custom AAD app registrations, sovereign clouds).")
	f.StringP("workspace", "w", "", "Workspace directory (default: current directory)")
	f.Int("max-turns", 20, "Maximum agentic loop turns")
	f.Int("timeout", 600, "Wall-clock timeout in seconds")
	f.String("trace", "", "Path to JSONL trace file (sets --trace-emitter to jsonl unless --trace-emitter is explicitly set)")
	f.String("transport", "stdio", "Transport type: stdio, grpc")
	f.String("transport-addr", "", "gRPC target address (required when transport is grpc)")
	f.Int("followup-grace", 0, "Seconds to keep gRPC transport open for follow-up requests (0 = disabled; env: STIRRUP_FOLLOWUP_GRACE)")
	f.String("log-level", "info", "Log level: debug, info, warn, error")
	f.String("prompt", "", "User prompt (can also be passed as a positional argument; falls back to --prompt-file then STIRRUP_PROMPT env var, then a prompt field in --config)")
	f.String("prompt-file", "", "Path to a file whose contents become the prompt. Read from CWD when relative. Trailing newlines are trimmed; the file is capped at 10 MiB and must be non-empty. Lower precedence than --prompt and the positional argument; higher than STIRRUP_PROMPT.")
	f.String("name", "", "Human-readable session label (metadata only, not injected into prompt)")

	// Component-selection flags. Escape hatches for callers who don't want
	// a full --config file but need to switch a single component. These
	// are still honoured (as overrides) when --config is set.
	f.String("executor", "local", "Executor: local, container, api")
	f.String("edit-strategy", "multi", "Edit strategy: whole-file, search-replace, udiff, multi (composite available only via --config)")
	f.String("verifier", "none", "Verifier: none, test-runner, llm-judge (composite available only via --config)")
	f.String("git-strategy", "none", "Git strategy: none, deterministic")
	f.String("trace-emitter", "jsonl", "Trace emitter: jsonl, otel")
	f.String("otel-endpoint", "", "OTLP endpoint for the otel trace emitter (default: localhost:4317 for grpc; full URL ending in the gateway base path for http/protobuf, e.g. https://otlp-gateway-prod-us-east-0.grafana.net/otlp)")
	f.String("otel-protocol", "", "OTLP wire protocol for the otel trace emitter: \"\" (default — grpc), grpc, http/protobuf. HTTP/JSON is not supported; managed gateways like Grafana Cloud use http/protobuf. See docs/observability-cloud.md.")

	// Safety-ring flags (issue #42). Each maps to a single RunConfig
	// field; precedence with --config is the same as the rest of the
	// override surface (file -> flag -> default; unset flags don't
	// override the file).
	f.String("container-runtime", "", "OCI runtime for the container executor: runc, runsc (gVisor), kata, kata-qemu, kata-fc. Empty means engine default (typically runc). Requires the runtime to be registered with the host Docker/Podman daemon — see docs/safety-rings.md.")
	f.String("permission-policy-file", "", "Path to a Cedar policy file for the policy-engine PermissionPolicy. When set and --permission-policy is unset elsewhere, also implies permissionPolicy.type=policy-engine. See examples/policies/ for starters.")
	f.String("code-scanner", "", "CodeScanner type: none, patterns, semgrep, composite. Composite requires --config (codeScanner.scanners). Empty defers to the mode-aware default (patterns for execution, none for read-only modes).")

	// GuardRail flags (issue #43). The composite layering used by the
	// operator escape hatch requires per-stage phase restrictions, which
	// flag syntax cannot express; composite stacks therefore round-trip
	// only through --config (see docs/guardrails.md).
	f.String("guardrail", "", "GuardRail type: none, granite-guardian, composite, cloud-judge. Composite requires --config (guardRail.stages).")
	f.String("guardrail-endpoint", "", "Endpoint URL for the granite-guardian or cloud-judge adapter.")
	f.String("guardrail-model", "", "Model identifier for the GuardRail classifier. Granite-guardian default: ibm-granite/granite-guardian-4.1-8b. Cloud-judge default: claude-haiku-4-5-20251001 (Anthropic API format) — when the primary provider is Bedrock, use the Bedrock-format ID (e.g. us.anthropic.claude-haiku-4-5-20251001-v1:0).")
	f.Bool("guardrail-fail-open", false, "When true, transport errors / timeouts produce VerdictAllow with a security event rather than blocking. Default false (fail closed).")

	// Observability resource attributes (issue #95). No default at the
	// flag level — empty values fall through to env-var fallbacks
	// (OTEL_DEPLOYMENT_ENVIRONMENT, OTEL_SERVICE_NAMESPACE) and finally
	// to defaults ("local" / "stirrup") at resource construction time.
	f.String("deployment-environment", "", "OTel deployment.environment resource attribute (e.g. production, staging). Empty falls through to OTEL_DEPLOYMENT_ENVIRONMENT, then to \"local\".")
	f.String("service-namespace", "", "OTel service.namespace resource attribute (e.g. stirrup-eval, team-a). Empty falls through to OTEL_SERVICE_NAMESPACE, then to \"stirrup\".")
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

	// Anthropic Workload Identity Federation overrides (issue #117).
	// Encapsulated for readability; the helper handles the four ID
	// fields, the inferred credential.type, the token-source inference
	// chain, and the apiKeyRef mutual-exclusion guard.
	if err := applyAnthropicWIFOverrides(cmd, cfg); err != nil {
		return err
	}

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
	return nil
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
		if err := applyOverrides(cmd, cfg, args); err != nil {
			return err
		}

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
		// --prompt-file and STIRRUP_PROMPT fall in below --prompt /
		// positional / file-prompt (applyOverrides has already resolved
		// those three by this point). Inlined rather than extracted into
		// a resolvePrompt helper per the house-style preference for
		// minimal-diff changes — the two call sites read the same five
		// sources in the same order, and a tiny duplication keeps the
		// resolution chain visible at both decision points.
		if cfg.Prompt == "" {
			if promptFile, _ := f.GetString("prompt-file"); promptFile != "" {
				p, err := readPromptFile(promptFile)
				if err != nil {
					return err
				}
				cfg.Prompt = p
			}
		}
		if cfg.Prompt == "" {
			if v := os.Getenv("STIRRUP_PROMPT"); v != "" {
				cfg.Prompt = v
			}
		}
		if cfg.Prompt == "" {
			return fmt.Errorf("prompt is required: pass via --prompt flag, as a positional argument, --prompt-file, STIRRUP_PROMPT env var, or the prompt field in --config")
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
	// --prompt-file and STIRRUP_PROMPT fall in below --prompt and the
	// positional argument. Inlined rather than extracted into a
	// resolvePrompt helper per the house-style preference for
	// minimal-diff changes; the chain is short and reads top-to-bottom
	// in precedence order, which is easier to audit than a separate
	// function with its own ordering.
	if prompt == "" {
		if promptFile, _ := f.GetString("prompt-file"); promptFile != "" {
			p, err := readPromptFile(promptFile)
			if err != nil {
				return err
			}
			prompt = p
		}
	}
	if prompt == "" {
		if v := os.Getenv("STIRRUP_PROMPT"); v != "" {
			prompt = v
		}
	}
	if prompt == "" {
		return fmt.Errorf("prompt is required: pass via --prompt flag, as a positional argument, --prompt-file, STIRRUP_PROMPT env var, or the prompt field in --config")
	}

	mode, _ := f.GetString("mode")
	sessionName, _ := f.GetString("name")
	model, _ := f.GetString("model")
	providerType, _ := f.GetString("provider")
	apiKeyRef, _ := f.GetString("api-key-ref")
	baseURL, _ := f.GetString("base-url")
	apiKeyHeader, _ := f.GetString("api-key-header")
	queryParamRaw, _ := f.GetStringArray("query-param")
	gcpProject, _ := f.GetString("gcp-project")
	gcpLocation, _ := f.GetString("gcp-location")
	gcpCredentialsFile, _ := f.GetString("gcp-credentials-file")
	anthropicFederationRuleID, _ := f.GetString("anthropic-federation-rule-id")
	anthropicOrganizationID, _ := f.GetString("anthropic-organization-id")
	anthropicServiceAccountID, _ := f.GetString("anthropic-service-account-id")
	anthropicWorkspaceID, _ := f.GetString("anthropic-workspace-id")
	anthropicFromGitHubActions, _ := f.GetBool("anthropic-from-github-actions")
	azureTenantID, _ := f.GetString("azure-tenant-id")
	azureClientID, _ := f.GetString("azure-client-id")
	azureScope, _ := f.GetString("azure-scope")
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
	otelProtocol, _ := f.GetString("otel-protocol")
	containerRuntime, _ := f.GetString("container-runtime")
	permissionPolicyFile, _ := f.GetString("permission-policy-file")
	codeScannerType, _ := f.GetString("code-scanner")
	guardRailType, _ := f.GetString("guardrail")
	guardRailEndpoint, _ := f.GetString("guardrail-endpoint")
	guardRailModel, _ := f.GetString("guardrail-model")
	guardRailFailOpen, _ := f.GetBool("guardrail-fail-open")
	deploymentEnvironment, _ := f.GetString("deployment-environment")
	serviceNamespace, _ := f.GetString("service-namespace")

	var queryParams map[string]string
	for _, entry := range queryParamRaw {
		k, v, err := parseQueryParam(entry)
		if err != nil {
			return fmt.Errorf("--query-param %q: %w", entry, err)
		}
		if queryParams == nil {
			queryParams = map[string]string{}
		}
		queryParams[k] = v
	}

	// Allow the env var to set the follow-up grace when the flag is not provided.
	if followUpGrace == 0 {
		if v := os.Getenv("STIRRUP_FOLLOWUP_GRACE"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				followUpGrace = n
			}
		}
	}

	config := buildHarnessRunConfig(harnessCLIOptions{
		RunID:                      generateRunID(),
		Mode:                       mode,
		SessionName:                sessionName,
		Prompt:                     prompt,
		ProviderType:               providerType,
		BaseURL:                    baseURL,
		APIKeyHeader:               apiKeyHeader,
		QueryParams:                queryParams,
		APIKeyRef:                  apiKeyRef,
		GCPProject:                 gcpProject,
		GCPLocation:                gcpLocation,
		GCPCredentialsFile:         gcpCredentialsFile,
		AnthropicFederationRuleID:  anthropicFederationRuleID,
		AnthropicOrganizationID:    anthropicOrganizationID,
		AnthropicServiceAccountID:  anthropicServiceAccountID,
		AnthropicWorkspaceID:       anthropicWorkspaceID,
		AnthropicFromGitHubActions: anthropicFromGitHubActions,
		AzureTenantID:              azureTenantID,
		AzureClientID:              azureClientID,
		AzureScope:                 azureScope,
		Model:                      model,
		Workspace:                  workspace,
		MaxTurns:                   maxTurns,
		Timeout:                    timeout,
		TracePath:                  tracePath,
		TransportType:              transportType,
		TransportAddr:              transportAddr,
		FollowUpGrace:              followUpGrace,
		LogLevel:                   logLevel,
		ExecutorType:               executorType,
		EditStrategyType:           editStrategyType,
		VerifierType:               verifierType,
		GitStrategyType:            gitStrategyType,
		TraceEmitterType:           traceEmitterType,
		OTelEndpoint:               otelEndpoint,
		OTelProtocol:               otelProtocol,
		ContainerRuntime:           containerRuntime,
		PermissionPolicyFile:       permissionPolicyFile,
		CodeScannerType:            codeScannerType,
		GuardRailType:              guardRailType,
		GuardRailEndpoint:          guardRailEndpoint,
		GuardRailModel:             guardRailModel,
		GuardRailFailOpen:          guardRailFailOpen,
		DeploymentEnvironment:      deploymentEnvironment,
		ServiceNamespace:           serviceNamespace,
	})

	// Anthropic WIF env-var fallbacks and token-source inference run
	// after buildHarnessRunConfig so the flag-only path mirrors the
	// --config path's resolution chain. Errors here surface with the
	// offending flag name rather than as an opaque ValidateRunConfig
	// rejection.
	if err := applyAnthropicWIFOverrides(cmd, config); err != nil {
		return err
	}

	if err := types.ValidateRunConfig(config); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	return runWithConfig(config)
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
