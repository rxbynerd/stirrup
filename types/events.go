package types

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// StreamEvent represents a single event from the model's streaming response.
//
// StopReason is only populated on a "message_complete" event: one of
// "end_turn", "tool_use", "max_tokens", "error", "incomplete", or a
// provider-specific value passed through verbatim as the run's outcome.
// Adapters MUST emit "tool_use" whenever the stream contains a tool call,
// regardless of the provider's native finish-reason vocabulary (see
// docs/providers.md for the Vertex AI STOP-remapping example).
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

	// ThoughtSignature is the opaque provider-private blob captured for
	// round-trip on the next turn (currently Gemini only). The agentic loop
	// copies it onto the persisted assistant ContentBlock verbatim; other
	// adapters must leave it at the zero value. See docs/provider-quirks.md.
	ThoughtSignature string `json:"thought_signature,omitempty"`

	// ReplayFields carries message-level provider-opaque state captured by
	// a quirks ReplayFields rule, keyed by the rule's path string.
	// Populated only on "message_complete" events; provider-opaque, so the
	// harness must not introspect or mutate the values. See
	// docs/provider-quirks.md for the flattening rule and threading design.
	ReplayFields map[string]json.RawMessage `json:"replay_fields,omitempty"`
}

// ToolChoiceMode is a closed enum selecting how the model is steered
// toward (or away from) tool use on a single turn. It is a
// provider-neutral control: each adapter projects it onto the provider's
// native tool_choice / functionCallingConfig shape, gated on the resolved
// provider capability. The zero value (ToolChoiceAuto) reproduces the
// historical behaviour — the model decides whether to call a tool — so
// every existing caller that never sets ToolChoice is unaffected.
type ToolChoiceMode int

const (
	// ToolChoiceAuto lets the model decide whether to call a tool. Zero
	// value: an adapter MUST treat it as "emit nothing on the wire" so
	// the request is byte-identical to the pre-tool-choice shape.
	ToolChoiceAuto ToolChoiceMode = iota

	// ToolChoiceRequired forces the model to call at least one tool on
	// this turn (Anthropic "any", OpenAI "required", Gemini "ANY"). The
	// loop escalation chunk (A2) drives this when a turn ended without a
	// tool call but one was expected.
	ToolChoiceRequired

	// ToolChoiceNone forbids tool calls on this turn (Anthropic "none"
	// has no native form and is handled by omitting tools; OpenAI
	// "none"; Gemini "NONE"). Reserved for callers that want a
	// text-only turn while leaving the tool definitions in place.
	ToolChoiceNone

	// ToolChoiceTool forces the model to call the tool named by
	// StreamParams.ToolChoiceName. An empty ToolChoiceName degrades to
	// ToolChoiceAuto at the adapter.
	ToolChoiceTool
)

// ToolChoiceAuto must stay the zero value: StreamParams.ToolChoice is
// omitempty, which only suppresses an integer zero. This array index
// fails to compile if a future reorder makes ToolChoiceAuto non-zero.
var _ = [1]struct{}{}[ToolChoiceAuto]

// String renders ToolChoiceMode as its stable lowercase wire form (the
// same tokens MarshalJSON/UnmarshalJSON use). An out-of-range value
// renders as "unknown(N)" rather than erroring; IsValid and MarshalJSON
// reject such values instead.
func (m ToolChoiceMode) String() string {
	switch m {
	case ToolChoiceAuto:
		return "auto"
	case ToolChoiceRequired:
		return "required"
	case ToolChoiceNone:
		return "none"
	case ToolChoiceTool:
		return "tool"
	default:
		return fmt.Sprintf("unknown(%d)", int(m))
	}
}

// IsValid reports whether m is one of the defined ToolChoiceMode members.
// Out-of-range integers (e.g. from a corrupt trace or a hostile JSON
// payload) are rejected rather than coerced to a default.
func (m ToolChoiceMode) IsValid() bool {
	switch m {
	case ToolChoiceAuto, ToolChoiceRequired, ToolChoiceNone, ToolChoiceTool:
		return true
	default:
		return false
	}
}

// MarshalJSON emits the lowercase string form; an out-of-range value is
// rejected rather than coerced. Mirrors OpenAITokenField and
// GeminiStreamArgsShape's marshalling style.
func (m ToolChoiceMode) MarshalJSON() ([]byte, error) {
	switch m {
	case ToolChoiceAuto:
		return []byte(`"auto"`), nil
	case ToolChoiceRequired:
		return []byte(`"required"`), nil
	case ToolChoiceNone:
		return []byte(`"none"`), nil
	case ToolChoiceTool:
		return []byte(`"tool"`), nil
	default:
		return nil, fmt.Errorf("types: invalid ToolChoiceMode %d", int(m))
	}
}

// UnmarshalJSON accepts only the defined string forms; an unknown string
// rejects with an error rather than silently mapping to ToolChoiceAuto.
func (m *ToolChoiceMode) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case `"auto"`:
		*m = ToolChoiceAuto
	case `"required"`:
		*m = ToolChoiceRequired
	case `"none"`:
		*m = ToolChoiceNone
	case `"tool"`:
		*m = ToolChoiceTool
	default:
		return fmt.Errorf("types: unknown ToolChoiceMode %s", truncateForError(data))
	}
	return nil
}

