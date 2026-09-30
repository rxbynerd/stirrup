package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

const (
	anthropicAPIURL     = "https://api.anthropic.com/v1/messages"
	anthropicAPIVersion = "2023-06-01"
	maxToolInputSize    = 10 * 1024 * 1024 // 10 MB cap on streamed tool input JSON
	// maxThinkingResponseSize caps the text plus signatures of every
	// thinking and redacted_thinking block in one response.
	maxThinkingResponseSize = 10 * 1024 * 1024
	// maxOpenContentBlocks caps the content blocks one response may have
	// started and not yet stopped.
	maxOpenContentBlocks = 64
)

// AuthMode selects the authentication header sent on every /v1/messages
// request. See docs/providers.md for why the two header shapes are not
// interchangeable.
type AuthMode int

const (
	// AuthModeAPIKey sends the credential in the x-api-key header (static keys).
	AuthModeAPIKey AuthMode = iota
	// AuthModeBearer sends the credential as Authorization: Bearer (WIF OAuth tokens).
	AuthModeBearer
)

// AnthropicAdapter implements ProviderAdapter for the Anthropic Messages API.
type AnthropicAdapter struct {
	bearer     credential.BearerTokenFunc
	authMode   AuthMode
	httpClient *http.Client
	baseURL    string // overridable for testing

	// AdapterDeps carries the factory-injected Tracer/Metrics/RetryPolicy/
	// Logger; see its doc comment for the field-by-field contract.
	AdapterDeps

	// Registry resolves per-(provider, model) quirks at the top of every
	// Stream call. Defaults to quirks.DefaultRegistry(); the nil-Registry
	// guard in Stream tolerates direct construction.
	Registry *quirks.Registry
}

// NewAnthropicAdapter creates an adapter for the Anthropic Messages API.
//
// bearer is invoked on every Stream call to fetch the current API key,
// letting refresh-aware credential sources (e.g. AnthropicWIFSource)
// rotate the token without rebuilding the adapter.
func NewAnthropicAdapter(bearer credential.BearerTokenFunc, authMode AuthMode) *AnthropicAdapter {
	return &AnthropicAdapter{
		bearer:   bearer,
		authMode: authMode,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
		baseURL:  anthropicAPIURL,
		Registry: quirks.DefaultRegistry(),
	}
}

// AuthMode returns the configured authentication header mode.
func (a *AnthropicAdapter) AuthMode() AuthMode {
	return a.authMode
}

// WireTap dumps every request and response this adapter sends, UNREDACTED,
// to out. Only the factory calls it, and only under a debug build with
// --trace-wire. See docs/security.md#debug-builds.
func (a *AnthropicAdapter) WireTap(out io.Writer) {
	a.httpClient.Transport = WireTapTransport(a.httpClient.Transport, out)
}

// anthropicRequest is the JSON body sent to the Anthropic Messages API.
//
// Temperature is *float64 with omitempty: nil omits the key entirely
// (Anthropic treats an explicit "temperature":0 as greedy decoding, not
// "use the service default"), while a non-nil pointer transmits the value
// verbatim including 0.0. buildAnthropicRequest forces it back to nil when
// BehaviourFlags.Anthropic.OmitSamplingParams is set.
//
// Messages is typed as []anthropicMessage, not []types.Message, to enforce
// a cross-provider confidentiality invariant — see docs/architecture.md
// (Provider adapters).
type anthropicRequest struct {
	Model string `json:"model"`
	// System is nil when the system prompt is empty, which omits the key.
	System      *anthropicSystemPrompt `json:"system,omitempty"`
	Messages    []anthropicMessage     `json:"messages"`
	Tools       []anthropicTool        `json:"tools,omitempty"`
	MaxTokens   int                    `json:"max_tokens"`
	Temperature *float64               `json:"temperature,omitempty"`
	// ToolChoice is the Anthropic tool_choice object. A nil pointer omits
	// the field (the zero-value ToolChoiceAuto path). Populated only when
	// the resolved quirks advertise native support for the requested mode.
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
	// OutputConfig carries output_config.effort. Nil omits the key, which
	// is required for models with no effort control (they 400 on it).
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
	// CacheControl is the top-level automatic-caching breakpoint: the API
	// places it on the last cacheable block of each request, so the cached
	// prefix grows with the conversation. Set only for requests carrying a
	// StreamParams.CacheKey; nil omits the key.
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
	Stream       bool                   `json:"stream"`
}

