package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// OpenAIAuthConfig carries the optional auth/URL knobs both OpenAI adapters
// share. It is a separate struct so the adapter constructors do not grow an
// unbounded number of positional arguments. A zero value preserves today's
// behaviour: Authorization: Bearer auth and no extra query parameters.
type OpenAIAuthConfig struct {
	// APIKeyHeader, when non-empty, replaces the default
	// "Authorization: Bearer <key>" header with "<APIKeyHeader>: <key>".
	// Used by Azure OpenAI key auth ("api-key") and similar gateways.
	APIKeyHeader string

	// QueryParams are appended to every request URL. Keys here override any
	// duplicate keys already present in BaseURL's query string — explicit
	// configuration always wins over BaseURL-encoded defaults.
	QueryParams map[string]string
}

const (
	openaiDefaultBaseURL   = "https://api.openai.com/v1"
	openaiMaxToolInputSize = 10 * 1024 * 1024 // 10 MB cap on streamed tool argument JSON

	// maxSSEScannerBuffer raises bufio.Scanner's 64 KB default: a single SSE
	// line (large reasoning_content, a big tool-call args blob) can exceed it.
	maxSSEScannerBuffer = 16 * 1024 * 1024

	// maxReplayFieldBytes caps the per-stream ReplayFields accumulator so a
	// malicious or misconfigured provider cannot balloon memory usage.
	// Overflow truncates to a clean prefix and logs one WARN per stream.
	maxReplayFieldBytes = 512 * 1024
)

// OpenAICompatibleAdapter implements ProviderAdapter for the OpenAI Chat
// Completions API. It works with any OpenAI-compatible endpoint: OpenAI,
// LiteLLM, Azure OpenAI, vLLM, Ollama, llama.cpp server.
//
// Azure OpenAI deployments accept either Entra ID bearer tokens (default:
// empty APIKeyHeader) or a plain API key (APIKeyHeader: "api-key");
// required api-version pins go through OpenAIAuthConfig.QueryParams.
type OpenAICompatibleAdapter struct {
	bearer       credential.BearerTokenFunc
	httpClient   *http.Client
	baseURL      string
	apiKeyHeader string
	queryParams  map[string]string

	// AdapterDeps carries the factory-injected Tracer/Metrics/RetryPolicy/
	// Logger; see its doc comment for the field-by-field contract.
	AdapterDeps

	// Registry resolves per-(provider, model) wire-shape and behaviour
	// overrides at the top of every Stream call. Seeded by the
	// constructor with quirks.DefaultRegistry().
	Registry *quirks.Registry

	// strictSchemas memoises strict-mode schema rewrites within this
	// adapter's lifetime, keyed by (model, tool-name, schema-hash). nil
	// disables caching; the constructor initialises a non-nil instance.
	strictSchemas *strictSchemaCache
}

// NewOpenAICompatibleAdapter creates an adapter for an OpenAI-compatible
// Chat Completions endpoint. baseURL is the API root (e.g.
// "https://api.openai.com/v1"); "/chat/completions" is appended
// automatically, and an empty string defaults to the OpenAI URL. auth
// carries optional header-name and query-parameter overrides. retry is the
// resolved retry policy; a zero RetryPolicy disables retries.
//
// bearer is invoked on every Stream call to fetch the current API key,
// letting refresh-aware credentials (e.g. Azure Entra ID) rotate tokens
// transparently. A nil bearer or empty-string return means no auth header.
func NewOpenAICompatibleAdapter(bearer credential.BearerTokenFunc, baseURL string, auth OpenAIAuthConfig, retry RetryPolicy) *OpenAICompatibleAdapter {
	if baseURL == "" {
		baseURL = openaiDefaultBaseURL
	}

	baseURL = strings.TrimRight(baseURL, "/")
	return &OpenAICompatibleAdapter{
		bearer: bearer,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
		baseURL:       baseURL,
		apiKeyHeader:  auth.APIKeyHeader,
		queryParams:   auth.QueryParams,
		AdapterDeps:   AdapterDeps{RetryPolicy: retry},
		Registry:      quirks.DefaultRegistry(),
		strictSchemas: newStrictSchemaCache(),
	}
}

// openaiRequest is the JSON body sent to the Chat Completions API.
//
// TokenField, OmitSamplingParams, and ExtraBodyFields carry the resolved
// quirks (see docs/provider-quirks.md) for this request; none is serialised
// under its own JSON key, all three steer MarshalJSON's projection instead.
type openaiRequest struct {
	Model              string          `json:"-"`
	Messages           []openaiMessage `json:"-"`
	Tools              []openaiTool    `json:"-"`
	MaxTokens          int             `json:"-"`
	Temperature        *float64        `json:"-"`
	Stream             bool            `json:"-"`
	TokenField         quirks.OpenAITokenField
	OmitSamplingParams bool
	ExtraBodyFields    map[string]any
	// ToolChoice is the wire value for the OpenAI "tool_choice" field:
	// a string ("auto"/"required"/"none") or an object naming a function.
	// A nil interface omits the field entirely; steers MarshalJSON only.
	ToolChoice any
	// ParallelToolCalls is the wire value for the top-level
	// "parallel_tool_calls" bool. A nil pointer omits the field; steers
	// MarshalJSON only.
	ParallelToolCalls *bool
}

