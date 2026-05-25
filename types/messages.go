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

	// Structured and Kind carry the optional typed tool-result envelope
	// (issue #231) from a tool_result ToolResult onto the message-history
	// content block so the provider adapters can decide, per the resolved
	// StructuredToolResults capability, whether to serialise the structured
	// shape or fall back to the text Content. They are populated only on
	// tool_result blocks and only when the producing tool emitted structured
	// data; both are omitempty so a text-only result serialises byte-
	// identically to the pre-#231 shape. Content remains the canonical
	// fallback and is always populated — an adapter that does not opt into
	// the structured shape ignores these fields entirely.
	Structured json.RawMessage `json:"structured,omitempty"`
	Kind       string          `json:"kind,omitempty"`
}

// ToolDefinition describes a tool available to the model.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`

	// Presentation carries optional, additive per-tool metadata (issue
	// #222): worked input examples and behavioural annotations. The
	// `json:"-"` tag is load-bearing, not cosmetic: the Anthropic adapter
	// historically serialised ToolDefinition onto the Messages wire
	// verbatim, and Anthropic rejects unknown top-level keys on a tool
	// object. Keeping Presentation off the default JSON encoding means the
	// nil zero value is byte-identical to the pre-#222 wire shape on every
	// adapter, and an adapter that wants to surface any part of Presentation
	// must read it explicitly and project it into its own wire struct
	// (gated on the resolved provider capability). An adapter with no
	// capability treats Presentation as a deliberate, test-covered no-op.
	Presentation *ToolPresentation `json:"-"`
}

// ToolPresentation is the optional per-tool metadata bundle introduced for
// issue #222. Both fields are advisory: serialisation is gated per-adapter on
// the resolved quirks capability, and the nil/empty zero value emits nothing.
type ToolPresentation struct {
	// InputExamples are worked example inputs for the tool, each a JSON
	// object valid against InputSchema. They migrate the inline "Example:
	// {…}" convention enriched into descriptions by #227 into structured
	// data. Adapters that advertise the examples capability fold these into
	// the JSON-Schema `examples` keyword inside the emitted parameters
	// object; adapters without it ignore them. Nothing is lost for the
	// latter — the #227 description text still carries the example for every
	// provider, unconditionally.
	InputExamples []json.RawMessage `json:"inputExamples,omitempty"`

	// Annotations are MCP-style behavioural hints (spec 2025-06-18). No
	// first-party provider (OpenAI, Anthropic, Gemini, Bedrock) exposes a
	// tool-annotation wire field today, so these are carried for internal
	// use and round-tripped from MCP servers; the first-party adapters treat
	// them as a deliberate, test-covered no-op. Built-in tools derive them
	// from their WorkspaceMutating flag; MCP-imported tools carry the
	// server-declared annotations verbatim.
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

// ToolAnnotations mirrors the MCP tool-annotations object (spec 2025-06-18).
// Each hint is a *bool so "unset" is distinguishable from an explicit "false":
// a built-in tool derives ReadOnlyHint/DestructiveHint from its mutation flag,
// while an MCP server may supply any subset, leaving the rest unset.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
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
//
// Kind names the shape of the Structured payload (e.g. "command_result",
// "file_excerpt") so B2's provider adapters and MCP bridge can route it by a
// stable discriminator instead of unmarshalling and sniffing the JSON (which
// would breach the typed-not-`any` rule). It is empty for text-only results
// and so omitted from the wire, preserving byte-identical pre-#231 output.
type ToolResult struct {
	ToolUseID  string          `json:"tool_use_id"`
	Content    string          `json:"content"`
	IsError    bool            `json:"is_error,omitempty"`
	Structured json.RawMessage `json:"structured,omitempty"`
	Kind       string          `json:"kind,omitempty"`
}

// Artifact represents a named output produced during a run.
type Artifact struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // "file" | "diff" | "text"
	Content string `json:"content"`
	Path    string `json:"path,omitempty"`
}