// truncateForError renders raw unmarshal input safely for an error message:
// quoted (escaping control bytes) and capped at 64 bytes so hostile input
// can't blow up a log line.
func truncateForError(data []byte) string {
	if len(data) > 64 {
		return fmt.Sprintf("%q…", data[:64])
	}
	return fmt.Sprintf("%q", data)
}

// toolChoiceNamePattern is the intersection of the Anthropic, OpenAI, and
// Gemini function-name grammars (Anthropic's is the tightest, so it governs).
var toolChoiceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// ValidateToolChoiceName reports whether a named-tool choice's tool name is
// safe to emit onto a provider request body. Adapters call this to fail
// closed (degrade to auto) rather than emit an unvalidated value.
func ValidateToolChoiceName(name string) error {
	if !toolChoiceNamePattern.MatchString(name) {
		return fmt.Errorf("tool choice name %q must match %s", name, toolChoiceNamePattern.String())
	}
	return nil
}

// StreamParams holds the parameters for a model streaming request.
type StreamParams struct {
	Model     string           `json:"model"`
	System    string           `json:"system"`
	Messages  []Message        `json:"messages"`
	Tools     []ToolDefinition `json:"tools"`
	MaxTokens int              `json:"maxTokens"`
	// Temperature controls sampling randomness. A nil pointer means "use the
	// provider's default": adapters MUST NOT transmit temperature on the
	// wire in that case (some endpoints, e.g. OpenAI reasoning models on
	// Chat Completions, reject any temperature value including zero). Use
	// Float64Ptr to construct a pointer from a literal.
	Temperature *float64 `json:"temperature,omitempty"`

	// ToolChoice steers tool use for this turn. The zero value
	// (ToolChoiceAuto) is omitted from the wire by every adapter. An
	// adapter emits a native tool_choice field only when the resolved
	// provider capability supports the requested mode; otherwise it is a
	// no-op.
	ToolChoice ToolChoiceMode `json:"toolChoice,omitempty"`

	// ToolChoiceName names the specific tool to force when ToolChoice is
	// ToolChoiceTool. Ignored for every other mode. An empty value with
	// ToolChoiceTool degrades to ToolChoiceAuto at the adapter.
	ToolChoiceName string `json:"toolChoiceName,omitempty"`

	// ParallelToolCalls steers whether the provider may emit more than one
	// tool call in a single turn. A nil pointer means "say nothing on the
	// wire". Projected onto the provider's native control only when the
	// resolved capability advertises support (OpenAI `parallel_tool_calls`,
	// Anthropic `disable_parallel_tool_use`); a no-op on Gemini and Bedrock,
	// with no prompt-based fallback since it is an efficiency hint, not a
	// correctness lever.
	ParallelToolCalls *bool `json:"parallelToolCalls,omitempty"`
}

// Float64Ptr returns a pointer to the given float64 value. It is a
// readability helper for constructing StreamParams.Temperature literals
// without a temporary variable.
func Float64Ptr(v float64) *float64 {
	return &v
}

// HarnessEvent is an event emitted by the harness to the control plane.
//
// Type discriminates the event shape: "text_delta", "tool_call",
// "tool_result", "done", "error", "warning", "heartbeat", "ready",
// "permission_request", "tool_result_request", "batch_submission",
// "batch_waiting", "batch_cancel_request". RequestID correlates a request
// event with its matching ControlEvent response. See docs/deployment.md
// for the gRPC sequence and docs/batch.md for the batch event correlation.
type HarnessEvent struct {
	Type           string          `json:"type"`
	Text           string          `json:"text,omitempty"`
	ID             string          `json:"id,omitempty"`
	Name           string          `json:"name,omitempty"`
	Input          json.RawMessage `json:"input,omitempty"`
	ToolUseID      string          `json:"toolUseId,omitempty"`
	Content        string          `json:"content,omitempty"`
	StopReason     string          `json:"stopReason,omitempty"`
	Message        string          `json:"message,omitempty"`
	Trace          *RunTrace       `json:"trace,omitempty"`
	RequestID      string          `json:"requestId,omitempty"`      // correlates permission/tool-result requests with their responses
	ToolName       string          `json:"toolName,omitempty"`       // tool name on permission_request and tool_result_request
	HarnessVersion string          `json:"harnessVersion,omitempty"` // harness build version (set on "ready" events)
}

// ControlEvent is an event received from the control plane.
//
// Type discriminates the event shape: "task_assignment", "user_response",
// "cancel", "permission_response", "tool_result_response", "batch_result".
// Each response type echoes RequestID from the HarnessEvent it completes.
// See docs/deployment.md and docs/batch.md.
type ControlEvent struct {
	Type         string     `json:"type"`
	Task         *RunConfig `json:"task,omitempty"`
	UserResponse string     `json:"userResponse,omitempty"`
	RequestID    string     `json:"requestId,omitempty"` // correlates response with the originating request
	Allowed      *bool      `json:"allowed,omitempty"`   // permission decision (permission_response only)
	Reason       string     `json:"reason,omitempty"`    // explanation for denial (permission_response only)
	Content      string     `json:"content,omitempty"`   // async tool result payload (tool_result_response only)
	IsError      *bool      `json:"isError,omitempty"`   // mark async tool result as an error (tool_result_response only)
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
