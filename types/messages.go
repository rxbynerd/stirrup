// Package types defines the shared type contracts for the stirrup harness.
// This module has zero dependencies — pure type definitions only.
package types

import "encoding/json"

// Message represents a single message in the conversation history.
type Message struct {
	Role    string         `json:"role"` // "user" | "assistant"
	Content []ContentBlock `json:"content"`
}

// ContentBlock is a single block of content within a message.
// Use the Type field to determine which variant fields are populated.
//
// ThoughtSignature is a provider-opaque blob attached to an assistant block
// so the harness can round-trip it back to the provider on the next turn.
// Currently it is only populated by the Gemini adapter, which threads the
// `thoughtSignature` field that Vertex AI emits on parts of 3.x model
// responses (Gemini's encrypted chain-of-thought for cross-turn reasoning
// continuity, see https://cloud.google.com/vertex-ai/generative-ai/docs/thinking).
// The field is `omitempty` so other adapters never see it on the wire.
// Treat the value as fully opaque — the harness must not introspect it,
// log it verbatim, or mutate it. A future generalisation (e.g. renaming
// to ProviderState or moving to a metadata map) is intentionally a non-goal
// for the current change.
//
// Rename-decision (recorded for the next maintainer who needs this):
// the name `ThoughtSignature` is Gemini-specific. When a second provider
// requires analogous opaque round-trip state, rename this field to
// `ProviderState` (JSON: `provider_state`) and update the RunRecording
// schema at the same time — do NOT add a second provider-specific field
// alongside this one. Adapter-private wire types (see
// anthropicContentBlock for the established pattern) must continue to
// omit any provider-state field they do not own, so the rename does
// not relax the cross-provider leakage guard introduced for #194.
type ContentBlock struct {
	Type             string          `json:"type"` // "text" | "tool_use" | "tool_result"
	Text             string          `json:"text,omitempty"`
	ID               string          `json:"id,omitempty"`
	Name             string          `json:"name,omitempty"`
	Input            json.RawMessage `json:"input,omitempty"`
	ToolUseID        string          `json:"tool_use_id,omitempty"`
	Content          string          `json:"content,omitempty"`
	IsError          bool            `json:"is_error,omitempty"`
	ThoughtSignature string          `json:"thought_signature,omitempty"`
}

// ToolDefinition describes a tool available to the model.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolCall represents a tool invocation by the model.
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResult represents the result of a tool invocation.
//
// Content is the canonical text rendering of the result and is always
// populated — it is the fallback every provider can accept. Structured is
// an optional, purely additive typed payload (issue #231): producers that
// can describe their output as stable fields (a command's stdout/stderr/exit
// code, a search's path/line/text matches, a file excerpt's line window)
// marshal a typed Go struct into it so downstream consumers can parse the
// result without re-deriving it from the text. The zero value (nil) means
// "no structured data", so a text-only result serialises byte-identically to
// the pre-#231 shape via omitempty. Whether a provider receives Content or
// Structured on the wire is decided by the provider adapters (issue #231 B2),
// not here; the harness always keeps Content populated as the safe fallback.
type ToolResult struct {
	ToolUseID  string          `json:"tool_use_id"`
	Content    string          `json:"content"`
	IsError    bool            `json:"is_error,omitempty"`
	Structured json.RawMessage `json:"structured,omitempty"`
}

// Artifact represents a named output produced during a run.
type Artifact struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // "file" | "diff" | "text"
	Content string `json:"content"`
	Path    string `json:"path,omitempty"`
}
