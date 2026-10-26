package trace

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

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
// one captured chat span.
type turnContent struct {
	inputMessages  string
	outputMessages string
}

// attributes renders the non-empty content fields (plus the run-level
// system instructions, pre-serialised by RecordSystemInstructions) as
// span attributes, each capped to maxBytes (0 = no cap). Empty values
// are skipped so spans never carry empty-string content attributes.
func (c *turnContent) attributes(systemInstructionsJSON string, maxBytes int) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 3)
	if c.inputMessages != "" {
		attrs = appendContentAttribute(attrs, genAIInputMessagesKey, c.inputMessages, maxBytes)
	}
	if c.outputMessages != "" {
		attrs = appendContentAttribute(attrs, genAIOutputMessagesKey, c.outputMessages, maxBytes)
	}
	if systemInstructionsJSON != "" {
		attrs = appendContentAttribute(attrs, genAISystemInstructionsKey, systemInstructionsJSON, maxBytes)
	}
	return attrs
}

// toolContent carries the content attributes for one captured
// execute_tool span: the call's arguments/result. They are the semconv
// gen_ai.tool.call.{arguments, result} attributes ("Any" typed in the
// spec; the JSON-string / plain-string fallback applies, same as the
// message attributes).
type toolContent struct {
	arguments json.RawMessage
	result    string
}

// attributes renders the non-empty fields as span attributes, mirroring
// turnContent.attributes' skip-empty and cap contract.
func (c *toolContent) attributes(maxBytes int) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 2)
	if len(c.arguments) > 0 {
		attrs = appendContentAttribute(attrs, genAIToolCallArgumentsKey, string(c.arguments), maxBytes)
	}
	if c.result != "" {
		attrs = appendContentAttribute(attrs, genAIToolCallResultKey, c.result, maxBytes)
	}
	return attrs
}

// contentTruncatedKey names the boolean attribute that marks a content
// attribute as capped by captureContentMaxBytes.
func contentTruncatedKey(key string) string {
	return "stirrup." + key + ".truncated"
}

// appendContentAttribute appends key=value, capping a value longer than
// maxBytes (0 = no cap) and marking it with contentTruncatedKey.
func appendContentAttribute(attrs []attribute.KeyValue, key, value string, maxBytes int) []attribute.KeyValue {
	if maxBytes > 0 && len(value) > maxBytes {
		value = capContentValue(key, value, maxBytes)
		attrs = append(attrs, attribute.Bool(contentTruncatedKey(key), true))
	}
	return append(attrs, attribute.String(key, value))
}

// capContentValue shortens a content attribute value to at most maxBytes
// while keeping JSON-valued attributes valid JSON. Message arrays drop
// whole messages, part arrays drop whole parts, and only when a single
// element cannot fit is its text shortened. Input history keeps its
// newest messages; output and system instructions keep their leading
// parts. Tool-call arguments become a JSON string of the truncated text.
// A hook output tail keeps its trailing bytes.
func capContentValue(key, value string, maxBytes int) string {
	var capped, minimal string
	switch key {
	case hookOutputTailKey:
		return truncateUTF8Tail(value, maxBytes)
	case genAIInputMessagesKey:
		capped, minimal = capMessagesJSON(value, maxBytes, true), "[]"
	case genAIOutputMessagesKey:
		capped, minimal = capMessagesJSON(value, maxBytes, false), "[]"
	case genAISystemInstructionsKey:
		capped, minimal = capPartsJSON(value, maxBytes), "[]"
	case genAIToolCallArgumentsKey:
		capped, minimal = fitJSONString(value, maxBytes), `""`
	default:
		return truncateUTF8(value, maxBytes)
	}
	if capped == "" || len(capped) > maxBytes {
		return minimal
	}
	return capped
}