type anthropicOutputConfig struct {
	Effort string `json:"effort"`
}

type anthropicCacheControl struct {
	Type string `json:"type"`
}

func ephemeralCacheControl() *anthropicCacheControl {
	return &anthropicCacheControl{Type: "ephemeral"}
}

// anthropicSystemPrompt renders the `system` field as a plain JSON string,
// or, when Cache is set, as a single text block carrying an ephemeral
// cache_control breakpoint. The block form keeps tools and system cached
// even after the context strategy rewrites earlier messages.
type anthropicSystemPrompt struct {
	Text  string
	Cache bool
}

type anthropicSystemBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

func (s anthropicSystemPrompt) MarshalJSON() ([]byte, error) {
	if !s.Cache {
		return json.Marshal(s.Text)
	}
	return json.Marshal([]anthropicSystemBlock{{
		Type:         "text",
		Text:         s.Text,
		CacheControl: ephemeralCacheControl(),
	}})
}

// anthropicToolChoice is the Anthropic Messages API tool_choice object.
// Type is one of "auto", "any", or "tool"; Name is set only for "tool".
// Anthropic has no native "none" — a no-tools turn is expressed by
// omitting the tools array — so ToolChoiceNone never produces this
// struct.
//
// DisableParallelToolUse forbids parallel tool calls; Anthropic has no
// top-level parallel field, so this rides on tool_choice instead. A nil
// pointer omits the key (parallel is Anthropic's default).
type anthropicToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

// applyAnthropicParallel folds a requested parallel-disable onto the
// tool_choice object. Only the disable direction is expressible — Anthropic's
// default is parallel-enabled — so a nil or =true StreamParams.ParallelToolCalls
// is a no-op. The structural "none" mode is left untouched: there are no tool
// calls to parallelise and synthesising auto would contradict the caller.
func applyAnthropicParallel(tc *anthropicToolChoice, params types.StreamParams, capability quirks.ParallelToolCallsCapability) *anthropicToolChoice {
	if params.ParallelToolCalls == nil || *params.ParallelToolCalls {
		return tc
	}
	if !capability.Supported || !capability.Disable {
		return tc
	}
	if tc == nil {
		if params.ToolChoice == types.ToolChoiceNone {
			return nil
		}
		tc = &anthropicToolChoice{Type: "auto"}
	}
	disable := true
	tc.DisableParallelToolUse = &disable
	return tc
}

// anthropicTool is the Messages API custom-tool object. Like
// anthropicContentBlock it is an explicit allowlist: types.ToolDefinition's
// Presentation never reaches the wire except through InputExamples.
type anthropicTool struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	InputSchema   json.RawMessage   `json:"input_schema"`
	InputExamples []json.RawMessage `json:"input_examples,omitempty"`
}

// translateToolsAnthropic projects the tool definitions onto the wire,
// carrying each tool's worked examples as the resolved capability directs:
// on the native input_examples field, folded into input_schema's
// `examples` keyword, or not at all. The schema is never mutated in place.
// Examples are advisory: a merge that cannot marshal leaves the schema
// as-is rather than failing the request.
func translateToolsAnthropic(tools []types.ToolDefinition, examples quirks.ToolExamplesCapability) []anthropicTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropicTool, len(tools))
	for i, t := range tools {
		out[i] = anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		}
		if !examples.Supported {
			continue
		}
		if examples.Native {
			out[i].InputExamples = toolInputExamples(t)
			continue
		}
		if merged, err := mergeSchemaExamples(t.InputSchema, toolInputExamples(t)); err == nil {
			out[i].InputSchema = merged
		}
	}
	return out
}