// MarshalJSON projects the canonical openaiRequest into the wire body the
// resolved quirks selected: the token-budget key follows TokenField,
// "temperature" is suppressed when OmitSamplingParams is set, and
// ExtraBodyFields are merged in after the canonical fields (a key collision
// with a canonical field is an error).
func (r openaiRequest) MarshalJSON() ([]byte, error) {
	tokenKey := openAIWireTokenKey(r.TokenField)
	out := map[string]any{
		"model":    r.Model,
		"messages": r.Messages,
		"stream":   r.Stream,
		tokenKey:   r.MaxTokens,
	}
	// Without stream_options.include_usage, streaming responses carry no
	// usage block; it rides in a trailing chunk with an empty choices array.
	if r.Stream {
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(r.Tools) > 0 {
		out["tools"] = r.Tools
	}
	if r.ToolChoice != nil {
		out["tool_choice"] = r.ToolChoice
	}
	if r.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *r.ParallelToolCalls
	}
	if !r.OmitSamplingParams && r.Temperature != nil {
		out["temperature"] = *r.Temperature
	}
	for k, v := range r.ExtraBodyFields {
		if _, exists := out[k]; exists {
			return nil, fmt.Errorf("openai quirk extra body field %q collides with canonical request field", k)
		}
		// Also reject collisions against canonical fields elided above
		// (e.g. temperature when suppressed) so a rule cannot sneak a
		// field past via the extras map.
		if isCanonicalOpenAIField(k) {
			return nil, fmt.Errorf("openai quirk extra body field %q collides with canonical request field", k)
		}
		out[k] = v
	}
	return json.Marshal(out)
}

// UnmarshalJSON is the inverse of MarshalJSON: it reads either
// "max_completion_tokens" or "max_tokens" into MaxTokens (setting
// TokenField accordingly) and populates the canonical fields. Used by
// tests that round-trip the wire body through the same struct that
// produced it. Any non-canonical top-level keys are collected into
// ExtraBodyFields so the round-trip is loss-free.
func (r *openaiRequest) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	if v, ok := raw["model"]; ok {
		if err := json.Unmarshal(v, &r.Model); err != nil {
			return fmt.Errorf("openaiRequest.model: %w", err)
		}
		delete(raw, "model")
	}
	if v, ok := raw["messages"]; ok {
		if err := json.Unmarshal(v, &r.Messages); err != nil {
			return fmt.Errorf("openaiRequest.messages: %w", err)
		}
		delete(raw, "messages")
	}
	if v, ok := raw["tools"]; ok {
		if err := json.Unmarshal(v, &r.Tools); err != nil {
			return fmt.Errorf("openaiRequest.tools: %w", err)
		}
		delete(raw, "tools")
	}
	if v, ok := raw["stream"]; ok {
		if err := json.Unmarshal(v, &r.Stream); err != nil {
			return fmt.Errorf("openaiRequest.stream: %w", err)
		}
		delete(raw, "stream")
	}
	// stream_options is derived from Stream by MarshalJSON, not stored on
	// the struct. Consume it so a Marshal→Unmarshal round-trip does not
	// leak it into ExtraBodyFields (which would then collide on re-marshal).
	delete(raw, "stream_options")
	if v, ok := raw["temperature"]; ok {
		var t float64
		if err := json.Unmarshal(v, &t); err != nil {
			return fmt.Errorf("openaiRequest.temperature: %w", err)
		}
		r.Temperature = &t
		delete(raw, "temperature")
	}
	if v, ok := raw["tool_choice"]; ok {
		var tc any
		if err := json.Unmarshal(v, &tc); err != nil {
			return fmt.Errorf("openaiRequest.tool_choice: %w", err)
		}
		r.ToolChoice = tc
		delete(raw, "tool_choice")
	}
	if v, ok := raw["parallel_tool_calls"]; ok {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return fmt.Errorf("openaiRequest.parallel_tool_calls: %w", err)
		}
		r.ParallelToolCalls = &b
		delete(raw, "parallel_tool_calls")
	}
	// Token budget: accept either canonical key. MarshalJSON emits
	// exactly one key, so a valid request body should not contain
	// both simultaneously; if both are present, reject rather than let one
	// silently clobber the other depending on decode order.
	_, hasMCT := raw["max_completion_tokens"]
	_, hasMT := raw["max_tokens"]
	if hasMCT && hasMT {
		return fmt.Errorf("openaiRequest: both max_completion_tokens and max_tokens present")
	}
	if v, ok := raw["max_completion_tokens"]; ok {
		if err := json.Unmarshal(v, &r.MaxTokens); err != nil {
			return fmt.Errorf("openaiRequest.max_completion_tokens: %w", err)
		}
		r.TokenField = quirks.TokenFieldMaxCompletionTokens
		delete(raw, "max_completion_tokens")
	}
	if v, ok := raw["max_tokens"]; ok {
		if err := json.Unmarshal(v, &r.MaxTokens); err != nil {
			return fmt.Errorf("openaiRequest.max_tokens: %w", err)
		}
		r.TokenField = quirks.TokenFieldMaxTokens
		delete(raw, "max_tokens")
	}

	if len(raw) > 0 {
		extra := make(map[string]any, len(raw))
		for k, v := range raw {
			var anyV any
			if err := json.Unmarshal(v, &anyV); err != nil {
				return fmt.Errorf("openaiRequest.%s: %w", k, err)
			}
			extra[k] = anyV
		}
		r.ExtraBodyFields = extra
	}
	return nil
}

// openAIWireTokenKey returns the wire JSON key for the resolved token
// field, defaulting to "max_completion_tokens" for the zero value.
func openAIWireTokenKey(f quirks.OpenAITokenField) string {
	if f == quirks.TokenFieldMaxTokens {
		return "max_tokens"
	}
	return "max_completion_tokens"
}

// summarizeReplayCaptures sums per-path piece counts and the string-or-
// JSON-encoded length of every captured value, returned as totals only (not
// per-path) so callers can attach it to an OTel span without leaking field
// names.
func summarizeReplayCaptures(capture map[string][]any) (totalCount, totalLen int) {
	for _, values := range capture {
		totalCount += len(values)
		for _, v := range values {
			switch s := v.(type) {
			case string:
				totalLen += len(s)
			default:
				b, err := json.Marshal(v)
				if err == nil {
					totalLen += len(b)
				}
			}
		}
	}
	return totalCount, totalLen
}

