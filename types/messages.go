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
// ThoughtSignature is an opaque, provider-specific blob the model emits
// alongside a part to encode its hidden chain-of-thought. Today only the
// Gemini 3.x adapter populates it (on text and tool_use blocks). The
// harness preserves the blob unchanged across turns so the model can
// resume its prior reasoning. Other providers ignore the field on
// translation.
//
// Invariants intentionally preserved:
//   - tool_result blocks never carry a signature. Signatures live on
//     model-produced parts; the tool-response leg is operator-side.
//   - The JSON tag uses snake_case here (`thought_signature`) to match
//     the rest of ContentBlock's tag style; the corresponding field on
//     StreamEvent uses camelCase (`thoughtSignature`) to match its
//     tag style. The asymmetry is deliberate and stays internal:
//     translation between the two struct shapes happens in
//     core.streamEventsToResult, so the wire shapes never need to
//     round-trip through both encodings.
//   - The field is JSON-only on the trace path. The gRPC RunTrace
//     proto representation does not embed ContentBlock today, so
//     thought_signature never crosses the gRPC wire. A future
//     refactor that embeds ContentBlock-equivalent into proto must
//     preserve the field explicitly.
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
type ToolResult struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

// Artifact represents a named output produced during a run.
type Artifact struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // "file" | "diff" | "text"
	Content string `json:"content"`
	Path    string `json:"path,omitempty"`
}