// anthropicToolChoiceFromParams projects the provider-neutral
// StreamParams.ToolChoice onto the Anthropic tool_choice object, gated on
// the resolved capability. Returns nil — meaning "emit no field" — for the
// auto mode, for an unsupported mode, and for the structural "none" case
// (Anthropic expresses none by omitting tools, not via tool_choice).
func anthropicToolChoiceFromParams(params types.StreamParams, cap quirks.ToolChoiceCapability) *anthropicToolChoice {
	if !cap.Supported {
		return nil
	}
	switch params.ToolChoice {
	case types.ToolChoiceRequired:
		if !cap.Required {
			return nil
		}
		return &anthropicToolChoice{Type: "any"}
	case types.ToolChoiceTool:
		// A named-tool choice with no name is not expressible; treat it
		// as auto (emit nothing) rather than send an invalid object.
		if !cap.NamedTool || params.ToolChoiceName == "" {
			return nil
		}
		// Defense-in-depth: ToolChoiceName may carry a model-influenced
		// value, so reject any name outside the shared grammar and degrade
		// to auto rather than forward it.
		if err := types.ValidateToolChoiceName(params.ToolChoiceName); err != nil {
			warnInvalidToolChoiceName("anthropic", params.Model, len(params.ToolChoiceName))
			return nil
		}
		return &anthropicToolChoice{Type: "tool", Name: params.ToolChoiceName}
	default:
		// ToolChoiceAuto and ToolChoiceNone both emit no tool_choice field:
		// auto is the wire default, and Anthropic has no native none. Since
		// this adapter does not also strip the tools array for "none", the
		// model may still emit a tool_use block; honouring none strictly
		// would require dropping tools from the request body, so "none" is
		// best-effort.
		return nil
	}
}

// anthropicMessage is the Anthropic-side wire shape for a single message.
// Locally defined so that the set of fields reaching api.anthropic.com is
// an explicit allowlist, not whatever types.ContentBlock happens to carry
// for some other provider's benefit. Adding a field here is an active
// decision that Anthropic's Messages API accepts it on the wire.
type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

// anthropicContentBlock is the allowlist of content-block fields the
// Anthropic Messages API accepts. Add a field only if Anthropic's API
// documents support for it; provider-private state on types.ContentBlock
// (added by other adapters) is dropped on egress to Anthropic by
// construction. See docs/architecture.md (Provider adapters).
//
// Content is a json.RawMessage rather than a string because the Messages
// API accepts a tool_result block's `content` as either a JSON string or an
// array of content blocks; the array form is emitted only when the resolved
// StructuredToolResults capability is on and the result carries a structured
// envelope. A nil Content omits the key.
//
// Signature is the thinking block's signature or the redacted_thinking
// block's opaque data; MarshalJSON places it under the key each type
// requires, so it never rides on any other block type.
type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Signature string          `json:"-"`
}

// anthropicContentBlockFields drops anthropicContentBlock's MarshalJSON so
// the struct-tag encoding can be reused without recursion.
type anthropicContentBlockFields anthropicContentBlock

// MarshalJSON emits the exact wire shape per block type. A thinking block
// must carry the "thinking" key even when the text is empty (the API omits
// the text by default and the signature carries the content), and a
// redacted_thinking block carries only "data". Every other type uses the
// struct tags unchanged.
func (b anthropicContentBlock) MarshalJSON() ([]byte, error) {
	switch b.Type {
	case "thinking":
		return json.Marshal(struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{Type: b.Type, Thinking: b.Text, Signature: b.Signature})
	case "redacted_thinking":
		return json.Marshal(struct {
			Type string `json:"type"`
			Data string `json:"data"`
		}{Type: b.Type, Data: b.Signature})
	default:
		return json.Marshal(anthropicContentBlockFields(b))
	}
}

// anthropicToolResultPart is one entry in the array form of a tool_result
// block's content. The Messages API's tool_result content array accepts
// "text" (and "image") parts; the harness uses only "text", carrying the
// structured envelope as a JSON-serialised text part alongside the canonical
// text part — there is no native JSON content type for tool_result.
type anthropicToolResultPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// translateMessagesAnthropic copies a slice of types.Message into the
// adapter-local anthropicMessage shape. It is the structural guard that
// enforces the cross-provider confidentiality invariant: any field on
// types.ContentBlock not mirrored onto anthropicContentBlock is dropped
// here, rather than relying on call sites to scrub egress.
//
// ThoughtSignature is read only from thinking and redacted_thinking blocks.
// A thinking block without one is dropped: the API cannot verify an
// unsigned block, so it is not replayable.
func translateMessagesAnthropic(messages []types.Message, cap quirks.StructuredToolResultCapability) []anthropicMessage {
	out := make([]anthropicMessage, 0, len(messages))
	skipped := false
	for _, msg := range messages {
		blocks := make([]anthropicContentBlock, 0, len(msg.Content))
		onlyThinking := true
		for _, b := range msg.Content {
			if types.IsThinkingBlock(b) {
				if b.ThoughtSignature == "" {
					continue
				}
				blocks = append(blocks, anthropicContentBlock{
					Type:      b.Type,
					Text:      b.Text,
					Signature: b.ThoughtSignature,
				})
				continue
			}
			onlyThinking = false
			blocks = append(blocks, anthropicContentBlock{
				Type:      b.Type,
				Text:      b.Text,
				ID:        b.ID,
				Name:      b.Name,
				Input:     anthropicToolUseInput(b),
				ToolUseID: b.ToolUseID,
				Content:   anthropicToolResultContent(b, cap),
				IsError:   b.IsError,
			})
		}
		// The API rejects an empty content array, so a message left empty by
		// stripped or unsigned thinking blocks is omitted, and the turns on
		// either side are joined when that leaves two of the same role.
		if len(blocks) == 0 && onlyThinking {
			skipped = true
			continue
		}
		if skipped && len(out) > 0 && out[len(out)-1].Role == msg.Role {
			out[len(out)-1].Content = append(out[len(out)-1].Content, blocks...)
		} else {
			out = append(out, anthropicMessage{Role: msg.Role, Content: blocks})
		}
		skipped = false
	}
	return out
}

