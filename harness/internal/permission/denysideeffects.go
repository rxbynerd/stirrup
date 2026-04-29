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
