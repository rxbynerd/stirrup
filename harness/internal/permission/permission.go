// Package permission defines the PermissionPolicy interface and
// implementations that gate tool execution based on policy rules.
package permission

import (
	"context"
	"encoding/json"

	"github.com/rubynerd/stirrup/types"
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
