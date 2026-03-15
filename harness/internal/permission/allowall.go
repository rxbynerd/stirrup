package permission

import (
	"context"
	"encoding/json"

	"github.com/rubynerd/stirrup/types"
)

// AllowAll is a PermissionPolicy that permits every tool call unconditionally.
type AllowAll struct{}

// NewAllowAll returns a new AllowAll policy.
func NewAllowAll() *AllowAll {
	return &AllowAll{}
}

// Check always returns Allowed: true.
func (a *AllowAll) Check(_ context.Context, _ types.ToolDefinition, _ json.RawMessage) (*PermissionResult, error) {
	return &PermissionResult{Allowed: true}, nil
}
