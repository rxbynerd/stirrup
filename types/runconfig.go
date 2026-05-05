package types

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// absoluteMaxTurns is the hard upper bound on MaxTurns enforced during
	// RunConfig validation, independent of what the caller requests.
	absoluteMaxTurns = 100

	// maxFollowUpGrace is the maximum allowed follow-up grace period in seconds.
	maxFollowUpGrace = 3600

	// maxCostBudget is the maximum allowed cost budget in dollars.
	maxCostBudget = 100.0

	// maxTokenBudget is the maximum allowed token budget.
	maxTokenBudget = 50_000_000

	// maxSessionNameLength is the maximum allowed length, in bytes, of
	// SessionName. Capped to keep log lines, OTel attribute values, and
	// trace JSON predictable; well above any genuine human-readable label.
	maxSessionNameLength = 255
)

// RunConfig fully describes a single harness run. It is the composition root:
// the control plane sends it (via TaskAssignment in the gRPC contract) and
// the CLI builds it from flags/env.
type RunConfig struct {
	// Identity
	RunID       string `json:"runId"`
	Mode        string `json:"mode"`                  // "execution" | "planning" | "review" | "research" | "toil"
	SessionName string `json:"sessionName,omitempty"` // human-readable label; never injected into the model's context

	// What to do
	Prompt         string            `json:"prompt"`
	DynamicContext map[string]string `json:"dynamicContext,omitempty"`

	// Component selections
	Provider         ProviderConfig            `json:"provider"`
	Providers        map[string]ProviderConfig `json:"providers,omitempty"`
	ModelRouter      ModelRouterConfig         `json:"modelRouter"`
	PromptBuilder    PromptBuilderConfig       `json:"promptBuilder"`
	ContextStrategy  ContextStrategyConfig     `json:"contextStrategy"`
	Executor         ExecutorConfig            `json:"executor"`
	EditStrategy     EditStrategyConfig        `json:"editStrategy"`
	Verifier         VerifierConfig            `json:"verifier"`
	PermissionPolicy PermissionPolicyConfig    `json:"permissionPolicy"`
	GitStrategy      GitStrategyConfig         `json:"gitStrategy"`
	Transport        TransportConfig           `json:"transport"`
	TraceEmitter     TraceEmitterConfig        `json:"traceEmitter"`
	Tools            ToolsConfig               `json:"tools"`

	// Limits
	MaxTurns       int      `json:"maxTurns"`
	MaxTokenBudget *int     `json:"maxTokenBudget,omitempty"`
	MaxCostBudget  *float64 `json:"maxCostBudget,omitempty"`
	Timeout        *int     `json:"timeout,omitempty"`

	// FollowUpGrace is the number of seconds to keep the transport open after
	// the primary run completes, waiting for follow-up user_response events.
	// A value of zero or nil disables the grace period (default behaviour).
	FollowUpGrace *int `json:"followUpGrace,omitempty"`

	// LogLevel controls the structured logger verbosity.
	// Valid values: "debug", "info", "warn", "error". Default: "info".
	LogLevel string `json:"logLevel,omitempty"`

	// SystemPromptOverride, when set, is used as the complete system prompt
	// preamble, bypassing prompt_builder mode selection. Workspace path,
	// turn budget, and dynamic_context sections are still appended.
	SystemPromptOverride string `json:"systemPromptOverride,omitempty"`

	// RuleOfTwo carries the operator override for the "Agents Rule of
	// Two" structural invariant enforced in ValidateRunConfig. When nil
	// (the default) the invariant is enforced; setting Enforce: false
	// is the only supported way to bypass it. The override exists so a
	// human reviewer can sign off on a config that legitimately needs
	// all three capabilities at once — it should not be set lightly.
	RuleOfTwo *RuleOfTwoConfig `json:"ruleOfTwo,omitempty"`

	// CodeScanner configures the post-edit static analysis pass that
	// scans every successful EditStrategy.Apply for hardcoded secrets,
	// eval/exec sinks, and other known-bad patterns. When nil,
	// ValidateRunConfig fills in a sensible default per mode
	// (patterns for execution, none for read-only modes).
	CodeScanner *CodeScannerConfig `json:"codeScanner,omitempty"`

	// GuardRail configures the LLM-based safety classifier that runs
	// at three intervention points in the agentic loop. When nil, the
	// factory installs a "none" guard so call sites never nil-check.
	// See GuardRailConfig for the available implementations.
	GuardRail *GuardRailConfig `json:"guardRail,omitempty"`
}

// RuleOfTwoConfig configures the Rule-of-Two structural invariant. The
// invariant: a single run must not simultaneously hold (a) untrusted
// input, (b) sensitive data, and (c) the ability to communicate
// externally — unless gated by ask-upstream.
//
// Enforce is a pointer so we can distinguish "unset" (default: enforce)
// from "explicit false" (override the rejection). An explicit true is
// equivalent to leaving the field unset.
type RuleOfTwoConfig struct {
	Enforce *bool `json:"enforce,omitempty"`
}

// CodeScannerConfig selects the static-analysis pass run after every
// successful file edit. The scanner inspects the edited content for
// hardcoded secrets, eval/exec sinks, and known-malicious patterns;
// findings labelled "block" turn into edit failures, "warn" findings
// just emit a security event.
//
//   - "none"      — disable scanning (default for read-only modes).
//   - "patterns"  — pure-Go regex pack (always available; default for
//     execution mode).
//   - "semgrep"   — shell out to a local semgrep binary if present.
//   - "composite" — union of multiple named scanners.
type CodeScannerConfig struct {
	Type        string   `json:"type"`
	Scanners    []string `json:"scanners,omitempty"`
	BlockOnWarn bool     `json:"blockOnWarn,omitempty"`

	// SemgrepConfigPath, when non-empty, is passed to semgrep as
	// `--config <path>` instead of the default `--config auto`. Set
	// this to a local rules bundle (e.g. /etc/stirrup/semgrep-rules/)
	// to disable the implicit network fetch of rule packs from
	// semgrep.dev — required for air-gapped deployments and the only
	// way to pin the scanner against supply-chain shifts (M7). Empty
	// preserves the historical default of "auto".
	SemgrepConfigPath string `json:"semgrepConfigPath,omitempty"`
}

