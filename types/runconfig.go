package types

import (
	"fmt"
	"log/slog"
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

	// maxTemperature is the upper bound on RunConfig.Temperature.
	// Chosen as the union of provider-side ranges: Anthropic accepts
	// [0, 1], OpenAI and Gemini accept [0, 2]. Values inside the union
	// validate cleanly here so a single config can target multiple
	// providers; the adapter still surfaces the provider's own rejection
	// when a value lands above that provider's narrower range.
	maxTemperature = 2.0

	// maxSessionNameLength is the maximum allowed length, in bytes, of
	// SessionName. Capped to keep log lines, OTel attribute values, and
	// trace JSON predictable; well above any genuine human-readable label.
	maxSessionNameLength = 255

	// Provider retry defaults. Filled in by applyProviderRetryDefaults
	// when the caller leaves a field zero so downstream consumers always
	// see a populated ProviderRetryConfig.
	defaultProviderRetryMaxAttempts       = 3
	defaultProviderRetryInitialDelayMs    = 500
	defaultProviderRetryMaxDelayMs        = 16000
	defaultProviderRetryWallClockBudgetMs = 90000

	// Provider retry hard ceilings. Enforced by
	// validateProviderRetryConfig regardless of whether the value came
	// from the caller or a default. InitialDelayMs has no independent
	// ceiling; it is transitively bounded by maxProviderRetryMaxDelayMs
	// via the cross-field invariant (initialDelayMs <= maxDelayMs).
	maxProviderRetryMaxAttempts       = 5
	maxProviderRetryMaxDelayMs        = 60000
	maxProviderRetryWallClockBudgetMs = 300000

	// DefaultToolDispatchMaxParallel is the fan-out applied when
	// ToolDispatchConfig is omitted or MaxParallel is zero. Chosen as a
	// balance between latency and over-subscription on the deep-research
	// multi-step-synthesis tier.
	DefaultToolDispatchMaxParallel = 4

	// MaxToolDispatchMaxParallel is the hard ceiling on
	// ToolDispatchConfig.MaxParallel enforced by ValidateRunConfig. Caps
	// runaway concurrency so a misconfigured run cannot saturate the
	// provider/transport beyond what the rest of the harness is sized
	// for.
	MaxToolDispatchMaxParallel = 16

	// DefaultBatchMaxWaitSeconds is the harness-side default wall-clock
	// cap on a batch wait (24 h), matching the Anthropic and OpenAI
	// provider-side SLA for batch completion. Applied by ValidateRunConfig
	// when Batch.Enabled and Batch.MaxWaitSeconds == nil.
	DefaultBatchMaxWaitSeconds = 86400

	// batchTurnsLatencyWarnThreshold is the maxTurns ceiling above which
	// ValidateRunConfig emits a slog WARN: each turn can take up to 24 h
	// of wall-clock waiting on the provider, so a run with many turns can
	// spend weeks in flight. The threshold is the operator's reasonable
	// upper bound before the latency exposure deserves a heads-up.
	batchTurnsLatencyWarnThreshold = 5
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
	Prompt         string                         `json:"prompt"`
	DynamicContext map[string]DynamicContextValue `json:"dynamicContext,omitempty"`

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
	ResultSink       *ResultSinkConfig         `json:"resultSink,omitempty"`
	Tools            ToolsConfig               `json:"tools"`

	// Limits
	MaxTurns       int      `json:"maxTurns"`
	MaxTokenBudget *int     `json:"maxTokenBudget,omitempty"`
	MaxCostBudget  *float64 `json:"maxCostBudget,omitempty"`
	Timeout        *int     `json:"timeout,omitempty"`

	// Temperature is the sampling temperature forwarded to the provider
	// on every turn. Nil means "use the harness default" (0.1 — a low
	// value that biases for determinism on coding tasks). A non-nil value
	// is forwarded verbatim, including an explicit 0.0 for greedy
	// decoding; this is the only way to override the default downwards.
	//
	// Validated against [0.0, maxTemperature]. The union of provider
	// ranges (Anthropic [0, 1], OpenAI/Gemini [0, 2]) means a value
	// inside the union may still be rejected by the chosen provider's
	// own API; the adapter surfaces that rejection at request time
	// rather than at validation.
	//
	// Reasoning models that reject temperature on the wire are a
	// separate concern: the provider adapter is responsible for
	// stripping the field when the selected model requires it.
	Temperature *float64 `json:"temperature,omitempty"`

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

	// SensitiveData, when explicitly true, declares that this run will
	// expose the agent to sensitive data inside its conversation. It is
	// the operator-supplied signal for the "sensitive data" leg of the
	// Rule of Two. Provider/VCS/MCP API keys are deliberately *not*
	// inferred as sensitive data: the harness keeps those out of the
	// agent's reach (env-allowlist on run_command, log scrubbing,
	// SecretStore deferred resolution). What "sensitive data" really
	// means for the rule is data the agent itself can read — and only
	// the operator can know that at config time. Per-entry sensitivity
	// can also be declared via DynamicContextValue.Sensitive; either
	// signal trips the rule's sensitive-data leg.
	//
	// Pointer so unset is wire-distinguishable from explicit false: the
	// secure default in v1 is "not sensitive unless declared". Future
	// work (#42 follow-up tracking GuardRail integration) may compute
	// this at runtime from observed conversation content.
	SensitiveData *bool `json:"sensitiveData,omitempty"`

	// GuardRail configures the LLM-based safety classifier that runs
	// at three intervention points in the agentic loop. When nil, the
	// factory installs a "none" guard so call sites never nil-check.
	// See GuardRailConfig for the available implementations.
	GuardRail *GuardRailConfig `json:"guardRail,omitempty"`

	// Observability carries the run-scoped attributes that ride on the
	// OTel Resource (deployment.environment, service.namespace) so
	// traces and metrics emitted by this run share a consistent
	// resource identity with other runs in the same deployment. Only
	// low-cardinality fields belong here — high-cardinality identifiers
	// like RunID stay span/instrument-level so metric series do not
	// explode. When unset the resource builder falls back to environment
	// variables and finally to safe defaults ("local" / "stirrup").
	Observability ObservabilityConfig `json:"observability,omitempty"`

	// ToolDispatch tunes the parallel async-tool dispatch loop (knob
	// applies to all AsyncHandler-backed tools, not just spawn_agent).
	// Nil (or a zero MaxParallel) selects DefaultToolDispatchMaxParallel;
	// see EffectiveToolDispatchMaxParallel for the resolution helper
	// used by the loop. The field is a pointer so an absent value on the
	// wire is distinguishable from an explicit zero — both are legal and
	// resolve to the default, but keeping the distinction lets future
	// fields (per-tool overrides, semaphore strategy) land without a
	// breaking change.
	ToolDispatch *ToolDispatchConfig `json:"toolDispatch,omitempty"`
}

// ObservabilityConfig carries operator-supplied labels that are promoted to
// the OTel Resource shared by all signals (traces, metrics) for a run. The
// fields are free-form labels with a deliberately conservative character
// set so they cannot inject CRLF, query-string separators, or other URL/
// header surprises into downstream backends.
//
// Derived attributes (e.g. harness.run.mode from RunConfig.Mode) are added
// at resource-construction time in observability.BuildResource and must NOT
// be added here or to the proto message — they are not operator-configurable
// labels, so plumbing them through the wire format would create a confusing
// "you can override the run mode in your RunConfig but only for telemetry"
// surface. New issue #94 attributes that are pure operator metadata belong
// here; new derived attributes belong in observability.ResourceOptions.
//
// Zero-value fields (empty string) are NOT stored defaults. Defaults
// ("local" for Environment, "stirrup" for ServiceNamespace) are applied in
// observability.BuildResource via a precedence chain:
// RunConfig → OTEL_* env vars → hardcoded defaults. A recorded RunConfig
// with empty observability fields does not mean the attribute was absent
// from emitted spans — it means the default or env-var value was used.
type ObservabilityConfig struct {
	Environment      string `json:"environment,omitempty"`
	ServiceNamespace string `json:"serviceNamespace,omitempty"`
}

// DynamicContextValue is a single dynamic-context value with metadata.
// The control plane / operator populates these from outside the immutable
// prompt — issue bodies, PR comments, retrieved documents, customer
// records pulled in for triage, etc. Every entry is treated as
// untrusted by the prompt builder (wrapped in <untrusted_context>);
// the Sensitive flag additionally marks an entry as carrying private
// data that the Rule of Two should weigh.
type DynamicContextValue struct {
	// Value is the entry text. Sanitization (XML/HTML tag stripping,
	// 50 KB length cap) runs over this field before it reaches the
	// prompt builder.
	Value string `json:"value"`

	// Sensitive, when true, marks the entry as carrying data the Rule
	// of Two should treat as the "sensitive data" leg. Defaults to
	// false. The flag is per-entry rather than global so an operator
	// can mix non-sensitive context (a public README) with sensitive
	// context (a customer record) in a single run.
	Sensitive bool `json:"sensitive,omitempty"`
}

// DynamicContextValues projects DynamicContext to a {key: Value} map
// for downstream consumers (PromptBuilder, Sanitize, PolicyEngine
// Cedar context) that only need the string content. The Sensitive
// flag is preserved on the original RunConfig.DynamicContext map for
// components that need it (Rule of Two, future GuardRail).
//
// Returns nil for nil/empty input.
func (rc *RunConfig) DynamicContextValues() map[string]string {
	if rc == nil || len(rc.DynamicContext) == 0 {
		return nil
	}
	out := make(map[string]string, len(rc.DynamicContext))
	for k, e := range rc.DynamicContext {
		out[k] = e.Value
	}
	return out
}

