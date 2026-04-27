// Package tool defines the Tool struct and ToolRegistry interface for managing
// tools available to the coding agent.
package tool

import (
	"context"
	"encoding/json"

	"github.com/rxbynerd/stirrup/types"
)

// Tool represents a single tool that the model can invoke.
//
// WorkspaceMutating and RequiresApproval are deliberately separate flags
// because the two concepts are distinct:
//
//   - WorkspaceMutating reports whether the tool modifies workspace state
//     (files, processes, on-disk artefacts). Read-only modes (research,
//     review, planning, toil) must reject these.
//   - RequiresApproval reports whether the tool should be gated by an
//     upstream approval policy. This includes WorkspaceMutating tools but
//     also covers non-mutating tools whose effects the operator may still
//     want to gate (e.g. web_fetch makes network requests; spawn_agent
//     consumes additional model budget and acts on its own).
//
// Tools may set neither, one, or both flags. Read-only tools (read_file,
// list_directory, search_files) set neither.
type Tool struct {
	Name              string
	Description       string
	InputSchema       json.RawMessage // JSON Schema for the tool's input
	WorkspaceMutating bool
	RequiresApproval  bool
	Handler           func(ctx context.Context, input json.RawMessage) (string, error)
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