// GuardRailConfig configures the LLM-based safety classifier that runs
// at three intervention points in the agentic loop: pre-turn (untrusted
// content before it enters context), pre-tool (model-proposed tool call
// before dispatch), and post-turn (assistant text before it is forwarded
// to the user). Defaults to "none" — guardrails are opt-in per run.
//
//   - "none"             — disable guardrails (default).
//   - "granite-guardian" — Granite Guardian 4.1-8B served via vLLM
//     using the OpenAI-compatible chat-completions API. Requires
//     Endpoint.
//   - "composite"        — sequential / parallel layering of stages.
//     Each entry in Stages must be a non-composite type;
//     composite-of-composite is rejected.
//   - "cloud-judge"      — reuse an existing ProviderAdapter so
//     environments that cannot run their own vLLM still have a guard
//     option.
type GuardRailConfig struct {
	Type   string            `json:"type"`             // "none" | "granite-guardian" | "composite" | "cloud-judge"
	Stages []GuardRailConfig `json:"stages,omitempty"` // composite only
	Phases []string          `json:"phases,omitempty"` // restrict to phases; default = all three

	// Composite layering mode is reserved for future use. v1 always
	// wires composites as Sequential (first-deny-wins, last decision
	// otherwise). The guard package exports a Parallel implementation
	// but no config field selects it; embedders who need parallel
	// composition must build the GuardRail tree manually.

	// Adapter config (granite-guardian, cloud-judge):

	// Endpoint is the URL of the classifier service. Must parse with
	// net/url and use scheme http or https. A path component is allowed
	// (vLLM typically serves at /v1/chat/completions); query strings are
	// accepted because some gateways encode their own pin parameters.
	// Required for "granite-guardian"; rejected for "none" / "composite"
	// because those types have no transport of their own and a stale
	// value would be silently ignored.
	Endpoint string `json:"endpoint,omitempty"`

	// Model identifies the classifier model (e.g.
	// "ibm-granite/granite-guardian-4.1-8b"). Adapter-defined default
	// applies when empty.
	Model string `json:"model,omitempty"`

	// Threshold is reserved; the Granite Guardian 4.1-8B head returns
	// binary yes/no — this field has no effect in v1. Do not rely on
	// it for admission control. Setting a non-zero value triggers a
	// startup warning so operators are not silently misled.
	Threshold float64 `json:"threshold,omitempty"`

	// Criteria are built-in criterion identifiers (e.g. "harm",
	// "jailbreak"). Adapter-defined per-phase default applies when
	// empty.
	Criteria []string `json:"criteria,omitempty"`

	// CustomCriteria are natural-language criteria keyed by ID. IDs
	// must conform to [a-z][a-z0-9_]* — the rule is wire-stable, kept
	// loggable, and doesn't collide with proto field-name shapes.
	CustomCriteria map[string]string `json:"customCriteria,omitempty"`

	// Think requests reasoning traces (<think>...</think>) from the
	// classifier when true. Pointer so unset is wire-distinguishable
	// from explicit false — same rationale as RuleOfTwoConfig.Enforce.
	Think *bool `json:"think,omitempty"`

	// TimeoutMs is the per-call timeout in milliseconds. Range
	// [50, 30000]. Zero means "use the adapter default".
	TimeoutMs int `json:"timeoutMs,omitempty"`

	// FailOpen, when true, converts transport errors and timeouts into
	// VerdictAllow plus a security event rather than blocking. Default
	// false (fail closed).
	FailOpen bool `json:"failOpen,omitempty"`

	// MinChunkChars is the pre-turn skip threshold: chunks shorter than
	// this are not sent to the classifier. Range [0, 4096]. Zero
	// disables skipping.
	MinChunkChars int `json:"minChunkChars,omitempty"`
}

// Redact returns a copy of the RunConfig with secret references replaced
// by placeholder values, safe for persistence in traces and recordings.
// Note: CredentialConfig fields (roleArn, audience, sessionName) are not
// secrets and are preserved for diagnostics.
func (rc RunConfig) Redact() RunConfig {
	redacted := rc
	if redacted.Provider.APIKeyRef != "" {
		redacted.Provider.APIKeyRef = "secret://[REDACTED]"
	}
	if len(redacted.Providers) > 0 {
		providers := make(map[string]ProviderConfig, len(redacted.Providers))
		for name, provider := range redacted.Providers {
			if provider.APIKeyRef != "" {
				provider.APIKeyRef = "secret://[REDACTED]"
			}
			providers[name] = provider
		}
		redacted.Providers = providers
	}
	if redacted.Executor.VcsBackend != nil && redacted.Executor.VcsBackend.APIKeyRef != "" {
		vcs := *redacted.Executor.VcsBackend
		vcs.APIKeyRef = "secret://[REDACTED]"
		redacted.Executor.VcsBackend = &vcs
	}
	if len(redacted.Tools.MCPServers) > 0 {
		servers := make([]MCPServerConfig, len(redacted.Tools.MCPServers))
		copy(servers, redacted.Tools.MCPServers)
		for i := range servers {
			if servers[i].APIKeyRef != "" {
				servers[i].APIKeyRef = "secret://[REDACTED]"
			}
		}
		redacted.Tools.MCPServers = servers
	}
	return redacted
}

// ProviderConfig selects the model provider implementation.
type ProviderConfig struct {
	Type       string            `json:"type"`                 // "anthropic" | "bedrock" | "openai-compatible" | "openai-responses"
	APIKeyRef  string            `json:"apiKeyRef,omitempty"`  // e.g. "secret://anthropic-key"
	Region     string            `json:"region,omitempty"`     // bedrock
	Profile    string            `json:"profile,omitempty"`    // bedrock
	BaseURL    string            `json:"baseUrl,omitempty"`    // openai-compatible, openai-responses
	Credential *CredentialConfig `json:"credential,omitempty"` // cross-cloud credential federation (nil = infer from provider type)

	// APIKeyHeader overrides the HTTP header used to send the resolved API
	// key. Empty string preserves today's "Authorization: Bearer <key>"
	// behaviour. Set to "api-key" for Azure OpenAI key auth, or to a
	// vendor-specific header name (e.g. "x-api-key") for other gateways.
	// Only consulted by the openai-compatible and openai-responses adapters;
	// ignored by anthropic and bedrock (which derive auth from CredentialConfig).
	APIKeyHeader string `json:"apiKeyHeader,omitempty"`

	// QueryParams are appended to every request URL by the openai-compatible
	// and openai-responses adapters. Used for Azure OpenAI's api-version pin
	// (e.g. {"api-version": "preview"}) and similar gateway parameters. Keys
	// supplied here override any duplicate keys present in BaseURL's query
	// string. Ignored by other provider types.
	QueryParams map[string]string `json:"queryParams,omitempty"`
}

// CredentialConfig selects the credential acquisition method for a provider.
// When omitted from ProviderConfig, the credential type is inferred:
// bedrock uses "aws-default", all others use "static" (resolving APIKeyRef).
type CredentialConfig struct {
	Type        string             `json:"type"`                  // "static" | "aws-default" | "web-identity"
	TokenSource *TokenSourceConfig `json:"tokenSource,omitempty"` // required for "web-identity"
	RoleARN     string             `json:"roleArn,omitempty"`     // required for "web-identity": IAM role to assume
	SessionName string             `json:"sessionName,omitempty"` // for "web-identity" (default: "stirrup")
}

// TokenSourceConfig selects where identity tokens are fetched from.
// Used by credential types that require an OIDC/JWT token for exchange.
type TokenSourceConfig struct {
	Type     string `json:"type"`               // "gke-metadata" | "file" | "env"
	Audience string `json:"audience,omitempty"` // for "gke-metadata": target audience claim (e.g. "sts.amazonaws.com")
	Path     string `json:"path,omitempty"`     // for "file": filesystem path to token
	EnvVar   string `json:"envVar,omitempty"`   // for "env": environment variable name
}

// ModelRouterConfig selects the model router implementation.
type ModelRouterConfig struct {
	Type       string            `json:"type"`                 // "static" | "per-mode" | "dynamic"
	Provider   string            `json:"provider,omitempty"`   // default provider (static + per-mode + dynamic)
	Model      string            `json:"model,omitempty"`      // default model (static + per-mode + dynamic)
	ModeModels map[string]string `json:"modeModels,omitempty"` // per-mode: mode -> "provider/model" override

	// Dynamic router fields: complexity-based model selection.
	CheapProvider           string   `json:"cheapProvider,omitempty"`
	CheapModel              string   `json:"cheapModel,omitempty"`
	ExpensiveProvider       string   `json:"expensiveProvider,omitempty"`
	ExpensiveModel          string   `json:"expensiveModel,omitempty"`
	ExpensiveTurnThreshold  int      `json:"expensiveTurnThreshold,omitempty"`
	ExpensiveTokenThreshold int      `json:"expensiveTokenThreshold,omitempty"`
	CheapStopReasons        []string `json:"cheapStopReasons,omitempty"`
}

