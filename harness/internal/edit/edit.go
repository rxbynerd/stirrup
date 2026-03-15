// Package edit defines the EditStrategy interface and implementations for
// applying file edits through the coding agent.
package edit

import (
	"context"
	"encoding/json"

	"github.com/rubynerd/stirrup/harness/internal/executor"
	"github.com/rubynerd/stirrup/types"
)

// EditResult describes the outcome of an edit operation.
type EditResult struct {
	Path    string
	Applied bool
	Diff    string
	Error   string
}

// EditStrategy defines how the agent applies file edits. Different strategies
// (whole-file, search-replace, udiff) implement this interface.
type EditStrategy interface {
	ToolDefinition() types.ToolDefinition
	Apply(ctx context.Context, input json.RawMessage, exec executor.Executor) (*EditResult, error)
}
