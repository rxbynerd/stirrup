// Package tool defines the Tool struct and ToolRegistry interface for managing
// tools available to the coding agent.
package tool

import (
	"context"
	"encoding/json"

	"github.com/rubynerd/stirrup/types"
)

// Tool represents a single tool that the model can invoke.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage // JSON Schema for the tool's input
	SideEffects bool
	Handler     func(ctx context.Context, input json.RawMessage) (string, error)
}

// Definition converts a Tool to the wire-format ToolDefinition used by the
// model provider.
func (t *Tool) Definition() types.ToolDefinition {
	return types.ToolDefinition{
		Name:        t.Name,
		Description: t.Description,
		InputSchema: t.InputSchema,
	}
}

// ToolRegistry provides lookup and listing of available tools.
type ToolRegistry interface {
	List() []types.ToolDefinition
	Resolve(name string) *Tool
}