// PromptBuilderConfig selects the prompt builder implementation.
type PromptBuilderConfig struct {
	Type     string `json:"type"`               // "default" | "custom"
	Template string `json:"template,omitempty"` // for custom
}

// ContextStrategyConfig selects the context strategy implementation.
type ContextStrategyConfig struct {
	Type      string `json:"type"`                // "sliding-window" | "summarise"
	MaxTokens int    `json:"maxTokens,omitempty"` // token budget
}

// ExecutorConfig selects the executor implementation.
type ExecutorConfig struct {
	Type       string            `json:"type"`                 // "api" | "local" | "container" | "microvm"
	VcsBackend *VcsBackendConfig `json:"vcsBackend,omitempty"` // type: "api"
	Workspace  string            `json:"workspace,omitempty"`
	Image      string            `json:"image,omitempty"`
	Network    *NetworkConfig    `json:"network,omitempty"`
	Resources  *ResourceLimits   `json:"resources,omitempty"`
	Proxy      string            `json:"proxy,omitempty"`

	// Runtime selects the OCI runtime for the container executor. Empty
	// string means "use the engine default" — i.e. the harness does not
	// pass a Runtime field on the create-container request. The closed set
	// of accepted values is enforced by ValidateRunConfig.
	//   ""           — engine default (typically runc)
	//   "runc"       — vanilla runc
	//   "runsc"      — gVisor (user-space kernel)
	//   "kata"       — Kata Containers (default flavour)
	//   "kata-qemu"  — Kata Containers backed by QEMU
	//   "kata-fc"    — Kata Containers backed by Firecracker
	Runtime string `json:"runtime,omitempty"`
}

// VcsBackendConfig selects the VCS backend for the API executor.
type VcsBackendConfig struct {
	Type      string `json:"type"` // "github" | "gitlab"
	APIKeyRef string `json:"apiKeyRef,omitempty"`
	Repo      string `json:"repo,omitempty"`
	Ref       string `json:"ref,omitempty"`
}

// NetworkConfig controls network egress for sandboxed executors.
type NetworkConfig struct {
	Mode      string   `json:"mode"` // "none" or "allowlist"
	Allowlist []string `json:"allowlist,omitempty"`
}

// ResourceLimits constrains resource usage for sandboxed executors.
type ResourceLimits struct {
	CPUs     float64 `json:"cpus"`
	MemoryMB int     `json:"memoryMb"`
	DiskMB   int     `json:"diskMb"`
	PIDs     int     `json:"pids"`
}

// EditStrategyConfig selects the edit strategy implementation.
type EditStrategyConfig struct {
	Type           string   `json:"type"`                     // "whole-file" | "search-replace" | "udiff" | "multi"
	FuzzyThreshold *float64 `json:"fuzzyThreshold,omitempty"` // udiff/multi: minimum similarity ratio for fuzzy matching (default 0.80)
}

// VerifierConfig selects the verifier implementation.
type VerifierConfig struct {
	Type      string           `json:"type"`                // "none" | "test-runner" | "llm-judge" | "composite"
	Command   string           `json:"command,omitempty"`   // for test-runner: the shell command to execute
	Timeout   int              `json:"timeout,omitempty"`   // for test-runner: timeout in seconds (default 300)
	Verifiers []VerifierConfig `json:"verifiers,omitempty"` // for composite: sub-verifiers to chain
	Criteria  string           `json:"criteria,omitempty"`  // for llm-judge: natural language evaluation criteria
	Model     string           `json:"model,omitempty"`     // for llm-judge: model to use for judging
}

// PermissionPolicyConfig selects the permission policy implementation.
type PermissionPolicyConfig struct {
	Type    string `json:"type"`              // "allow-all" | "deny-side-effects" | "ask-upstream" | "policy-engine"
	Timeout int    `json:"timeout,omitempty"` // ask-upstream: seconds to wait for a response (0 = 60s default)

	// PolicyFile is the filesystem path to a Cedar policy file
	// (`.cedar`). Required when Type == "policy-engine"; ignored
	// otherwise.
	PolicyFile string `json:"policyFile,omitempty"`

	// Fallback names the permission policy to consult when the Cedar
	// engine returns "no decision" for a request. Must be one of the
	// non-policy-engine types ("allow-all", "deny-side-effects",
	// "ask-upstream"). When unset, callers should treat the default as
	// "deny-side-effects" — fail closed.
	Fallback string `json:"fallback,omitempty"`
}

// GitStrategyConfig selects the git strategy implementation.
type GitStrategyConfig struct {
	Type string `json:"type"` // "none" | "deterministic"
}

// TransportConfig selects the transport implementation.
type TransportConfig struct {
	Type    string `json:"type"`              // "stdio" | "grpc"
	Address string `json:"address,omitempty"` // gRPC target address (required when type is "grpc")
}

// TraceEmitterConfig selects the trace emitter implementation.
type TraceEmitterConfig struct {
	Type            string `json:"type"`                      // "jsonl" | "otel"
	FilePath        string `json:"filePath,omitempty"`        // for jsonl
	Endpoint        string `json:"endpoint,omitempty"`        // for otel tracing (default: localhost:4317)
	MetricsEndpoint string `json:"metricsEndpoint,omitempty"` // for otel metrics (defaults to Endpoint if unset)
}

// ToolsConfig holds the tool configuration.
type ToolsConfig struct {
	BuiltIn    []string          `json:"builtIn,omitempty"`    // which built-in tools to enable
	MCPServers []MCPServerConfig `json:"mcpServers,omitempty"` // MCP server connections
}

// MCPServerConfig describes a single MCP server connection.
type MCPServerConfig struct {
	Name      string `json:"name"`
	URI       string `json:"uri"`
	APIKeyRef string `json:"apiKeyRef,omitempty"`
}

var validProviderTypes = map[string]bool{
	"anthropic":         true,
	"bedrock":           true,
	"openai-compatible": true,
	"openai-responses":  true,
}

// apiKeyHeaderPattern restricts APIKeyHeader to a conservative subset of
// HTTP token characters so a user cannot inject CRLF / colon / whitespace
// into the request. The pattern intentionally excludes "_" and "."; if a
// future gateway header requires them, expand here with an explicit
// rationale rather than relaxing to a broader RFC 7230 token.
var apiKeyHeaderPattern = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// queryParamKeyPattern restricts QueryParams keys to a conservative subset.
// Allows "_" and "." which are common in vendor-defined parameter names
// (e.g. "deployment.id").
var queryParamKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// maxQueryStringBytes caps the encoded-form size of QueryParams to bound
// the URL we eventually emit. 2 KiB is comfortably above what any real
// gateway-pin scenario needs while still rejecting a footgun like
// "QueryParams: <some_program_dumped_a_megabyte_in_here>".
const maxQueryStringBytes = 2048

var validModelRouterTypes = map[string]bool{
	"static":   true,
	"per-mode": true,
	"dynamic":  true,
}

var validPromptBuilderTypes = map[string]bool{
	"default":  true,
	"composed": true,
}

