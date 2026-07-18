package trace

import (
	"encoding/json"
	"log/slog"

	"go.opentelemetry.io/otel/attribute"

	"github.com/rxbynerd/stirrup/types"
)

// This file maps stirrup's transcript types onto the OTel GenAI
// semconv message schemas for the opt-in content-capture path
// (traceEmitter.captureContent). See docs/observability-cloud.md
// for the attribute reference and safety properties.
//
// Every value serialised here MUST already be scrubbed: callers pass
// content through scrubTurnRecord / security.Scrub before these
// helpers run.

// turnContent carries the pre-serialised GenAI message attributes for
// one captured turn span.
type turnContent struct {
	inputMessages  string
	outputMessages string
}

// attributes renders the non-empty content fields (plus the run-level
// system instructions, pre-serialised by RecordSystemInstructions) as
// span attributes. Empty values are skipped so spans never carry
// empty-string content attributes.
func (c *turnContent) attributes(systemInstructionsJSON string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 3)
	if c.inputMessages != "" {
		attrs = append(attrs, attribute.String(genAIInputMessagesKey, c.inputMessages))
	}
	if c.outputMessages != "" {
		attrs = append(attrs, attribute.String(genAIOutputMessagesKey, c.outputMessages))
	}
	if systemInstructionsJSON != "" {
		attrs = append(attrs, attribute.String(genAISystemInstructionsKey, systemInstructionsJSON))
	}
	return attrs
}

// toolContent carries the content attributes for one captured
// execute_tool span: the tool_use ID and the call's arguments/result.
// Arguments and result are the semconv gen_ai.tool.call.{arguments,
// result} attributes ("Any" typed in the spec; the JSON-string /
// plain-string fallback applies, same as the message attributes).
type toolContent struct {
	id        string
	arguments json.RawMessage
	result    string
}

// attributes renders the non-empty fields as span attributes, mirroring
// turnContent.attributes' skip-empty contract.
func (c *toolContent) attributes() []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 3)
	if c.id != "" {
		attrs = append(attrs, attribute.String(genAIToolCallIDKey, c.id))
	}
	if len(c.arguments) > 0 {
		attrs = append(attrs, attribute.String(genAIToolCallArgumentsKey, string(c.arguments)))
	}
	if c.result != "" {
		attrs = append(attrs, attribute.String(genAIToolCallResultKey, c.result))
	}
	return attrs
}

// genAIMessage mirrors the ChatMessage / OutputMessage shape shared by
// the input- and output-messages schemas. FinishReason is only set on
// output messages (omitempty keeps it off input messages).
type genAIMessage struct {
	Role         string      `json:"role"`
	Parts        []genAIPart `json:"parts"`
	FinishReason string      `json:"finish_reason,omitempty"`
}

// genAIPart is the union of the schema part shapes. Type selects the
// variant: "text" carries Content; "tool_call" carries ID/Name/Arguments;
// "tool_call_response" carries ID/Result. omitempty keeps each variant's
// unused fields off the wire.
type genAIPart struct {
	Type      string          `json:"type"`
	Content   string          `json:"content,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Result    string          `json:"result,omitempty"`
}

// genAIInputMessagesJSON serialises the turn's message history as the
// gen_ai.input.messages attribute value. Roles are stirrup's wire roles
// verbatim ("user" / "assistant") — the schema documents role as "the
// actual role used by the GenAI system", and stirrup transmits tool
// results inside user messages (the Anthropic shape), so no synthetic
// "tool" role is invented. Returns "" when there is nothing to record.
func genAIInputMessagesJSON(messages []types.Message) string {
	if len(messages) == 0 {
		return ""
	}
	msgs := make([]genAIMessage, 0, len(messages))
	for _, m := range messages {
		msgs = append(msgs, genAIMessage{
			Role:  m.Role,
			Parts: genAIParts(m.Content),
		})
	}
	return marshalGenAI(msgs)
}

// genAIOutputMessagesJSON serialises the model's output blocks as the
// gen_ai.output.messages attribute value: a single assistant message
// (stirrup streams exactly one choice per turn). finishReason is the
// raw provider stop reason, matching the vocabulary the span's
// gen_ai.response.finish_reasons attribute already uses; empty is
// omitted (the unpaired-record fallback path has no summary to read a
// stop reason from).
func genAIOutputMessagesJSON(blocks []types.ContentBlock, finishReason string) string {
	if len(blocks) == 0 {
		return ""
	}
	return marshalGenAI([]genAIMessage{{
		Role:         "assistant",
		Parts:        genAIParts(blocks),
		FinishReason: finishReason,
	}})
}

// genAISystemInstructionsJSON serialises the run's system prompt as the
// gen_ai.system_instructions attribute value: a single text part, per
// the schema's "provided separately from the chat history" shape (the
// system prompt is a dedicated request field on every stirrup provider
// adapter, not a message). Returns "" when no prompt was recorded.
func genAISystemInstructionsJSON(system string) string {
	if system == "" {
		return ""
	}
	return marshalGenAI([]genAIPart{{Type: "text", Content: system}})
}

// genAIParts maps stirrup content blocks onto schema parts.
//
//   - "text"        → TextPart{type: "text", content}
//   - "tool_use"    → ToolCallRequestPart{type: "tool_call", id, name, arguments}
//   - "tool_result" → ToolCallResponsePart{type: "tool_call_response", id, result}
//
// Anything else is deliberately dropped: unknown block types have no
// schema shape, and the one concrete case today — Gemini's opaque
// ThoughtSignature — is provider state the harness must never log.
// A tool_result's optional Structured envelope is likewise not
// serialised; Content is the canonical text rendering and the
// structured payload has no part shape in the schema. Text blocks
// with empty content (e.g. a placeholder block a provider emitted
// alongside tool calls) are skipped rather than serialised as an
// empty part.
func genAIParts(blocks []types.ContentBlock) []genAIPart {
	parts := make([]genAIPart, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			parts = append(parts, genAIPart{Type: "text", Content: b.Text})
		case "tool_use":
			parts = append(parts, genAIPart{
				Type:      "tool_call",
				ID:        b.ID,
				Name:      b.Name,
				Arguments: validRawJSON(b.Input),
			})
		case "tool_result":
			parts = append(parts, genAIPart{
				Type:   "tool_call_response",
				ID:     b.ToolUseID,
				Result: b.Content,
			})
		}
	}
	return parts
}

// validRawJSON guards a json.RawMessage destined for re-marshalling:
// an invalid payload (a provider emitting a malformed tool_use input
// would be the only path here) is wrapped as a JSON string literal so
// marshalGenAI cannot fail on it, mirroring scrubRawJSON's
// shape-preserving fallback.
func validRawJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	if json.Valid(raw) {
		return raw
	}
	wrapped, err := json.Marshal(string(raw))
	if err != nil {
		// Unreachable in practice (marshalling a string cannot fail);
		// drop to an empty object so the part stays well-formed.
		return json.RawMessage(`{}`)
	}
	return wrapped
}

// marshalGenAI serialises a schema value to the JSON-string attribute
// encoding. Marshal failure is effectively unreachable — every
// embedded RawMessage passed validRawJSON — but is handled by warning
// (without the payload; the value is already scrubbed but error text
// composition is not worth the risk) and dropping the attribute.
func marshalGenAI(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		slog.Default().Warn("genai content attribute marshal failed; dropping attribute", "error", err)
		return ""
	}
	return string(data)
}
