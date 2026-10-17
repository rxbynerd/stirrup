package permission

import (
	"context"
	"encoding/json"

	"github.com/rxbynerd/stirrup/types"
)

// DenySideEffects is a PermissionPolicy that rejects tool calls for tools
// known to mutate workspace state. Tools not in the set — including
// non-mutating tools that may still require approval such as web_fetch and
// spawn_agent — are allowed.
//
// The "side effects" name is retained for backwards compatibility with
// the deny-side-effects RunConfig type; semantically the policy now denies
// only WorkspaceMutating tools (see Tool.WorkspaceMutating).
type DenySideEffects struct {
	mutatingTools map[string]bool
}

// NewDenySideEffects returns a new DenySideEffects policy. The provided map
// keys are tool names that mutate workspace state; calls to those tools are
// denied. The factory builds this set from the registry by collecting tools
// whose Tool.WorkspaceMutating flag is true.
func NewDenySideEffects(mutatingTools map[string]bool) *DenySideEffects {
	return &DenySideEffects{mutatingTools: mutatingTools}
}

// Check returns Allowed: false for tools in the workspace-mutating set, and
// Allowed: true for all others (including approval-required tools like
// web_fetch and spawn_agent which the operator may still wish to run).
func (d *DenySideEffects) Check(_ context.Context, tool types.ToolDefinition, _ json.RawMessage) (*PermissionResult, error) {
	if d.mutatingTools[tool.Name] {
		return &PermissionResult{
			Allowed: false,
			Reason:  "workspace-mutating tools are not permitted in this mode",
		}, nil
	}
	return &PermissionResult{Allowed: true}, nil
}

// SideEffectGate denies workspace-mutating tools before consulting the
// wrapped policy, so a call must pass both. It narrows a read-only
// sub-agent's inherited permission chain without replacing it: the
// inner chain still decides every call the gate lets through, and
// mutating calls never reach an inner ask-upstream policy.
type SideEffectGate struct {
	deny  *DenySideEffects
	inner PermissionPolicy
}

var (
	_ PermissionPolicy = (*SideEffectGate)(nil)
	_ Rewrapper        = (*SideEffectGate)(nil)
)

// NewSideEffectGate returns a SideEffectGate denying the tools in
// mutatingTools in front of inner.
func NewSideEffectGate(mutatingTools map[string]bool, inner PermissionPolicy) *SideEffectGate {
	return &SideEffectGate{deny: NewDenySideEffects(mutatingTools), inner: inner}
}

// Check denies a mutating tool outright and otherwise returns the inner
// policy's decision.
func (g *SideEffectGate) Check(ctx context.Context, tool types.ToolDefinition, input json.RawMessage) (*PermissionResult, error) {
	result, err := g.deny.Check(ctx, tool, input)
	if err != nil || !result.Allowed {
		return result, err
	}
	return g.inner.Check(ctx, tool, input)
}

// Unwrap returns the wrapped policy so Unwrap and RewrapChain see through
// the gate.
func (g *SideEffectGate) Unwrap() PermissionPolicy {
	return g.inner
}

// Rewrap reproduces the gate, with the same denied set, around inner.
func (g *SideEffectGate) Rewrap(inner PermissionPolicy) PermissionPolicy {
	return &SideEffectGate{deny: g.deny, inner: inner}
}