var validContextStrategyTypes = map[string]bool{
	"sliding-window":  true,
	"summarise":       true,
	"offload-to-file": true,
}

var validExecutorTypes = map[string]bool{
	"api":       true,
	"local":     true,
	"container": true,
}

// validExecutorRuntimes is the closed set of OCI runtimes the container
// executor may select. The empty string is accepted and means "use the
// engine default" — the harness omits the Runtime field on the create
// request. Adding a new runtime here is the only supported way to extend
// the set; ValidateRunConfig rejects everything else.
var validExecutorRuntimes = map[string]bool{
	"":          true,
	"runc":      true,
	"runsc":     true,
	"kata":      true,
	"kata-qemu": true,
	"kata-fc":   true,
}

var validEditStrategyTypes = map[string]bool{
	"whole-file":     true,
	"search-replace": true,
	"udiff":          true,
	"multi":          true,
}

var validVerifierTypes = map[string]bool{
	"none":        true,
	"test-runner": true,
	"llm-judge":   true,
	"composite":   true,
}

var validPermissionPolicyTypes = map[string]bool{
	"allow-all":         true,
	"deny-side-effects": true,
	"ask-upstream":      true,
	"policy-engine":     true,
}

// validFallbackPolicyTypes is the set of permission policies that may be
// referenced from PermissionPolicyConfig.Fallback. The policy-engine
// itself is excluded — chained policy engines are explicitly out of
// scope and would loop on a no-decision response.
var validFallbackPolicyTypes = map[string]bool{
	"allow-all":         true,
	"deny-side-effects": true,
	"ask-upstream":      true,
}

var validGitStrategyTypes = map[string]bool{
	"none":          true,
	"deterministic": true,
}

var validTransportTypes = map[string]bool{
	"stdio": true,
	"grpc":  true,
}

var validTraceEmitterTypes = map[string]bool{
	"jsonl": true,
	"otel":  true,
}

var validCredentialTypes = map[string]bool{
	"static":       true,
	"aws-default":  true,
	"web-identity": true,
}

var validTokenSourceTypes = map[string]bool{
	"gke-metadata": true,
	"file":         true,
	"env":          true,
}

// validCodeScannerTypes is the closed set of CodeScanner.Type values.
var validCodeScannerTypes = map[string]bool{
	"none":      true,
	"patterns":  true,
	"semgrep":   true,
	"composite": true,
}

// validCompositeCodeScannerTypes is the subset of scanner types that
// may appear in CodeScannerConfig.Scanners. Composite-of-composite is
// excluded so the config cannot recurse.
var validCompositeCodeScannerTypes = map[string]bool{
	"none":     true,
	"patterns": true,
	"semgrep":  true,
}

// validGuardRailTypes is the closed set of GuardRailConfig.Type values.
var validGuardRailTypes = map[string]bool{
	"none":             true,
	"granite-guardian": true,
	"composite":        true,
	"cloud-judge":      true,
}

// validGuardRailPhases is the closed set of intervention points the
// guard may be bound to.
var validGuardRailPhases = map[string]bool{
	"pre_turn":  true,
	"pre_tool":  true,
	"post_turn": true,
}

// validCompositeGuardRailTypes is the subset of guard types that may
// appear inside Stages. composite-of-composite is excluded so the
// config cannot recurse and so failure modes stay tractable for
// operators reading a stack trace.
var validCompositeGuardRailTypes = map[string]bool{
	"none":             true,
	"granite-guardian": true,
	"cloud-judge":      true,
}

// guardRailCustomCriterionPattern restricts CustomCriteria keys to
// snake_case identifiers. Keeps IDs loggable, OTel-attribute-safe, and
// stable across the wire — the same constraint we apply elsewhere when
// an operator-supplied string ends up as a metric or trace attribute.
var guardRailCustomCriterionPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// guardRail bounds.
const (
	guardRailMinTimeoutMs     = 50
	guardRailMaxTimeoutMs     = 30000
	guardRailMaxMinChunkChars = 4096
)

var validBuiltInToolNames = map[string]bool{
	"read_file":      true,
	"write_file":     true,
	"search_replace": true,
	"apply_diff":     true,
	"edit_file":      true,
	"list_directory": true,
	"search_files":   true,
	"run_command":    true,
	"web_fetch":      true,
	"spawn_agent":    true,
}

var readOnlyModes = map[string]bool{
	"planning": true, "review": true, "research": true, "toil": true,
}

// IsReadOnlyMode reports whether the named run mode is a read-only mode
// (one that must not enable write-capable tools).
func IsReadOnlyMode(mode string) bool {
	return readOnlyModes[mode]
}

// DefaultReadOnlyBuiltInTools returns the default set of built-in tools
// enabled for read-only modes when the caller has not supplied an explicit
// Tools.BuiltIn list. The list deliberately excludes every tool in
// mutatingTools so the result always passes ValidateRunConfig for a
// read-only mode.
func DefaultReadOnlyBuiltInTools() []string {
	return []string{
		"read_file",
		"list_directory",
		"search_files",
		"web_fetch",
		"spawn_agent",
	}
}

// mutatingTools enumerates built-in tools that mutate workspace state and
// must therefore be excluded from read-only modes (research, review,
// planning, toil). Other policy-relevant attributes (such as whether a
// tool requires upstream approval) live on the Tool struct itself; this
// list exists purely so RunConfig validation can reject impossible
// combinations before the harness boots.
var mutatingTools = map[string]bool{
	"write_file":  true,
	"run_command": true,
}

// ModePreset is a named set of RunConfig overrides.
type ModePreset struct {
	Name             string                 `json:"name"`
	PromptBuilder    PromptBuilderConfig    `json:"promptBuilder"`
	ModelRouter      ModelRouterConfig      `json:"modelRouter"`
	Tools            ToolsConfig            `json:"tools"`
	EditStrategy     EditStrategyConfig     `json:"editStrategy"`
	Verifier         VerifierConfig         `json:"verifier"`
	PermissionPolicy PermissionPolicyConfig `json:"permissionPolicy"`
	MaxTurns         int                    `json:"maxTurns"`
}