// logReplayFieldsCapture emits a debug-level, length-only summary of the
// per-stream ReplayFields capture: the captured values are provider-private
// blobs (DeepSeek's reasoning_content, Gemini's thoughtSignature) that must
// never reach a log sink. Shared by both the openai and gemini adapters.
func logReplayFieldsCapture(ctx context.Context, logger *slog.Logger, providerType, model string, capture map[string][]any) {
	if logger == nil {
		logger = slog.Default()
	}
	// Sort so output is deterministic despite Go's random map order.
	paths := make([]string, 0, len(capture))
	for k := range capture {
		paths = append(paths, k)
	}
	sort.Strings(paths)

	summaries := make([]any, 0, len(paths))
	for _, p := range paths {
		values := capture[p]
		totalLen := 0
		for _, v := range values {
			switch s := v.(type) {
			case string:
				totalLen += len(s)
			default:
				b, err := json.Marshal(v)
				if err == nil {
					totalLen += len(b)
				}
			}
		}
		summaries = append(summaries,
			slog.Group(p,
				slog.Int("count", len(values)),
				slog.Int("total_len", totalLen),
			),
		)
	}
	logger.DebugContext(ctx, "quirks replay fields captured",
		slog.String("provider.type", providerType),
		slog.String("provider.model", model),
		slog.Group("replay_fields_captured", summaries...),
	)
}

