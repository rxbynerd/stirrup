package types

import "encoding/json"

// StreamEvent represents a single event from the model's streaming response.
type StreamEvent struct {
	Type       string         `json:"type"` // "text_delta" | "tool_call" | "message_complete" | "error"
	Text       string         `json:"text,omitempty"`
	ID         string         `json:"id,omitempty"`
	Name       string         `json:"name,omitempty"`
	Input      map[string]any `json:"input,omitempty"`
	StopReason   string         `json:"stopReason,omitempty"`
	OutputTokens int            `json:"outputTokens,omitempty"`
	Content      []ContentBlock `json:"content,omitempty"`
	Error        error          `json:"-"`
}

// StreamParams holds the parameters for a model streaming request.
type StreamParams struct {
	Model       string           `json:"model"`
	System      string           `json:"system"`
	Messages    []Message        `json:"messages"`
	Tools       []ToolDefinition `json:"tools"`
	MaxTokens   int              `json:"maxTokens"`
	Temperature float64          `json:"temperature"`
}

// HarnessEvent is an event emitted by the harness to the control plane.
type HarnessEvent struct {
	Type       string          `json:"type"` // "text_delta" | "tool_call" | "tool_result" | "done" | "error" | "heartbeat" | "ready" | "permission_request"
	Text       string          `json:"text,omitempty"`
	ID         string          `json:"id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Input      json.RawMessage `json:"input,omitempty"`
	ToolUseID  string          `json:"toolUseId,omitempty"`
	Content    string          `json:"content,omitempty"`
	StopReason string          `json:"stopReason,omitempty"`
	Message    string          `json:"message,omitempty"`
	Trace      *RunTrace       `json:"trace,omitempty"`
	RequestID  string          `json:"requestId,omitempty"` // correlates permission_request with permission_response
	ToolName   string          `json:"toolName,omitempty"`  // tool name in permission_request
}

// ControlEvent is an event received from the control plane.
type ControlEvent struct {
	Type         string     `json:"type"` // "task_assignment" | "user_response" | "cancel" | "permission_response"
	Task         *RunConfig `json:"task,omitempty"`
	UserResponse string     `json:"userResponse,omitempty"`
	RequestID    string     `json:"requestId,omitempty"` // correlates permission_response with permission_request
	Allowed      *bool      `json:"allowed,omitempty"`   // permission decision
	Reason       string     `json:"reason,omitempty"`    // explanation for denial
}

// HarnessLifecycleEvent represents lifecycle signals sent on the transport.
type HarnessLifecycleEvent struct {
	Type     string     `json:"type"` // "ready" | "heartbeat" | "shutdown"
	RunID    string     `json:"runId"`
	Config   *RunConfig `json:"config,omitempty"`
	Turn     int        `json:"turn,omitempty"`
	UptimeMs int64      `json:"uptimeMs,omitempty"`
	Reason   string     `json:"reason,omitempty"`
}

// LogEvent is the structured log schema.
type LogEvent struct {
	Timestamp  string         `json:"timestamp"`
	Level      string         `json:"level"`
	RunID      string         `json:"runId"`
	Component  string         `json:"component"`
	Event      string         `json:"event"`
	Data       map[string]any `json:"data,omitempty"`
	DurationMs *int64         `json:"durationMs,omitempty"`
}