// anthropicToolUseInput renders the `input` field of one content block for
// the Anthropic wire. For non-tool_use blocks it returns nil so the key is
// omitted. A tool_use block is always given an object, since the API rejects
// a null input on a replayed assistant turn.
func anthropicToolUseInput(b types.ContentBlock) json.RawMessage {
	if b.Type != "tool_use" {
		return nil
	}
	return types.NormalizeToolInput(b.Input)
}

// anthropicToolResultContent renders the `content` field of one content
// block for the Anthropic wire. For non-tool_result blocks it returns nil so
// the key is omitted (text/tool_use blocks never carry tool-result content).
// For tool_result blocks it returns the JSON-string form by default; when the
// resolved capability accepts the content-block array shape and the block
// carries a structured envelope, it returns the array form — the canonical
// text part plus a text part holding the structured JSON — so the model
// receives both renderings and the text fallback survives even if a consumer
// reads only the first part.
//
// A marshalling failure falls back to the plain string content: structured
// serialisation is purely additive and must never drop the canonical text.
func anthropicToolResultContent(b types.ContentBlock, cap quirks.StructuredToolResultCapability) json.RawMessage {
	if b.Type != "tool_result" {
		return nil
	}
	// The array form requires a non-empty canonical text: an empty first part
	// ({"type":"text","text":""}) is meaningless, so an empty Content falls
	// through to the nil-return below regardless of the structured payload.
	// A structured envelope byte-identical to the canonical text would put
	// the same bytes on the wire twice, so those collapse to the single
	// string form below.
	if cap.Supported && cap.ContentBlockArray && len(b.Structured) > 0 && b.Content != "" &&
		string(b.Structured) != b.Content {
		parts := []anthropicToolResultPart{
			{Type: "text", Text: b.Content},
			{Type: "text", Text: string(b.Structured)},
		}
		if raw, err := json.Marshal(parts); err == nil {
			return raw
		}
	}

	if b.Content == "" {
		return nil
	}
	raw, err := json.Marshal(b.Content)
	if err != nil {
		// json.Marshal on a string cannot fail in practice; emit an empty
		// JSON string so the wire shape stays valid.
		return json.RawMessage(`""`)
	}
	return raw
}

// SSE event types from the Anthropic API.
type sseContentBlockStart struct {
	Index        int             `json:"index"`
	ContentBlock sseContentBlock `json:"content_block"`
}