// ValidateRunConfig enforces hard security constraints that cannot be
// overridden by the control plane or CLI flags.
//
// As a side-effect, ValidateRunConfig fills in a default
// CodeScannerConfig when the caller has left it nil, so downstream
// consumers always see a populated value: "patterns" for execution
// mode (active scanning) and "none" for read-only modes (no edits
// happen anyway).
func ValidateRunConfig(config *RunConfig) error {
	applyCodeScannerDefault(config)

	var errs []string

	validateSessionName(config.SessionName, &errs)
	validateRequiredType("provider", config.Provider.Type, validProviderTypes, &errs)
	validateOptionalType("modelRouter", config.ModelRouter.Type, validModelRouterTypes, &errs)
	validateOptionalType("promptBuilder", config.PromptBuilder.Type, validPromptBuilderTypes, &errs)
	validateOptionalType("contextStrategy", config.ContextStrategy.Type, validContextStrategyTypes, &errs)
	validateOptionalType("executor", config.Executor.Type, validExecutorTypes, &errs)
	if !validExecutorRuntimes[config.Executor.Runtime] {
		errs = append(errs, fmt.Sprintf("unsupported executor.runtime %q", config.Executor.Runtime))
	}
	// executor.runtime only changes behaviour for the container executor;
	// silently dropping it on a "local" or "api" run lets an operator
	// believe they have gVisor isolation when in fact the workload is
	// running on the host (S8).
	if config.Executor.Runtime != "" && config.Executor.Type != "container" && config.Executor.Type != "" {
		errs = append(errs, "executor.runtime is only valid when executor.type is \"container\"")
	}
	validateOptionalType("editStrategy", config.EditStrategy.Type, validEditStrategyTypes, &errs)
	validateOptionalType("permissionPolicy", config.PermissionPolicy.Type, validPermissionPolicyTypes, &errs)
	validatePermissionPolicyFields(config.PermissionPolicy, &errs)
	validateOptionalType("gitStrategy", config.GitStrategy.Type, validGitStrategyTypes, &errs)
	validateOptionalType("transport", config.Transport.Type, validTransportTypes, &errs)
	validateOptionalType("traceEmitter", config.TraceEmitter.Type, validTraceEmitterTypes, &errs)
	validateVerifierConfig(config.Verifier, "verifier", &errs)
	validateProviderConfigs(config, &errs)
	validateBuiltInTools(config.Tools.BuiltIn, &errs)
	validateCredentialConfig(config.Provider.Credential, "provider.credential", &errs)
	for name, prov := range config.Providers {
		validateCredentialConfig(prov.Credential, fmt.Sprintf("providers[%s].credential", name), &errs)
	}

	// Read-only modes must use deny-side-effects or ask-upstream
	if readOnlyModes[config.Mode] && config.PermissionPolicy.Type == "allow-all" {
		errs = append(errs, fmt.Sprintf("mode %q requires a restrictive permission policy", config.Mode))
	}

	// Read-only modes must not enable write-capable tools
	if readOnlyModes[config.Mode] {
		if len(config.Tools.BuiltIn) == 0 {
			errs = append(errs, fmt.Sprintf(
				"read-only mode %q requires an explicit tools.builtIn list that excludes write tools (write_file, run_command)",
				config.Mode))
		} else {
			for _, tool := range config.Tools.BuiltIn {
				if mutatingTools[tool] {
					errs = append(errs, fmt.Sprintf("read-only mode %q must not enable write tool %q", config.Mode, tool))
				}
			}
		}
	}

	// maxTurns must be bounded
	if config.MaxTurns > absoluteMaxTurns {
		errs = append(errs, fmt.Sprintf("maxTurns exceeds maximum of %d", absoluteMaxTurns))
	}
	if config.MaxTurns <= 0 {
		errs = append(errs, "maxTurns must be positive")
	}

	// timeout must be set
	if config.Timeout == nil || *config.Timeout <= 0 || *config.Timeout > 3600 {
		errs = append(errs, "timeout is required and must be > 0 and <= 3600 seconds")
	}

	// followUpGrace must be bounded
	if config.FollowUpGrace != nil && *config.FollowUpGrace > maxFollowUpGrace {
		errs = append(errs, fmt.Sprintf("followUpGrace must be <= %d seconds", maxFollowUpGrace))
	}

	// maxCostBudget must be bounded
	if config.MaxCostBudget != nil && *config.MaxCostBudget > maxCostBudget {
		errs = append(errs, fmt.Sprintf("maxCostBudget must be <= $%.2f", maxCostBudget))
	}

	// maxTokenBudget must be bounded
	if config.MaxTokenBudget != nil && *config.MaxTokenBudget > maxTokenBudget {
		errs = append(errs, fmt.Sprintf("maxTokenBudget must be <= %d", maxTokenBudget))
	}

	validateRuleOfTwo(config, &errs)
	validateCodeScannerConfig(config.CodeScanner, &errs)
	validateGuardRailConfig(config.GuardRail, "guardRail", false, &errs)

	if len(errs) > 0 {
		return fmt.Errorf("RunConfig validation failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

func validateRequiredType(name, value string, valid map[string]bool, errs *[]string) {
	if value == "" {
		*errs = append(*errs, fmt.Sprintf("%s type is required", name))
		return
	}
	validateOptionalType(name, value, valid, errs)
}

func validateOptionalType(name, value string, valid map[string]bool, errs *[]string) {
	if value == "" {
		return
	}
	if !valid[value] {
		*errs = append(*errs, fmt.Sprintf("unsupported %s type %q", name, value))
	}
}

// pathHasDotDotSegment reports whether path contains a literal ".."
// component when split on either '/' or '\'. A substring check on
// `..` would also match harmless filenames like "foo..bar.cedar"; we
// only want to reject paths whose individual segments are ".." — the
// shape that turns `os.ReadFile(p)` into a host-file traversal.
func pathHasDotDotSegment(path string) bool {
	for _, sep := range []string{"/", "\\"} {
		for _, seg := range strings.Split(path, sep) {
			if seg == ".." {
				return true
			}
		}
	}
	return false
}

// validatePermissionPolicyFields enforces the cross-field constraints on
// PermissionPolicyConfig that the closed-set validation in
// validateOptionalType cannot express on its own.
//
//   - Type "policy-engine" requires a PolicyFile path so the harness has
//     something concrete to load at boot. A missing file path is almost
//     always a config-typo and we want to fail loudly rather than fall
//     through silently to the fallback policy.
//   - PolicyFile is operator-supplied and may arrive over gRPC. Reject
//     traversal sequences (e.g. "../../etc/passwd") so a malicious
//     control plane cannot trick the harness into reading sensitive
//     host files and surfacing chunks of their content via Cedar
//     parser error messages (M6).
//   - PolicyFile set with a non-policy-engine type is a misconfiguration
//     footgun: the file is silently ignored and the operator believes
//     they have applied a Cedar policy. Reject it loudly (S7).
//   - Fallback, when set, must name one of the three non-policy-engine
//     policies. policy-engine -> policy-engine fallback would loop on a
//     no-decision response, so it's rejected here.
func validatePermissionPolicyFields(cfg PermissionPolicyConfig, errs *[]string) {
	if cfg.Type == "policy-engine" && cfg.PolicyFile == "" {
		*errs = append(*errs, "permissionPolicy type \"policy-engine\" requires policyFile")
	}
	if cfg.PolicyFile != "" && cfg.Type != "policy-engine" {
		*errs = append(*errs, fmt.Sprintf(
			"permissionPolicy.policyFile is set but permissionPolicy.type is %q — policyFile is only used with type=policy-engine",
			cfg.Type))
	}
	if cfg.PolicyFile != "" {
		// We accept either absolute paths (operator-managed) or
		// workspace-relative paths (used by examples/runconfig/full.json)
		// — but never a path that contains ".." segments. Both the raw
		// and cleaned forms are checked: filepath.Clean rewrites
		// "/a/../b" to "/b", which would slip past a post-clean check
		// even though the operator clearly intended traversal.
		if pathHasDotDotSegment(cfg.PolicyFile) || pathHasDotDotSegment(filepath.Clean(cfg.PolicyFile)) {
			*errs = append(*errs, "permissionPolicy.policyFile must not contain \"..\" path segments")
		}
	}
	if cfg.Fallback != "" && !validFallbackPolicyTypes[cfg.Fallback] {
		*errs = append(*errs, fmt.Sprintf("permissionPolicy.fallback %q is not a valid fallback policy type", cfg.Fallback))
	}
}

// RuleOfTwoState reports the three Rule-of-Two booleans for a config:
// holdsUntrusted (untrusted input ingress), holdsSensitive (sensitive
// data on hand), and canCommExternal (external communication).
//
// Exposed so factory wiring can emit security events without
// re-implementing the heuristics. Returns the same booleans the
// validator computes internally — single source of truth.
func RuleOfTwoState(config *RunConfig) (holdsUntrusted, holdsSensitive, canCommExternal bool) {
	if config == nil {
		return false, false, false
	}
	return ruleOfTwoUntrustedInput(config), ruleOfTwoSensitiveData(config), ruleOfTwoExternalComm(config)
}

// validateRuleOfTwo enforces Meta's "Agents Rule of Two": a single
// session must not simultaneously hold (a) untrusted input, (b)
// sensitive data, and (c) the ability to communicate externally —
// unless gated by the ask-upstream permission policy. The override
// (RuleOfTwo.Enforce == false) is honoured silently here; the factory
// emits a rule_of_two_disabled security event at run start to keep the
// override auditable.
//
// The three booleans are deliberately crude in v1 (per the issue
// brief). They will be refined as we collect eval-suite signal.
func validateRuleOfTwo(config *RunConfig, errs *[]string) {
	holdsUntrusted := ruleOfTwoUntrustedInput(config)
	holdsSensitive := ruleOfTwoSensitiveData(config)
	canCommExternal := ruleOfTwoExternalComm(config)

	if !(holdsUntrusted && holdsSensitive && canCommExternal) {
		return
	}
	if config.PermissionPolicy.Type == "ask-upstream" {
		return
	}
	if config.RuleOfTwo != nil && config.RuleOfTwo.Enforce != nil && !*config.RuleOfTwo.Enforce {
		return
	}
	*errs = append(*errs,
		"all three of {untrusted-input, sensitive-data, external-communication} cannot simultaneously hold without the ask-upstream permission policy (Rule of Two)")
}

// ruleOfTwoUntrustedInput reports whether the run can ingest content
// from outside the operator's trust boundary. Dynamic context entries
// are populated by the control plane from issue bodies / PR comments
// / etc. and must be treated as untrusted; web_fetch and MCP servers
// pull live data from arbitrary remote endpoints.
func ruleOfTwoUntrustedInput(config *RunConfig) bool {
	if len(config.DynamicContext) > 0 {
		return true
	}
	if isToolEnabled(config.Tools.BuiltIn, "web_fetch") {
		return true
	}
	if len(config.Tools.MCPServers) > 0 {
		return true
	}
	return false
}

// ruleOfTwoSensitiveData reports whether the run carries credentials
// or other sensitive data the agent could exfiltrate.
//
// "Allowlisted secret env vars" interpretation: ExecutorConfig has no
// dedicated env-allowlist field today, and the brief is explicit that
// we must not invent one in this wave. We therefore inspect the
// secret references that are actually carried in RunConfig — APIKeyRef
// fields on the default provider, the named providers map, the VCS
// backend, and MCP servers — and treat any whose name matches the
// secret-name heuristic (*KEY*, *TOKEN*, *SECRET*, *PASSWORD*, case-
// insensitive) as a "sensitive env var" for Rule-of-Two purposes. Any
// secret://ssm:// reference also triggers this flag, regardless of
// name, because SSM-backed values are by definition real secrets.
func ruleOfTwoSensitiveData(config *RunConfig) bool {
	if isSensitiveSecretRef(config.Provider.APIKeyRef) {
		return true
	}
	for _, prov := range config.Providers {
		if isSensitiveSecretRef(prov.APIKeyRef) {
			return true
		}
	}
	if config.Executor.VcsBackend != nil && isSensitiveSecretRef(config.Executor.VcsBackend.APIKeyRef) {
		return true
	}
	for _, server := range config.Tools.MCPServers {
		if isSensitiveSecretRef(server.APIKeyRef) {
			return true
		}
	}
	return false
}

// ruleOfTwoExternalComm reports whether the run can send data to
// systems outside the harness sandbox. run_command escapes via the
// shell; web_fetch sends arbitrary HTTP requests; MCP servers receive
// every tool call payload; non-"none" network configs let the
// container reach the internet.
func ruleOfTwoExternalComm(config *RunConfig) bool {
	if isToolEnabled(config.Tools.BuiltIn, "run_command") {
		return true
	}
	if isToolEnabled(config.Tools.BuiltIn, "web_fetch") {
		return true
	}
	if len(config.Tools.MCPServers) > 0 {
		return true
	}
	if config.Executor.Network != nil && config.Executor.Network.Mode != "" && config.Executor.Network.Mode != "none" {
		return true
	}
	return false
}

// isToolEnabled mirrors the semantics used by harness/internal/core
// for resolving Tools.BuiltIn: an empty list means "all built-in tools
// are enabled", a non-empty list is treated as an explicit allowlist.
// Read-only modes already constrain the tool set elsewhere so this
// just answers "would the loop expose this tool to the model".
func isToolEnabled(enabled []string, name string) bool {
	if len(enabled) == 0 {
		return true
	}
	for _, candidate := range enabled {
		if candidate == name {
			return true
		}
	}
	return false
}

// isSensitiveSecretRef reports whether the supplied secret reference
// names a credential the Rule-of-Two should treat as sensitive. SSM
// references are always sensitive; env/file refs are sensitive only
// when the referenced name matches one of the conventional secret
// substrings (key/token/secret/password, case-insensitive).
func isSensitiveSecretRef(ref string) bool {
	if ref == "" {
		return false
	}
	const prefix = "secret://"
	rest := ref
	if strings.HasPrefix(rest, prefix) {
		rest = rest[len(prefix):]
	}
	if strings.HasPrefix(rest, "ssm://") || strings.HasPrefix(rest, "ssm:///") {
		return true
	}
	upper := strings.ToUpper(rest)
	return strings.Contains(upper, "KEY") ||
		strings.Contains(upper, "TOKEN") ||
		strings.Contains(upper, "SECRET") ||
		strings.Contains(upper, "PASSWORD")
}

// applyCodeScannerDefault fills CodeScanner with a sensible default
// when the caller has not set one. The default is "patterns" for
// execution mode (active scanning on every successful edit) and
// "none" for read-only modes (no edits happen so there's nothing to
// scan). Defaulting at validation time means downstream consumers
// always see a populated value and can avoid nil-checking.
func applyCodeScannerDefault(config *RunConfig) {
	if config.CodeScanner != nil {
		return
	}
	if readOnlyModes[config.Mode] {
		config.CodeScanner = &CodeScannerConfig{Type: "none"}
		return
	}
	config.CodeScanner = &CodeScannerConfig{Type: "patterns"}
}

// validateCodeScannerConfig enforces the closed-set Type and the
// composite-only Scanners field. A composite scanner with an empty
// Scanners list is always a config error (no work to do); each
// scanner referenced must be a known non-composite type to prevent
// the config from recursing.
func validateCodeScannerConfig(cfg *CodeScannerConfig, errs *[]string) {
	if cfg == nil {
		return
	}
	if !validCodeScannerTypes[cfg.Type] {
		*errs = append(*errs, fmt.Sprintf("unsupported codeScanner.type %q", cfg.Type))
		return
	}
	if cfg.Type == "composite" {
		if len(cfg.Scanners) == 0 {
			*errs = append(*errs, "codeScanner.type \"composite\" requires a non-empty scanners list")
			return
		}
		for i, name := range cfg.Scanners {
			if !validCompositeCodeScannerTypes[name] {
				*errs = append(*errs, fmt.Sprintf("codeScanner.scanners[%d] %q is not a valid scanner type", i, name))
			}
		}
	}
}

func validateVerifierConfig(cfg VerifierConfig, path string, errs *[]string) {
	validateOptionalType(path, cfg.Type, validVerifierTypes, errs)
	for i, sub := range cfg.Verifiers {
		validateVerifierConfig(sub, fmt.Sprintf("%s.verifiers[%d]", path, i), errs)
	}
}

func validateProviderConfigs(config *RunConfig, errs *[]string) {
	knownProviders := map[string]bool{}
	if config.Provider.Type != "" {
		knownProviders[config.Provider.Type] = true
	}
	validateOpenAIAuthFields("provider", config.Provider, errs)
	for name, provider := range config.Providers {
		if name == "" {
			*errs = append(*errs, "providers map contains an empty provider name")
			continue
		}
		if knownProviders[name] {
			*errs = append(*errs, fmt.Sprintf("provider name %q is defined more than once", name))
			continue
		}
		knownProviders[name] = true
		validateRequiredType(fmt.Sprintf("providers[%s]", name), provider.Type, validProviderTypes, errs)
		validateOpenAIAuthFields(fmt.Sprintf("providers[%s]", name), provider, errs)
	}

	checkProviderRef := func(path, name string) {
		if name == "" {
			return
		}
		if !knownProviders[name] {
			*errs = append(*errs, fmt.Sprintf("%s references unknown provider %q", path, name))
		}
	}

	checkProviderRef("modelRouter.provider", config.ModelRouter.Provider)
	checkProviderRef("modelRouter.cheapProvider", config.ModelRouter.CheapProvider)
	checkProviderRef("modelRouter.expensiveProvider", config.ModelRouter.ExpensiveProvider)
	for mode, spec := range config.ModelRouter.ModeModels {
		if providerName, _, ok := strings.Cut(spec, "/"); ok {
			checkProviderRef(fmt.Sprintf("modelRouter.modeModels[%s]", mode), providerName)
		}
	}
}

// validateSessionName enforces the SessionName invariants: bounded length and
// printable, non-control characters only. Empty is valid (means unset). The
// goal is to keep the value safe to drop into a log line, an OTel attribute,
// or a trace JSON record without truncation, escaping, or line corruption.
func validateSessionName(name string, errs *[]string) {
	if name == "" {
		return
	}
	if len(name) > maxSessionNameLength {
		*errs = append(*errs, fmt.Sprintf("sessionName must be <= %d bytes, got %d", maxSessionNameLength, len(name)))
		return
	}
	if !utf8.ValidString(name) {
		*errs = append(*errs, "sessionName must be valid UTF-8")
		return
	}
	for i, r := range name {
		// Reject every non-printable rune, including line terminators,
		// tabs, NUL, and DEL. unicode.IsPrint returns false for control
		// characters and for the Unicode separators we don't want either.
		if !unicode.IsPrint(r) {
			*errs = append(*errs, fmt.Sprintf("sessionName contains non-printable character at byte %d (U+%04X)", i, r))
			return
		}
	}
}

func validateBuiltInTools(builtIns []string, errs *[]string) {
	for _, name := range builtIns {
		if !validBuiltInToolNames[name] {
			*errs = append(*errs, fmt.Sprintf("tools.builtIn contains unsupported tool %q", name))
		}
	}
}

func validateCredentialConfig(cfg *CredentialConfig, path string, errs *[]string) {
	if cfg == nil {
		return
	}
	validateRequiredType(path, cfg.Type, validCredentialTypes, errs)

	if cfg.Type == "web-identity" {
		if cfg.RoleARN == "" {
			*errs = append(*errs, fmt.Sprintf("%s: web-identity requires roleArn", path))
		}
		if cfg.TokenSource == nil {
			*errs = append(*errs, fmt.Sprintf("%s: web-identity requires tokenSource", path))
		} else {
			validateTokenSourceConfig(cfg.TokenSource, path+".tokenSource", errs)
		}
	}
}

func validateTokenSourceConfig(cfg *TokenSourceConfig, path string, errs *[]string) {
	validateRequiredType(path, cfg.Type, validTokenSourceTypes, errs)

	switch cfg.Type {
	case "gke-metadata":
		if cfg.Audience == "" {
			*errs = append(*errs, fmt.Sprintf("%s: gke-metadata requires audience", path))
		}
	case "file":
		if cfg.Path == "" {
			*errs = append(*errs, fmt.Sprintf("%s: file requires path", path))
		}
	case "env":
		if cfg.EnvVar == "" {
			*errs = append(*errs, fmt.Sprintf("%s: env requires envVar", path))
		}
	}
}

// validateOpenAIAuthFields enforces the safety invariants on the optional
// APIKeyHeader and QueryParams fields. The fields are only meaningful for
// the OpenAI-shaped adapters; for other provider types they are ignored
// at runtime, but we still validate the values so a stale config does
// not silently keep a bad value alive across a provider-type change.
//
// Header values are never logged anywhere — these checks bound only the
// header name, which the request emits in clear; CRLF or whitespace there
// would let an attacker who controls config inject extra headers.
func validateOpenAIAuthFields(path string, cfg ProviderConfig, errs *[]string) {
	if cfg.APIKeyHeader != "" {
		// Reject CR/LF and whitespace explicitly so the error message names
		// the failure mode rather than just "invalid pattern". Anyone who
		// hits this is likely a misuse, not a charset surprise.
		if strings.ContainsAny(cfg.APIKeyHeader, "\r\n\t ") || strings.ContainsRune(cfg.APIKeyHeader, ':') {
			*errs = append(*errs, fmt.Sprintf("%s.apiKeyHeader must not contain whitespace, ':' or CR/LF", path))
		} else if !apiKeyHeaderPattern.MatchString(cfg.APIKeyHeader) {
			*errs = append(*errs, fmt.Sprintf("%s.apiKeyHeader %q must match %s", path, cfg.APIKeyHeader, apiKeyHeaderPattern.String()))
		}
	}

	if len(cfg.QueryParams) > 0 {
		encoded := url.Values{}
		for k, v := range cfg.QueryParams {
			if k == "" {
				*errs = append(*errs, fmt.Sprintf("%s.queryParams contains an empty key", path))
				continue
			}
			if !queryParamKeyPattern.MatchString(k) {
				*errs = append(*errs, fmt.Sprintf("%s.queryParams key %q must match %s", path, k, queryParamKeyPattern.String()))
			}
			if strings.ContainsAny(v, "\r\n") {
				*errs = append(*errs, fmt.Sprintf("%s.queryParams[%s] value must not contain CR/LF", path, k))
			}
			encoded.Set(k, v)
		}
		if size := len(encoded.Encode()); size > maxQueryStringBytes {
			*errs = append(*errs, fmt.Sprintf("%s.queryParams encoded form is %d bytes, exceeds %d byte cap", path, size, maxQueryStringBytes))
		}
	}
}

// validateGuardRailConfig enforces the closed-set Type, the
// composite-only Stages field, range checks, and the cross-field
// constraints. A nil config is valid (means "no guardrails", treated as
// type=none by the factory). An empty Type is also valid for the same
// reason — it lets operators omit guard config entirely without
// scattering nil checks downstream.
//
//   - Type, when non-empty, must be in validGuardRailTypes;
//     "composite" requires a non-empty Stages list.
//   - Each Stages entry must validate as a non-composite
//     GuardRailConfig (composite-of-composite is rejected so the
//     config cannot recurse).
//   - Phases values must be in {pre_turn, pre_tool, post_turn};
//     duplicates are rejected.
//   - Threshold ∈ [0.0, 1.0].
//   - TimeoutMs, when set, ∈ [50, 30000].
//   - MinChunkChars, when set, ∈ [0, 4096].
//   - "granite-guardian" requires Endpoint.
//   - Endpoint set with Type=none / Type=composite is rejected (a
//     stale value would be silently ignored — the kind of footgun that
//     bites at 03:00).
//   - Endpoint, when set, must parse with net/url and use scheme
//     http or https, with a non-empty host. A path is allowed (vLLM
//     typically serves at /v1/chat/completions).
//   - CustomCriteria keys must be non-empty and conform to
//     [a-z][a-z0-9_]*. Each Criteria entry must be non-empty.
//
// nestedComposite is true when validating an entry inside Stages — the
// closed set tightens to validCompositeGuardRailTypes for that call.
func validateGuardRailConfig(cfg *GuardRailConfig, path string, nestedComposite bool, errs *[]string) {
	if cfg == nil {
		return
	}

	// Empty type is treated as "none" by the factory; no further
	// validation applies. An entirely-empty GuardRailConfig{} is a
	// valid shape on the wire even though it's not useful in practice.
	if cfg.Type == "" {
		// Reject the case where the operator forgot the type but
		// populated adapter fields — a typo we want to surface.
		if guardRailHasAdapterFields(cfg) {
			*errs = append(*errs, fmt.Sprintf("%s.type is required when other guardRail fields are set", path))
		}
		return
	}

	allowedTypes := validGuardRailTypes
	if nestedComposite {
		allowedTypes = validCompositeGuardRailTypes
	}
	if !allowedTypes[cfg.Type] {
		*errs = append(*errs, fmt.Sprintf("unsupported %s.type %q", path, cfg.Type))
		return
	}

	// composite vs leaf branching.
	if cfg.Type == "composite" {
		if len(cfg.Stages) == 0 {
			*errs = append(*errs, fmt.Sprintf("%s.type \"composite\" requires a non-empty stages list", path))
		}
		// Composite has no transport of its own; adapter fields on it
		// are silently ignored and almost always indicate the operator
		// thought the field would propagate. Reject loudly.
		if cfg.Endpoint != "" {
			*errs = append(*errs, fmt.Sprintf("%s.endpoint is not valid for type=composite", path))
		}
		for i, stage := range cfg.Stages {
			subPath := fmt.Sprintf("%s.stages[%d]", path, i)
			stage := stage // capture loop variable
			validateGuardRailConfig(&stage, subPath, true, errs)
		}
	} else {
		// Leaf types share the same field shape; per-type required
		// fields and footguns are checked here.
		switch cfg.Type {
		case "none":
			if cfg.Endpoint != "" {
				*errs = append(*errs, fmt.Sprintf("%s.endpoint is not valid for type=none", path))
			}
		case "granite-guardian":
			if cfg.Endpoint == "" {
				*errs = append(*errs, fmt.Sprintf("%s.type %q requires endpoint", path, cfg.Type))
			}
		case "cloud-judge":
			// cloud-judge reuses an existing ProviderAdapter; an
			// explicit endpoint is allowed but optional (the adapter
			// resolves it from the underlying provider when omitted).
		}
	}

	// Endpoint URL shape, applied to whichever leaf types accept it.
	if cfg.Endpoint != "" && cfg.Type != "none" && cfg.Type != "composite" {
		validateGuardRailEndpoint(cfg.Endpoint, path+".endpoint", errs)
	}

	// Phases — closed set + duplicate rejection.
	if len(cfg.Phases) > 0 {
		seen := make(map[string]bool, len(cfg.Phases))
		for i, phase := range cfg.Phases {
			if !validGuardRailPhases[phase] {
				*errs = append(*errs, fmt.Sprintf("%s.phases[%d] %q is not a valid phase", path, i, phase))
				continue
			}
			if seen[phase] {
				*errs = append(*errs, fmt.Sprintf("%s.phases contains duplicate %q", path, phase))
				continue
			}
			seen[phase] = true
		}
	}

	// Range checks.
	if cfg.Threshold < 0.0 || cfg.Threshold > 1.0 {
		*errs = append(*errs, fmt.Sprintf("%s.threshold must be in [0.0, 1.0], got %v", path, cfg.Threshold))
	}
	if cfg.TimeoutMs != 0 && (cfg.TimeoutMs < guardRailMinTimeoutMs || cfg.TimeoutMs > guardRailMaxTimeoutMs) {
		*errs = append(*errs, fmt.Sprintf("%s.timeoutMs must be in [%d, %d], got %d", path, guardRailMinTimeoutMs, guardRailMaxTimeoutMs, cfg.TimeoutMs))
	}
	if cfg.MinChunkChars < 0 || cfg.MinChunkChars > guardRailMaxMinChunkChars {
		*errs = append(*errs, fmt.Sprintf("%s.minChunkChars must be in [0, %d], got %d", path, guardRailMaxMinChunkChars, cfg.MinChunkChars))
	}

	// Criteria entries must be non-empty — a blank ID would map to no
	// vetted text in the adapter and silently degrade to "no
	// criterion".
	for i, c := range cfg.Criteria {
		if c == "" {
			*errs = append(*errs, fmt.Sprintf("%s.criteria[%d] is empty", path, i))
		}
	}

	// CustomCriteria keys — closed format; values may be empty (the
	// adapter treats that as "use the built-in default text for the
	// id" in some configurations, so we don't reject empty values
	// here).
	for k := range cfg.CustomCriteria {
		if k == "" {
			*errs = append(*errs, fmt.Sprintf("%s.customCriteria contains an empty key", path))
			continue
		}
		if !guardRailCustomCriterionPattern.MatchString(k) {
			*errs = append(*errs, fmt.Sprintf("%s.customCriteria key %q must match %s", path, k, guardRailCustomCriterionPattern.String()))
		}
	}
}

// guardRailHasAdapterFields reports whether the config has any
// non-zero leaf field. Used to detect the "operator forgot Type but
// filled in Endpoint / Threshold / etc." typo so we can fail loudly
// instead of silently treating the config as type=none.
func guardRailHasAdapterFields(cfg *GuardRailConfig) bool {
	if cfg == nil {
		return false
	}
	return cfg.Endpoint != "" ||
		cfg.Model != "" ||
		cfg.Threshold != 0 ||
		len(cfg.Criteria) > 0 ||
		len(cfg.CustomCriteria) > 0 ||
		cfg.Think != nil ||
		cfg.TimeoutMs != 0 ||
		cfg.FailOpen ||
		cfg.MinChunkChars != 0 ||
		len(cfg.Stages) > 0 ||
		len(cfg.Phases) > 0
}

// validateGuardRailEndpoint enforces that the endpoint URL parses, has
// scheme http or https, and a non-empty host. A path is permitted
// (vLLM serves at /v1/chat/completions); query strings are permitted
// because gateways may legitimately need them.
func validateGuardRailEndpoint(endpoint, path string, errs *[]string) {
	u, err := url.Parse(endpoint)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s %q must be a valid URL: %v", path, endpoint, err))
		return
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		*errs = append(*errs, fmt.Sprintf("%s %q must use scheme http or https, got %q", path, endpoint, u.Scheme))
	}
	if u.Host == "" {
		*errs = append(*errs, fmt.Sprintf("%s %q must include a host", path, endpoint))
	}
}