// capMessagesJSON keeps the longest run of whole messages from one end
// of the array that fits maxBytes, shrinking the end message's parts
// when none fits whole. Returns "" when nothing fits.
func capMessagesJSON(value string, maxBytes int, keepNewest bool) string {
	var msgs []genAIMessage
	if err := json.Unmarshal([]byte(value), &msgs); err != nil || len(msgs) == 0 {
		return ""
	}
	if keepNewest {
		slices.Reverse(msgs)
	}
	encoded := make([]string, len(msgs))
	for i, m := range msgs {
		encoded[i] = marshalGenAI(m)
	}
	kept := fitElements(encoded, maxBytes)
	if len(kept) == 0 {
		msg := msgs[0]
		envelope := msg
		envelope.Parts = []genAIPart{}
		budget := maxBytes - len("[]") - (len(marshalGenAI(envelope)) - len("[]"))
		parts := fitParts(msg.Parts, budget)
		if parts == nil {
			return ""
		}
		msg.Parts = parts
		return "[" + marshalGenAI(msg) + "]"
	}
	if keepNewest {
		slices.Reverse(kept)
	}
	return "[" + strings.Join(kept, ",") + "]"
}

// capPartsJSON caps a JSON array of parts to maxBytes, or returns "" when
// nothing fits.
func capPartsJSON(value string, maxBytes int) string {
	var parts []genAIPart
	if err := json.Unmarshal([]byte(value), &parts); err != nil || len(parts) == 0 {
		return ""
	}
	kept := fitParts(parts, maxBytes)
	if kept == nil {
		return ""
	}
	return marshalGenAI(kept)
}

// fitParts returns the leading parts whose JSON array fits maxBytes,
// shortening the first part when it does not fit whole. Returns nil when
// not even a shortened first part fits.
func fitParts(parts []genAIPart, maxBytes int) []genAIPart {
	if len(parts) == 0 {
		return nil
	}
	encoded := make([]string, len(parts))
	for i, p := range parts {
		encoded[i] = marshalGenAI(p)
	}
	if n := len(fitElements(encoded, maxBytes)); n > 0 {
		return parts[:n]
	}
	shrunk, ok := shrinkPart(parts[0], maxBytes-len("[]"))
	if !ok {
		return nil
	}
	return []genAIPart{shrunk}
}

// fitElements returns the longest prefix of encoded JSON values whose
// array encoding ("[a,b,...]") fits maxBytes.
func fitElements(encoded []string, maxBytes int) []string {
	size := len("[]")
	for i, e := range encoded {
		add := len(e)
		if i > 0 {
			add++
		}
		if size+add > maxBytes {
			return encoded[:i]
		}
		size += add
	}
	return encoded
}

// shrinkPart replaces the part's text payload (text content, tool
// response, or tool-call arguments, the last re-encoded as a JSON
// string) with its longest prefix whose part encoding fits maxBytes.
func shrinkPart(p genAIPart, maxBytes int) (genAIPart, bool) {
	var text string
	var with func(string) genAIPart
	switch {
	case p.Response != nil:
		text = *p.Response
		with = func(s string) genAIPart { q := p; q.Response = &s; return q }
	case len(p.Arguments) > 0:
		text = string(p.Arguments)
		with = func(s string) genAIPart { q := p; q.Arguments = jsonStringLiteral(s); return q }
	default:
		text = p.Content
		with = func(s string) genAIPart { q := p; q.Content = s; return q }
	}
	prefix, ok := longestFittingPrefix(text, func(s string) bool {
		return len(marshalGenAI(with(s))) <= maxBytes
	})
	if !ok {
		return p, false
	}
	return with(prefix), true
}

// fitJSONString encodes the longest prefix of s as a JSON string literal
// that fits maxBytes, or returns "" when not even an empty literal fits.
func fitJSONString(s string, maxBytes int) string {
	prefix, ok := longestFittingPrefix(s, func(s string) bool {
		return len(jsonStringLiteral(s)) <= maxBytes
	})
	if !ok {
		return ""
	}
	return string(jsonStringLiteral(prefix))
}

// longestFittingPrefix returns the longest UTF-8-safe prefix of s that
// satisfies fits, which must be monotonic in prefix length. JSON escaping
// makes encoded size non-linear in raw length, hence the search.
func longestFittingPrefix(s string, fits func(string) bool) (string, bool) {
	if !fits("") {
		return "", false
	}
	lo, hi := 0, len(s)
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if fits(truncateUTF8(s, mid)) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return truncateUTF8(s, lo), true
}

func jsonStringLiteral(s string) json.RawMessage {
	data, err := json.Marshal(s)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return data
}

