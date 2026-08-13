// Package permission defines the PermissionPolicy interface and
// implementations that gate tool execution based on policy rules.
package permission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rxbynerd/stirrup/types"
)

// PermissionResult indicates whether a tool call is allowed.
type PermissionResult struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// PermissionPolicy decides whether a tool call should proceed.
type PermissionPolicy interface {
	Check(ctx context.Context, tool types.ToolDefinition, input json.RawMessage) (*PermissionResult, error)
}

// FallbackBuilder produces a PermissionPolicy of the named non-policy-engine
// type. The factory provides this closure when constructing a
// PolicyEnginePolicy so the permission package does not need to know
// about Transport, registries, or tool sets — those live in
// harness/internal/core.
//
// Valid input values: "allow-all", "deny-all", "deny-side-effects",
// "ask-upstream". Implementations must reject "policy-engine" to
// prevent infinite chains.
type FallbackBuilder func(typeName string) (PermissionPolicy, error)

// PolicyEngineEnv carries the per-run identity passed through to a
// PolicyEnginePolicy at construction time, populated from RunConfig and
// the runtime context.
type PolicyEngineEnv struct {
	RunID          string
	Mode           string
	Workspace      string
	ParentRunID    string
	Capabilities   []string
	DynamicContext map[string]string
	Security       SecurityEventEmitter

	// ToolSchemas indexes every tool registered for this run by name,
	// carrying the slice of its JSON Schema the policy linter needs. It
	// must be populated after MCP tool discovery so remote tools are
	// visible to the lint; an empty map disables the registry-aware lint
	// tier entirely (the dry-run preflight's component build registers no
	// tools). See LintPolicySetTools.
	ToolSchemas map[string]ToolSchema
}

// New constructs a PermissionPolicy from cfg. It is the entry point for
// "policy-engine" (loads the Cedar policy file and recursively
// constructs the fallback via fallback) and "allow-all"; other types
// require registry/transport context this package does not own and
// must use their dedicated constructors (NewDenySideEffects,
// NewAskUpstreamPolicy) directly. fallback and env are only used for
// cfg.Type == "policy-engine".
func New(cfg types.PermissionPolicyConfig, env PolicyEngineEnv, fallback FallbackBuilder) (PermissionPolicy, error) {
	switch cfg.Type {
	case "policy-engine":
		return newPolicyEngineFromConfig(cfg, env, fallback)
	case "allow-all":
		return NewAllowAll(), nil
	case "":
		// Empty type used to silently coerce to allow-all, handing a
		// misconfigured caller unrestricted access with no error to
		// investigate. Make the omission explicit.
		return nil, errors.New("permission.New: type is required")
	default:
		// Other types (deny-side-effects, ask-upstream) require
		// registry/transport context the permission package does not
		// own. Callers should use the dedicated constructors.
		return nil, fmt.Errorf("permission.New does not handle type %q; use the dedicated constructor", cfg.Type)
	}
}

// newPolicyEngineFromConfig parses the Cedar policy file referenced by
// cfg.PolicyFile, resolves cfg.Fallback (defaulting to "deny-side-effects"
// and rejecting "policy-engine" to prevent infinite chains), and
// constructs the PolicyEnginePolicy.
func newPolicyEngineFromConfig(cfg types.PermissionPolicyConfig, env PolicyEngineEnv, fallback FallbackBuilder) (PermissionPolicy, error) {
	if cfg.PolicyFile == "" {
		return nil, errors.New("permission: policy-engine requires policyFile")
	}
	if fallback == nil {
		return nil, errors.New("permission: policy-engine requires a FallbackBuilder")
	}

	fallbackType := cfg.Fallback
	if fallbackType == "" {
		fallbackType = "deny-side-effects"
	}
	if fallbackType == "policy-engine" {
		// Defensive re-check: ValidateRunConfig already rejects this, but
		// the constructor is a public entry point and must not assume
		// callers validated the config first.
		return nil, errors.New("permission: policy-engine fallback may not itself be policy-engine")
	}

	// LoadPolicySetFromFile has already applied the structural lint tier
	// and rejected its errors. Re-running it here costs a pure AST walk
	// and is what surfaces the structural WARNINGS (an @stirrupLintIgnore
	// downgrade) on the same audit path as the registry-aware tier.
	source := fmt.Sprintf("policy file %q", cfg.PolicyFile)
	policySet, err := LoadPolicySetFromFile(cfg.PolicyFile)
	if err != nil {
		return nil, err
	}
	findings := append(
		LintPolicySetStructure(policySet),
		LintPolicySetTools(policySet, env.ToolSchemas)...,
	)
	emitLintFindings(env.Security, cfg.PolicyFile, findings)
	if err := LintErrors(source, findings); err != nil {
		return nil, err
	}

	fb, err := fallback(fallbackType)
	if err != nil {
		return nil, fmt.Errorf("permission: build fallback %q: %w", fallbackType, err)
	}
	if fb == nil {
		return nil, fmt.Errorf("permission: fallback builder returned nil for type %q", fallbackType)
	}

	return NewPolicyEnginePolicy(PolicyEngineConfig{
		PolicySet:      policySet,
		Fallback:       fb,
		Security:       env.Security,
		RunID:          env.RunID,
		Mode:           env.Mode,
		Workspace:      env.Workspace,
		ParentRunID:    env.ParentRunID,
		Capabilities:   env.Capabilities,
		DynamicContext: env.DynamicContext,
	})
}
