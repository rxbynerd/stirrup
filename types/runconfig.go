package types

import (
	"fmt"
	"strings"
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
)

// RunConfig fully describes a single harness run. It is the composition root:
// the control plane sends it (via TaskAssignment in the gRPC contract) and
// the CLI builds it from flags/env.
type RunConfig struct {
	// Identity
	RunID string `json:"runId"`
	Mode  string `json:"mode"` // "execution" | "planning" | "review" | "research" | "toil"

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
	Type       string            `json:"type"`                 // "anthropic" | "bedrock" | "openai-compatible"
	APIKeyRef  string            `json:"apiKeyRef,omitempty"`  // e.g. "secret://anthropic-key"
	Region     string            `json:"region,omitempty"`     // bedrock
	Profile    string            `json:"profile,omitempty"`    // bedrock
	BaseURL    string            `json:"baseUrl,omitempty"`    // openai-compatible
	Credential *CredentialConfig `json:"credential,omitempty"` // cross-cloud credential federation (nil = infer from provider type)
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
	Type    string `json:"type"`              // "allow-all" | "deny-side-effects" | "ask-upstream"
	Timeout int    `json:"timeout,omitempty"` // ask-upstream: seconds to wait for a response (0 = 60s default)
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
}

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
func ValidateRunConfig(config *RunConfig) error {
	var errs []string

	validateRequiredType("provider", config.Provider.Type, validProviderTypes, &errs)
	validateOptionalType("modelRouter", config.ModelRouter.Type, validModelRouterTypes, &errs)
	validateOptionalType("promptBuilder", config.PromptBuilder.Type, validPromptBuilderTypes, &errs)
	validateOptionalType("contextStrategy", config.ContextStrategy.Type, validContextStrategyTypes, &errs)
	validateOptionalType("executor", config.Executor.Type, validExecutorTypes, &errs)
	validateOptionalType("editStrategy", config.EditStrategy.Type, validEditStrategyTypes, &errs)
	validateOptionalType("permissionPolicy", config.PermissionPolicy.Type, validPermissionPolicyTypes, &errs)
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
