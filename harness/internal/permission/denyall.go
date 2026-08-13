package permission

import (
	"context"
	"encoding/json"

	"github.com/rxbynerd/stirrup/types"
)

// DenyAll is a PermissionPolicy that denies every tool call
// unconditionally. Its intended role is the fallback of a policy-engine
// whose Cedar file is a permit-based allow-list: the policy file grants,
// and everything the file does not grant is denied here. Unlike
// DenySideEffects it also denies non-mutating tools (read_file,
// web_fetch, ...), so an allow-list over web_fetch URLs actually denies
// the URLs it does not name.
type DenyAll struct{}

// NewDenyAll returns a new DenyAll policy.
func NewDenyAll() *DenyAll {
	return &DenyAll{}
}

// Check always returns Allowed: false.
func (d *DenyAll) Check(_ context.Context, _ types.ToolDefinition, _ json.RawMessage) (*PermissionResult, error) {
	return &PermissionResult{
		Allowed: false,
		Reason:  "not permitted by policy (deny-all)",
	}, nil
}
