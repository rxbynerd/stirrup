package types

import (
	"encoding/json"
	"fmt"
	"regexp"
)

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
//
// ProviderAdapter implementer note: the agentic loop dispatches tool
// calls only when StopReason == "tool_use". Adapters MUST emit
// "tool_use" whenever the stream contains one or more tool/function
// calls, regardless of the provider's own finish-reason vocabulary —
// for example, Vertex AI returns finishReason="STOP" for both plain
// end-of-turn responses and tool-call turns, and the gemini adapter
// remaps STOP → "tool_use" when at least one functionCall part was
// observed in the stream. Skipping this remap leaves the model's tool
// calls undispatched and the loop terminates with end_turn instead.
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
	// round-trip on the next turn. Currently only populated by the Gemini
	// adapter on "tool_call" events (and, in the future, "text_delta"
	// events when the text-part case is wired). The agentic loop copies
	// this onto the persisted assistant ContentBlock so that the next
	// request reproduces it verbatim. `omitempty` keeps it off the wire
	// for adapters that do not emit it.
	//
	// ProviderAdapter implementations that do not support per-turn
	// reasoning state MUST leave this field at its zero value. Adapter-
	// side wire types are expected to drop the field (see
	// anthropicContentBlock for the established pattern) so a populated
	// value on a ContentBlock cannot accidentally cross provider
	// boundaries.
	ThoughtSignature string `json:"thought_signature,omitempty"`
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

	// ToolChoiceTool forces the model to call one specific tool, named by
	// StreamParams.ToolChoiceName. Anthropic {"type":"tool","name":...},
	// OpenAI {"type":"function","function":{"name":...}}, Gemini ANY mode
	// with allowedFunctionNames. A ToolChoice of ToolChoiceTool with an
	// empty ToolChoiceName is treated by adapters as ToolChoiceAuto
	// (defensive: a named-tool choice with no name is not expressible).
	ToolChoiceTool
)

// ToolChoiceAuto must remain the zero value: StreamParams.ToolChoice is
// tagged omitempty, which suppresses the field only when the value is the
// integer zero. If a future edit reorders the iota (e.g. promoting
// ToolChoiceNone to 0 as a "safer" default), omitempty would silently
// start suppressing the wrong mode and break every JSON round-trip
// without a compilation error. Indexing a one-element array at
// ToolChoiceAuto fails to compile the moment ToolChoiceAuto is non-zero.
var _ = [1]struct{}{}[ToolChoiceAuto]

// String renders ToolChoiceMode as its stable lowercase wire form. These
// are the same tokens MarshalJSON/UnmarshalJSON round-trip through, so a
// trace or log line names the mode rather than an opaque integer. An
// out-of-range value renders as "unknown(N)" — String never returns an
// error, so it stays usable in fmt verbs; IsValid is the predicate that
// rejects such values, and MarshalJSON refuses to emit them.
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

// MarshalJSON emits the lowercase string form so ToolChoice is
// self-documenting wherever a StreamParams is serialised for a trace or
// recording. omitempty on StreamParams.ToolChoice still suppresses the
// zero value (ToolChoiceAuto): encoding/json tests the Go zero value
// before invoking MarshalJSON, so the field stays off the wire for the
// default and this method is only reached for explicitly-set modes.
// Providers never serialise a ToolChoiceMode onto a request body — they
// project the enum value directly onto their native tool_choice shape —
// so the string form does not alter any outbound provider request.
// An out-of-range value is rejected rather than coerced.
func (m ToolChoiceMode) MarshalJSON() ([]byte, error) {
	if !m.IsValid() {
		return nil, fmt.Errorf("types: invalid ToolChoiceMode %d", int(m))
	}
	return []byte(`"` + m.String() + `"`), nil
}

// UnmarshalJSON is the inverse of MarshalJSON. It accepts only the defined
// string forms; an unknown string or out-of-range value is rejected with
// an error rather than silently mapped to ToolChoiceAuto. Permissive
// coercion here would defeat the closed-enum contract and let malformed
// trace/config input flow into StreamParams unchecked.
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
		return fmt.Errorf("types: unknown ToolChoiceMode %s", data)
	}
	return nil
}

