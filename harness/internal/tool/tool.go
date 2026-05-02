// Package tool defines the Tool struct and ToolRegistry interface for managing
// tools available to the coding agent.
package tool

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// AsyncDispatch carries the metadata an async tool returns from its preflight
// step. Returning AsyncDispatch tells the agentic loop "do not finalise this
// tool call yet — emit a tool_result_request, then block on the matching
// tool_result_response under ctx cancellation and the per-call timeout".
//
// Timeout, when positive, overrides the loop's default per-call timeout for
// just this call. Zero means use the loop default.
//
// The loop allocates the wire-level request ID via its transport correlator;
// tool authors do not control it. This keeps correlation single-source so a
// caller-supplied value cannot diverge from the value emitted on the wire.
// The struct is retained as the AsyncHandler return type for future
// extensibility.
type AsyncDispatch struct {
	Timeout time.Duration
}

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
//
// A tool is async when AsyncHandler is non-nil. The agentic loop will:
//
//  1. Run permission and security checks exactly as for a synchronous tool.
//  2. Invoke AsyncHandler as a preflight. The handler may emit any
//     side-effecting message it likes (the loop does not require it to);
//     it returns an AsyncDispatch describing the request_id and per-call
//     timeout to use.
//  3. Emit a "tool_result_request" HarnessEvent carrying that request_id,
//     the tool name, the model's tool_use_id, and the tool input.
//  4. Block on the matching "tool_result_response" ControlEvent via the
//     loop's transport correlator, under run-context cancellation and the
//     per-call timeout. The control plane's response payload becomes the
//     tool's output; its is_error flag becomes ToolResult.IsError.
//
// If both Handler and AsyncHandler are set, the loop prefers AsyncHandler.
// If neither is set the tool is unusable (Resolve returns it but dispatch
// will fail with a "tool has no handler" error).
//
// Async tools require a transport that can deliver control-plane responses.
// Sub-agents run with NullTransport (whose OnControl is a no-op), so an
// async tool dispatched on a sub-agent loop fails fast with a clear error
// rather than blocking until the per-call timeout — see core/loop.go's
// async dispatch path.
type Tool struct {
	Name              string
	Description       string
	InputSchema       json.RawMessage // JSON Schema for the tool's input
	WorkspaceMutating bool
	RequiresApproval  bool
	Handler           func(ctx context.Context, input json.RawMessage) (string, error)

	// AsyncHandler, when non-nil, marks the tool as async. The loop calls
	// it after permission/security checks and uses the returned
	// AsyncDispatch to drive the request/response correlation. The handler
	// may return an error to abort dispatch before any wire event is
	// emitted; the error message is surfaced to the model as a tool
	// internal error.
	AsyncHandler func(ctx context.Context, input json.RawMessage) (AsyncDispatch, error)
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
