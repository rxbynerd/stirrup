package permission

import (
	"context"
	"encoding/json"

	"github.com/rxbynerd/stirrup/types"
)

// DenySideEffects is a PermissionPolicy that rejects tool calls for tools
// known to have side effects. Tools not in the set are allowed.
type DenySideEffects struct {
	sideEffectingTools map[string]bool
}

// NewDenySideEffects returns a new DenySideEffects policy. The provided map
// keys are tool names that are considered to have side effects; calls to
// those tools will be denied.
func NewDenySideEffects(sideEffectingTools map[string]bool) *DenySideEffects {
	return &DenySideEffects{sideEffectingTools: sideEffectingTools}
}

// Check returns Allowed: false for tools in the side-effecting set, and
// Allowed: true for all others.
func (d *DenySideEffects) Check(_ context.Context, tool types.ToolDefinition, _ json.RawMessage) (*PermissionResult, error) {
	if d.sideEffectingTools[tool.Name] {
		return &PermissionResult{
			Allowed: false,
			Reason:  "side effects not permitted in this mode",
		}, nil
	}
	return &PermissionResult{Allowed: true}, nil
}