// toolChoiceNamePattern is the character-set and length bound enforced on
// StreamParams.ToolChoiceName before it is serialised onto any provider
// wire. It is the intersection of the three providers' documented
// function-name grammars (Anthropic's `^[a-zA-Z0-9_-]{1,64}$` is the
// tightest, so it governs). See ValidateToolChoiceName.
var toolChoiceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// ValidateToolChoiceName reports whether a named-tool choice's tool name
// is safe to emit onto a provider request body. It enforces
// `^[a-zA-Z0-9_-]{1,64}$` — the intersection of the Anthropic, OpenAI,
// and Gemini function-name grammars. A1 owns the tool-choice wire format,
// so validation lives at the serialization boundary: the loop escalation
// path (A2) will be the first caller to feed model-influenced names
// through ToolChoiceName, and adapters call this to fail closed (degrade
// the named-tool form to auto) rather than emit an unvalidated value.
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
	// Temperature controls sampling randomness. A nil pointer means "use
	// the provider's default" — adapters MUST NOT transmit a temperature
	// field on the wire in that case (some endpoints, notably OpenAI's
	// reasoning models on Chat Completions, reject any temperature value
	// including zero). A non-nil pointer transmits the dereferenced value
	// verbatim, including an explicit 0.0 to request greedy decoding.
	// Use Float64Ptr to construct a pointer from a literal.
	Temperature *float64 `json:"temperature,omitempty"`

	// ToolChoice steers tool use for this turn. The zero value
	// (ToolChoiceAuto) preserves the historical behaviour and is omitted
	// from the wire by every adapter, so existing callers are byte-for-
	// byte unchanged. An adapter emits a native tool_choice field only
	// when the resolved provider capability advertises support for the
	// requested mode; otherwise it is a graceful no-op (the prompt-based
	// fallback is the escalation chunk's responsibility, not the
	// adapter's).
	ToolChoice ToolChoiceMode `json:"toolChoice,omitempty"`

	// ToolChoiceName names the specific tool to force when ToolChoice is
	// ToolChoiceTool. Ignored for every other mode. An empty value with
	// ToolChoiceTool degrades to ToolChoiceAuto at the adapter.
	ToolChoiceName string `json:"toolChoiceName,omitempty"`

	// ParallelToolCalls steers whether the provider may emit more than one
	// tool call in a single turn (issue #222). It is a *bool, mirroring the
	// Temperature precedent above: a nil pointer is the zero value and means
	// "say nothing on the wire", so the request is byte-identical to the
	// pre-#222 shape and every existing caller is unaffected. A non-nil
	// value is projected onto the provider's native control only when the
	// resolved ParallelToolCalls capability advertises support — top-level
	// `parallel_tool_calls` on OpenAI Chat/Responses, `disable_parallel_tool_use`
	// inside Anthropic's tool_choice object — and is a graceful no-op
	// otherwise (Gemini and Bedrock have no native control). Parallelism is
	// an efficiency hint, not a correctness lever, so an unsupported provider
	// gets no prompt-based fallback the way ToolChoice does.
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
// Type discriminates the event shape. Recognised values:
//
//   - "text_delta", "tool_call", "tool_result", "done", "error", "warning",
//     "heartbeat", "ready"
//   - "permission_request"   — emitted by the AskUpstreamPolicy when a tool
//     call needs operator approval; correlated by RequestID with the
//     incoming "permission_response" ControlEvent.
//   - "tool_result_request"  — emitted by the agentic loop when an async
//     tool defers its result; correlated by RequestID with the incoming
//     "tool_result_response" ControlEvent. Carries ToolUseID, ToolName and
//     Input so the control plane can correlate to the original tool_call.
//   - "batch_submission"     — emitted by the gRPC BatchAdapter for a turn
//     it wants the control plane to dispatch as a batch entry; Input carries
//     the provider-shaped request body, RequestID correlates the matching
//     "batch_result" ControlEvent.
//   - "batch_waiting"        — periodic heartbeat from the BatchAdapter
//     while a "batch_submission" is in flight; RequestID echoes the
//     originating submission so the control plane can keep its slot alive.
//   - "batch_cancel_request" — emitted by the BatchAdapter when the run
//     cancels or its wall-clock cap fires and the operator opted into
//     CancelBundleOnRunCancel; RequestID echoes the submission so the
//     control plane can cancel the matching provider-side batch entry.
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
// Type discriminates the event shape. Recognised values:
//
//   - "task_assignment", "user_response", "cancel"
//   - "permission_response"   — completes a "permission_request" HarnessEvent.
//     RequestID echoes the originating request; Allowed (and optional Reason)
//     carry the decision.
//   - "tool_result_response"  — completes a "tool_result_request" HarnessEvent
//     for an async tool. RequestID echoes the originating request; Content
//     carries the result payload; IsError, when set, marks the result as a
//     tool failure (the model sees IsError=true on the ToolResult).
//   - "batch_result"          — completes a "batch_submission" HarnessEvent.
//     RequestID echoes the originating submission; Content carries the JSON
//     BatchResult (provider response on success, BatchResultError on failure)
//     consumed by harness/internal/provider.decodeBatchResult.
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
