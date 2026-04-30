package types

import "encoding/json"

// StreamEvent represents a single event from the model's streaming response.
//
// StopReason is only populated on a "message_complete" event. The harness
// recognises these canonical values:
//
//   - "end_turn"   — model finished a normal turn with no further action
//   - "tool_use"   — model emitted one or more tool calls; the agentic
//     loop should dispatch them and continue the conversation
//   - "max_tokens" — provider stopped because of an output token budget
//   - "error"      — provider returned an empty stop reason (defensive)
//   - "incomplete" — Responses-API status with no specific reason
//
// Any other value is passed through verbatim to the agentic loop and
// surfaces as the run's outcome string. The authoritative consumer is
// harness/internal/core/loop.go (see the run loop's stop-reason switch
// near the end of the turn handler).
type StreamEvent struct {
	Type         string         `json:"type"` // "text_delta" | "tool_call" | "message_complete" | "error"
	Text         string         `json:"text,omitempty"`
	ID           string         `json:"id,omitempty"`
	Name         string         `json:"name,omitempty"`
	Input        map[string]any `json:"input,omitempty"`
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
	Type           string          `json:"type"` // "text_delta" | "tool_call" | "tool_result" | "done" | "error" | "heartbeat" | "ready" | "permission_request"
	Text           string          `json:"text,omitempty"`
	ID             string          `json:"id,omitempty"`
	Name           string          `json:"name,omitempty"`
	Input          json.RawMessage `json:"input,omitempty"`
	ToolUseID      string          `json:"toolUseId,omitempty"`
	Content        string          `json:"content,omitempty"`
	StopReason     string          `json:"stopReason,omitempty"`
	Message        string          `json:"message,omitempty"`
	Trace          *RunTrace       `json:"trace,omitempty"`
	RequestID      string          `json:"requestId,omitempty"`      // correlates permission_request with permission_response
	ToolName       string          `json:"toolName,omitempty"`       // tool name in permission_request
	HarnessVersion string          `json:"harnessVersion,omitempty"` // harness build version (set on "ready" events)
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