// EffectiveToolDispatchMaxParallel returns the fan-out the async-tool
// dispatch loop should apply. Returns DefaultToolDispatchMaxParallel
// when ToolDispatch is nil or MaxParallel is zero; otherwise returns
// MaxParallel verbatim (ValidateRunConfig has already bounded it to
// [1, MaxToolDispatchMaxParallel]).
func (rc *RunConfig) EffectiveToolDispatchMaxParallel() int {
	if rc == nil || rc.ToolDispatch == nil || rc.ToolDispatch.MaxParallel == 0 {
		return DefaultToolDispatchMaxParallel
	}
	return rc.ToolDispatch.MaxParallel
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
// Note: CredentialConfig fields (roleArn, audience, sessionName,
// federationRuleId, organizationId, serviceAccountId, workspaceId) are
// not secrets and are preserved for diagnostics. Anthropic's WIF docs
// explicitly call out the four federation identifiers as non-secret
// values safe to commit to source control or bake into a container
// image.
func (rc RunConfig) Redact() RunConfig {
	redacted := rc
	if redacted.Provider.APIKeyRef != "" {
		redacted.Provider.APIKeyRef = "secret://[REDACTED]"
	}
	// Deep-copy Provider.Retry so a downstream consumer holding the
	// redacted config cannot reach back into the live RunConfig via the
	// shared pointer. Redact() does not mutate Retry today, but every
	// other pointer field it touches is deep-copied; matching the
	// established pattern closes the aliasing window before a Wave 2
	// retry-helper that mutates Retry could silently corrupt the live
	// config it was handed a "safe copy" of.
	if redacted.Provider.Retry != nil {
		retry := *redacted.Provider.Retry
		redacted.Provider.Retry = &retry
	}
	// Mirror the Retry deep-copy for Provider.Batch. The aliasing risk
	// is concrete: validateBatchConfig mutates Batch.MaxWaitSeconds in
	// place to apply the default, and the phase-2 BatchAdapter (#135)
	// is expected to hold a reference to the Redact()-derived snapshot
	// across the run. Without the deep copy, a later default-apply
	// write would reach the redacted copy through the shared pointer
	// and break the snapshot contract.
	if redacted.Provider.Batch != nil {
		batch := *redacted.Provider.Batch
		redacted.Provider.Batch = &batch
	}
	if len(redacted.Providers) > 0 {
		providers := make(map[string]ProviderConfig, len(redacted.Providers))
		for name, provider := range redacted.Providers {
			if provider.APIKeyRef != "" {
				provider.APIKeyRef = "secret://[REDACTED]"
			}
			if provider.Retry != nil {
				retry := *provider.Retry
				provider.Retry = &retry
			}
			if provider.Batch != nil {
				batch := *provider.Batch
				provider.Batch = &batch
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
	// Trace-emitter headers may carry secret:// references for cloud
	// gateway auth (e.g. {"Authorization": "secret://GRAFANA_CLOUD_AUTH"}).
	// The reference itself is not sensitive (it points at the env var
	// name, not the token) but persisting it through to the trace would
	// undercut the SecretStore contract and surprise an operator who
	// rotated a key. Plaintext values stay untouched — operators who
	// inline a header (e.g. {"X-Tenant": "team-a"}) consented to having
	// it appear in traces.
	if len(redacted.TraceEmitter.Headers) > 0 {
		headers := make(map[string]string, len(redacted.TraceEmitter.Headers))
		for k, v := range redacted.TraceEmitter.Headers {
			if strings.HasPrefix(v, "secret://") {
				headers[k] = "secret://[REDACTED]"
			} else {
				headers[k] = v
			}
		}
		redacted.TraceEmitter.Headers = headers
	}
	// ResultSink.Attributes (reserved for the future gcp-pubsub adapter)
	// may carry "secret://" references the same way trace-emitter
	// headers do. Mirror the same rewrite so a recorded RunConfig
	// never persists the reference into a trace or recording.
	if redacted.ResultSink != nil && len(redacted.ResultSink.Attributes) > 0 {
		sink := *redacted.ResultSink
		attrs := make(map[string]string, len(sink.Attributes))
		for k, v := range sink.Attributes {
			if strings.HasPrefix(v, "secret://") {
				attrs[k] = "secret://[REDACTED]"
			} else {
				attrs[k] = v
			}
		}
		sink.Attributes = attrs
		redacted.ResultSink = &sink
	}
	return redacted
}

// ProviderConfig selects the model provider implementation.
type ProviderConfig struct {
	Type       string            `json:"type"`                 // "anthropic" | "bedrock" | "openai-compatible" | "openai-responses" | "gemini"
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

	// GCPProject is the Google Cloud project ID hosting the Vertex AI
	// usage. Required for the "gemini" provider; rejected for every
	// other type so a stale config does not silently keep a project
	// reference alive across a provider-type change.
	GCPProject string `json:"gcpProject,omitempty"`

	// GCPLocation is the Vertex AI location. Either "global" or a
	// region like "us-central1". Determines both the URL host and the
	// project location path segment of the streamGenerateContent
	// endpoint. Required for the "gemini" provider.
	GCPLocation string `json:"gcpLocation,omitempty"`

	// GCPCredentialsFile is the path to a Google service account JSON
	// key file. Only consulted when Credential.Type ==
	// "gcp-service-account"; setting it with any other credential type
	// is a hard error so a misconfigured run fails loudly rather than
	// silently ignoring the path. The path itself is not treated as a
	// secret (it appears in traces); the file's contents are.
	GCPCredentialsFile string `json:"gcpCredentialsFile,omitempty"`

	// GeminiSafetySettings overrides the default safety thresholds for
	// the "gemini" provider. When the list is empty, the adapter
	// applies BLOCK_NONE to all five HARM_CATEGORY_* categories — the
	// only sane default for a coding harness producing security
	// tooling, where false positives on code samples are operationally
	// unacceptable.
	GeminiSafetySettings []GeminiSafetySetting `json:"geminiSafetySettings,omitempty"`

	// Retry overrides the per-call retry policy applied by adapters that
	// honour it. Nil = use defaults. Defaults are filled in by
	// ValidateRunConfig so downstream consumers always see a populated
	// value.
	Retry *ProviderRetryConfig `json:"retry,omitempty"`

	// Batch enables async batch submission for every provider turn in this
	// run. Only the top-level RunConfig.Provider entry is consulted in v1;
	// entries in RunConfig.Providers are streaming-only. See
	// BatchProviderConfig for the supported provider types and the
	// transport / mode cross-field invariants enforced by ValidateRunConfig.
	Batch *BatchProviderConfig `json:"batch,omitempty"`
}

// BatchProviderConfig controls async batch submission for a provider turn.
// Only Anthropic and OpenAI providers support batch in v1. Setting Enabled=true
// with an unsupported provider type is a validation error.
type BatchProviderConfig struct {
	// Enabled opts this run into async batch submission for every provider turn.
	Enabled bool `json:"enabled,omitempty"`

	// MaxWaitSeconds is the harness-side wall-clock cap on the batch wait,
	// in seconds. Defaults to 86400 (24 h, matching the provider SLA) when
	// nil and Enabled=true. Must be in the range (0, 86400].
	MaxWaitSeconds *int `json:"maxWaitSeconds,omitempty"`

	// HarnessSidePolling enables direct HTTP polling from the harness
	// process (required when transport.type == "stdio"). Mutually exclusive
	// with transport.type == "grpc".
	HarnessSidePolling bool `json:"harnessSidePolling,omitempty"`

	// FallbackOnTimeout switches to the streaming adapter for a turn when
	// the harness-side MaxWaitSeconds fires. Defaults to false.
	FallbackOnTimeout bool `json:"fallbackOnTimeout,omitempty"`

	// CancelBundleOnRunCancel causes a single run's cancel to cancel the
	// entire bundled provider batch (gRPC transport only). Defaults to false.
	CancelBundleOnRunCancel bool `json:"cancelBundleOnRunCancel,omitempty"`

	// AllowInteractiveModes permits batch.enabled with mode == "planning" or
	// mode == "review". Has no effect on mode == "execution" (always rejected).
	AllowInteractiveModes bool `json:"allowInteractiveModes,omitempty"`
}

// ProviderRetryConfig bounds the retry behaviour an adapter applies to a
// single provider call. Defaults are filled in at validation time so a
// nil pointer never reaches a consumer.
type ProviderRetryConfig struct {
	// MaxAttempts is the total number of HTTP attempts (including the
	// first). A value of 1 disables retry. Default: 3. Hard ceiling: 5.
	MaxAttempts int `json:"maxAttempts,omitempty"`

	// InitialDelayMs is the base delay for exponential backoff before
	// jitter, in milliseconds. Default: 500. A value of 0 (the JSON
	// omitempty zero) is treated as unset and inherits the default. To
	// request near-zero initial delay, use 1; zero cannot be expressed
	// as an explicit policy in the current wire contract. Defaulting
	// runs before cross-field validation, so when this field is unset
	// and maxDelayMs is pinned below 500, the resulting error message
	// annotates the 500 ms value as "(default)" to make the source of
	// the constraint clear.
	InitialDelayMs int `json:"initialDelayMs,omitempty"`

	// MaxDelayMs caps the per-attempt backoff and also caps any
	// server-supplied Retry-After hint (defence against pathological
	// values), in milliseconds. Default: 16000. Hard ceiling: 60000.
	MaxDelayMs int `json:"maxDelayMs,omitempty"`

	// WallClockBudgetMs bounds total time spent across all attempts
	// (including the first request), in milliseconds. Default: 90000.
	// Hard ceiling: 300000. Must be >= MaxDelayMs.
	WallClockBudgetMs int `json:"wallClockBudgetMs,omitempty"`
}

// GeminiSafetySetting overrides the threshold for one Gemini safety
// category. Used by the "gemini" provider only.
type GeminiSafetySetting struct {
	// Category is one of the five HARM_CATEGORY_* identifiers accepted
	// by Vertex AI:
	//   HARM_CATEGORY_HATE_SPEECH | HARM_CATEGORY_HARASSMENT |
	//   HARM_CATEGORY_DANGEROUS_CONTENT | HARM_CATEGORY_SEXUALLY_EXPLICIT |
	//   HARM_CATEGORY_CIVIC_INTEGRITY
	Category string `json:"category"`
	// Threshold is one of:
	//   BLOCK_NONE | BLOCK_LOW_AND_ABOVE | BLOCK_MEDIUM_AND_ABOVE | BLOCK_ONLY_HIGH
	Threshold string `json:"threshold"`
}

// CredentialConfig selects the credential acquisition method for a provider.
// When omitted from ProviderConfig, the credential type is inferred:
// bedrock uses "aws-default", gemini uses "gcp-default", all others use
// "static" (resolving APIKeyRef).
//
// Valid Type values:
//   - "static"                            — resolve APIKeyRef via SecretStore.
//   - "aws-default"                       — AWS SDK default credential chain.
//   - "web-identity"                      — OIDC -> STS AssumeRoleWithWebIdentity.
//   - "gcp-default"                       — Google Application Default
//     Credentials. Default for "gemini". Rejects user-mode gcloud creds.
//   - "gcp-service-account"               — explicit service account JSON key
//     file. Requires ProviderConfig.GCPCredentialsFile.
//   - "gcp-workload-identity"             — GKE Workload Identity (compute
//     metadata access token).
//   - "gcp-workload-identity-federation"  — non-GCP runtime → GCP via Workload
//     Identity Federation (STS + optional service-account impersonation).
//     Requires Audience and TokenSource.
//   - "anthropic-wif"                     — non-Anthropic runtime → Anthropic
//     API via Workload Identity Federation. Exchanges an OIDC JWT (from
//     TokenSource) at https://api.anthropic.com/v1/oauth/token for a
//     short-lived Anthropic access token. Requires FederationRuleID,
//     OrganizationID, ServiceAccountID, and TokenSource. Mutually exclusive
//     with the provider's APIKeyRef on the same provider entry.
//   - "azure-workload-identity"           — Azure Entra ID Workload Identity
//     Federation. Exchanges an OIDC JWT (from TokenSource) for an Entra
//     access token via the OAuth2 client_credentials + jwt-bearer grant
//     against login.microsoftonline.com. Requires AzureTenantID,
//     AzureClientID, and TokenSource. Bearer is sent as
//     "Authorization: Bearer" — APIKeyRef and APIKeyHeader="api-key" are
//     mutually exclusive with this type.
type CredentialConfig struct {
	Type           string             `json:"type"`
	TokenSource    *TokenSourceConfig `json:"tokenSource,omitempty"`    // required for "web-identity", "gcp-workload-identity-federation", "anthropic-wif", "azure-workload-identity"
	RoleARN        string             `json:"roleArn,omitempty"`        // required for "web-identity": IAM role to assume
	SessionName    string             `json:"sessionName,omitempty"`    // for "web-identity" (default: "stirrup")
	Audience       string             `json:"audience,omitempty"`       // required for "gcp-workload-identity-federation": WIF provider audience
	ServiceAccount string             `json:"serviceAccount,omitempty"` // optional for "gcp-workload-identity-federation": SA email to impersonate

	// FederationRuleID is required for "anthropic-wif". Format: "fdrl_...".
	// Per Anthropic's WIF reference docs this is a non-secret identifier
	// safe to commit to source control or bake into a container image.
	FederationRuleID string `json:"federationRuleId,omitempty"`

	// OrganizationID is required for "anthropic-wif". Format: lowercase
	// RFC 4122 UUID (e.g. "550e8400-e29b-41d4-a716-446655440000").
	// Identifies which Anthropic organization the federation rule belongs
	// to. Non-secret per Anthropic's docs.
	OrganizationID string `json:"organizationId,omitempty"`

	// ServiceAccountID is required for "anthropic-wif". Format: "svac_...".
	// The non-human principal the resulting access token acts as.
	// Non-secret per Anthropic's docs.
	ServiceAccountID string `json:"serviceAccountId,omitempty"`

	// WorkspaceID is conditional for "anthropic-wif". Either the literal
	// string "default" or a "wrkspc_..." identifier. Required when the
	// federation rule is enabled for more than one workspace; omit (empty
	// string) when the rule is bound to a single workspace.
	// Non-secret per Anthropic's docs.
	WorkspaceID string `json:"workspaceId,omitempty"`

	// AzureTenantID is required for "azure-workload-identity". UUID of the
	// Azure AD tenant that owns the App Registration / federated identity
	// credential. Lowercase canonical 8-4-4-4-12 form is required.
	AzureTenantID string `json:"azureTenantId,omitempty"`

	// AzureClientID is required for "azure-workload-identity". UUID of the
	// App Registration / federated identity credential client ID. Same
	// canonical form as AzureTenantID.
	AzureClientID string `json:"azureClientId,omitempty"`

	// AzureScope is optional for "azure-workload-identity". OAuth2 scope to
	// request; default is "https://cognitiveservices.azure.com/.default"
	// (Azure OpenAI / Foundry). Override for non-default Azure audiences
	// (custom AAD app registrations, sovereign clouds). Must be a valid
	// HTTPS URL when set.
	AzureScope string `json:"azureScope,omitempty"`

	// AzureTokenURL is optional for "azure-workload-identity". Overrides
	// the OAuth2 token endpoint URL. Default fills in
	// https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token (Azure
	// global cloud); set this for sovereign clouds — login.microsoftonline.us
	// (Azure Government), login.partner.microsoftonline.cn (Azure China),
	// or login.microsoftonline.de (Azure Germany, deprecated). Must be a
	// syntactically valid HTTPS URL when set.
	AzureTokenURL string `json:"azureTokenUrl,omitempty"`
}

// TokenSourceConfig selects where identity tokens are fetched from.
// Used by credential types that require an OIDC/JWT token for exchange.
type TokenSourceConfig struct {
	Type     string `json:"type"`               // "gke-metadata" | "file" | "env" | "aws-irsa" | "azure-imds" | "github-actions-oidc"
	Audience string `json:"audience,omitempty"` // for "gke-metadata", "github-actions-oidc": target audience claim (e.g. "sts.amazonaws.com")
	Path     string `json:"path,omitempty"`     // for "file": filesystem path to token
	EnvVar   string `json:"envVar,omitempty"`   // for "env": environment variable name
	Resource string `json:"resource,omitempty"` // for "azure-imds": Azure AD resource URI (e.g. "https://management.azure.com/")
	ClientID string `json:"clientId,omitempty"` // for "azure-imds": user-assigned managed identity client ID (optional)
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

	// WorkspaceExportTo, when set, instructs the harness to tarball the
	// executor's workspace at end-of-run and upload it to the named URI.
	// Currently only "gs://bucket/path" is accepted. The "api" executor
	// (read-only) and any run with an empty workspace skip the upload
	// silently. Future S3 / Azure Blob support will broaden the scheme
	// set.
	WorkspaceExportTo string `json:"workspaceExportTo,omitempty"`
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

// ToolDispatchConfig tunes the parallel async-tool dispatch loop. The
// loop fans out async tool calls (any AsyncHandler-backed tool) emitted
// within a single assistant turn under a semaphore so a multi-worker
// deep-research query does not serialise on the slowest worker.
// MaxParallel == 0 (or a nil ToolDispatch) resolves to
// DefaultToolDispatchMaxParallel via EffectiveToolDispatchMaxParallel;
// values outside [1, MaxToolDispatchMaxParallel] are rejected by
// ValidateRunConfig.
type ToolDispatchConfig struct {
	MaxParallel int `json:"maxParallel,omitempty"`
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
	Type            string `json:"type"`                      // "jsonl" | "otel" | "gcs"
	FilePath        string `json:"filePath,omitempty"`        // for jsonl
	Endpoint        string `json:"endpoint,omitempty"`        // for otel tracing (default: localhost:4317 for grpc; full URL for http/protobuf)
	MetricsEndpoint string `json:"metricsEndpoint,omitempty"` // for otel metrics (defaults to Endpoint if unset)

	// Bucket is the GCS bucket the "gcs" emitter writes the run's JSONL
	// trace to. Required when Type == "gcs"; rejected for every other
	// emitter type so a stale config does not silently keep a bucket
	// reference alive across a type change. Validated against the GCS
	// bucket naming rules (lowercase, no slashes, 3-63 chars).
	Bucket string `json:"bucket,omitempty"`

	// ObjectPrefix is joined with the run ID at write time to form the
	// final GCS object name (e.g. "traces/" + RunID + ".jsonl"). Empty
	// is allowed; trailing slash is treated as implicit. Only consulted
	// by the "gcs" emitter.
	ObjectPrefix string `json:"objectPrefix,omitempty"`

	// Credential, when set, overrides the default credential resolution
	// for the "gcs" emitter (which defaults to gcp-workload-identity
	// against the runtime's metadata server). The field has no meaning
	// for jsonl or otel and is rejected on those types. Secret-bearing
	// sub-fields are scrubbed by RunConfig.Redact() the same way as
	// Provider.Credential.
	Credential *CredentialConfig `json:"credential,omitempty"`

	// Protocol selects the OTLP wire protocol for the otel emitter.
	// Closed set: "" (defaults to "grpc"), "grpc", "http/protobuf".
	// HTTP/JSON is intentionally not supported; binary protobuf is the only
	// HTTP encoding accepted by Grafana Cloud and most managed OTLP gateways
	// and adding JSON has no clear demand. Ignored for the "jsonl" emitter.
	Protocol string `json:"protocol,omitempty"`

	// Headers are extra HTTP headers attached to every OTLP export
	// request. Keys are header names; values may be plaintext or a
	// "secret://" reference resolved via the SecretStore at exporter
	// init time (e.g. {"Authorization": "secret://GRAFANA_CLOUD_AUTH"}).
	// Resolved values flow through the same scrubbing layer as logs and
	// are rewritten to "secret://[REDACTED]" by RunConfig.Redact() before
	// any persisted trace or recording is written.
	//
	// Only applied when type=="otel". For protocol "grpc" the SDK sends
	// these as gRPC metadata; for "http/protobuf" they are attached as
	// HTTP request headers.
	Headers map[string]string `json:"headers,omitempty"`
}

// ResultSinkConfig selects the result sink implementation. The
// discriminator values are a closed set so AWS / Azure adapters can land
// without breaking changes. Only "none" and "stdout-json" are
// implemented in this issue; "gcp-pubsub" and "gcs" are reserved values
// that fail validation with a "not yet implemented" error until their
// adapters land. The "gcp-" prefix on the cloud-provider discriminator
// mirrors the credential type prefix ("gcp-workload-identity") and
// reserves sibling slots for "aws-sns" / "azure-eventgrid" without
// requiring a deprecation cycle on the bare "pubsub" name.
//
// The result sink carries the run's *answer* (a small RunResult JSON
// payload) — distinct from the trace emitter, which carries the run's
// *evidence* (full JSONL trace + spans). A run can configure both
// independently.
type ResultSinkConfig struct {
	// Type selects the adapter. Closed set:
	//   "none"        — disabled (the default when ResultSink is nil).
	//   "stdout-json" — write a single "STIRRUP_RESULT <json>" line to
	//                   stdout at end-of-run. Reads cleanly from Cloud
	//                   Logging / CloudWatch / journald.
	//   "gcp-pubsub"  — RESERVED. Will publish RunResult to a GCP
	//                   Pub/Sub topic; rejected with a "not yet
	//                   implemented" error today.
	//   "gcs"         — RESERVED. Will write RunResult as a JSON
	//                   object to GCS; rejected with a "not yet
	//                   implemented" error today.
	Type string `json:"type"`

	// Topic is the Pub/Sub topic name for the future "gcp-pubsub"
	// adapter. Parsed but currently unused — the validator rejects
	// gcp-pubsub with "not yet implemented" before this field is
	// consulted. Carrying topic on a non-"gcp-pubsub" type is rejected
	// to avoid silent drop when an operator copies a Pub/Sub block into
	// a different sink config.
	Topic string `json:"topic,omitempty"`

	// Attributes are extra message attributes attached to the future
	// "gcp-pubsub" adapter's published message. Values may carry
	// "secret://" references which RunConfig.Redact() rewrites to
	// "secret://[REDACTED]" before any trace or recording is
	// persisted. Parsed but currently unused — the validator rejects
	// gcp-pubsub with "not yet implemented" before this field is
	// consulted. Carrying attributes on a non-"gcp-pubsub" type is
	// rejected for the same reason as Topic.
	Attributes map[string]string `json:"attributes,omitempty"`
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
	"gemini":            true,
}

// validBatchProviderTypes is the closed set of provider types whose adapters
// implement the async batch submission path in v1. Anthropic via the
// /v1/messages/batches endpoint; OpenAI Chat Completions and Responses via
// /v1/batches. Bedrock and Gemini are out of scope for v1 (Bedrock batch is
// S3-mediated and reuses none of the streaming adapter shape; Vertex AI has
// no equivalent endpoint).
var validBatchProviderTypes = map[string]bool{
	"anthropic":         true,
	"openai-compatible": true,
	"openai-responses":  true,
}

// gcpProjectIDPattern matches the GCP project ID rules: starts with a
// lowercase letter, 6-30 chars, lowercase letters/digits/hyphen, ends in
// alphanumeric. The closed shape lets us reject obvious typos at config
// time rather than waiting for a 403 from the Vertex AI endpoint.
var gcpProjectIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

// gcpLocationPattern intentionally accepts "global" or a region-like
// string. The regional set evolves too quickly for a closed list (Vertex
// adds new regions multiple times per year); we just bound the shape so
// an obviously bad value (CRLF, path component) is rejected at boot.
var gcpLocationPattern = regexp.MustCompile(`^global$|^[a-z][a-z0-9-]{1,30}$`)

// geminiModelNamePattern bounds the character set of a Vertex AI model
// name so a value containing slashes or percent signs cannot rewrite
// the request URL. Standard Vertex names like "gemini-2.5-pro",
// "gemini-2.0-flash", "publishers/google/models/gemini-..." are not
// allowed here — the harness uses the short name and lets the URL
// builder add the publisher prefix. The 64-char cap matches the
// longest published model identifier with comfortable headroom.
var geminiModelNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// validGeminiSafetyCategories enumerates the five HARM_CATEGORY_*
// identifiers Vertex AI accepts for the safetySettings array. Closed
// set so a typo surfaces at config time rather than as a silent default
// from the API.
var validGeminiSafetyCategories = map[string]bool{
	"HARM_CATEGORY_HATE_SPEECH":       true,
	"HARM_CATEGORY_HARASSMENT":        true,
	"HARM_CATEGORY_DANGEROUS_CONTENT": true,
	"HARM_CATEGORY_SEXUALLY_EXPLICIT": true,
	"HARM_CATEGORY_CIVIC_INTEGRITY":   true,
}

// validGeminiSafetyThresholds enumerates the four threshold values
// Vertex AI accepts. BLOCK_NONE is the harness's adapter-default for
// every category; the other three exist so an operator can dial the
// classifier up for non-coding workloads.
var validGeminiSafetyThresholds = map[string]bool{
	"BLOCK_NONE":             true,
	"BLOCK_LOW_AND_ABOVE":    true,
	"BLOCK_MEDIUM_AND_ABOVE": true,
	"BLOCK_ONLY_HIGH":        true,
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

// observabilityLabelPattern bounds the character set of operator-supplied
// OTel resource labels (deployment.environment, service.namespace) so an
// errant value cannot inject path separators or wire-protocol delimiters
// into the resource attributes that ride on every span and metric. The
// 64-char cap matches what backends like Grafana truncate at in default
// label rendering, and the closed character set is the intersection of
// what OTLP, Prometheus, and most backend-side label encoders accept
// without further escaping.
var observabilityLabelPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// traceEmitterHeaderNamePattern restricts trace-emitter header keys to
// the same minimal set used elsewhere for HTTP header names. Including
// `_` accommodates underscore-variant headers (e.g. `x_honeycomb_team`)
// that some gateways accept; bracketing characters, whitespace, colon,
// and CRLF are intentionally excluded so a typo in a RunConfig file
// cannot smuggle a CRLF-injected secondary header into the OTel
// SDK's request builder. Mirrors the validation contract on
// `apiKeyHeaderPattern` above; see runconfig.go:1862-1887 for the
// OpenAI-side counterpart.
var traceEmitterHeaderNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

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
	"gcs":   true,
}

// runIDPattern bounds the characters allowed in RunConfig.RunID. RunID
// is a defence-in-depth value: it is interpolated into the GCS object
// name produced by the "gcs" trace emitter (and the future S3/Azure
// equivalents) and into Cloud Logging labels. urlPathEscape passes "/"
// through unchanged, so an unfiltered slash in RunID would silently
// alter the object path. The closed set [a-zA-Z0-9_-] with a leading
// alphanumeric and a 128-char ceiling is wide enough for the common
// IDs operators paste in (UUIDs, Cloud Run execution names, integer
// counters) and narrow enough to reject path traversal, control bytes,
// and CRLF.
var runIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_\-]{0,127}$`)

// gcsBucketNamePattern is a minimal shape check for a GCS bucket name.
// The full GCS bucket-name rules (no leading "goog" prefix, no
// "google" substring in obfuscated forms, dotted forms requiring DNS
// labels, etc.) are operator-facing and produce precise errors at
// `gcloud storage buckets create` time; validating shape here is
// purely so an obvious typo (slashes, uppercase, CRLF) fails at boot
// rather than as a 400 from the GCS REST API. Range 3-63 chars
// matches the documented bucket-name length limit for the simple
// (non-dotted) form.
var gcsBucketNamePattern = regexp.MustCompile(`^[a-z0-9._-]{3,63}$`)

// validTraceEmitterProtocols is the closed set of OTLP wire protocols
// accepted on TraceEmitterConfig.Protocol. Empty string is the unset
// form and defaults to "grpc" at exporter-construction time. "http/json"
// is intentionally excluded — Grafana Cloud and the managed APMs we
// target prefer binary protobuf, and adding the JSON variant would
// double the surface area of the exporter init path without an
// operator demand to justify it. See docs/observability-cloud.md.
var validTraceEmitterProtocols = map[string]bool{
	"":              true,
	"grpc":          true,
	"http/protobuf": true,
}

// validResultSinkTypes is the closed set of ResultSinkConfig.Type
// values. Only the entries in implementedResultSinkTypes are wired in
// this release; the remainder are reserved so AWS / Azure / gcp-pubsub
// adapters can ship without breaking the config wire schema. The
// cloud-provider entries carry an explicit "gcp-" prefix to match the
// credential discriminators ("gcp-workload-identity") and to reserve
// sibling slots for "aws-sns" and "azure-eventgrid" without a
// deprecation cycle.
var validResultSinkTypes = map[string]bool{
	"none":        true,
	"stdout-json": true,
	"gcp-pubsub":  true,
	"gcs":         true,
}

// implementedResultSinkTypes is the subset of validResultSinkTypes for
// which an adapter is wired today. ValidateRunConfig rejects every
// other entry in validResultSinkTypes with a "not yet implemented"
// error so an operator sees a clear message instead of a surprising
// nil-component crash at boot.
var implementedResultSinkTypes = map[string]bool{
	"none":        true,
	"stdout-json": true,
}

var validCredentialTypes = map[string]bool{
	"static":                           true,
	"aws-default":                      true,
	"web-identity":                     true,
	"gcp-default":                      true,
	"gcp-service-account":              true,
	"gcp-workload-identity":            true,
	"gcp-workload-identity-federation": true,
	"anthropic-wif":                    true,
	"azure-workload-identity":          true,
}

// GCPWIFAudiencePatternString bounds the shape of a Workload Identity
// Federation audience. The full identifier always takes the form
//
//	//iam.googleapis.com/projects/{N}/locations/global/workloadIdentityPools/{POOL}/providers/{PROVIDER}
//
// Validating the shape at config time gives operators a precise error
// message ("must match …") instead of a 400 from the STS exchange when
// the audience is ill-formed (typo, missing segment, wrong host).
//
// The pool/provider segments use Google's documented identifier rules
// (lowercase letter + lowercase letters/digits/hyphen, 4–32 chars,
// ending in alphanumeric). Project number is purely digits.
//
// Exported as a string (rather than a *regexp.Regexp) so the
// credential package can compile its own copy without taking a runtime
// dependency on this var. This is the single source of truth — the
// credential layer's federatedAudiencePattern is built from this
// constant so the two regexes cannot drift.
const GCPWIFAudiencePatternString = `^//iam\.googleapis\.com/projects/[0-9]+/locations/global/workloadIdentityPools/[a-z][a-z0-9-]{2,30}[a-z0-9]/providers/[a-z][a-z0-9-]{2,30}[a-z0-9]$`

var gcpWIFAudiencePattern = regexp.MustCompile(GCPWIFAudiencePatternString)

// gcpServiceAccountPattern bounds the shape of a GCP service-account
// email used for impersonation under Workload Identity Federation.
// The format is documented at
// https://cloud.google.com/iam/docs/service-account-overview#identifying-projects:
// the local part must start with a lowercase letter, be 6–30 chars
// total, end in a letter or digit, and the domain segment names a
// project ID (lowercase letter prefix, lowercase letters/digits/hyphen)
// followed by the fixed `.iam.gserviceaccount.com` suffix.
//
// Validating at config time gives operators a precise error rather
// than a 403/404 from IAM Credentials with up to 1024 bytes of error
// body wrapped in the bearer-resolution failure message — the
// federation source's truncateForError keeps the body bounded but a
// typo'd email is still better caught locally.
var gcpServiceAccountPattern = regexp.MustCompile(
	`^[a-z][a-z0-9-]{4,28}[a-z0-9]@[a-z][a-z0-9-]+\.iam\.gserviceaccount\.com$`,
)

// AnthropicFederationRuleIDPatternString bounds the shape of an Anthropic
// federation rule identifier. The Anthropic Console issues these as
// "fdrl_" + an opaque base62-style suffix; we accept any non-empty
// alphanumeric suffix to stay forward-compatible with future tweaks to
// Anthropic's identifier shape while still rejecting obvious typos
// (missing prefix, embedded whitespace, control characters) up front
// rather than letting them surface as a 400 from /v1/oauth/token.
//
// Exported as a string so the credential package can compile its own
// regex without a runtime dependency on this var (mirrors
// GCPWIFAudiencePatternString).
const AnthropicFederationRuleIDPatternString = `^fdrl_[A-Za-z0-9]+$`

// AnthropicServiceAccountIDPatternString bounds the shape of an
// Anthropic service-account identifier. Format is "svac_" plus an
// opaque alphanumeric suffix. See the doc on
// AnthropicFederationRuleIDPatternString for the rationale on
// validating shape at config time.
const AnthropicServiceAccountIDPatternString = `^svac_[A-Za-z0-9]+$`

// AnthropicWorkspaceIDPatternString bounds the shape of an Anthropic
// workspace identifier. Format is "wrkspc_" plus an opaque alphanumeric
// suffix. The literal string "default" is also accepted by Anthropic's
// /v1/oauth/token endpoint as a workspace selector but is handled
// outside this regex (in validateCredentialConfig) — the regex is the
// shape check for the structured form only.
const AnthropicWorkspaceIDPatternString = `^wrkspc_[A-Za-z0-9]+$`

// AnthropicOrganizationIDPatternString bounds the shape of an Anthropic
// organization identifier. The Console issues these as lowercase
// RFC 4122 UUIDs (e.g. "550e8400-e29b-41d4-a716-446655440000"); reject
// uppercase hex and surrounding whitespace at config time so a typo
// surfaces with a "must match …" error here rather than as an
// invalid_grant 400 from the token-exchange endpoint.
const AnthropicOrganizationIDPatternString = `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`

// azureUUIDPattern bounds Azure tenant and client IDs to the canonical
// 8-4-4-4-12 lowercase hex form. Microsoft documents both fields as
// UUIDs and the Azure portal renders them in lowercase; we deliberately
// reject upper-case and case-fold variants here so a typo (e.g. an "O"
// for a "0") does not pass validation only to fail later as an opaque
// 400 from login.microsoftonline.com. login.microsoftonline.com itself
// canonicalises the path, so accepting upper-case at the config layer
// would also create a UX trap where two superficially-different configs
// behave identically — better to enforce one form.
//
// Note: the regex is identical to AnthropicOrganizationIDPatternString,
// but the two are intentionally kept separate. The Anthropic constant
// is exported (consumed by the credential package's own pattern compile)
// while azureUUIDPattern is a private validator local to this package; they
// have different semantic provenances and may diverge if either vendor
// relaxes their canonical form.
var (
	anthropicFederationRuleIDPattern = regexp.MustCompile(AnthropicFederationRuleIDPatternString)
	anthropicServiceAccountIDPattern = regexp.MustCompile(AnthropicServiceAccountIDPatternString)
	anthropicWorkspaceIDPattern      = regexp.MustCompile(AnthropicWorkspaceIDPatternString)
	anthropicOrganizationIDPattern   = regexp.MustCompile(AnthropicOrganizationIDPatternString)
	azureUUIDPattern                 = regexp.MustCompile(
		`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`,
	)
)

var validTokenSourceTypes = map[string]bool{
	"gke-metadata":        true,
	"file":                true,
	"env":                 true,
	"aws-irsa":            true,
	"azure-imds":          true,
	"github-actions-oidc": true,
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

// validRunModes is the closed set of values accepted on RunConfig.Mode.
// "execution" is the default editable mode; the read-only modes are
// enumerated separately in readOnlyModes for tool-level enforcement.
// Any string outside this set would otherwise flow verbatim into the
// `parent.mode` / `run.mode` metric attribute, where an attacker-
// controlled value could blow up cardinality on the OTLP exporter.
var validRunModes = map[string]bool{
	"execution": true,
	"planning":  true,
	"review":    true,
	"research":  true,
	"toil":      true,
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
	"write_file":     true,
	"run_command":    true,
	"edit_file":      true,
	"search_replace": true,
	"apply_diff":     true,
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
// happen anyway). Also applies ProviderRetryConfig defaults to
// Provider.Retry and each entry in Providers so adapters never have
// to nil-check the per-call retry policy.
//
// Note: ValidateRunConfig mutates its argument in place to apply
// per-provider defaults (Provider.Retry fields, Provider.Batch.MaxWaitSeconds
// when Batch.Enabled=true, CodeScanner type). Callers that need an
// unmodified copy must clone before calling. Redact() deep-copies the
// affected pointer fields so a snapshot taken before validation does
// not alias the live config.
func ValidateRunConfig(config *RunConfig) error {
	applyCodeScannerDefault(config)
	retryDefaulted := applyProviderRetryDefaults(config)

	var errs []string

	validateSessionName(config.SessionName, &errs)
	// RunID is optional at this layer (the CLI / control plane assigns
	// one before construction), but when set it is interpolated verbatim
	// into the GCS object name produced by the gcs trace emitter. The
	// pattern rejects path separators, ".." segments, control bytes, and
	// CRLF so a hostile or typo'd RunID cannot rewrite the object path.
	if config.RunID != "" && !runIDPattern.MatchString(config.RunID) {
		errs = append(errs, fmt.Sprintf("runId %q must match %s", config.RunID, runIDPattern.String()))
	}
	validateRequiredType("mode", config.Mode, validRunModes, &errs)
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
	validateTraceEmitterProtocolAndHeaders(&config.TraceEmitter, &errs)
	validateResultSinkConfig(config.ResultSink, &errs)
	validateExecutorWorkspaceExportTo(config.Executor, &errs)
	validateVerifierConfig(config.Verifier, "verifier", &errs)
	validateProviderConfigs(config, retryDefaulted, &errs)
	validateAPIKeyRefs(config, &errs)
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
				"read-only mode %q requires an explicit tools.builtIn list that excludes write tools (write_file, run_command, edit_file, search_replace, apply_diff)",
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

	// temperature must lie inside the union of provider ranges. Negative
	// values are nonsensical (all providers reject them); >2 is outside
	// every provider's documented ceiling.
	if config.Temperature != nil {
		t := *config.Temperature
		if t < 0 {
			errs = append(errs, "temperature must be >= 0.0")
		}
		if t > maxTemperature {
			errs = append(errs, fmt.Sprintf("temperature must be <= %.1f", maxTemperature))
		}
	}

	validateRuleOfTwo(config, &errs)
	validateCodeScannerConfig(config.CodeScanner, &errs)
	validateGuardRailConfig(config.GuardRail, "guardRail", false, &errs)
	validateObservabilityConfig(config.Observability, &errs)
	validateToolDispatchConfig(config.ToolDispatch, &errs)
	validateBatchConfig(config, &errs)

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

	if !holdsUntrusted || !holdsSensitive || !canCommExternal {
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

// ruleOfTwoSensitiveData reports whether the run carries sensitive
// data inside the agent's reach.
//
// The semantic alignment: "sensitive data" in the Agents Rule of Two
// means data the agent itself can see — content inside its
// conversation context, files in its workspace, dynamic context
// supplied by the control plane. It deliberately does NOT mean
// host-level operational secrets (provider/VCS/MCP API keys), because
// the harness already keeps those out of the agent's reach: the
// run_command env-allowlist excludes API keys, the log scrubber
// redacts them, and SecretStore resolves them only at provider call
// time so they never enter the conversation. Treating those keys as
// "sensitive data" would make this leg trip on every working config
// and degrade the rule to "rule of one".
//
// Two operator-supplied signals trip the leg today:
//
//   - RunConfig.SensitiveData explicitly set to true. Operator
//     declares the run will work with sensitive data.
//   - Any DynamicContextValue with Sensitive == true. Operator marks
//     specific entries (customer record, private doc) as sensitive.
//
// The intent of having two signals is granularity: the SensitiveData
// flag covers cases where sensitivity comes from somewhere outside
// dynamic context (workspace, future MCP-resourced data, etc.); the
// per-entry flag covers the common case where the sensitivity rides
// the dynamic context block.
func ruleOfTwoSensitiveData(config *RunConfig) bool {
	if config.SensitiveData != nil && *config.SensitiveData {
		return true
	}
	for _, entry := range config.DynamicContext {
		if entry.Sensitive {
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

// providerRetryDefaulted records which ProviderRetryConfig fields were
// filled in by applyProviderRetryDefaults rather than supplied by the
// caller. Used by validateProviderRetryConfig to annotate cross-field
// error messages so an operator sees which value was inherited from the
// default and which they supplied. Without this distinction, a config
// that omits initialDelayMs while pinning maxDelayMs=100 produces the
// confusing error "initialDelayMs (500) must be <= maxDelayMs (100)"
// where 500 is a value the caller never wrote.
type providerRetryDefaulted struct {
	maxAttempts       bool
	initialDelayMs    bool
	maxDelayMs        bool
	wallClockBudgetMs bool
}

// applyProviderRetryDefaults populates ProviderConfig.Retry with the
// documented defaults so downstream consumers can dereference the
// pointer without a nil check. Operates on the top-level Provider and
// every entry in Providers. Each field is treated independently: a
// caller may pin one knob and inherit defaults for the rest.
//
// Returns a map keyed by the validation path (e.g. "provider.retry",
// "providers[secondary].retry") so validateProviderRetryConfig can
// annotate cross-field errors when a defaulted value appears next to a
// caller-supplied one.
func applyProviderRetryDefaults(config *RunConfig) map[string]providerRetryDefaulted {
	defaulted := map[string]providerRetryDefaulted{}
	defaulted["provider.retry"] = defaultProviderRetry(&config.Provider)
	if len(config.Providers) == 0 {
		return defaulted
	}
	for name, provider := range config.Providers {
		d := defaultProviderRetry(&provider)
		config.Providers[name] = provider
		defaulted[fmt.Sprintf("providers[%s].retry", name)] = d
	}
	return defaulted
}

func defaultProviderRetry(provider *ProviderConfig) providerRetryDefaulted {
	var d providerRetryDefaulted
	if provider.Retry == nil {
		provider.Retry = &ProviderRetryConfig{}
	}
	if provider.Retry.MaxAttempts == 0 {
		provider.Retry.MaxAttempts = defaultProviderRetryMaxAttempts
		d.maxAttempts = true
	}
	if provider.Retry.InitialDelayMs == 0 {
		provider.Retry.InitialDelayMs = defaultProviderRetryInitialDelayMs
		d.initialDelayMs = true
	}
	if provider.Retry.MaxDelayMs == 0 {
		provider.Retry.MaxDelayMs = defaultProviderRetryMaxDelayMs
		d.maxDelayMs = true
	}
	if provider.Retry.WallClockBudgetMs == 0 {
		provider.Retry.WallClockBudgetMs = defaultProviderRetryWallClockBudgetMs
		d.wallClockBudgetMs = true
	}
	return d
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

// validateToolDispatchConfig bounds ToolDispatch.MaxParallel to the
// hard ceiling MaxToolDispatchMaxParallel. A nil ToolDispatch is legal
// — the loop reads the effective value via
// EffectiveToolDispatchMaxParallel and falls back to
// DefaultToolDispatchMaxParallel. An explicit zero is also legal and
// resolves to the default, so unmarshalled wire payloads with an empty
// ToolDispatch sub-message survive validation.
func validateToolDispatchConfig(cfg *ToolDispatchConfig, errs *[]string) {
	if cfg == nil {
		return
	}
	if cfg.MaxParallel < 0 || cfg.MaxParallel > MaxToolDispatchMaxParallel {
		// The accepted-zero sentinel is called out in the message so an
		// operator who hits this validation error for, say, -1 does not
		// have to infer from "between 1 and 16" whether 0 is also
		// rejected. Zero IS legal and resolves to the library default.
		*errs = append(*errs, fmt.Sprintf("toolDispatch.maxParallel must be 0 (use default) or between 1 and %d", MaxToolDispatchMaxParallel))
	}
}

func validateVerifierConfig(cfg VerifierConfig, path string, errs *[]string) {
	validateOptionalType(path, cfg.Type, validVerifierTypes, errs)
	for i, sub := range cfg.Verifiers {
		validateVerifierConfig(sub, fmt.Sprintf("%s.verifiers[%d]", path, i), errs)
	}
}

// validateProviderRetryConfig enforces the hard ceilings and field
// relationships on a ProviderRetryConfig. All fields except
// InitialDelayMs are guaranteed non-zero after defaulting; the `< 0`
// check below handles the case where a caller bypasses the defaulter
// with a negative value. The `defaulted` record annotates cross-field
// error messages so a caller who omitted a field sees "(default)"
// rather than a value they never wrote.
func validateProviderRetryConfig(path string, cfg *ProviderRetryConfig, defaulted providerRetryDefaulted, errs *[]string) {
	if cfg == nil {
		return
	}
	if cfg.MaxAttempts < 1 || cfg.MaxAttempts > maxProviderRetryMaxAttempts {
		*errs = append(*errs, fmt.Sprintf(
			"%s.maxAttempts must be in [1, %d] (got %d)",
			path, maxProviderRetryMaxAttempts, cfg.MaxAttempts,
		))
	}
	if cfg.MaxDelayMs <= 0 || cfg.MaxDelayMs > maxProviderRetryMaxDelayMs {
		*errs = append(*errs, fmt.Sprintf(
			"%s.maxDelayMs must be in (0, %d] (got %d)",
			path, maxProviderRetryMaxDelayMs, cfg.MaxDelayMs,
		))
	}
	if cfg.InitialDelayMs < 0 {
		*errs = append(*errs, fmt.Sprintf(
			"%s.initialDelayMs must be >= 0 (got %d)",
			path, cfg.InitialDelayMs,
		))
	}
	if cfg.MaxDelayMs > 0 && cfg.InitialDelayMs > cfg.MaxDelayMs {
		*errs = append(*errs, fmt.Sprintf(
			"%s.initialDelayMs (%s) must be <= maxDelayMs (%s)",
			path,
			fmtProviderRetryValue(cfg.InitialDelayMs, defaulted.initialDelayMs),
			fmtProviderRetryValue(cfg.MaxDelayMs, defaulted.maxDelayMs),
		))
	}
	if cfg.WallClockBudgetMs <= 0 || cfg.WallClockBudgetMs > maxProviderRetryWallClockBudgetMs {
		*errs = append(*errs, fmt.Sprintf(
			"%s.wallClockBudgetMs must be in (0, %d] (got %d)",
			path, maxProviderRetryWallClockBudgetMs, cfg.WallClockBudgetMs,
		))
	}
	if cfg.MaxDelayMs > 0 && cfg.WallClockBudgetMs > 0 && cfg.WallClockBudgetMs < cfg.MaxDelayMs {
		*errs = append(*errs, fmt.Sprintf(
			"%s.wallClockBudgetMs (%s) must be >= maxDelayMs (%s)",
			path,
			fmtProviderRetryValue(cfg.WallClockBudgetMs, defaulted.wallClockBudgetMs),
			fmtProviderRetryValue(cfg.MaxDelayMs, defaulted.maxDelayMs),
		))
	}
}

// fmtProviderRetryValue renders a ProviderRetryConfig value for use in
// cross-field error messages, appending " (default)" when the value
// was filled in by applyProviderRetryDefaults rather than supplied by
// the caller.
func fmtProviderRetryValue(value int, isDefault bool) string {
	if isDefault {
		return fmt.Sprintf("%d, default", value)
	}
	return fmt.Sprintf("%d", value)
}

// validateBatchConfig enforces the cross-field invariants on
// ProviderConfig.Batch and applies the MaxWaitSeconds default. Batch
// only applies to the top-level Provider in v1; entries in
// Providers[] are streaming-only and validateProviderConfigs rejects
// any non-nil Batch on a map entry. The validator mutates *config when
// Batch.Enabled and MaxWaitSeconds is unset — downstream consumers
// should always see a populated value so the adapter wiring (phase 2)
// can avoid nil-checking on the hot path. The MaxWaitSeconds default
// is intentionally withheld when Enabled=false: phase-2 callers rely
// on nil to distinguish "operator did not configure this field" from
// "default applied", and the field is meaningless when batch is off.
func validateBatchConfig(config *RunConfig, errs *[]string) {
	batch := config.Provider.Batch
	if batch == nil {
		return
	}

	// HarnessSidePolling and CancelBundleOnRunCancel constrain the
	// transport regardless of Enabled, so a future-disabled config does
	// not silently retain a contradictory flag combination.
	if batch.HarnessSidePolling && config.Transport.Type == "grpc" {
		*errs = append(*errs, "batch.harnessSidePolling must not be set with transport=grpc")
	}
	if batch.CancelBundleOnRunCancel && config.Transport.Type == "stdio" {
		*errs = append(*errs, "batch.cancelBundleOnRunCancel requires transport=grpc")
	}

	if !batch.Enabled {
		return
	}

	// Skip the batch provider-type check when the underlying provider
	// type is itself invalid: validateRequiredType has already
	// appended a "provider type is required" / "unsupported provider
	// type" error, and the secondary message ("batch is not supported
	// for provider type \"\"") would just mislead the operator about
	// the root cause.
	if validProviderTypes[config.Provider.Type] && !validBatchProviderTypes[config.Provider.Type] {
		*errs = append(*errs, fmt.Sprintf("batch is not supported for provider type %q in v1", config.Provider.Type))
	}
	switch config.Mode {
	case "execution":
		*errs = append(*errs, "batch cannot be used with mode=execution")
	case "planning", "review":
		if !batch.AllowInteractiveModes {
			*errs = append(*errs, fmt.Sprintf("batch requires allowInteractiveModes=true for mode=%s", config.Mode))
		}
	}
	if config.Transport.Type == "stdio" && !batch.HarnessSidePolling {
		*errs = append(*errs, "batch with transport=stdio requires harnessSidePolling=true")
	}
	if batch.MaxWaitSeconds != nil {
		if *batch.MaxWaitSeconds <= 0 || *batch.MaxWaitSeconds > DefaultBatchMaxWaitSeconds {
			*errs = append(*errs, "batch.maxWaitSeconds must be in range (0, 86400]")
		}
	} else {
		// Default applied in-place so phase-2 adapter wiring can rely
		// on a populated value without re-reading the default. Only
		// runs on the Enabled=true branch (see comment above the func)
		// so a disabled batch block keeps MaxWaitSeconds nil and the
		// "operator did not configure" signal survives.
		def := DefaultBatchMaxWaitSeconds
		batch.MaxWaitSeconds = &def
	}
	if config.MaxTurns > batchTurnsLatencyWarnThreshold {
		// Layering note: this warn is in types/ for spec-faithfulness
		// (gh-134 requires the same mechanism as rule_of_two_warning).
		// Follow-up: move to harness/internal/core/factory.go via a
		// ValidateRunConfigResult or companion ValidateRunConfigWarnings
		// function so callers (tests, eval runner, library embedders)
		// stop having to manipulate slog.Default to observe or suppress
		// the warning.
		slog.Warn(
			"batch with maxTurns above the latency-warning threshold may incur extended wall-clock latency",
			"maxTurns", config.MaxTurns,
			"thresholdTurns", batchTurnsLatencyWarnThreshold,
			"estimatedMaxHours", config.MaxTurns*24,
		)
	}
}

func validateProviderConfigs(config *RunConfig, retryDefaulted map[string]providerRetryDefaulted, errs *[]string) {
	knownProviders := map[string]bool{}
	if config.Provider.Type != "" {
		knownProviders[config.Provider.Type] = true
	}
	validateOpenAIAuthFields("provider", config.Provider, errs)
	validateGeminiProviderFields("provider", config.Provider, errs)
	validateAnthropicProviderFields("provider", config.Provider, errs)
	validateAzureWIFCrossField("provider", config.Provider, errs)
	validateProviderRetryConfig("provider.retry", config.Provider.Retry, retryDefaulted["provider.retry"], errs)
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
		path := fmt.Sprintf("providers[%s]", name)
		validateRequiredType(path, provider.Type, validProviderTypes, errs)
		validateOpenAIAuthFields(path, provider, errs)
		validateGeminiProviderFields(path, provider, errs)
		validateAnthropicProviderFields(path, provider, errs)
		validateAzureWIFCrossField(path, provider, errs)
		retryPath := fmt.Sprintf("%s.retry", path)
		validateProviderRetryConfig(retryPath, provider.Retry, retryDefaulted[retryPath], errs)
		// Batch is a top-level-only concept in v1: the spec wires
		// per-turn batching against RunConfig.Provider, and a Batch
		// block on a named entry would silently parse, store, and have
		// no behavioural effect. Reject any non-nil Batch (not just
		// Enabled=true) so a partially-filled block ("operator added
		// the key but left enabled=false") also fails loudly — the
		// foot-gun is real and the strict error is cheap.
		if provider.Batch != nil {
			*errs = append(*errs, fmt.Sprintf(
				"providers[%s].batch is not supported in v1; batch applies only to the top-level provider",
				name,
			))
		}
	}

	// Per-provider model-name validation for Vertex AI Gemini. The
	// model substring is interpolated into the request URL path; the
	// adapter url.PathEscape's it as defence-in-depth, but reject
	// obvious abuse here so a typo or injection attempt surfaces at
	// boot rather than producing a request to an unintended Vertex
	// endpoint. Apply when the static / per-mode default provider
	// resolves to a gemini-typed entry, OR when the top-level
	// Provider.Type is "gemini" and ModelRouter.Provider is unset.
	validateGeminiModelName(config, errs)

	// Per-provider model-id validation for Bedrock. The Anthropic-API
	// alias shape ("claude-sonnet-4-6") is structurally invalid against
	// Bedrock but the failure only surfaces after IAM/SigV4 setup and a
	// network round-trip. Reject the shape here with an actionable
	// pointer to the inference-profile path so the operator never sees
	// the opaque ValidationException from AWS.
	validateBedrockModelID(config, errs)

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

// validateAPIKeyRefs enforces that every secret-bearing apiKeyRef on
// the config is either empty (the field is optional / driven by
// credential federation) or begins with the "secret://" scheme.
// Without this, an operator who pastes a literal API key into a
// suite's run_config gets a confusing runtime failure (the harness
// will try to resolve "sk-ant-..." through SecretStore and surface a
// generic "no such secret" error) instead of a clear validation
// message. Apply to every field redactProviderAPIKeyRefs and friends
// know to scrub — the set of secret-bearing fields and the set of
// fields the redactor handles must stay in lockstep, otherwise a new
// field that the redactor missed would also miss this check.
func validateAPIKeyRefs(config *RunConfig, errs *[]string) {
	check := func(path, ref string) {
		if ref == "" {
			return
		}
		if !strings.HasPrefix(ref, "secret://") {
			*errs = append(*errs, fmt.Sprintf("%s must be a secret reference (e.g. \"secret://NAME\"), got a literal value", path))
		}
	}
	check("provider.apiKeyRef", config.Provider.APIKeyRef)
	for name, prov := range config.Providers {
		check(fmt.Sprintf("providers[%s].apiKeyRef", name), prov.APIKeyRef)
	}
	if config.Executor.VcsBackend != nil {
		check("executor.vcsBackend.apiKeyRef", config.Executor.VcsBackend.APIKeyRef)
	}
	for i, server := range config.Tools.MCPServers {
		check(fmt.Sprintf("tools.mcpServers[%d].apiKeyRef", i), server.APIKeyRef)
	}
}

func validateCredentialConfig(cfg *CredentialConfig, path string, errs *[]string) {
	if cfg == nil {
		return
	}
	validateRequiredType(path, cfg.Type, validCredentialTypes, errs)

	switch cfg.Type {
	case "web-identity":
		if cfg.RoleARN == "" {
			*errs = append(*errs, fmt.Sprintf("%s: web-identity requires roleArn", path))
		}
		if cfg.TokenSource == nil {
			*errs = append(*errs, fmt.Sprintf("%s: web-identity requires tokenSource", path))
		} else {
			validateTokenSourceConfig(cfg.TokenSource, path+".tokenSource", errs)
		}
	case "gcp-workload-identity-federation":
		if cfg.Audience == "" {
			*errs = append(*errs, fmt.Sprintf("%s: gcp-workload-identity-federation requires audience", path))
		} else if !gcpWIFAudiencePattern.MatchString(cfg.Audience) {
			*errs = append(*errs, fmt.Sprintf(
				"%s.audience %q must match //iam.googleapis.com/projects/{N}/locations/global/workloadIdentityPools/{POOL}/providers/{PROVIDER}",
				path, cfg.Audience,
			))
		}
		if cfg.TokenSource == nil {
			*errs = append(*errs, fmt.Sprintf("%s: gcp-workload-identity-federation requires tokenSource", path))
		} else {
			validateTokenSourceConfig(cfg.TokenSource, path+".tokenSource", errs)
		}
		// ServiceAccount is optional (omitted = use the federated
		// identity directly). When set, validate the email format so
		// operators see a precise error here rather than a 403/404
		// from iamcredentials.googleapis.com at first use.
		if cfg.ServiceAccount != "" && !gcpServiceAccountPattern.MatchString(cfg.ServiceAccount) {
			*errs = append(*errs, fmt.Sprintf(
				"%s.serviceAccount %q is not a valid service account email (expected <name>@<project>.iam.gserviceaccount.com)",
				path, cfg.ServiceAccount,
			))
		}
	case "anthropic-wif":
		if cfg.FederationRuleID == "" {
			*errs = append(*errs, fmt.Sprintf("%s: anthropic-wif requires federationRuleId", path))
		} else if !anthropicFederationRuleIDPattern.MatchString(cfg.FederationRuleID) {
			*errs = append(*errs, fmt.Sprintf(
				"%s.federationRuleId %q must match %s (e.g. \"fdrl_abc123\")",
				path, cfg.FederationRuleID, AnthropicFederationRuleIDPatternString,
			))
		}
		if cfg.OrganizationID == "" {
			*errs = append(*errs, fmt.Sprintf("%s: anthropic-wif requires organizationId", path))
		} else if !anthropicOrganizationIDPattern.MatchString(cfg.OrganizationID) {
			*errs = append(*errs, fmt.Sprintf(
				"%s.organizationId %q must be a lowercase RFC 4122 UUID (e.g. \"550e8400-e29b-41d4-a716-446655440000\")",
				path, cfg.OrganizationID,
			))
		}
		if cfg.ServiceAccountID == "" {
			*errs = append(*errs, fmt.Sprintf("%s: anthropic-wif requires serviceAccountId", path))
		} else if !anthropicServiceAccountIDPattern.MatchString(cfg.ServiceAccountID) {
			*errs = append(*errs, fmt.Sprintf(
				"%s.serviceAccountId %q must match %s (e.g. \"svac_abc123\")",
				path, cfg.ServiceAccountID, AnthropicServiceAccountIDPatternString,
			))
		}
		// WorkspaceID is conditional. Empty is valid (rule bound to a
		// single workspace); when set, accept either the literal
		// "default" magic string or a structured "wrkspc_..." identifier.
		if cfg.WorkspaceID != "" && cfg.WorkspaceID != "default" && !anthropicWorkspaceIDPattern.MatchString(cfg.WorkspaceID) {
			*errs = append(*errs, fmt.Sprintf(
				"%s.workspaceId %q must be \"default\" or match %s (e.g. \"wrkspc_abc123\")",
				path, cfg.WorkspaceID, AnthropicWorkspaceIDPatternString,
			))
		}
		if cfg.TokenSource == nil {
			*errs = append(*errs, fmt.Sprintf("%s: anthropic-wif requires tokenSource", path))
		} else {
			validateTokenSourceConfig(cfg.TokenSource, path+".tokenSource", errs)
		}
		// Mutual-exclusion: anthropic-wif consumes the four federation
		// fields, not the AWS web-identity / GCP WIF fields. A stale
		// roleArn / audience / serviceAccount on an anthropic-wif config
		// is almost always a copy-paste error; surface it loudly rather
		// than silently ignoring the value.
		if cfg.RoleARN != "" {
			*errs = append(*errs, fmt.Sprintf("%s.roleArn is only valid for credential type %q", path, "web-identity"))
		}
		if cfg.SessionName != "" {
			*errs = append(*errs, fmt.Sprintf("%s.sessionName is only valid for credential type %q", path, "web-identity"))
		}
		if cfg.Audience != "" {
			*errs = append(*errs, fmt.Sprintf("%s.audience is only valid for credential type %q", path, "gcp-workload-identity-federation"))
		}
		if cfg.ServiceAccount != "" {
			*errs = append(*errs, fmt.Sprintf("%s.serviceAccount is only valid for credential type %q", path, "gcp-workload-identity-federation"))
		}
	case "azure-workload-identity":
		// AzureTenantID and AzureClientID are both required and must
		// match the canonical lowercase UUID form. Validate at config
		// time so an operator who pastes a malformed ID sees a precise
		// error here rather than a generic 400 from
		// login.microsoftonline.com on the first token exchange.
		if cfg.AzureTenantID == "" {
			*errs = append(*errs, fmt.Sprintf("%s: azure-workload-identity requires azureTenantId", path))
		} else if !azureUUIDPattern.MatchString(cfg.AzureTenantID) {
			*errs = append(*errs, fmt.Sprintf(
				"%s.azureTenantId %q is not a canonical lowercase UUID (expected 8-4-4-4-12 hex form)",
				path, cfg.AzureTenantID,
			))
		}
		if cfg.AzureClientID == "" {
			*errs = append(*errs, fmt.Sprintf("%s: azure-workload-identity requires azureClientId", path))
		} else if !azureUUIDPattern.MatchString(cfg.AzureClientID) {
			*errs = append(*errs, fmt.Sprintf(
				"%s.azureClientId %q is not a canonical lowercase UUID (expected 8-4-4-4-12 hex form)",
				path, cfg.AzureClientID,
			))
		}
		if cfg.TokenSource == nil {
			*errs = append(*errs, fmt.Sprintf("%s: azure-workload-identity requires tokenSource", path))
		} else {
			validateTokenSourceConfig(cfg.TokenSource, path+".tokenSource", errs)
		}
		// AzureScope is optional; the credential source applies the
		// Azure OpenAI default ("https://cognitiveservices.azure.com/.default")
		// when empty. When set, must be a syntactically valid HTTPS
		// URL — Azure rejects http and bare strings, so failing here
		// is strictly more useful than letting the token-exchange
		// surface the same error 60 minutes into a long run.
		if cfg.AzureScope != "" {
			u, err := url.Parse(cfg.AzureScope)
			if err != nil {
				*errs = append(*errs, fmt.Sprintf(
					"%s.azureScope %q is not a valid URL: %v",
					path, cfg.AzureScope, err,
				))
			} else if u.Scheme != "https" || u.Host == "" {
				*errs = append(*errs, fmt.Sprintf(
					"%s.azureScope %q must be an https:// URL with a host (Azure scopes are HTTPS-only)",
					path, cfg.AzureScope,
				))
			}
		}
		// AzureTokenURL is optional; the credential source fills in
		// the global-cloud authority
		// (https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token)
		// when empty. The override exists for sovereign clouds whose
		// authorities live at login.microsoftonline.us /
		// .partner.microsoftonline.cn / .microsoftonline.de. Same
		// HTTPS-only invariant as azureScope: a misconfigured http://
		// or schemeless override would surface as a token-exchange
		// failure rather than a validation error, which is a worse
		// debugging experience.
		if cfg.AzureTokenURL != "" {
			u, err := url.Parse(cfg.AzureTokenURL)
			if err != nil {
				*errs = append(*errs, fmt.Sprintf(
					"%s.azureTokenUrl %q is not a valid URL: %v",
					path, cfg.AzureTokenURL, err,
				))
			} else if u.Scheme != "https" || u.Host == "" {
				*errs = append(*errs, fmt.Sprintf(
					"%s.azureTokenUrl %q must be an https:// URL with a host (Entra authorities are HTTPS-only)",
					path, cfg.AzureTokenURL,
				))
			}
		}
		// Mutual-exclusion: azure-workload-identity consumes its
		// Azure fields, not AWS / GCP / Anthropic federation fields.
		// Surface stale copy-paste values loudly.
		if cfg.RoleARN != "" {
			*errs = append(*errs, fmt.Sprintf("%s.roleArn is only valid for credential type %q", path, "web-identity"))
		}
		if cfg.SessionName != "" {
			*errs = append(*errs, fmt.Sprintf("%s.sessionName is only valid for credential type %q", path, "web-identity"))
		}
		if cfg.Audience != "" {
			*errs = append(*errs, fmt.Sprintf("%s.audience is only valid for credential type %q", path, "gcp-workload-identity-federation"))
		}
		if cfg.ServiceAccount != "" {
			*errs = append(*errs, fmt.Sprintf("%s.serviceAccount is only valid for credential type %q", path, "gcp-workload-identity-federation"))
		}
	}

	// Reciprocal mutual-exclusion: the four anthropic-wif fields are
	// scoped to type="anthropic-wif". Setting any of them on another
	// credential type is a hard error so a stale value does not silently
	// linger across a credential-type change. Mirrors how the gemini
	// validator scopes GCPProject / GCPLocation / GCPCredentialsFile to
	// type="gemini" only.
	if cfg.Type != "anthropic-wif" {
		if cfg.FederationRuleID != "" {
			*errs = append(*errs, fmt.Sprintf("%s.federationRuleId is only valid for credential type %q", path, "anthropic-wif"))
		}
		if cfg.OrganizationID != "" {
			*errs = append(*errs, fmt.Sprintf("%s.organizationId is only valid for credential type %q", path, "anthropic-wif"))
		}
		if cfg.ServiceAccountID != "" {
			*errs = append(*errs, fmt.Sprintf("%s.serviceAccountId is only valid for credential type %q", path, "anthropic-wif"))
		}
		if cfg.WorkspaceID != "" {
			*errs = append(*errs, fmt.Sprintf("%s.workspaceId is only valid for credential type %q", path, "anthropic-wif"))
		}
	}

	// Reciprocal mutual-exclusion: the four azure-workload-identity
	// fields are scoped to type="azure-workload-identity". Same rationale
	// as the anthropic-wif block above.
	if cfg.Type != "azure-workload-identity" {
		if cfg.AzureTenantID != "" {
			*errs = append(*errs, fmt.Sprintf("%s.azureTenantId is only valid for credential type %q", path, "azure-workload-identity"))
		}
		if cfg.AzureClientID != "" {
			*errs = append(*errs, fmt.Sprintf("%s.azureClientId is only valid for credential type %q", path, "azure-workload-identity"))
		}
		if cfg.AzureScope != "" {
			*errs = append(*errs, fmt.Sprintf("%s.azureScope is only valid for credential type %q", path, "azure-workload-identity"))
		}
		if cfg.AzureTokenURL != "" {
			*errs = append(*errs, fmt.Sprintf("%s.azureTokenUrl is only valid for credential type %q", path, "azure-workload-identity"))
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
		} else if pathHasDotDotSegment(cfg.Path) || pathHasDotDotSegment(filepath.Clean(cfg.Path)) {
			// Reject ".." segments with the same logic
			// permissionPolicy.policyFile and provider.gcpCredentialsFile
			// already use. Without this an env var like
			// ANTHROPIC_IDENTITY_TOKEN_FILE=../../etc/passwd would pass
			// validation; the credential source fails closed at
			// Token()-time, but a config-load error is cleaner.
			*errs = append(*errs, fmt.Sprintf("%s.path must not contain \"..\" segments: %q", path, cfg.Path))
		}
	case "env":
		if cfg.EnvVar == "" {
			*errs = append(*errs, fmt.Sprintf("%s: env requires envVar", path))
		}
	case "aws-irsa":
		// No required fields. AWS_WEB_IDENTITY_TOKEN_FILE is read at
		// Token() time and validated against the running environment;
		// validating it at config-load time would prevent operators
		// from authoring a config that runs in an EKS environment but
		// is loaded in a CI step that has no IRSA mount.
	case "azure-imds":
		if cfg.Resource == "" {
			*errs = append(*errs, fmt.Sprintf("%s: azure-imds requires resource", path))
		}
	case "github-actions-oidc":
		if cfg.Audience == "" {
			*errs = append(*errs, fmt.Sprintf("%s: github-actions-oidc requires audience", path))
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

// validateAzureWIFCrossField enforces the auth-field invariants that
// only make sense once we know the credential type is
// "azure-workload-identity". Two combinations are rejected:
//
//   - APIKeyRef set: Azure WIF resolves the bearer dynamically via the
//     OAuth2 token-exchange flow; an APIKeyRef alongside would either
//     be silently ignored (confusing UX) or actively conflict with
//     the federated bearer (security: a stale static key may be picked
//     up if the federation flow ever falls back). Either way, mixing
//     the two is a configuration error.
//   - APIKeyHeader == "api-key": Entra ID access tokens are only
//     accepted on the Authorization: Bearer header; the "api-key"
//     header is reserved for static Azure OpenAI key auth and would
//     produce a 401 from the Azure resource. Catching this at config
//     load saves the operator the surprise of a 401 mid-run.
//
// Both errors are written with the field paths that surface in the
// JSON config so an operator can grep for them.
func validateAzureWIFCrossField(path string, cfg ProviderConfig, errs *[]string) {
	if cfg.Credential == nil || cfg.Credential.Type != "azure-workload-identity" {
		return
	}
	// Azure WIF only makes sense for the OpenAI-shaped adapters that
	// speak to Azure OpenAI / Foundry. The other provider types
	// (anthropic, bedrock, gemini) have their own auth contracts and
	// would silently ignore a Bearer produced by an Entra exchange,
	// so an operator pointing them at azure-workload-identity is
	// almost certainly a configuration mistake. Defence-in-depth: the
	// CLI shortcut path requires --provider=openai-* in practice, but
	// a hand-authored --config or a control-plane payload could still
	// reach validation with this combination.
	if cfg.Type != "openai-compatible" && cfg.Type != "openai-responses" {
		*errs = append(*errs, fmt.Sprintf(
			"%s: azure-workload-identity is only supported with openai-compatible or openai-responses provider types (got %q)",
			path, cfg.Type,
		))
	}
	if cfg.APIKeyRef != "" {
		*errs = append(*errs, fmt.Sprintf(
			"%s: azure-workload-identity does not use apiKeyRef; remove it (the bearer is fetched via OAuth2 token exchange)",
			path,
		))
	}
	if cfg.APIKeyHeader == "api-key" {
		*errs = append(*errs, fmt.Sprintf(
			"%s: azure-workload-identity requires Authorization: Bearer; apiKeyHeader=\"api-key\" is mutually exclusive (use empty apiKeyHeader for Bearer auth)",
			path,
		))
	}
}

// validateGeminiProviderFields enforces the cross-field constraints on
// the four Vertex AI Gemini fields (GCPProject, GCPLocation,
// GCPCredentialsFile, GeminiSafetySettings). Guard rails:
//
//   - The four fields are scoped to type="gemini". Setting any of
//     them on a non-gemini provider is a hard error so a stale value
//     does not silently linger across a provider-type change.
//   - "gemini" requires both GCPProject and GCPLocation; the URL the
//     adapter builds depends on each.
//   - "gemini" runs use OAuth2 Bearer tokens from
//     google.golang.org/x/oauth2/google — APIKeyRef has no meaning
//     here. Reject loudly with the redirect to provider.credential.
//   - GCPCredentialsFile pairs only with the gcp-service-account
//     credential type. Set with another type, the file is silently
//     ignored and the operator never knows the SA key did nothing;
//     unset with gcp-service-account, the credential source has
//     nothing to load. Both shapes are hard errors.
//   - GCPCredentialsFile may arrive over gRPC. Reject ".." segments
//     with the same logic permissionPolicy.policyFile uses (M6).
//   - Each GeminiSafetySetting must reference a category and threshold
//     from the closed Vertex AI set; values that pass through to the
//     API verbatim would otherwise produce confusing 400s.
func validateGeminiProviderFields(path string, cfg ProviderConfig, errs *[]string) {
	if cfg.Type != "gemini" {
		// Reject leakage of gemini-shaped fields onto non-gemini providers.
		if cfg.GCPProject != "" {
			*errs = append(*errs, fmt.Sprintf("%s.gcpProject is only valid for provider type %q", path, "gemini"))
		}
		if cfg.GCPLocation != "" {
			*errs = append(*errs, fmt.Sprintf("%s.gcpLocation is only valid for provider type %q", path, "gemini"))
		}
		if cfg.GCPCredentialsFile != "" {
			*errs = append(*errs, fmt.Sprintf("%s.gcpCredentialsFile is only valid for provider type %q", path, "gemini"))
		}
		if len(cfg.GeminiSafetySettings) > 0 {
			*errs = append(*errs, fmt.Sprintf("%s.geminiSafetySettings is only valid for provider type %q", path, "gemini"))
		}
		return
	}

	// Gemini-only constraints from here.
	if cfg.GCPProject == "" {
		*errs = append(*errs, fmt.Sprintf("%s.gcpProject is required for provider type %q", path, "gemini"))
	} else if !gcpProjectIDPattern.MatchString(cfg.GCPProject) {
		*errs = append(*errs, fmt.Sprintf("%s.gcpProject %q must match %s", path, cfg.GCPProject, gcpProjectIDPattern.String()))
	}

	if cfg.GCPLocation == "" {
		*errs = append(*errs, fmt.Sprintf("%s.gcpLocation is required for provider type %q", path, "gemini"))
	} else if !gcpLocationPattern.MatchString(cfg.GCPLocation) {
		*errs = append(*errs, fmt.Sprintf("%s.gcpLocation %q must match %s", path, cfg.GCPLocation, gcpLocationPattern.String()))
	}

	// Vertex AI uses GCP IAM, not API keys. APIKeyRef on a gemini
	// provider is almost always a copy-paste from another provider
	// config — surface it loudly with a redirect to the right field.
	if cfg.APIKeyRef != "" {
		*errs = append(*errs, fmt.Sprintf(
			"%s.apiKeyRef must not be set for provider type %q; Vertex AI uses GCP IAM (configure provider.credential instead)",
			path, "gemini"))
	}

	// Pair gcpCredentialsFile with gcp-service-account in both directions.
	credType := ""
	if cfg.Credential != nil {
		credType = cfg.Credential.Type
	}
	if credType == "gcp-service-account" && cfg.GCPCredentialsFile == "" {
		*errs = append(*errs, fmt.Sprintf(
			"%s.gcpCredentialsFile is required when credential.type is %q",
			path, "gcp-service-account"))
	}
	if cfg.GCPCredentialsFile != "" && credType != "" && credType != "gcp-service-account" {
		*errs = append(*errs, fmt.Sprintf(
			"%s.gcpCredentialsFile is only valid when credential.type is %q (got %q)",
			path, "gcp-service-account", credType))
	}
	if cfg.GCPCredentialsFile != "" {
		// Mirror the policyFile traversal check: a malicious control plane
		// could otherwise craft a path that escapes the workspace and
		// surface the file's contents as part of an oauth2 parser error.
		if pathHasDotDotSegment(cfg.GCPCredentialsFile) || pathHasDotDotSegment(filepath.Clean(cfg.GCPCredentialsFile)) {
			*errs = append(*errs, fmt.Sprintf("%s.gcpCredentialsFile must not contain \"..\" path segments", path))
		}
	}

	for i, s := range cfg.GeminiSafetySettings {
		entryPath := fmt.Sprintf("%s.geminiSafetySettings[%d]", path, i)
		if s.Category == "" {
			*errs = append(*errs, fmt.Sprintf("%s.category is required", entryPath))
		} else if !validGeminiSafetyCategories[s.Category] {
			*errs = append(*errs, fmt.Sprintf("%s.category %q is not a valid HARM_CATEGORY_*", entryPath, s.Category))
		}
		if s.Threshold == "" {
			*errs = append(*errs, fmt.Sprintf("%s.threshold is required", entryPath))
		} else if !validGeminiSafetyThresholds[s.Threshold] {
			*errs = append(*errs, fmt.Sprintf("%s.threshold %q is not a valid BLOCK_* value", entryPath, s.Threshold))
		}
	}
}

// validateAnthropicProviderFields enforces two cross-field invariants
// related to the anthropic-wif credential type:
//
//  1. An "anthropic" provider using credential.type="anthropic-wif"
//     must NOT also carry an apiKeyRef. The Anthropic SDK precedence
//     chain puts ANTHROPIC_API_KEY above federation, which means a
//     leftover key silently shadows WIF and the operator never knows
//     the federated path went unused. Per issue #117 (Risk #4),
//     stirrup fails closed at validation time rather than replicating
//     that surprise.
//
//  2. credential.type="anthropic-wif" must only pair with
//     provider.type="anthropic". An operator who passes
//     `--anthropic-federation-rule-id ... --provider openai-compatible`
//     would otherwise exchange a WIF token (sk-ant-oat01-...) and
//     hand it to a third-party endpoint. The third party rejects the
//     token (fail-closed at runtime), but a validation error is
//     cleaner — and it prevents the very narrow case of a malicious
//     base-url being asked to exfiltrate the access token.
//
// Beyond those two cases this function is a no-op (Bedrock keeps the
// key as a no-op, Gemini already rejects apiKeyRef in
// validateGeminiProviderFields, and the OpenAI adapters legitimately
// use apiKeyRef alongside any of their supported credential types).
func validateAnthropicProviderFields(path string, cfg ProviderConfig, errs *[]string) {
	// Cross-provider check applies regardless of cfg.Type; an
	// anthropic-wif credential paired with a non-anthropic provider
	// is structurally a misconfiguration.
	if cfg.Credential != nil && cfg.Credential.Type == "anthropic-wif" && cfg.Type != "anthropic" {
		*errs = append(*errs, fmt.Sprintf(
			"%s.credential.type=%q is only valid when provider.type=%q; got provider.type=%q "+
				"(an Anthropic WIF access token must not be sent to a non-Anthropic endpoint)",
			path, "anthropic-wif", "anthropic", cfg.Type))
		return
	}

	if cfg.Type != "anthropic" {
		return
	}
	if cfg.Credential == nil || cfg.Credential.Type != "anthropic-wif" {
		return
	}
	if cfg.APIKeyRef != "" {
		*errs = append(*errs, fmt.Sprintf(
			"%s.apiKeyRef must not be set when credential.type is %q; "+
				"Anthropic WIF authenticates via OAuth bearer tokens, and a static "+
				"API key would silently shadow the federated credential",
			path, "anthropic-wif"))
	}
}

// validateGeminiModelName rejects Vertex AI model identifiers that
// contain reserved URL bytes (slashes, percent signs, etc.). The
// adapter url.PathEscape's the model name into the request URL path,
// but operators frequently copy-paste model identifiers between
// providers and we want a typo like "publishers/google/models/foo" or
// a deliberate traversal like "gemini-pro/../../evil" to fail at
// boot, not as a confusing 404 from an unintended Vertex endpoint.
//
// We apply the check whenever a gemini-typed provider would actually
// service the configured ModelRouter. Three cases trigger the check:
//
//   - ModelRouter.Provider explicitly names a gemini-typed entry
//     (default Provider OR a Providers[name] entry).
//   - ModelRouter.Provider is empty and the top-level Provider.Type is
//     "gemini" (the implicit default).
//   - ModelRouter.ModeModels[*] contains "gemini-named-entry/<model>"
//     overrides — each is checked individually.
//
// We do not check CheapModel / ExpensiveModel here because the dynamic
// router never targets gemini today; revisit when that lands.
func validateGeminiModelName(config *RunConfig, errs *[]string) {
	// providerIsGemini reports whether a ModelRouter.Provider name
	// (possibly empty) ends up resolving to a gemini-typed provider.
	providerIsGemini := func(name string) bool {
		// Two shapes: name == "" and we fall back to top-level Provider;
		// or name matches a Providers[k] entry whose Type is gemini.
		// Note that names in validProviderTypes are also accepted as
		// type aliases, so name == "gemini" is gemini regardless of
		// whether the operator declared it under Providers.
		if name == "" {
			return config.Provider.Type == "gemini"
		}
		if name == "gemini" {
			return true
		}
		if p, ok := config.Providers[name]; ok {
			return p.Type == "gemini"
		}
		return false
	}

	checkModel := func(label, model string) {
		if model == "" {
			return
		}
		if !geminiModelNamePattern.MatchString(model) {
			*errs = append(*errs, fmt.Sprintf(
				"%s %q must match %s for provider type %q (no slashes, percent signs, or special characters)",
				label, model, geminiModelNamePattern.String(), "gemini"))
		}
	}

	if providerIsGemini(config.ModelRouter.Provider) {
		checkModel("modelRouter.model", config.ModelRouter.Model)
	}

	for mode, spec := range config.ModelRouter.ModeModels {
		// Per-mode entries take the form "provider/model". When the
		// provider half resolves to gemini we check the model half.
		providerName, model, ok := strings.Cut(spec, "/")
		if !ok {
			// No provider prefix — the entry inherits ModelRouter.Provider.
			if providerIsGemini(config.ModelRouter.Provider) {
				checkModel(fmt.Sprintf("modelRouter.modeModels[%s]", mode), spec)
			}
			continue
		}
		if providerIsGemini(providerName) {
			checkModel(fmt.Sprintf("modelRouter.modeModels[%s]", mode), model)
		}
	}
}

// validateBedrockModelID rejects model identifiers that cannot be the
// thing a Bedrock endpoint expects. Bedrock model ids fall into two
// shapes: a dotted form like "anthropic.<model>" or
// "<region>.anthropic.<model>" (inference profile), or a full ARN. The
// CLI's flag-only default is "claude-sonnet-4-6" — the Anthropic API
// alias — which Bedrock rejects with an opaque ValidationException only
// after IAM/SigV4 setup and a network round-trip (see #65). We have
// enough information at config-load time to fail closed with a message
// that names the inference-profile shape and points the operator at
// `aws bedrock list-inference-profiles`.
//
// The check trips whenever the resolved router provider is bedrock,
// mirroring the resolution rules in validateGeminiModelName: explicit
// Providers[name] entries, the "bedrock" type alias, ModeModels
// "provider/model" overrides, and the implicit-default case where
// ModelRouter.Provider is empty and Provider.Type == "bedrock".
//
// The shape test is deliberately permissive in the "looks valid"
// direction: any identifier containing "." or starting with "arn:" is
// accepted. We do not enumerate the set of valid Bedrock model ids
// here — AWS adds new model and inference-profile ids continuously,
// and a closed list would become a maintenance burden that produces
// false negatives. The only thing we want to catch is the obvious
// Anthropic-API alias shape (no separator, no ARN prefix) that has
// already cost operators a wall-clock-second on-ramp failure.
func validateBedrockModelID(config *RunConfig, errs *[]string) {
	providerIsBedrock := func(name string) bool {
		if name == "" {
			return config.Provider.Type == "bedrock"
		}
		if name == "bedrock" {
			return true
		}
		if p, ok := config.Providers[name]; ok {
			return p.Type == "bedrock"
		}
		return false
	}

	checkModel := func(label, model string) {
		// Empty string is handled by the router's own required-field
		// validation (or simply not reaching a provider call for a
		// static router that never sees a turn); skip it here so the
		// operator gets the focused "required" error rather than a
		// shape complaint about a value they did not set. Whitespace-
		// only strings are not "empty" in that sense and trip the
		// check below — they are never a valid Bedrock id and the
		// operator almost certainly meant to type something else.
		if model == "" {
			return
		}
		if strings.TrimSpace(model) != "" && (strings.Contains(model, ".") || strings.HasPrefix(model, "arn:")) {
			return
		}
		*errs = append(*errs, fmt.Sprintf(
			"%s %q is invalid for provider type %q; Bedrock requires either a model id like "+
				`"anthropic.<model>" or an inference profile id like "<region>.anthropic.<model>" `+
				`(e.g. "eu.anthropic.claude-sonnet-4-6"). `+
				`Use "aws bedrock list-inference-profiles" to enumerate profiles available in your region.`,
			label, model, "bedrock"))
	}

	if providerIsBedrock(config.ModelRouter.Provider) {
		checkModel("modelRouter.model", config.ModelRouter.Model)
	}

	for mode, spec := range config.ModelRouter.ModeModels {
		providerName, model, ok := strings.Cut(spec, "/")
		if !ok {
			if providerIsBedrock(config.ModelRouter.Provider) {
				checkModel(fmt.Sprintf("modelRouter.modeModels[%s]", mode), spec)
			}
			continue
		}
		if providerIsBedrock(providerName) {
			checkModel(fmt.Sprintf("modelRouter.modeModels[%s]", mode), model)
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

// validateObservabilityConfig rejects operator-supplied OTel resource
// labels that would not survive round-tripping through the wire format
// (CRLF, path separators, embedded quotes, etc.). Empty values are
// permitted — they fall through to env-var fallbacks at resource-
// construction time. The shape check is deliberately pragmatic: we don't
// constrain values to a list because operators legitimately want labels
// like "stirrup-eval", "production-eu", "shadow-canary".
func validateObservabilityConfig(cfg ObservabilityConfig, errs *[]string) {
	if cfg.Environment != "" && !observabilityLabelPattern.MatchString(cfg.Environment) {
		*errs = append(*errs, fmt.Sprintf("observability.environment %q must match %s", cfg.Environment, observabilityLabelPattern))
	}
	if cfg.ServiceNamespace != "" && !observabilityLabelPattern.MatchString(cfg.ServiceNamespace) {
		*errs = append(*errs, fmt.Sprintf("observability.serviceNamespace %q must match %s", cfg.ServiceNamespace, observabilityLabelPattern))
	}
}

// validateTraceEmitterProtocolAndHeaders enforces the closed set of OTLP
// wire protocols accepted on TraceEmitterConfig.Protocol and rejects
// non-otel emitters that carry stale protocol/header fields. Catching a
// typo'd "http" or "grpcs" here gives the operator a precise error at
// boot rather than a "no exporter could be created" log line at the
// first export.
//
// Protocol and Headers only have meaning for type=="otel"; rejecting them
// on the jsonl emitter makes the wire schema unambiguous and prevents a
// future migration from silently keeping a stale config working in a
// way that hides the operator's intent.
//
// gcs-specific fields (Bucket, ObjectPrefix, Credential) are validated
// here too: Bucket is required when Type=="gcs" and rejected on every
// other type, ObjectPrefix is optional, Credential is gcs-only.
//
// cfg is passed by pointer so the validator can normalise ObjectPrefix
// in place (trailing slash). gcsObjectName in harness/internal/trace/gcs.go
// concatenates prefix and run-id without inserting a separator, so a
// missing trailing slash would silently produce a malformed object name.
func validateTraceEmitterProtocolAndHeaders(cfg *TraceEmitterConfig, errs *[]string) {
	if !validTraceEmitterProtocols[cfg.Protocol] {
		*errs = append(*errs, fmt.Sprintf(
			"unsupported traceEmitter.protocol %q (allowed: \"\", grpc, http/protobuf)",
			cfg.Protocol,
		))
	}
	// Protocol/Headers are otel-only. The jsonl and gcs emitters do
	// not negotiate a wire protocol or send HTTP headers; carrying
	// these fields on a non-otel run is almost certainly a leftover
	// from a migration and should fail loudly.
	if cfg.Type != "otel" && cfg.Type != "" {
		if cfg.Protocol != "" {
			*errs = append(*errs, fmt.Sprintf(
				"traceEmitter.protocol is only valid when traceEmitter.type is \"otel\" (got type %q)",
				cfg.Type,
			))
		}
		if len(cfg.Headers) > 0 {
			*errs = append(*errs, fmt.Sprintf(
				"traceEmitter.headers is only valid when traceEmitter.type is \"otel\" (got type %q)",
				cfg.Type,
			))
		}
	}
	// gcs-specific field validation. Bucket is required for gcs and
	// rejected on every other type so a stale value cannot silently
	// keep a bucket reference alive across a type change.
	switch cfg.Type {
	case "gcs":
		if cfg.Bucket == "" {
			*errs = append(*errs, "traceEmitter type \"gcs\" requires bucket")
		} else if !gcsBucketNamePattern.MatchString(cfg.Bucket) {
			*errs = append(*errs, fmt.Sprintf(
				"traceEmitter.bucket %q must match %s (lowercase letters/digits/._-, 3-63 chars; no slashes)",
				cfg.Bucket, gcsBucketNamePattern.String(),
			))
		}
		// Reject ".." path segments in objectPrefix. urlPathEscape
		// intentionally passes "/" and "." through unchanged so a prefix
		// like "../../prod-traces/" would otherwise produce an object
		// name that GCS stores verbatim under a different logical prefix
		// — a quiet collision risk if a single bucket holds traces from
		// multiple runs. The check runs before the trailing-slash
		// normalisation below so a stray ".." in any segment is caught.
		if cfg.ObjectPrefix != "" {
			for _, seg := range strings.Split(strings.Trim(cfg.ObjectPrefix, "/"), "/") {
				if seg == ".." {
					*errs = append(*errs, `traceEmitter.objectPrefix must not contain ".." path segments`)
					break
				}
			}
		}
		// Normalise the prefix to ensure a trailing slash.
		// gcsObjectName in harness/internal/trace/gcs.go relies on this:
		// it concatenates prefix and run-id directly, so a prefix
		// supplied as "traces" instead of "traces/" would produce
		// "tracesRUNID.jsonl" — silently malformed. Normalising here is
		// more ergonomic than rejecting; operators rarely think of the
		// slash as load-bearing.
		if cfg.ObjectPrefix != "" && !strings.HasSuffix(cfg.ObjectPrefix, "/") {
			cfg.ObjectPrefix += "/"
		}
		if cfg.Credential != nil {
			validateCredentialConfig(cfg.Credential, "traceEmitter.credential", errs)
		}
	default:
		if cfg.Bucket != "" {
			*errs = append(*errs, fmt.Sprintf(
				"traceEmitter.bucket is only valid when traceEmitter.type is \"gcs\" (got type %q)",
				cfg.Type,
			))
		}
		if cfg.ObjectPrefix != "" {
			*errs = append(*errs, fmt.Sprintf(
				"traceEmitter.objectPrefix is only valid when traceEmitter.type is \"gcs\" (got type %q)",
				cfg.Type,
			))
		}
		if cfg.Credential != nil {
			*errs = append(*errs, fmt.Sprintf(
				"traceEmitter.credential is only valid when traceEmitter.type is \"gcs\" (got type %q)",
				cfg.Type,
			))
		}
	}
	// Reject `headers` on the gRPC transport. The gRPC exporter path
	// in harness/internal/trace/otel.go and observability/metrics.go
	// unconditionally calls WithInsecure(), so any bearer/Basic
	// credential supplied via headers would be transmitted in
	// plaintext. The native HTTP path is the only protocol that
	// accepts headers in this PR; full gRPC TLS support is a deferred
	// follow-up (see synthesis DF-4). Empty Protocol defaults to gRPC
	// at exporter construction time, so the check applies there too.
	if (cfg.Protocol == "" || cfg.Protocol == "grpc") && len(cfg.Headers) > 0 {
		*errs = append(*errs, "traceEmitter.headers requires protocol=http/protobuf; gRPC transport uses WithInsecure() and would send credentials in plaintext")
	}
	// Header name and value validation. Block CRLF injection at
	// config-load time rather than letting a "Bearer foo\r\nX-Inj: e"
	// value reach the net/http header builder, which panics on CRLF
	// in Go 1.26. This mirrors the OpenAI-side validation at
	// validateOpenAIAuthFields (runconfig.go:1862-1887) so the two
	// auth-header surfaces share the same hardening.
	for k, v := range cfg.Headers {
		if !traceEmitterHeaderNamePattern.MatchString(k) {
			*errs = append(*errs, fmt.Sprintf(
				"traceEmitter.headers key %q must contain only alphanumeric, hyphen, or underscore characters (no CRLF, colon, or whitespace)",
				k,
			))
		}
		if strings.ContainsAny(v, "\r\n") {
			*errs = append(*errs, fmt.Sprintf(
				"traceEmitter.headers value for key %q must not contain CRLF",
				k,
			))
		}
	}
}

// validateResultSinkConfig enforces the closed set of resultSink types
// and rejects reserved-but-not-implemented variants with a clear
// "not yet implemented" message so an operator sees actionable text
// instead of a nil-component crash at boot.
//
// A nil ResultSink is equivalent to type=="none" (sink disabled).
func validateResultSinkConfig(cfg *ResultSinkConfig, errs *[]string) {
	if cfg == nil {
		return
	}
	if cfg.Type == "" {
		*errs = append(*errs, "resultSink type is required")
		return
	}
	if !validResultSinkTypes[cfg.Type] {
		*errs = append(*errs, fmt.Sprintf("unsupported resultSink type %q", cfg.Type))
		return
	}
	if !implementedResultSinkTypes[cfg.Type] {
		*errs = append(*errs, fmt.Sprintf(
			"resultSink type %q is reserved but not yet implemented in this release",
			cfg.Type,
		))
	}
	// Topic and Attributes belong only to the gcp-pubsub adapter.
	// Rejecting them on every other type mirrors the
	// bucket/objectPrefix/credential rejection on TraceEmitterConfig
	// and prevents the silent "where did my topic go" confusion when
	// an operator copies a Pub/Sub block into a stdout-json config.
	if cfg.Topic != "" && cfg.Type != "gcp-pubsub" {
		*errs = append(*errs, fmt.Sprintf(
			"resultSink.topic is only valid when type is \"gcp-pubsub\" (got type %q)",
			cfg.Type,
		))
	}
	if len(cfg.Attributes) > 0 && cfg.Type != "gcp-pubsub" {
		*errs = append(*errs, fmt.Sprintf(
			"resultSink.attributes is only valid when type is \"gcp-pubsub\" (got type %q)",
			cfg.Type,
		))
	}
}

// validateExecutorWorkspaceExportTo enforces the URI shape on the
// optional Executor.WorkspaceExportTo field and the cross-field
// constraint that the api executor (read-only, no workspace) cannot
// produce a workspace tarball.
func validateExecutorWorkspaceExportTo(cfg ExecutorConfig, errs *[]string) {
	if cfg.WorkspaceExportTo == "" {
		return
	}
	// The api executor has no workspace to export. Rejecting here
	// catches a config-typo before the run no-ops at end-of-run with
	// a silent skip.
	if cfg.Type == "api" {
		*errs = append(*errs, "executor.workspaceExportTo is not valid for executor.type=\"api\" (api executor has no workspace)")
		return
	}
	if cfg.Type == "" {
		*errs = append(*errs, "executor.workspaceExportTo requires an explicit executor.type other than 'api'")
		return
	}
	// Only gs:// is accepted today. Future S3 / Azure Blob support
	// will broaden the scheme set; for now keep the surface narrow so
	// a typo (http://, gs:/, gcs://) fails at config load instead of
	// during the post-run upload.
	if !strings.HasPrefix(cfg.WorkspaceExportTo, "gs://") {
		*errs = append(*errs, fmt.Sprintf(
			"executor.workspaceExportTo %q must use the gs:// scheme",
			cfg.WorkspaceExportTo,
		))
		return
	}
	// "gs://" alone, or "gs:///" (empty bucket), are operator errors.
	// Strip the scheme and require a non-empty bucket-path component.
	rest := strings.TrimPrefix(cfg.WorkspaceExportTo, "gs://")
	if rest == "" || strings.HasPrefix(rest, "/") {
		*errs = append(*errs, fmt.Sprintf(
			"executor.workspaceExportTo %q must contain a non-empty bucket path after gs://",
			cfg.WorkspaceExportTo,
		))
	}
}
