// Package tool defines the Tool struct and ToolRegistry interface for managing
// tools available to the coding agent.
package tool

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// AsyncDispatch carries the metadata an async tool returns from its
// preflight step, telling the loop to emit a tool_result_request and block
// on the matching tool_result_response (see docs/deployment.md).
//
// Timeout, when positive, overrides the loop's default per-call timeout for
// just this call. Zero means use the loop default. The wire-level request
// ID is allocated by the loop's transport correlator, not by tool authors,
// keeping correlation single-source.
type AsyncDispatch struct {
	Timeout time.Duration
}

// StructuredResult is the return value of a StructuredHandler. It carries
// the canonical Text fallback (always populated, identical to what a plain
// Handler would return) plus an optional typed Structured payload and a
// Kind discriminator naming the payload's shape, letting the MCP bridge and
// provider adapters route it without unmarshalling. A non-empty Structured
// with an empty Kind is a producer bug; dispatch does not enforce it.
type StructuredResult struct {
	Text       string
	Structured json.RawMessage
	Kind       string
}

// Tool represents a single tool that the model can invoke.
//
// WorkspaceMutating and RequiresApproval are separate flags: the former
// gates read-only modes, the latter gates upstream approval and can be set
// on non-mutating tools too (e.g. web_fetch, spawn_agent). See
// docs/configuration.md for the full semantics and examples.
//
// A tool is async when AsyncHandler is non-nil; see AsyncDispatch and
// docs/deployment.md for the request/response protocol. For synchronous
// tools the loop prefers StructuredHandler over Handler when both are set;
// AsyncHandler takes priority over both. If none of the three is set,
// dispatch fails with a "tool has no handler" error.
//
// Async tools require a transport that can deliver control-plane responses;
// sub-agents run with NullTransport, so an async tool dispatched on a
// sub-agent loop fails fast rather than blocking until the per-call
// timeout — see core/loop.go's async dispatch path.
type Tool struct {
	Name              string
	Description       string
	InputSchema       json.RawMessage // JSON Schema for the tool's input
	WorkspaceMutating bool
	RequiresApproval  bool
	Handler           func(ctx context.Context, input json.RawMessage) (string, error)

	// StructuredHandler, when non-nil, is preferred over Handler for
	// synchronous dispatch. It returns the same canonical text the plain
	// Handler would, plus an optional typed structured payload. Tool authors
	// must marshal a typed Go struct into the payload, never a
	// map[string]any. The dispatch path scrubs the payload on the same
	// footing as the text before it reaches a persisted trace.
	StructuredHandler func(ctx context.Context, input json.RawMessage) (StructuredResult, error)

	// AsyncHandler, when non-nil, marks the tool as async. The handler may
	// return an error to abort dispatch before any wire event is emitted;
	// the error message is surfaced to the model as a tool internal error.
	AsyncHandler func(ctx context.Context, input json.RawMessage) (AsyncDispatch, error)

	// InputExamples are optional worked example inputs, each a JSON object
	// valid against InputSchema. Adapters fold it into the JSON-Schema
	// `examples` keyword where the resolved provider capability allows.
	// MCP-imported tools leave it nil.
	InputExamples []json.RawMessage

	// Annotations, when non-nil, supplies MCP-style behavioural hints
	// verbatim and overrides the hints Definition() would otherwise derive
	// from WorkspaceMutating. The MCP bridge sets it to the server-declared
	// tool annotations; built-in tools leave it nil and accept the derived
	// ReadOnlyHint/DestructiveHint.
	Annotations *types.ToolAnnotations
}

// Definition converts a Tool to the wire-format ToolDefinition used by the
// model provider.
func (t *Tool) Definition() types.ToolDefinition {
	return types.ToolDefinition{
		Name:         t.Name,
		Description:  t.Description,
		InputSchema:  t.InputSchema,
		Presentation: t.presentation(),
	}
}

// presentation builds the optional per-tool metadata bundle. It returns nil
// when the tool carries neither examples nor explicit annotations. When a
// bundle is warranted, annotations are derived from the WorkspaceMutating
// flag unless the tool supplied its own (an MCP-imported tool carries the
// server-declared annotations verbatim).
func (t *Tool) presentation() *types.ToolPresentation {
	if len(t.InputExamples) == 0 && t.Annotations == nil {
		return nil
	}
	annotations := t.Annotations
	if annotations == nil {
		readOnly := !t.WorkspaceMutating
		destructive := t.WorkspaceMutating
		annotations = &types.ToolAnnotations{
			ReadOnlyHint:    &readOnly,
			DestructiveHint: &destructive,
		}
	}
	return &types.ToolPresentation{
		InputExamples: t.InputExamples,
		Annotations:   annotations,
	}
}

// ToolRegistry provides lookup and listing of available tools.
type ToolRegistry interface {
	List() []types.ToolDefinition
	Resolve(name string) *Tool
}