// replayCaptureByteLen returns the byte-length proxy for one captured
// replay value: raw length for strings, JSON-encoded length otherwise
// (zero on a marshal failure). The same proxy summarizeReplayCaptures
// and logReplayFieldsCapture use, so the maxReplayFieldBytes cap
// accounting agrees with the observability surfaces.
func replayCaptureByteLen(v any) int {
	if s, ok := v.(string); ok {
		return len(s)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// flattenReplayCapture projects the per-stream ReplayFields accumulator
// onto the map a "message_complete" StreamEvent carries (see
// docs/provider-quirks.md §3.1 for the flattening rule). JSON nulls are
// stripped before the all-strings check so a provider that opens the
// stream with a null placeholder does not flip the path onto the
// last-value snapshot arm. Returns nil when nothing was captured.
func flattenReplayCapture(capture map[string][]any) map[string]json.RawMessage {
	if len(capture) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(capture))
	for path, values := range capture {
		nonNil := make([]any, 0, len(values))
		for _, v := range values {
			if v != nil {
				nonNil = append(nonNil, v)
			}
		}
		if len(nonNil) == 0 {
			continue
		}
		allStrings := true
		for _, v := range nonNil {
			if _, ok := v.(string); !ok {
				allStrings = false
				break
			}
		}
		var encoded []byte
		var err error
		if allStrings {
			var b strings.Builder
			for _, v := range nonNil {
				b.WriteString(v.(string))
			}
			encoded, err = json.Marshal(b.String())
		} else {
			encoded, err = json.Marshal(nonNil[len(nonNil)-1])
		}
		if err != nil {
			continue
		}
		out[path] = encoded
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// canonicalOpenAIFields enumerates the top-level Chat Completions request
// fields the adapter emits. ExtraBodyFields rules must not collide with any.
var canonicalOpenAIFields = map[string]struct{}{
	"model":                 {},
	"messages":              {},
	"tools":                 {},
	"tool_choice":           {},
	"max_completion_tokens": {},
	"max_tokens":            {},
	"temperature":           {},
	"top_p":                 {},
	"presence_penalty":      {},
	"frequency_penalty":     {},
	"logprobs":              {},
	"top_logprobs":          {},
	"logit_bias":            {},
	"stream":                {},
	"stream_options":        {},
	"parallel_tool_calls":   {},
}

// isCanonicalOpenAIField gates ExtraBodyFields merges to prevent a rule
// from overriding a struct-mediated field by way of the extras map.
func isCanonicalOpenAIField(k string) bool {
	_, ok := canonicalOpenAIFields[k]
	return ok
}

// openaiMessage is a single message in OpenAI's Chat Completions format.
//
// ReplayFields carries provider-opaque round-trip state (see
// docs/provider-quirks.md) emitted as additional top-level keys on an
// assistant message, keyed by the rule's single-segment path. Populated by
// translateMessages only for paths the resolved quirks name for THIS
// stream, so stale state from a different model never leaks onto the wire.
type openaiMessage struct {
	Role         string                     `json:"role"` // "system" | "user" | "assistant" | "tool"
	Content      any                        `json:"content,omitempty"`
	ToolCalls    []openaiToolCall           `json:"tool_calls,omitempty"`
	ToolCallID   string                     `json:"tool_call_id,omitempty"`
	ReplayFields map[string]json.RawMessage `json:"-"`
}

// canonicalOpenAIMessageFields enumerates the top-level Chat Completions
// message keys the adapter owns; a ReplayFields path colliding with one of
// these is never emitted as an extra key. "name" is included even though
// the struct does not carry it — it is a documented optional message key.
var canonicalOpenAIMessageFields = map[string]struct{}{
	"role":         {},
	"content":      {},
	"tool_calls":   {},
	"tool_call_id": {},
	"name":         {},
}

// isCanonicalOpenAIMessageField reports whether the given key is owned
// by the canonical openaiMessage projection.
func isCanonicalOpenAIMessageField(k string) bool {
	_, ok := canonicalOpenAIMessageFields[k]
	return ok
}

// threadableOpenAIReplayPath reports whether a quirks ReplayFields path can
// be echoed back as a top-level key on an assistant wire message. Only
// single-segment, non-canonical paths qualify — a nested or array-iterated
// path (e.g. Gemini's candidates[].content.parts[]) has no faithful flat
// representation, and canonical keys must not be overwritten via replay.
func threadableOpenAIReplayPath(path string) bool {
	segments, err := quirks.ParseReplayPath(path)
	if err != nil || len(segments) != 1 || segments[0].IsArray {
		return false
	}
	return !isCanonicalOpenAIMessageField(path)
}

// MarshalJSON emits the canonical message fields in struct order, then
// appends the ReplayFields entries (sorted by key for determinism). A
// ReplayFields key that collides with a canonical field, or carries invalid
// JSON, is an error: translateMessages filters both cases, so reaching
// either arm means a caller populated the map by hand.
func (m openaiMessage) MarshalJSON() ([]byte, error) {
	type alias openaiMessage
	base, err := json.Marshal(alias(m))
	if err != nil {
		return nil, err
	}
	if len(m.ReplayFields) == 0 {
		return base, nil
	}
	keys := make([]string, 0, len(m.ReplayFields))
	for k := range m.ReplayFields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// base is always a non-empty object ("role" has no omitempty), so
	// stripping the closing brace and splicing ",<key>:<value>" pairs is
	// safe.
	buf := bytes.NewBuffer(base[:len(base)-1])
	for _, k := range keys {
		if isCanonicalOpenAIMessageField(k) {
			return nil, fmt.Errorf("openai replay field %q collides with canonical message field", k)
		}
		v := m.ReplayFields[k]
		if !json.Valid(v) {
			return nil, fmt.Errorf("openai replay field %q carries invalid JSON", k)
		}
		keyJSON, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.WriteByte(',')
		buf.Write(keyJSON)
		buf.WriteByte(':')
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// openaiToolCall represents a tool invocation in an assistant message.
type openaiToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"` // "function"
	Function openaiToolFunction `json:"function"`
}

// openaiToolFunction is the function payload inside a tool call.
type openaiToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// openaiTool describes a tool in OpenAI's function calling format.
type openaiTool struct {
	Type     string               `json:"type"` // "function"
	Function openaiToolDefinition `json:"function"`
}

// openAINamedToolChoice is the object form of OpenAI's tool_choice that
// forces a specific function. Typed (rather than a map[string]any) so the
// marshalled key order is deterministic — "type" before "function". The
// string forms ("required"/"none") are emitted directly as a string value,
// so only the named form needs a struct.
type openAINamedToolChoice struct {
	Type     string                    `json:"type"` // "function"
	Function openAINamedToolChoiceFunc `json:"function"`
}

// openAINamedToolChoiceFunc is the function payload inside an
// openAINamedToolChoice.
type openAINamedToolChoiceFunc struct {
	Name string `json:"name"`
}

// openaiToolDefinition is the function definition inside an openaiTool.
//
// Strict is a *bool so the zero-value request body omits the field
// entirely; a quirks rule that pins strict mode emits an explicit
// `"strict": true`.
type openaiToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}

// openaiChunk is a single SSE chunk from the streaming Chat Completions API.
type openaiChunk struct {
	ID      string              `json:"id"`
	Choices []openaiChunkChoice `json:"choices"`
	Usage   *openaiUsage        `json:"usage,omitempty"`
}

// openaiChunkChoice is a single choice within a streaming chunk.
//
// RawDelta is the un-decoded JSON bytes of the same delta object Delta
// carries, captured so ReplayFields rules can walk non-canonical fields
// (e.g. DeepSeek's `reasoning_content`) without a typed field on
// openaiDelta. Populated by openaiChunkChoice.UnmarshalJSON.
type openaiChunkChoice struct {
	Index        int             `json:"index"`
	Delta        openaiDelta     `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
	RawDelta     json.RawMessage `json:"-"`
}

// UnmarshalJSON captures the raw bytes of the `delta` field alongside
// the typed decode. The two-pass shape mirrors openaiRequest's
// MarshalJSON / UnmarshalJSON: the typed fields drive the normal SSE
// loop; the RawMessage gives the ReplayFields path walker a document
// to descend without coupling the walker to the typed struct.
func (c *openaiChunkChoice) UnmarshalJSON(data []byte) error {
	type alias openaiChunkChoice
	var helper struct {
		alias
		Delta json.RawMessage `json:"delta"`
	}
	if err := json.Unmarshal(data, &helper); err != nil {
		return err
	}
	*c = openaiChunkChoice(helper.alias)
	if len(helper.Delta) > 0 {
		c.RawDelta = append(c.RawDelta[:0], helper.Delta...)
		if err := json.Unmarshal(helper.Delta, &c.Delta); err != nil {
			// No security.Scrub here: stdlib json decode errors carry
			// only the Go type name and field path, never the value.
			return fmt.Errorf("openaiChunkChoice.delta: %w", err)
		}
	}
	return nil
}

// openaiDelta is the incremental content in a streaming chunk.
type openaiDelta struct {
	Role      string                `json:"role,omitempty"`
	Content   *string               `json:"content,omitempty"`
	ToolCalls []openaiToolCallDelta `json:"tool_calls,omitempty"`
}

// openaiToolCallDelta is an incremental tool call in a streaming chunk.
type openaiToolCallDelta struct {
	Index    int                     `json:"index"`
	ID       string                  `json:"id,omitempty"`
	Type     string                  `json:"type,omitempty"`
	Function openaiToolFunctionDelta `json:"function"`
}

// openaiToolFunctionDelta is the incremental function data in a tool call delta.
type openaiToolFunctionDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// openaiUsage tracks token usage in the final chunk.
type openaiUsage struct {
	CompletionTokens int `json:"completion_tokens"`
}

// openaiErrorResponse is the error format returned by the OpenAI API.
type openaiErrorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// openaiToolCallState tracks the accumulation of a single tool call's
// arguments across multiple SSE chunks.
type openaiToolCallState struct {
	id      string
	name    string
	argsBuf strings.Builder
}

// translateMessages converts stirrup's internal Message/ContentBlock format
// to OpenAI's chat message format. The system prompt is prepended as a
// system message.
//
// replayPaths is the resolved quirks ReplayFields for THIS stream. For
// each assistant message carrying Message.ReplayFields, the entries
// whose path is (a) named by replayPaths, (b) single-segment, and (c)
// not a canonical message key are emitted as top-level keys on the
// assistant wire message. Gating on replayPaths keeps the registry the
// single source of truth: state captured under a different model's rules
// (e.g. after a mid-run model switch) is not replayed to
// a provider whose resolved rules never asked for it.
func translateMessages(system string, messages []types.Message, replayPaths []string) []openaiMessage {
	var out []openaiMessage

	if system != "" {
		out = append(out, openaiMessage{
			Role:    "system",
			Content: system,
		})
	}

	for _, msg := range messages {
		switch msg.Role {
		case "assistant":
			oai := openaiMessage{Role: "assistant"}
			var textParts []string
			var toolCalls []openaiToolCall

			for _, block := range msg.Content {
				switch block.Type {
				case "text":
					textParts = append(textParts, block.Text)
				case "tool_use":
					args := string(block.Input)
					if args == "" {
						args = "{}"
					}
					toolCalls = append(toolCalls, openaiToolCall{
						ID:   block.ID,
						Type: "function",
						Function: openaiToolFunction{
							Name:      block.Name,
							Arguments: args,
						},
					})
				}
			}

			if len(textParts) > 0 {
				oai.Content = strings.Join(textParts, "")
			}
			if len(toolCalls) > 0 {
				oai.ToolCalls = toolCalls
			}
			if len(msg.ReplayFields) > 0 {
				var extra map[string]json.RawMessage
				for _, path := range replayPaths {
					v, ok := msg.ReplayFields[path]
					if !ok || len(v) == 0 || !threadableOpenAIReplayPath(path) {
						continue
					}
					if extra == nil {
						extra = make(map[string]json.RawMessage)
					}
					extra[path] = v
				}
				oai.ReplayFields = extra
			}
			out = append(out, oai)

		case "user":
			// Tool results must be sent as separate "tool" role messages.
			var textParts []string
			for _, block := range msg.Content {
				switch block.Type {
				case "text":
					textParts = append(textParts, block.Text)
				case "tool_result":

					content := block.Content
					if block.IsError {
						content = "Error: " + content
					}
					out = append(out, openaiMessage{
						Role:       "tool",
						Content:    content,
						ToolCallID: block.ToolUseID,
					})
				}
			}
			if len(textParts) > 0 {
				out = append(out, openaiMessage{
					Role:    "user",
					Content: strings.Join(textParts, ""),
				})
			}
		}
	}

	return out
}

// translateTools converts stirrup ToolDefinitions to OpenAI's function
// format. When strict is true, every tool's Parameters is rewritten by
// NormalizeStrictSchema and the Strict flag is set on the wire entry; the
// cache memoises rewrites by (model, tool-name, schema-hash). Returns an
// error when strict-mode normalisation rejects a tool's schema — the
// request must not be sent in that case (fail-closed).
func translateTools(tools []types.ToolDefinition, strict, examples bool, model string, cache *strictSchemaCache) ([]openaiTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]openaiTool, len(tools))
	for i, t := range tools {
		def := openaiToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		}
		if strict {
			normalised, err := normalizeStrictWithCache(cache, model, t.Name, t.InputSchema)
			if err != nil {
				return nil, err
			}
			def.Parameters = normalised
			truthy := true
			def.Strict = &truthy
		} else if examples {
			// Fold worked examples into the schema's `examples` keyword —
			// only for non-strict tools, since OpenAI's structured-outputs
			// subset rejects `examples`; strict tools rely on the
			// description text instead.
			merged, err := mergeSchemaExamples(def.Parameters, toolInputExamples(t))
			if err != nil {
				return nil, fmt.Errorf("tool %q: merge examples: %w", t.Name, err)
			}
			def.Parameters = merged
		}
		out[i] = openaiTool{
			Type:     "function",
			Function: def,
		}
	}
	return out, nil
}

// openAIToolChoiceFromParams projects the provider-neutral
// StreamParams.ToolChoice onto the OpenAI tool_choice wire value, gated on
// the resolved capability. Returns nil ("emit no field") for auto mode and
// any unsupported mode. The named-tool form degrades to nil when the tool
// name is empty or fails ValidateToolChoiceName rather than emitting an
// invalid object. The return type is `any` because tool_choice is a sum
// type (string OR object).
func openAIToolChoiceFromParams(params types.StreamParams, capability quirks.ToolChoiceCapability) any {
	if !capability.Supported {
		return nil
	}
	switch params.ToolChoice {
	case types.ToolChoiceRequired:
		if !capability.Required {
			return nil
		}
		return "required"
	case types.ToolChoiceNone:
		if !capability.None {
			return nil
		}
		return "none"
	case types.ToolChoiceTool:
		if !capability.NamedTool || params.ToolChoiceName == "" {
			return nil
		}
		if err := types.ValidateToolChoiceName(params.ToolChoiceName); err != nil {
			warnInvalidToolChoiceName("openai-compatible", params.Model, len(params.ToolChoiceName))
			return nil
		}
		return openAINamedToolChoice{
			Type:     "function",
			Function: openAINamedToolChoiceFunc{Name: params.ToolChoiceName},
		}
	default:
		// ToolChoiceAuto (zero value): auto is the wire default, so emit
		// nothing rather than an explicit "auto" string.
		return nil
	}
}

// openAIParallelFromParams projects StreamParams.ParallelToolCalls onto the
// OpenAI top-level parallel_tool_calls bool, gated on the resolved
// capability. Returns nil when the caller did not set the control or the
// provider does not advertise support. Shared by the Chat and Responses
// adapters, which use the identical top-level wire field.
func openAIParallelFromParams(params types.StreamParams, capability quirks.ParallelToolCallsCapability) *bool {
	if params.ParallelToolCalls == nil || !capability.Supported {
		return nil
	}
	return params.ParallelToolCalls
}

// warnInvalidToolChoiceName logs a single warn when a ToolChoiceTool
// request carried a name that failed ValidateToolChoiceName, so the
// degradation to auto is observable. Shared by all three adapters'
// projection helpers.
//
// The offending name is NOT logged: it is caller/model-influenced input
// that could carry log-injection bytes. The fixed grammar in the message
// and the name's length are enough for an operator to understand the
// rejection without the value.
func warnInvalidToolChoiceName(providerType, model string, nameLen int) {
	slog.Default().Warn("tool choice name failed validation; degrading to auto",
		slog.String("provider.type", providerType),
		slog.String("provider.model", model),
		slog.String("grammar", "^[a-zA-Z0-9_-]{1,64}$"),
		slog.Int("name_len", nameLen),
	)
}

// mapFinishReason converts OpenAI's finish_reason to stirrup's stop reason.
func mapFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return reason
	}
}

// buildOpenAIRequest projects a StreamParams into the Chat Completions wire
// body. The stream argument toggles the "stream" field so a future
// non-streaming caller (batch submission) can reuse the same projection.
//
// q carries the resolved quirks for the (provider, model) pair; the zero
// value reproduces today's default behaviour for callers that skip
// resolution. strictCache memoises strict-mode schema rewrites across
// turns; pass nil to disable caching. Returns an error when a tool's
// schema fails the strict-mode lint — the caller must NOT send a request
// in that case.
//
// TODO(batch): if the batch endpoint rejects fields the streaming endpoint
// accepts (e.g. top_p on Responses, equivalent constraints on Chat
// Completions), apply a batch-specific projection here.
func buildOpenAIRequest(params types.StreamParams, stream bool, q quirks.ProviderQuirks, strictCache *strictSchemaCache) (openaiRequest, error) {
	tools, err := translateTools(params.Tools, q.BehaviourFlags.OpenAI.StrictMode, q.ToolExamples.Supported, params.Model, strictCache)
	if err != nil {
		return openaiRequest{}, err
	}
	return openaiRequest{
		Model:              params.Model,
		Messages:           translateMessages(params.System, params.Messages, q.ReplayFields),
		Tools:              tools,
		MaxTokens:          params.MaxTokens,
		Temperature:        params.Temperature,
		Stream:             stream,
		TokenField:         q.BehaviourFlags.OpenAI.TokenField,
		OmitSamplingParams: q.BehaviourFlags.OpenAI.OmitSamplingParams,
		ExtraBodyFields:    q.BehaviourFlags.OpenAI.ExtraBodyFields,
		ToolChoice:         openAIToolChoiceFromParams(params, q.ToolChoice),
		ParallelToolCalls:  openAIParallelFromParams(params, q.ParallelToolCalls),
	}, nil
}

// Stream sends a streaming request to the OpenAI Chat Completions API and
// returns a channel of StreamEvents. The channel is closed when the stream
// ends or an error occurs. Cancelling the context terminates the stream.
func (o *OpenAICompatibleAdapter) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	start := time.Now()
	metricAttrs := metric.WithAttributes(
		attribute.String("provider.type", "openai-compatible"),
		attribute.String("provider.model", params.Model),
	)

	// Resolved per-stream so the same run can switch models turn to turn
	// under a dynamic router.
	registry := o.Registry
	if registry == nil {
		registry = quirks.DefaultRegistry()
	}
	q, appliedRules := registry.ResolveWithRules("openai-compatible", params.Model)

	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Emitted even when the rule list is empty so an operator grepping the
	// line knows resolution ran; list is in apply order (last entry wins).
	logger.DebugContext(ctx, "openai quirks resolved",
		slog.String("provider.type", "openai-compatible"),
		slog.String("provider.model", params.Model),
		slog.Any("rules", ruleDescriptions(appliedRules)),
	)

	// Mirrors the slog line onto the span so trace-only consumers also see
	// which rules fired; IsValid keeps the allocation off the no-tracer path.
	if span := oteltrace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		span.SetAttributes(attribute.StringSlice("provider.quirk.applied", ruleDescriptions(appliedRules)))
	}

	// Without this signal an operator who set --temperature would silently
	// observe greedy decoding. The suppressed value itself is not logged.
	if q.BehaviourFlags.OpenAI.OmitSamplingParams && params.Temperature != nil {
		logger.WarnContext(ctx, "openai quirks suppressed caller temperature",
			slog.String("provider.type", "openai-compatible"),
			slog.String("provider.model", params.Model),
			slog.Any("quirk.rules", ruleDescriptions(appliedRules)),
		)
	}

	// Emitted before buildOpenAIRequest so a fail-closed lint error surfaces
	// against a context that already shows strict-mode was active.
	if q.BehaviourFlags.OpenAI.StrictMode {
		logger.DebugContext(ctx, "openai strict mode applied",
			slog.String("provider.type", "openai-compatible"),
			slog.String("provider.model", params.Model),
			slog.Any("quirk.rules", ruleDescriptions(appliedRules)),
		)
	}

	reqBody, err := buildOpenAIRequest(params, true, q, o.strictSchemas)
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("build request: %w", err)
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Resolve the bearer credential before issuing the HTTP request so a
	// failure in the credential layer surfaces synchronously.
	apiKey, err := resolveBearer(ctx, o.bearer)
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, err
	}

	requestURL, err := composeOpenAIURL(o.baseURL, "/chat/completions", o.queryParams)
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("compose request URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, strings.NewReader(string(bodyBytes)))
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	setOpenAIAuthHeader(req, apiKey, o.apiKeyHeader)

	resp, err := DoWithRetry(ctx, o.httpClient, req, RetryOptions{
		Policy:       o.RetryPolicy,
		Logger:       o.Logger,
		Metrics:      o.Metrics,
		ProviderType: "openai-compatible",
		Model:        params.Model,
	})
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		// DoWithRetry surfaces the raw transport *url.Error, whose embedded
		// URL (o.baseURL + o.queryParams) may carry a gateway credential Go
		// does not query-redact; unwrap before wrapping (CWE-532).
		return nil, fmt.Errorf("execute request: %w", security.UnwrapURLError(err))
	}

	// rate_limited fires on a terminal 429 (retries exhausted or disabled);
	// DoWithRetry records provider_retry_attempt for intermediate retries.
	if o.Tracer != nil {
		span := oteltrace.SpanFromContext(ctx)
		span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
		if resp.StatusCode == 429 {
			retryAfter := resp.Header.Get("Retry-After")
			span.AddEvent("rate_limited", oteltrace.WithAttributes(
				attribute.String("retry_after", retryAfter),
			))
		}
	}

	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		o.recordLatency(ctx, start, metricAttrs)
		var errResp openaiErrorResponse
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&errResp); err == nil && errResp.Error.Message != "" {
			return nil, fmt.Errorf("openai API returned status %d: %s", resp.StatusCode, errResp.Error.Message)
		}
		return nil, fmt.Errorf("openai API returned status %d", resp.StatusCode)
	}

	ch := make(chan types.StreamEvent, 64)
	go func() {
		o.consumeSSE(ctx, resp, ch, start, metricAttrs, q, logger, params.Model)
		o.recordLatency(ctx, start, metricAttrs)
	}()
	return ch, nil
}

// recordLatency records the total provider request latency to the
// ProviderLatency histogram. Safe to call when Metrics is nil.
func (o *OpenAICompatibleAdapter) recordLatency(ctx context.Context, start time.Time, attrs metric.MeasurementOption) {
	if o.Metrics == nil {
		return
	}
	o.Metrics.ProviderLatency.Record(ctx, float64(time.Since(start).Milliseconds()), attrs)
}

// consumeSSE reads SSE lines from the response body and sends StreamEvents
// to the channel. It closes the channel and the response body when done.
//
// streamStart and metricAttrs are forwarded for ProviderTTFB measurement:
// the first non-empty stream event observed marks "time to first byte".
//
// q carries the resolved per-(provider, model) quirks for this stream; the
// parser reads q.ReplayFields and captures the named paths from each
// chunk's delta object (see docs/provider-quirks.md), surfacing them in a
// per-stream debug log and, flattened, on the message_complete event.
func (o *OpenAICompatibleAdapter) consumeSSE(ctx context.Context, resp *http.Response, ch chan<- types.StreamEvent, streamStart time.Time, metricAttrs metric.MeasurementOption, q quirks.ProviderQuirks, logger *slog.Logger, model string) {
	defer close(ch)
	defer func() { _ = resp.Body.Close() }()

	ttfbRecorded := false
	emitEvent := func(ev types.StreamEvent) {
		if !ttfbRecorded && o.Metrics != nil {
			o.Metrics.ProviderTTFB.Record(ctx, float64(time.Since(streamStart).Milliseconds()), metricAttrs)
			ttfbRecorded = true
		}
		ch <- ev
	}

	toolCalls := make(map[int]*openaiToolCallState)

	// Per-stream ReplayFields accumulator, keyed by path; stored as a slice
	// per path so a multi-chunk delta (e.g. DeepSeek's reasoning_content)
	// records each piece in arrival order rather than only the last value.
	// replayFieldsTotalBytes tracks the accumulator against
	// maxReplayFieldBytes; once capped, later captures are dropped so the
	// flattened value stays a clean prefix rather than gaining holes.
	replayFieldsCapture := map[string][]any{}
	replayFieldsTotalBytes := 0
	replayFieldsCapped := false
	// Whether a finish_reason chunk already produced a message_complete, so
	// the bare-[DONE] fallback below knows if the stream still owes one.
	messageCompleted := false
	// Most recent usage block seen on any chunk: compatible servers may send
	// it in a trailing empty-choices chunk after finish_reason, or attach it
	// to the finish chunk itself.
	var streamUsage *openaiUsage
	// Guards against double-counting once a message_complete already
	// carried the output-token count.
	outputTokensEmitted := false

	flushTrailingUsage := func() {
		if streamUsage == nil || outputTokensEmitted {
			return
		}
		emitEvent(types.StreamEvent{
			Type:         "message_complete",
			OutputTokens: streamUsage.CompletionTokens,
		})
		outputTokensEmitted = true
	}
	// Emit the per-stream ReplayFields summary on any exit path. Length-only:
	// captured content must never reach a log or trace sink.
	defer func() {
		if len(replayFieldsCapture) == 0 {
			return
		}

		if span := oteltrace.SpanFromContext(ctx); span.SpanContext().IsValid() {
			totalCount, totalLen := summarizeReplayCaptures(replayFieldsCapture)
			span.SetAttributes(
				attribute.Int("replay_fields_captured.count", totalCount),
				attribute.Int("replay_fields_captured.total_len", totalLen),
			)
		}
		logReplayFieldsCapture(ctx, logger, "openai-compatible", model, replayFieldsCapture)
	}()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), maxSSEScannerBuffer)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			emitEvent(types.StreamEvent{Type: "error", Error: ctx.Err()})
			return
		default:
		}

		line := scanner.Text()

		// Skip empty lines (SSE separators) and comments.
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")

		if data == "[DONE]" {

			o.flushToolCallsVia(toolCalls, emitEvent)
			// Some gateways terminate with a bare [DONE] and never send a
			// finish_reason chunk; synthesise message_complete so captured
			// replay state still reaches the persisted assistant message
			// (otherwise the next turn 400s against DeepSeek v4 thinking
			// mode). Gated on messageCompleted so an already-completed
			// stream does not get a second event that clobbers the real
			// stop reason.
			if !messageCompleted && len(replayFieldsCapture) > 0 {
				emitEvent(types.StreamEvent{
					Type:         "message_complete",
					StopReason:   mapFinishReason("stop"),
					ReplayFields: flattenReplayCapture(replayFieldsCapture),
				})
			}
			flushTrailingUsage()
			return
		}

		var chunk openaiChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse chunk: %w", err)})
			return
		}

		// The usage block may ride on the finish chunk or on a trailing
		// choices-empty chunk; capture it wherever it lands.
		if chunk.Usage != nil {
			streamUsage = chunk.Usage
		}

		for _, choice := range chunk.Choices {

			if !replayFieldsCapped && len(q.ReplayFields) > 0 && len(choice.RawDelta) > 0 {
				if captured := quirks.CaptureFromJSON(choice.RawDelta, q.ReplayFields); len(captured) > 0 {
					for k, v := range captured {
						add := 0
						for _, item := range v {
							add += replayCaptureByteLen(item)
						}
						if replayFieldsTotalBytes+add > maxReplayFieldBytes {
							replayFieldsCapped = true
							// One WARN per stream; the captured values
							// themselves are never logged.
							logger.WarnContext(ctx, "replay field accumulator cap reached, truncating",
								slog.String("provider.type", "openai-compatible"),
								slog.String("provider.model", model),
								slog.String("path", k),
								slog.Int("limit_bytes", maxReplayFieldBytes),
							)
							break
						}
						replayFieldsTotalBytes += add
						replayFieldsCapture[k] = append(replayFieldsCapture[k], v...)
					}
				}
			}

			// Text content delta.
			if choice.Delta.Content != nil && *choice.Delta.Content != "" {
				emitEvent(types.StreamEvent{
					Type: "text_delta",
					Text: *choice.Delta.Content,
				})
			}

			// Tool call deltas — accumulate arguments by index.
			for _, tc := range choice.Delta.ToolCalls {
				state, exists := toolCalls[tc.Index]
				if !exists {
					state = &openaiToolCallState{}
					toolCalls[tc.Index] = state
				}
				if tc.ID != "" {
					state.id = tc.ID
				}
				if tc.Function.Name != "" {
					state.name = tc.Function.Name
				}
				if state.argsBuf.Len()+len(tc.Function.Arguments) > openaiMaxToolInputSize {
					emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("tool arguments exceed %d byte limit", openaiMaxToolInputSize)})
					return
				}
				state.argsBuf.WriteString(tc.Function.Arguments)
			}

			if choice.FinishReason != nil {

				o.flushToolCallsVia(toolCalls, emitEvent)

				stopReason := mapFinishReason(*choice.FinishReason)
				ev := types.StreamEvent{
					Type:       "message_complete",
					StopReason: stopReason,
					// Rides on message_complete so the agentic loop persists
					// it on the assistant Message and threads it back via
					// translateMessages on the next turn.
					ReplayFields: flattenReplayCapture(replayFieldsCapture),
				}
				// Attach usage only when it has already arrived (servers
				// that put it on the finish chunk). LM Studio and OpenAI
				// send it in a later chunk, handled by flushTrailingUsage.
				if streamUsage != nil {
					ev.OutputTokens = streamUsage.CompletionTokens
					outputTokensEmitted = true
				}
				emitEvent(ev)
				messageCompleted = true
			}
		}
	}

	if err := scanner.Err(); err != nil {
		emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("read SSE stream: %w", err)})
		return
	}
	// Stream ended without a [DONE] terminator (some servers just close
	// the connection); surface any usage captured before the close.
	flushTrailingUsage()
}

// composeOpenAIURL parses baseURL, appends path (using Path so existing
// path components survive a trailing slash), then merges queryParams into
// the existing query string. Keys present in queryParams override any
// duplicates already encoded on baseURL — explicit configuration always
// wins over BaseURL-encoded defaults. Shared by both OpenAI adapters so
// switching between provider.type "openai-compatible" and "openai-responses"
// produces identical URL composition behaviour.
func composeOpenAIURL(baseURL, path string, queryParams map[string]string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("parse baseURL: %w", err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	if len(queryParams) > 0 {
		q := u.Query()
		for k, v := range queryParams {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// setOpenAIAuthHeader applies the configured auth header to req. With an
// empty apiKey this is a no-op (some local gateways accept anonymous
// requests). With a non-empty apiKeyHeader, the resolved key is sent under
// that header name verbatim — caller-side validation (ValidateRunConfig)
// is responsible for rejecting header names containing CRLF or whitespace.
// With an empty apiKeyHeader, today's "Authorization: Bearer <key>"
// behaviour is preserved.
func setOpenAIAuthHeader(req *http.Request, apiKey, apiKeyHeader string) {
	if apiKey == "" {
		return
	}
	if apiKeyHeader == "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		return
	}
	req.Header.Set(apiKeyHeader, apiKey)
}

// resolveBearer invokes the bearer closure to fetch the current API key. A
// nil closure is treated as "no auth"; empty-string returns are also valid
// for local gateways that accept anonymous requests. Errors are wrapped so
// the provider name does not need to be repeated at every call site.
func resolveBearer(ctx context.Context, bearer credential.BearerTokenFunc) (string, error) {
	if bearer == nil {
		return "", nil
	}
	tok, err := bearer(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve bearer token: %w", err)
	}
	return tok, nil
}

// flushToolCallsVia emits tool_call events for all accumulated tool calls via
// the supplied emit function (which may also record TTFB), then clears the
// state map. Called when the stream signals completion.
func (o *OpenAICompatibleAdapter) flushToolCallsVia(toolCalls map[int]*openaiToolCallState, emit func(types.StreamEvent)) {
	// Emit in index order for determinism.
	for idx := 0; idx < len(toolCalls); idx++ {
		state, ok := toolCalls[idx]
		if !ok {
			continue
		}
		var input map[string]any
		raw := state.argsBuf.String()
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &input); err != nil {
				emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse tool arguments JSON: %w", err)})
				return
			}
		}
		emit(types.StreamEvent{
			Type:  "tool_call",
			ID:    state.id,
			Name:  state.name,
			Input: input,
		})
	}

	for k := range toolCalls {
		delete(toolCalls, k)
	}
}