// truncateUTF8 returns the longest prefix of s of at most n bytes that
// does not split a UTF-8 sequence.
func truncateUTF8(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// truncateUTF8Tail returns the last n bytes of s or fewer, starting on a
// rune boundary.
func truncateUTF8Tail(s string, n int) string {
	if n >= len(s) {
		return s
	}
	if n <= 0 {
		return ""
	}
	start := len(s) - n
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
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
// "tool_call_response" carries ID/Response; "server_tool_call" carries
// ID/Name/ServerToolCall; "server_tool_call_response" carries
// ID/ServerToolCallResponse. omitempty keeps each variant's unused fields
// off the wire; Name and Response are pointers because the schema requires
// the key on the variants that carry them even when the value is empty.
type genAIPart struct {
	Type      string          `json:"type"`
	Content   string          `json:"content,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      *string         `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Response  *string         `json:"response,omitempty"`

	ServerToolCall         *genAIServerToolCall         `json:"server_tool_call,omitempty"`
	ServerToolCallResponse *genAIServerToolCallResponse `json:"server_tool_call_response,omitempty"`
}

// genAIToolSearchType is the server tool type a provider-side tool search
// and its result are recorded under.
const genAIToolSearchType = "tool_search"

// genAIServerToolCall is a GenericServerToolCall: a type discriminator
// plus the call's arguments.
type genAIServerToolCall struct {
	Type      string          `json:"type"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// genAIServerToolCallResponse is a GenericServerToolCallResponse for a
// tool search: the tools it loaded, or its error code. Both are pointers
// so a search that loaded nothing still carries an empty list and an
// error without a code still carries the key.
type genAIServerToolCallResponse struct {
	Type           string    `json:"type"`
	ToolReferences *[]string `json:"tool_references,omitempty"`
	Error          *string   `json:"error,omitempty"`
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
//   - "text"               → TextPart{type: "text", content}
//   - "tool_use"           → ToolCallRequestPart{type: "tool_call", id, name, arguments}
//   - "tool_result"        → ToolCallResponsePart{type: "tool_call_response", id, result}
//   - "tool_search"        → ServerToolCallPart{type: "server_tool_call", id, name,
//     server_tool_call: {type: "tool_search", arguments}}
//   - "tool_search_result" → ServerToolCallResponsePart{type: "server_tool_call_response",
//     id, server_tool_call_response: {type: "tool_search", tool_references | error}}
//
// Anything else is deliberately dropped: unknown block types have no
// schema shape, and thinking / redacted_thinking blocks carry
// provider-opaque signatures the harness must never log. A tool_result's
// optional Structured envelope is likewise not serialised; Content is the
// canonical text rendering and the structured payload has no part shape in
// the schema. Text blocks with empty content (e.g. a placeholder block a
// provider emitted alongside tool calls) are skipped rather than
// serialised as an empty part.
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
				Name:      &b.Name,
				Arguments: validRawJSON(b.Input),
			})
		case "tool_result":
			response := b.Content
			parts = append(parts, genAIPart{
				Type:     "tool_call_response",
				ID:       b.ToolUseID,
				Response: &response,
			})
		case "tool_search":
			parts = append(parts, genAIPart{
				Type:           "server_tool_call",
				ID:             b.ID,
				Name:           &b.Name,
				ServerToolCall: &genAIServerToolCall{Type: genAIToolSearchType, Arguments: validRawJSON(b.Input)},
			})
		case "tool_search_result":
			parts = append(parts, genAIPart{
				Type:                   "server_tool_call_response",
				ID:                     b.ToolUseID,
				ServerToolCallResponse: toolSearchResponse(b),
			})
		}
	}
	return parts
}

// toolSearchResponse returns a tool_search_result block's outcome: its
// error code when the search failed, otherwise the tools it loaded.
func toolSearchResponse(b types.ContentBlock) *genAIServerToolCallResponse {
	response := &genAIServerToolCallResponse{Type: genAIToolSearchType}
	if b.IsError {
		code := b.Name
		response.Error = &code
		return response
	}
	refs := append([]string{}, b.ToolReferences...)
	response.ToolReferences = &refs
	return response
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
