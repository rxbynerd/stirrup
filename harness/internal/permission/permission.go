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
// Valid input values: "allow-all", "deny-side-effects", "ask-upstream".
// Implementations must reject "policy-engine" to prevent infinite chains.
type FallbackBuilder func(typeName string) (PermissionPolicy, error)

// PolicyEngineEnv carries the per-run identity passed through to a
// PolicyEnginePolicy at construction time. Wave 4 wiring populates this
// from RunConfig + the runtime context.
type PolicyEngineEnv struct {
	RunID          string
	Mode           string
	Workspace      string
	ParentRunID    string
	Capabilities   []string
	DynamicContext map[string]string
	Security       SecurityEventEmitter
}

// New constructs a PermissionPolicy from cfg. It handles every type the
// permission package owns end-to-end, including "policy-engine" — which
// requires loading the Cedar policy file and recursively constructing the
// fallback policy via fallback.
//
// allowAll, denySideEffects, and ask-upstream remain available via their
// dedicated constructors (NewAllowAll, NewDenySideEffects,
// NewAskUpstreamPolicy). New is the entry point for the policy-engine
// type because that arm needs the file loader and the fallback resolver
// in one place.
//
// fallback is invoked only when cfg.Type == "policy-engine"; for all
// other types, callers should use the dedicated constructors directly.
// fallback may be nil when cfg.Type != "policy-engine".
//
// env carries per-run identity passed into the Cedar request context.
// env is unused for non-policy-engine types.
func New(cfg types.PermissionPolicyConfig, env PolicyEngineEnv, fallback FallbackBuilder) (PermissionPolicy, error) {
	switch cfg.Type {
	case "policy-engine":
		return newPolicyEngineFromConfig(cfg, env, fallback)
	case "allow-all", "":
		return NewAllowAll(), nil
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
		// Defensive re-check: ValidateRunConfig already rejects this in
		// Wave 1, but the constructor is a public entry point and must
		// not assume callers validated the config first.
		return nil, errors.New("permission: policy-engine fallback may not itself be policy-engine")
	}

	policySet, err := LoadPolicySetFromFile(cfg.PolicyFile)
	if err != nil {
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
