package types

import (
	"fmt"
	"strings"
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
	Provider         ProviderConfig         `json:"provider"`
	ModelRouter      ModelRouterConfig      `json:"modelRouter"`
	PromptBuilder    PromptBuilderConfig    `json:"promptBuilder"`
	ContextStrategy  ContextStrategyConfig  `json:"contextStrategy"`
	Executor         ExecutorConfig         `json:"executor"`
	EditStrategy     EditStrategyConfig     `json:"editStrategy"`
	Verifier         VerifierConfig         `json:"verifier"`
	PermissionPolicy PermissionPolicyConfig `json:"permissionPolicy"`
	GitStrategy      GitStrategyConfig      `json:"gitStrategy"`
	TraceEmitter     TraceEmitterConfig     `json:"traceEmitter"`
	Tools            ToolsConfig            `json:"tools"`

	// Limits
	MaxTurns       int      `json:"maxTurns"`
	MaxTokenBudget *int     `json:"maxTokenBudget,omitempty"`
	MaxCostBudget  *float64 `json:"maxCostBudget,omitempty"`
	Timeout        *int     `json:"timeout,omitempty"`
}

// Redact returns a copy of the RunConfig with secret references replaced
// by placeholder values, safe for persistence in traces and recordings.
func (rc RunConfig) Redact() RunConfig {
	redacted := rc
	if redacted.Provider.APIKeyRef != "" {
		redacted.Provider.APIKeyRef = "secret://[REDACTED]"
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
	Type      string `json:"type"`                    // "anthropic" | "bedrock" | "openai-compatible"
	APIKeyRef string `json:"apiKeyRef,omitempty"`      // e.g. "secret://anthropic-key"
	Region    string `json:"region,omitempty"`          // bedrock
	Profile   string `json:"profile,omitempty"`         // bedrock
	BaseURL   string `json:"baseUrl,omitempty"`         // openai-compatible
}

// ModelRouterConfig selects the model router implementation.
type ModelRouterConfig struct {
	Type     string `json:"type"`               // "static" | "per-mode"
	Provider string `json:"provider,omitempty"` // for static
	Model    string `json:"model,omitempty"`    // for static
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
	Type       string          `json:"type"`                    // "api" | "local" | "container" | "microvm"
	VcsBackend *VcsBackendConfig `json:"vcsBackend,omitempty"` // type: "api"
	Workspace  string          `json:"workspace,omitempty"`
	Image      string          `json:"image,omitempty"`
	Network    *NetworkConfig  `json:"network,omitempty"`
	Resources  *ResourceLimits `json:"resources,omitempty"`
	Proxy      string          `json:"proxy,omitempty"`
}

// VcsBackendConfig selects the VCS backend for the API executor.
type VcsBackendConfig struct {
	Type      string `json:"type"`                // "github" | "gitlab"
	APIKeyRef string `json:"apiKeyRef,omitempty"`
	Repo      string `json:"repo,omitempty"`
	Ref       string `json:"ref,omitempty"`
}

// NetworkConfig controls network egress for sandboxed executors.
type NetworkConfig struct {
	Mode      string   `json:"mode"`                  // "none" or "allowlist"
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
	Type string `json:"type"` // "whole-file" | "search-replace" | "udiff"
}

// VerifierConfig selects the verifier implementation.
type VerifierConfig struct {
	Type    string `json:"type"`              // "none" | "test-runner" | "composite"
	Command string `json:"command,omitempty"` // for test-runner
}

// PermissionPolicyConfig selects the permission policy implementation.
type PermissionPolicyConfig struct {
	Type string `json:"type"` // "allow-all" | "deny-side-effects" | "ask-upstream"
}

// GitStrategyConfig selects the git strategy implementation.
type GitStrategyConfig struct {
	Type string `json:"type"` // "none" | "deterministic"
}

// TraceEmitterConfig selects the trace emitter implementation.
type TraceEmitterConfig struct {
	Type     string `json:"type"`               // "jsonl" | "otel"
	FilePath string `json:"filePath,omitempty"` // for jsonl
}

// ToolsConfig holds the tool configuration.
type ToolsConfig struct {
	BuiltIn    []string         `json:"builtIn,omitempty"`    // which built-in tools to enable
	MCPServers []MCPServerConfig `json:"mcpServers,omitempty"` // MCP server connections
}

// MCPServerConfig describes a single MCP server connection.
type MCPServerConfig struct {
	URI       string `json:"uri"`
	APIKeyRef string `json:"apiKeyRef,omitempty"`
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

	// Read-only modes must use deny-side-effects or ask-upstream
	readOnlyModes := map[string]bool{
		"planning": true, "review": true, "research": true, "toil": true,
	}
	if readOnlyModes[config.Mode] && config.PermissionPolicy.Type == "allow-all" {
		errs = append(errs, fmt.Sprintf("mode %q requires a restrictive permission policy", config.Mode))
	}

	// maxTurns must be bounded
	if config.MaxTurns > 100 {
		errs = append(errs, "maxTurns exceeds maximum of 100")
	}
	if config.MaxTurns <= 0 {
		errs = append(errs, "maxTurns must be positive")
	}

	// timeout must be set
	if config.Timeout == nil || *config.Timeout > 3600 {
		errs = append(errs, "timeout is required and must be <= 3600 seconds")
	}

	if len(errs) > 0 {
		return fmt.Errorf("RunConfig validation failed: %s", strings.Join(errs, "; "))
	}
	return nil
}
