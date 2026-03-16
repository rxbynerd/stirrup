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
type ContentBlock struct {
	Type      string          `json:"type"` // "text" | "tool_use" | "tool_result"
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
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