type sseContentBlock struct {
	Type      string          `json:"type"` // "text" | "tool_use" | "thinking" | "redacted_thinking"
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Text      string          `json:"text,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Data      string          `json:"data,omitempty"`
}

type sseContentBlockDelta struct {
	Index int      `json:"index"`
	Delta sseDelta `json:"delta"`
}

type sseDelta struct {
	Type        string `json:"type"` // "text_delta" | "input_json_delta" | "thinking_delta" | "signature_delta"
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
}

type sseMessageDelta struct {
	Delta struct {
		StopReason string `json:"stop_reason"`
		// StopDetails is decoded separately by parseAnthropicStopDetails so
		// an unexpected shape degrades to "no details" instead of failing
		// the whole message_delta.
		StopDetails json.RawMessage `json:"stop_details,omitempty"`
	} `json:"delta"`
	Usage *anthropicUsage `json:"usage,omitempty"`
}

// anthropicUsage is the usage object on a streamed message_delta and on
// a non-streaming Messages response. input_tokens excludes both cache
// figures; thinking_tokens is a subset of output_tokens.
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	OutputTokensDetails      struct {
		ThinkingTokens int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

// applyTo copies the usage onto a message_complete event, folding the
// cache figures into InputTokens.
func (u anthropicUsage) applyTo(ev *types.StreamEvent) {
	cacheRead := clampTokens(u.CacheReadInputTokens)
	cacheWrite := clampTokens(u.CacheCreationInputTokens)
	setEventUsage(ev, tokenReport{
		Input:      clampTokens(u.InputTokens) + cacheWrite + cacheRead,
		Output:     u.OutputTokens,
		CacheRead:  cacheRead,
		CacheWrite: cacheWrite,
		Reasoning:  u.OutputTokensDetails.ThinkingTokens,
	})
}

// Byte caps for stop_details strings, which reach traces, logs, and span
// attributes.
const (
	maxStopDetailsTypeBytes        = 64
	maxStopDetailsExplanationBytes = 1024
)

// stopDetailsOtherCategory replaces any non-empty refusal category outside
// anthropicRefusalCategories, keeping category a closed set.
const stopDetailsOtherCategory = "other"

// anthropicRefusalCategories is the documented stop_details.category set.
var anthropicRefusalCategories = map[string]bool{
	"cyber":                true,
	"bio":                  true,
	"frontier_llm":         true,
	"reasoning_extraction": true,
	"general_harms":        true,
}

// parseAnthropicStopDetails reads the documented refusal fields of
// stop_details (type, category, explanation) and ignores the rest. Each
// field is decoded on its own so a malformed one does not drop the
// others. It returns nil for an absent, null, or non-object value, or one
// with none of those fields set.
func parseAnthropicStopDetails(raw json.RawMessage) *types.StopDetails {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	d := types.StopDetails{
		Type:        capUTF8(jsonStringField(fields["type"]), maxStopDetailsTypeBytes),
		Category:    anthropicRefusalCategory(fields["category"]),
		Explanation: capUTF8(jsonStringField(fields["explanation"]), maxStopDetailsExplanationBytes),
	}
	if d == (types.StopDetails{}) {
		return nil
	}
	return &d
}

// anthropicRefusalCategory maps a stop_details.category value onto the
// documented set: absent, null, or "" gives "", a documented value is kept,
// and anything else is stopDetailsOtherCategory.
func anthropicRefusalCategory(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var category string
	if json.Unmarshal(raw, &category) != nil {
		return stopDetailsOtherCategory
	}
	if category == "" || anthropicRefusalCategories[category] {
		return category
	}
	return stopDetailsOtherCategory
}

// jsonStringField decodes raw as a JSON string, returning "" for a missing
// value or any other JSON type.
func jsonStringField(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// capUTF8 truncates s to at most n bytes without splitting a rune.
func capUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// sseError is the payload of an SSE "error" event, which the API sends in
// place of the remaining stream when a request fails after streaming has
// begun (e.g. overloaded_error).
type sseError struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// anthropicStreamError builds the error for an SSE "error" event, keeping
// the provider's error type so it survives into logs and the run trace.
func anthropicStreamError(data string) error {
	var se sseError
	_ = json.Unmarshal([]byte(data), &se)
	switch {
	case se.Error.Type != "":
		return fmt.Errorf("anthropic API stream error (%s): %s", se.Error.Type, se.Error.Message)
	case se.Error.Message != "":
		return fmt.Errorf("anthropic API stream error: %s", se.Error.Message)
	default:
		return errors.New("anthropic API stream error")
	}
}

// buildAnthropicRequest projects a StreamParams into the Anthropic Messages
// wire body. The stream argument toggles the "stream" field so a future
// non-streaming (batch) caller can reuse the same projection.
//
// q carries the resolved per-(provider, model) quirks. A zero-value q (both
// Supported=false) emits no tool_choice field, string-only tool results, a
// string system prompt and no cache_control, so callers that do not route
// through the registry get the baseline shape. With PromptCaching set, the
// system breakpoint is sent on every request and the top-level breakpoint
// only when params.CacheKey marks a multi-turn conversation.
//
// TODO(batch): if the batch endpoint rejects fields the streaming endpoint
// accepts (e.g. thinking_config), change the return type to
// (json.RawMessage, error) and apply a batch-specific projection here.
func buildAnthropicRequest(params types.StreamParams, stream bool, q quirks.ProviderQuirks) anthropicRequest {
	temperature := params.Temperature
	if q.BehaviourFlags.Anthropic.OmitSamplingParams {

		temperature = nil
	}
	var outputConfig *anthropicOutputConfig
	if effort := projectReasoningEffort(params.ReasoningEffort, q.BehaviourFlags.Anthropic.EffortLevels); effort != "" {
		outputConfig = &anthropicOutputConfig{Effort: effort}
	}
	caching := q.BehaviourFlags.Anthropic.PromptCaching
	var system *anthropicSystemPrompt
	if params.System != "" {
		system = &anthropicSystemPrompt{Text: params.System, Cache: caching}
	}
	var cacheControl *anthropicCacheControl
	if caching && params.CacheKey != "" {
		cacheControl = ephemeralCacheControl()
	}
	return anthropicRequest{
		Model:    params.Model,
		System:   system,
		Messages: translateMessagesAnthropic(params.Messages, q.StructuredToolResults),

		Tools:        translateToolsAnthropic(params.Tools, q.ToolExamples),
		MaxTokens:    params.MaxTokens,
		Temperature:  temperature,
		ToolChoice:   applyAnthropicParallel(anthropicToolChoiceFromParams(params, q.ToolChoice), params, q.ParallelToolCalls),
		OutputConfig: outputConfig,
		CacheControl: cacheControl,
		Stream:       stream,
	}
}

// Stream sends a streaming request to the Anthropic Messages API and returns
// a channel of StreamEvents. The channel is closed when the stream ends or
// an error occurs. Cancelling the context terminates the stream.
func (a *AnthropicAdapter) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	start := time.Now()
	metricAttrs := metric.WithAttributes(
		attribute.String("provider.type", "anthropic"),
		attribute.String("provider.model", params.Model),
	)

	// Resolve quirks for this (provider, model) pair. Per design D4 the
	// resolution is per-stream. The nil-Registry guard tolerates callers
	// that build the adapter outside the factory without going through
	// NewAnthropicAdapter.
	registry := a.Registry
	if registry == nil {
		registry = quirks.DefaultRegistry()
	}
	q, appliedRules := registry.ResolveWithRules("anthropic", params.Model)

	logger := a.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Emitted even when the rules list is empty so an operator grepping
	// for the line knows the resolution ran.
	logger.DebugContext(ctx, "anthropic quirks resolved",
		slog.String("provider.type", "anthropic"),
		slog.String("provider.model", params.Model),
		slog.Any("rules", ruleDescriptions(appliedRules)),
	)

	if span := oteltrace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		span.SetAttributes(attribute.StringSlice("provider.quirk.applied", ruleDescriptions(appliedRules)))
	}

	// Without this warning, an operator who set RunConfig.Temperature
	// explicitly would silently observe it dropped. The suppressed value
	// itself is intentionally NOT logged.
	if q.BehaviourFlags.Anthropic.OmitSamplingParams && params.Temperature != nil {
		logger.WarnContext(ctx, "anthropic quirks suppressed caller temperature",
			slog.String("provider.type", "anthropic"),
			slog.String("provider.model", params.Model),
			slog.Any("quirk.rules", ruleDescriptions(appliedRules)),
		)
	}

	if err := validateReasoningEffort("anthropic", params.ReasoningEffort, params.Model, q.BehaviourFlags.Anthropic.EffortLevels); err != nil {
		a.recordLatency(ctx, start, metricAttrs)
		return nil, err
	}
	warnDroppedReasoningEffort(ctx, logger, "anthropic", params.ReasoningEffort, params.Model, q.BehaviourFlags.Anthropic.EffortLevels)

	reqBody := buildAnthropicRequest(params, true, q)

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		a.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Resolve the bearer credential before issuing the HTTP request so a
	// failure in the credential layer is surfaced as a synchronous Stream
	// error rather than a half-built request with a missing header.
	apiKey, err := a.bearer(ctx)
	if err != nil {
		a.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("resolve bearer token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL, strings.NewReader(string(bodyBytes)))
	if err != nil {
		a.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	switch a.authMode {
	case AuthModeBearer:
		req.Header.Set("Authorization", "Bearer "+apiKey)
	default:
		req.Header.Set("x-api-key", apiKey)
	}
	req.Header.Set("anthropic-version", anthropicAPIVersion)

	// DoWithRetry retries only this pre-stream call (connection errors, or a
	// 429/5xx on the initial response). It is never invoked again once the
	// channel below is returned, so a failure after streaming has begun is
	// never replayed — the same boundary the openai-compatible adapter
	// relies on.
	resp, err := DoWithRetry(ctx, a.httpClient, req, RetryOptions{
		Policy:       a.RetryPolicy,
		Logger:       a.Logger,
		Metrics:      a.Metrics,
		ProviderType: "anthropic",
		Model:        params.Model,
	})
	if err != nil {
		a.recordLatency(ctx, start, metricAttrs)
		// a.baseURL is operator-configurable and may carry a credential in
		// its query string; unwrap the *url.Error so its embedded URL (which
		// Go does not query-redact) never reaches a log or caller (CWE-532).
		return nil, fmt.Errorf("execute request: %w", security.UnwrapURLError(err))
	}

	if a.Tracer != nil {
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		a.recordLatency(ctx, start, metricAttrs)
		if len(body) > 0 {
			return nil, fmt.Errorf("anthropic API returned status %d: %s", resp.StatusCode, body)
		}
		return nil, fmt.Errorf("anthropic API returned status %d", resp.StatusCode)
	}

	ch := make(chan types.StreamEvent, 64)
	go func() {
		a.consumeSSE(ctx, resp, ch, start, metricAttrs, logger, params.Model)
		a.recordLatency(ctx, start, metricAttrs)
	}()
	return ch, nil
}

// recordLatency records the total provider request latency to the
// ProviderLatency histogram. Safe to call when Metrics is nil.
func (a *AnthropicAdapter) recordLatency(ctx context.Context, start time.Time, attrs metric.MeasurementOption) {
	if a.Metrics == nil {
		return
	}
	a.Metrics.ProviderLatency.Record(ctx, float64(time.Since(start).Milliseconds()), attrs)
}

// logPromptCacheUsage records the cache split of one turn's input from the
// event's (clamped) counts, so the log agrees with the trace. A turn with
// no reported input emits nothing.
func logPromptCacheUsage(ctx context.Context, logger *slog.Logger, model string, ev types.StreamEvent) {
	uncached := max(ev.InputTokens-ev.CacheReadTokens-ev.CacheWriteTokens, 0)
	if ev.CacheReadTokens == 0 && ev.CacheWriteTokens == 0 && uncached == 0 {
		return
	}
	logger.DebugContext(ctx, "anthropic prompt cache",
		slog.String("provider.type", "anthropic"),
		slog.String("provider.model", model),
		slog.Int("cache.read", ev.CacheReadTokens),
		slog.Int("cache.write", ev.CacheWriteTokens),
		slog.Int("input.uncached", uncached),
	)
}

// consumeSSE reads SSE events from the response body and sends StreamEvents
// to the channel. It closes the channel and the response body when done.
//
// streamStart and metricAttrs are forwarded for ProviderTTFB measurement: the
// first non-empty stream event observed marks "time to first byte" for this
// request. TTFB is recorded at most once per stream.
func (a *AnthropicAdapter) consumeSSE(ctx context.Context, resp *http.Response, ch chan<- types.StreamEvent, streamStart time.Time, metricAttrs metric.MeasurementOption, logger *slog.Logger, model string) {
	defer close(ch)
	defer func() { _ = resp.Body.Close() }()

	// emitEvent sends an event on the output channel and records TTFB on the
	// first non-empty event observed. Closes around ttfbRecorded so each call
	// site does not need to check.
	ttfbRecorded := false
	emitEvent := func(ev types.StreamEvent) {
		if !ttfbRecorded && a.Metrics != nil {
			a.Metrics.ProviderTTFB.Record(ctx, float64(time.Since(streamStart).Milliseconds()), metricAttrs)
			ttfbRecorded = true
		}
		ch <- ev
	}

	// Track in-flight content blocks by index for tool_use JSON and thinking
	// accumulation. signature holds a redacted_thinking block's data.
	type blockState struct {
		blockType string
		id        string
		name      string
		jsonBuf   strings.Builder
		thinking  strings.Builder
		signature strings.Builder
	}
	blocks := make(map[int]*blockState)

	// appendThinking grows a thinking block's text or signature, enforcing
	// the response-wide size cap. Reports false after emitting the error
	// event.
	thinkingBytes := 0
	appendThinking := func(dst *strings.Builder, s string) bool {
		if thinkingBytes+len(s) > maxThinkingResponseSize {
			emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("thinking blocks exceed %d byte limit per response", maxThinkingResponseSize)})
			return false
		}
		thinkingBytes += len(s)
		dst.WriteString(s)
		return true
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), maxSSEScannerBuffer)
	var currentEvent string

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			emitEvent(types.StreamEvent{Type: "error", Error: ctx.Err()})
			return
		default:
		}

		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")

		switch currentEvent {
		case "content_block_start":
			var cbs sseContentBlockStart
			if err := json.Unmarshal([]byte(data), &cbs); err != nil {
				emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse content_block_start: %w", err)})
				return
			}
			if _, open := blocks[cbs.Index]; !open && len(blocks) >= maxOpenContentBlocks {
				emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("more than %d content blocks open at once", maxOpenContentBlocks)})
				return
			}
			bs := &blockState{
				blockType: cbs.ContentBlock.Type,
				id:        cbs.ContentBlock.ID,
				name:      cbs.ContentBlock.Name,
			}
			switch bs.blockType {
			case "thinking":
				if !appendThinking(&bs.thinking, cbs.ContentBlock.Thinking) ||
					!appendThinking(&bs.signature, cbs.ContentBlock.Signature) {
					return
				}
			case "redacted_thinking":
				if !appendThinking(&bs.signature, cbs.ContentBlock.Data) {
					return
				}
			}
			blocks[cbs.Index] = bs

		case "content_block_delta":
			var cbd sseContentBlockDelta
			if err := json.Unmarshal([]byte(data), &cbd); err != nil {
				emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse content_block_delta: %w", err)})
				return
			}
			bs := blocks[cbd.Index]
			if bs == nil {
				continue
			}
			switch cbd.Delta.Type {
			case "text_delta":
				emitEvent(types.StreamEvent{
					Type: "text_delta",
					Text: cbd.Delta.Text,
				})
			case "input_json_delta":
				if bs.jsonBuf.Len()+len(cbd.Delta.PartialJSON) > maxToolInputSize {
					emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("tool input exceeds %d byte limit", maxToolInputSize)})
					return
				}
				bs.jsonBuf.WriteString(cbd.Delta.PartialJSON)
			case "thinking_delta":
				if !appendThinking(&bs.thinking, cbd.Delta.Thinking) {
					return
				}
			case "signature_delta":
				if !appendThinking(&bs.signature, cbd.Delta.Signature) {
					return
				}
			}

		case "content_block_stop":

			var stopData struct {
				Index int `json:"index"`
			}
			if err := json.Unmarshal([]byte(data), &stopData); err != nil {
				emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse content_block_stop: %w", err)})
				return
			}
			var blockType string
			bs := blocks[stopData.Index]
			if bs != nil {
				blockType = bs.blockType
			}
			switch blockType {
			case "tool_use":
				var input map[string]any
				raw := bs.jsonBuf.String()
				if raw != "" {
					if err := json.Unmarshal([]byte(raw), &input); err != nil {
						emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse tool input JSON: %w", err)})
						return
					}
				}
				emitEvent(types.StreamEvent{
					Type:  "tool_call",
					ID:    bs.id,
					Name:  bs.name,
					Input: input,
				})
			case "thinking":
				emitEvent(types.StreamEvent{
					Type:             "thinking",
					Text:             bs.thinking.String(),
					ThoughtSignature: bs.signature.String(),
				})
			case "redacted_thinking":
				emitEvent(types.StreamEvent{
					Type:             "redacted_thinking",
					ThoughtSignature: bs.signature.String(),
				})
			}
			delete(blocks, stopData.Index)

		case "message_delta":
			var md sseMessageDelta
			if err := json.Unmarshal([]byte(data), &md); err != nil {
				emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse message_delta: %w", err)})
				return
			}
			ev := types.StreamEvent{
				Type:        "message_complete",
				StopReason:  md.Delta.StopReason,
				StopDetails: parseAnthropicStopDetails(md.Delta.StopDetails),
			}
			if md.Usage != nil {
				md.Usage.applyTo(&ev)
				logPromptCacheUsage(ctx, logger, model, ev)
			}
			emitEvent(ev)

		case "message_stop":
			// Stream is done; the goroutine will exit and close the channel.
			return

		case "error":
			emitEvent(types.StreamEvent{Type: "error", Error: anthropicStreamError(data)})
			return
		}

		currentEvent = ""
	}

	if err := scanner.Err(); err != nil {
		emitEvent(types.StreamEvent{Type: "error", Error: fmt.Errorf("read SSE stream: %w", err)})
	}
}
