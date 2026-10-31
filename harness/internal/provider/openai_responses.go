package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// OpenAIResponsesAdapter implements ProviderAdapter for the OpenAI Responses
// API (POST /v1/responses) — a distinct wire format from Chat Completions,
// including Azure Foundry's wire-compatible endpoint. See docs/providers.md.
type OpenAIResponsesAdapter struct {
	bearer       credential.BearerTokenFunc
	httpClient   *http.Client
	baseURL      string
	apiKeyHeader string
	queryParams  map[string]string

	// streamIdleTimeout bounds silence on a streamed response body; zero
	// selects defaultStreamIdleTimeout.
	streamIdleTimeout time.Duration

	// AdapterDeps carries the factory-injected Tracer/Metrics/RetryPolicy/
	// Logger; see its doc comment for the field-by-field contract.
	AdapterDeps

	// Registry resolves per-(provider, model) quirks at the top of
	// every Stream call. Defaults to quirks.DefaultRegistry().
	Registry *quirks.Registry

	// EndpointProfile is the endpoint the quirks registry resolves on.
	// See OpenAICompatibleAdapter.EndpointProfile.
	EndpointProfile string

	// strictSchemas memoises strict-mode schema rewrites within this
	// adapter's lifetime. See OpenAICompatibleAdapter.strictSchemas.
	strictSchemas *strictSchemaCache

	// replayWarnedNoEncrypted limits the missing encrypted_content replay
	// WARN to once per adapter.
	replayWarnedNoEncrypted atomic.Bool
}

// NewOpenAIResponsesAdapter creates an adapter for the OpenAI Responses API.
// baseURL is the API root (e.g. "https://api.openai.com/v1"); the /responses
// path is appended automatically, and an empty string defaults to OpenAI's
// URL. bearer is invoked on every Stream call to fetch the current API key.
func NewOpenAIResponsesAdapter(bearer credential.BearerTokenFunc, baseURL string, auth OpenAIAuthConfig) *OpenAIResponsesAdapter {
	if baseURL == "" {
		baseURL = openaiDefaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &OpenAIResponsesAdapter{
		bearer:          bearer,
		httpClient:      newStreamingHTTPClient(),
		baseURL:         baseURL,
		apiKeyHeader:    auth.APIKeyHeader,
		queryParams:     auth.QueryParams,
		Registry:        quirks.DefaultRegistry(),
		EndpointProfile: derivedEndpointProfile(baseURL),
		strictSchemas:   newStrictSchemaCache(),
	}
}

// quirksTarget is the registry lookup key for a request to model through
// this adapter.
func (o *OpenAIResponsesAdapter) quirksTarget(model string) quirks.Target {
	return quirks.Target{ProviderType: "openai-responses", Endpoint: o.EndpointProfile, Model: model}
}

// WireTap dumps every request and response this adapter sends, UNREDACTED,
// to out. Only the factory calls it, and only under a debug build with
// --trace-wire. See docs/security.md#debug-builds.
func (a *OpenAIResponsesAdapter) WireTap(out io.Writer) {
	a.httpClient.Transport = WireTapTransport(a.httpClient.Transport, out)
}

// --- Responses API wire format ---

// responsesIncludeEncryptedReasoning is the include value that returns each
// reasoning item's encrypted_content, which stateless replay requires.
const responsesIncludeEncryptedReasoning = "reasoning.encrypted_content"

// responsesReasoningSummaryAuto asks for the most detailed reasoning summary
// the model offers.
const responsesReasoningSummaryAuto = "auto"

// responsesRequest is the JSON body sent to POST /v1/responses.
//
// Like the Chat Completions openaiRequest, the canonical fields carry no
// JSON struct tag (json:"-"): MarshalJSON is the single point that projects
// the struct into the wire body the resolved quirks selected. The resolved
// Responses behaviour flags (TokenField / StoreMode / InputItemShape) steer
// that projection and are the single source of truth for the Responses send
// path.
type responsesRequest struct {
	Model        string           `json:"-"`
	Instructions string           `json:"-"`
	Input        []responsesInput `json:"-"`
	Tools        []responsesTool  `json:"-"`
	MaxTokens    int              `json:"-"`
	// Temperature is *float64: nil omits the key entirely (the Responses API
	// rejects "temperature" outright on reasoning models). A non-nil pointer
	// transmits the value verbatim, including an explicit 0.0.
	Temperature *float64 `json:"-"`
	// Stream is omitted from the wire body when false; the streaming caller
	// sets reqBody.Stream = true after the builder returns. Omission is the
	// safer default for a future batch caller until that endpoint's contract
	// is verified.
	Stream bool `json:"-"`
	// ParallelToolCalls carries the top-level parallel_tool_calls bool,
	// shared with the Chat Completions API. A nil pointer omits the key;
	// buildResponsesRequest sets it only when requested and supported.
	ParallelToolCalls *bool `json:"-"`
	// ReasoningEffort is the wire value for reasoning.effort. Empty omits
	// the reasoning object.
	ReasoningEffort string `json:"-"`
	// ReasoningSummary is the wire value for reasoning.summary. It rides
	// only inside a reasoning object, so it is dropped when ReasoningEffort
	// is empty.
	ReasoningSummary string `json:"-"`
	// PromptCacheKey is the wire value for prompt_cache_key. Empty omits
	// the key.
	PromptCacheKey string `json:"-"`
	// TextFormat is the wire value for text.format. A nil pointer omits
	// the text object.
	TextFormat *responsesTextFormat `json:"-"`
	// IncludeEncryptedReasoning emits include:["reasoning.encrypted_content"].
	IncludeEncryptedReasoning bool `json:"-"`

	// TokenField / StoreMode / InputItemShape carry the resolved Responses
	// quirks for this request and steer MarshalJSON; none is serialised under
	// its own JSON key. The zero value of each reproduces the adapter's
	// pre-quirks hard-coded shape, so a request built with no rule is
	// byte-identical.
	TokenField     quirks.OpenAIResponsesTokenField `json:"-"`
	StoreMode      quirks.OpenAIResponsesStoreMode  `json:"-"`
	InputItemShape quirks.OpenAIResponsesInputShape `json:"-"`

	// strictFallbacks lists the tools newly sent with strict:false because
	// their schema failed the strict-mode lint; Stream logs them. Not
	// serialised.
	strictFallbacks []*strictSchemaError
	// strictNulls lists the optional properties of strict-sent tools whose
	// synthetic nulls the stream parser drops. Not serialised.
	strictNulls strictNullables
}

// MarshalJSON projects the canonical responsesRequest into the Responses
// wire body the resolved quirks selected. Keys are emitted in a fixed
// order — model, instructions, input, tools, <token key>, temperature,
// stream, store, include, parallel_tool_calls, reasoning, text,
// prompt_cache_key — and the token-budget key and store value come from
// the resolved behaviour flags rather than static struct tags.
//
// Projection rules:
//   - "model" — always.
//   - "instructions" — omitted when empty (system prompt absent).
//   - "input" — always (even an empty array).
//   - "tools" — omitted when empty.
//   - the token-budget field uses the key selected by TokenField
//     ("max_output_tokens"); omitted when zero, matching the prior
//     omitempty tag.
//   - "temperature" — omitted when nil.
//   - "stream" — omitted when false.
//   - "store" — always emitted; StoreMode selects the value (false today).
//   - "include" — ["reasoning.encrypted_content"] when
//     IncludeEncryptedReasoning is set; omitted otherwise.
//   - "parallel_tool_calls" — omitted when nil.
//   - "reasoning" — {"effort": ..., "summary": ...}; omitted when
//     ReasoningEffort is empty, and "summary" omitted when
//     ReasoningSummary is empty.
//   - "text" — {"format": {"type": "json_schema", ...}}; omitted when
//     TextFormat is nil.
//   - "prompt_cache_key" — omitted when PromptCacheKey is empty.
func (r responsesRequest) MarshalJSON() ([]byte, error) {
	if r.InputItemShape != quirks.TypedInputItems {
		return nil, fmt.Errorf("responsesRequest: unsupported input-item shape %v", r.InputItemShape)
	}

	var buf strings.Builder
	buf.WriteByte('{')

	writeKey := func(first bool, key string) bool {
		if !first {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(key)
		buf.Write(kb)
		buf.WriteByte(':')
		return false
	}
	writeRaw := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		buf.Write(b)
		return nil
	}

	first := true

	first = writeKey(first, "model")
	if err := writeRaw(r.Model); err != nil {
		return nil, err
	}

	if r.Instructions != "" {
		first = writeKey(first, "instructions")
		if err := writeRaw(r.Instructions); err != nil {
			return nil, err
		}
	}

	first = writeKey(first, "input")
	if err := writeRaw(r.Input); err != nil {
		return nil, err
	}

	if len(r.Tools) > 0 {
		first = writeKey(first, "tools")
		if err := writeRaw(r.Tools); err != nil {
			return nil, err
		}
	}

	if r.MaxTokens != 0 {
		first = writeKey(first, responsesWireTokenKey(r.TokenField))
		if err := writeRaw(r.MaxTokens); err != nil {
			return nil, err
		}
	}

	if r.Temperature != nil {
		first = writeKey(first, "temperature")
		if err := writeRaw(*r.Temperature); err != nil {
			return nil, err
		}
	}

	if r.Stream {
		first = writeKey(first, "stream")
		if err := writeRaw(r.Stream); err != nil {
			return nil, err
		}
	}

	first = writeKey(first, "store")
	if err := writeRaw(responsesStoreValue(r.StoreMode)); err != nil {
		return nil, err
	}

	if r.IncludeEncryptedReasoning {
		first = writeKey(first, "include")
		if err := writeRaw([]string{responsesIncludeEncryptedReasoning}); err != nil {
			return nil, err
		}
	}

	if r.ParallelToolCalls != nil {
		first = writeKey(first, "parallel_tool_calls")
		if err := writeRaw(*r.ParallelToolCalls); err != nil {
			return nil, err
		}
	}

	if r.ReasoningEffort != "" {
		first = writeKey(first, "reasoning")
		reasoning := map[string]string{"effort": r.ReasoningEffort}
		if r.ReasoningSummary != "" {
			reasoning["summary"] = r.ReasoningSummary
		}
		if err := writeRaw(reasoning); err != nil {
			return nil, err
		}
	}

	if r.TextFormat != nil {
		first = writeKey(first, "text")
		if err := writeRaw(responsesText{Format: r.TextFormat}); err != nil {
			return nil, err
		}
	}

	if r.PromptCacheKey != "" {
		first = writeKey(first, "prompt_cache_key")
		if err := writeRaw(r.PromptCacheKey); err != nil {
			return nil, err
		}
	}
	_ = first

	buf.WriteByte('}')
	return []byte(buf.String()), nil
}

// UnmarshalJSON recovers the canonical fields and the steering flags that the
// wire makes unambiguous: the token key implies TokenField. It is a near-, not
// exact, inverse of MarshalJSON — StoreMode is the one asymmetry. MarshalJSON
// emits "store":false for the sole modelled mode (StoreFalse), so the recovered
// store value carries no extra information and is intentionally not mapped back
// onto StoreMode (see the store branch below). Used by tests that round-trip
// the body through the same struct that produced it.
func (r *responsesRequest) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if v, ok := raw["model"]; ok {
		if err := json.Unmarshal(v, &r.Model); err != nil {
			return fmt.Errorf("responsesRequest.model: %w", err)
		}
	}
	if v, ok := raw["instructions"]; ok {
		if err := json.Unmarshal(v, &r.Instructions); err != nil {
			return fmt.Errorf("responsesRequest.instructions: %w", err)
		}
	}
	if v, ok := raw["input"]; ok {
		if err := json.Unmarshal(v, &r.Input); err != nil {
			return fmt.Errorf("responsesRequest.input: %w", err)
		}
	}
	if v, ok := raw["tools"]; ok {
		if err := json.Unmarshal(v, &r.Tools); err != nil {
			return fmt.Errorf("responsesRequest.tools: %w", err)
		}
	}
	if v, ok := raw["max_output_tokens"]; ok {
		if err := json.Unmarshal(v, &r.MaxTokens); err != nil {
			return fmt.Errorf("responsesRequest.max_output_tokens: %w", err)
		}
		r.TokenField = quirks.TokenFieldMaxOutputTokens
	}
	if v, ok := raw["temperature"]; ok {
		var t float64
		if err := json.Unmarshal(v, &t); err != nil {
			return fmt.Errorf("responsesRequest.temperature: %w", err)
		}
		r.Temperature = &t
	}
	if v, ok := raw["stream"]; ok {
		if err := json.Unmarshal(v, &r.Stream); err != nil {
			return fmt.Errorf("responsesRequest.stream: %w", err)
		}
	}
	if v, ok := raw["store"]; ok {
		var store bool
		if err := json.Unmarshal(v, &store); err != nil {
			return fmt.Errorf("responsesRequest.store: %w", err)
		}
		// The decoded store value is intentionally discarded: StoreFalse is the
		// only modelled mode, so there is no enum value to recover it into. The
		// decode still runs to reject a malformed (non-bool) store field rather
		// than silently ignoring it. StoreMode keeps its zero value (StoreFalse).
		_ = store
	}
	if v, ok := raw["include"]; ok {
		var include []string
		if err := json.Unmarshal(v, &include); err != nil {
			return fmt.Errorf("responsesRequest.include: %w", err)
		}
		r.IncludeEncryptedReasoning = slices.Contains(include, responsesIncludeEncryptedReasoning)
	}
	if v, ok := raw["parallel_tool_calls"]; ok {
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return fmt.Errorf("responsesRequest.parallel_tool_calls: %w", err)
		}
		r.ParallelToolCalls = &b
	}
	if v, ok := raw["reasoning"]; ok {
		var reasoning struct {
			Effort  string `json:"effort"`
			Summary string `json:"summary"`
		}
		if err := json.Unmarshal(v, &reasoning); err != nil {
			return fmt.Errorf("responsesRequest.reasoning: %w", err)
		}
		r.ReasoningEffort = reasoning.Effort
		r.ReasoningSummary = reasoning.Summary
	}
	if v, ok := raw["text"]; ok {
		var text responsesText
		if err := json.Unmarshal(v, &text); err != nil {
			return fmt.Errorf("responsesRequest.text: %w", err)
		}
		r.TextFormat = text.Format
	}
	if v, ok := raw["prompt_cache_key"]; ok {
		if err := json.Unmarshal(v, &r.PromptCacheKey); err != nil {
			return fmt.Errorf("responsesRequest.prompt_cache_key: %w", err)
		}
	}
	return nil
}

// responsesText is the Responses `text` object; only its format member is
// sent.
type responsesText struct {
	Format *responsesTextFormat `json:"format,omitempty"`
}

// responsesTextFormat is text.format in its json_schema form. Strict is
// always true for the same reason as openaiResponseFormat.
type responsesTextFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

// responsesTextFormatFromParams projects StreamParams.OutputSchema onto
// text.format, or nil when the resolved capability does not advertise
// support.
func responsesTextFormatFromParams(params types.StreamParams, capability quirks.OutputSchemaCapability) *responsesTextFormat {
	schema := projectOutputSchema(params.OutputSchema, capability)
	if schema == nil {
		return nil
	}
	return &responsesTextFormat{Type: "json_schema", Name: schema.Name, Strict: true, Schema: schema.Schema}
}

// responsesWireTokenKey returns the wire JSON key for the resolved token
// field. The switch is exhaustive over the OpenAIResponsesTokenField enum so
// that adding a second value (e.g. a gateway that wants max_completion_tokens)
// forces a wire-key here rather than silently emitting the zero-value key —
// the resolved quirk would otherwise change without the body following. A
// value outside the enum is a programmer error (a flag resolved but never
// wired) and panics rather than emitting a wrong key.
func responsesWireTokenKey(f quirks.OpenAIResponsesTokenField) string {
	switch f {
	case quirks.TokenFieldMaxOutputTokens:
		return "max_output_tokens"
	default:
		panic(fmt.Sprintf("responsesRequest: unwired OpenAIResponsesTokenField %v", f))
	}
}

// responsesStoreValue returns the wire value of the `store` field for the
// resolved store mode. Exhaustive over the OpenAIResponsesStoreMode enum for
// the same reason as responsesWireTokenKey: a future StoreTrue must change the
// emitted value here, not resolve in ProviderQuirks and be silently dropped.
func responsesStoreValue(m quirks.OpenAIResponsesStoreMode) bool {
	switch m {
	case quirks.StoreFalse:
		return false
	default:
		panic(fmt.Sprintf("responsesRequest: unwired OpenAIResponsesStoreMode %v", m))
	}
}

// responsesInput is one item in the Responses API input array. The Type
// field selects which other fields are populated; this matches the
// discriminated-union shape OpenAI publishes for typed input items.
//
// The struct keeps an ergonomic flat shape so construction-site code
// can build items without branching on type. MarshalJSON below switches
// on Type and emits only the keys valid for that variant, via per-type
// wire structs — stricter validators (e.g. Azure OpenAI's Responses
// endpoint) reject an empty "output" on message / function_call items.
// See docs/provider-quirks.md for the function_call_output invariant.
type responsesInput struct {
	Type      string                  `json:"-"` // "message" | "function_call" | "function_call_output" | "tool_search_call" | "tool_search_output" | "reasoning" (Raw only)
	Role      string                  `json:"-"` // for "message"
	Content   []responsesContentBlock `json:"-"` // for "message"
	Name      string                  `json:"-"` // for "function_call"
	CallID    string                  `json:"-"` // for "function_call" / "function_call_output"
	Arguments string                  `json:"-"` // for "function_call" — JSON string
	Output    string                  `json:"-"` // for "function_call_output" — required even when empty

	// Namespace is a function_call's namespace: the API rejects a call to
	// a function a tool_search_output in the same input loaded unless it
	// names the function's namespace (its own name when ungrouped).
	Namespace string `json:"-"`

	SearchArguments json.RawMessage `json:"-"` // for "tool_search_call" — JSON object
	LoadedTools     []responsesTool `json:"-"` // for "tool_search_output"

	// Raw is a stored output item to replay; when set, MarshalJSON returns
	// it and ignores the typed fields. The enclosing json.Marshal compacts
	// and HTML-escapes it, so the wire JSON is semantically identical.
	Raw json.RawMessage `json:"-"`
}

// MarshalJSON emits only the wire fields valid for the input item's Type
// discriminant. Each Type maps to a dedicated wire struct so a future
// edit cannot accidentally leak a field across variants. function_call_output
// requires the "output" key even when empty, so its wire struct's Output
// field has no omitempty. A replayed item (Raw set) is emitted as stored.
func (r responsesInput) MarshalJSON() ([]byte, error) {
	if len(r.Raw) > 0 {
		return r.Raw, nil
	}
	switch r.Type {
	case "message":
		return json.Marshal(responsesMessageInputWire{
			Type:    r.Type,
			Role:    r.Role,
			Content: r.Content,
		})
	case "function_call":
		return json.Marshal(responsesFunctionCallInputWire{
			Type:      r.Type,
			CallID:    r.CallID,
			Name:      r.Name,
			Arguments: r.Arguments,
			Namespace: r.Namespace,
		})
	case "function_call_output":
		return json.Marshal(responsesFunctionCallOutputInputWire{
			Type:   r.Type,
			CallID: r.CallID,
			Output: r.Output,
		})
	case "tool_search_call":
		return json.Marshal(responsesToolSearchCallInputWire{
			Type:      r.Type,
			Execution: responsesToolSearchExecution,
			Status:    "completed",
			Arguments: r.SearchArguments,
		})
	case "tool_search_output":
		tools := r.LoadedTools
		if tools == nil {
			tools = []responsesTool{}
		}
		return json.Marshal(responsesToolSearchOutputInputWire{
			Type:      r.Type,
			Execution: responsesToolSearchExecution,
			Status:    "completed",
			Tools:     tools,
		})
	default:
		// Unknown variants would previously have been serialised as a
		// pile of empty-string keys. Surfacing the type explicitly makes
		// the failure mode debuggable rather than a silent wire-format
		// mismatch.
		return nil, fmt.Errorf("responsesInput: unknown type %q", r.Type)
	}
}

// UnmarshalJSON is the inverse of MarshalJSON: it accepts any of the
// per-type wire shapes and populates the ergonomic flat struct so test
// roundtrips and any future caller-side parsing continues to work. The
// adapter itself never decodes request bodies — this exists for symmetry
// and to keep tests that send-then-receive a request able to inspect the
// shape through the same struct that built it. An item carrying an id is a
// replayed output item (reconstructed items never carry one) and is kept
// in Raw.
func (r *responsesInput) UnmarshalJSON(data []byte) error {
	var head struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return err
	}
	r.Type = head.Type
	if head.ID != "" && replayableResponsesItem(head.Type) {
		r.Raw = append(json.RawMessage(nil), data...)
		return nil
	}
	switch head.Type {
	case "message":
		var w responsesMessageInputWire
		if err := json.Unmarshal(data, &w); err != nil {
			return err
		}
		r.Role = w.Role
		r.Content = w.Content
	case "function_call":
		var w responsesFunctionCallInputWire
		if err := json.Unmarshal(data, &w); err != nil {
			return err
		}
		r.CallID = w.CallID
		r.Name = w.Name
		r.Arguments = w.Arguments
		r.Namespace = w.Namespace
	case "function_call_output":
		var w responsesFunctionCallOutputInputWire
		if err := json.Unmarshal(data, &w); err != nil {
			return err
		}
		r.CallID = w.CallID
		r.Output = w.Output
	case "tool_search_call":
		var w responsesToolSearchCallInputWire
		if err := json.Unmarshal(data, &w); err != nil {
			return err
		}
		r.SearchArguments = w.Arguments
	case "tool_search_output":
		var w responsesToolSearchOutputInputWire
		if err := json.Unmarshal(data, &w); err != nil {
			return err
		}
		r.LoadedTools = w.Tools
	default:
		return fmt.Errorf("responsesInput: unknown type %q", head.Type)
	}
	return nil
}

// responsesToolSearchExecution is the execution mode of hosted tool
// search items.
const responsesToolSearchExecution = "server"

// responsesToolSearchCallInputWire is the wire shape for a reconstructed
// type=="tool_search_call". CallID is always null for hosted search.
type responsesToolSearchCallInputWire struct {
	Type      string          `json:"type"`
	Execution string          `json:"execution"`
	CallID    *string         `json:"call_id"`
	Status    string          `json:"status"`
	Arguments json.RawMessage `json:"arguments"`
}

// responsesToolSearchOutputInputWire is the wire shape for a reconstructed
// type=="tool_search_output". CallID is always null for hosted search.
type responsesToolSearchOutputInputWire struct {
	Type      string          `json:"type"`
	Execution string          `json:"execution"`
	CallID    *string         `json:"call_id"`
	Status    string          `json:"status"`
	Tools     []responsesTool `json:"tools"`
}

// responsesMessageInputWire is the wire shape for type=="message".
// Only type/role/content are valid for this variant.
type responsesMessageInputWire struct {
	Type    string                  `json:"type"`
	Role    string                  `json:"role,omitempty"`
	Content []responsesContentBlock `json:"content,omitempty"`
}

// responsesFunctionCallInputWire is the wire shape for type=="function_call".
// Only type/call_id/name/arguments/namespace are valid for this variant.
type responsesFunctionCallInputWire struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// responsesFunctionCallOutputInputWire is the wire shape for
// type=="function_call_output". Output deliberately lacks omitempty: the
// Responses API rejects function_call_output items missing the "output"
// key with HTTP 400, even when the value is the empty string.
type responsesFunctionCallOutputInputWire struct {
	Type   string `json:"type"`
	CallID string `json:"call_id,omitempty"`
	Output string `json:"output"`
}

// responsesContentBlock is one part inside a message item.
// OpenAI uses "input_text" for user/system messages and "output_text" for
// assistant messages — the asymmetry is part of their wire format.
//
// Text deliberately lacks omitempty: the Responses API requires the "text"
// key on input_text / output_text content parts, even when the value is
// the empty string. Both content-block variants today carry the same set
// of fields (type, text), so a single struct expresses the wire shape
// without the cross-variant leakage that motivated splitting
// responsesInput. If a future variant introduces non-shared fields, this
// struct should be split along the same lines.
type responsesContentBlock struct {
	Type string `json:"type"` // "input_text" | "output_text"
	Text string `json:"text"`
}

// responsesTool describes a tool in the Responses API's flatter format.
// (Compare with Chat Completions, which nests under a "function" object.)
//
// Strict is a *bool so the zero-value body omits the field; a quirks
// rule that pins strict mode causes the adapter to emit an explicit
// `"strict": true` on every tool entry. See openaiToolDefinition for
// the equivalent on the Chat Completions side.
//
// DeferLoading marks a function the hosted tool search loads on demand.
// A Type of responsesToolSearchType is the hosted search entry itself,
// which MarshalJSON emits with no other key. A Type of
// responsesNamespaceType groups the functions in Tools under Name, and
// the hosted search loads them by that name.
type responsesTool struct {
	Type         string          `json:"type"` // "function" | "namespace" | "tool_search"
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	Parameters   json.RawMessage `json:"parameters"`
	Strict       *bool           `json:"strict,omitempty"`
	DeferLoading *bool           `json:"defer_loading,omitempty"`
	Tools        []responsesTool `json:"tools,omitempty"`
}

const (
	// responsesToolSearchType is the tools[] entry type of OpenAI's
	// hosted tool search.
	responsesToolSearchType = "tool_search"
	// responsesNamespaceType is the tools[] entry type that groups
	// functions for the hosted tool search.
	responsesNamespaceType = "namespace"
)

// MarshalJSON emits the hosted tool search entry as {"type":"tool_search"},
// a namespace with only its type, name, description and tools, and every
// other entry with its struct tags.
func (t responsesTool) MarshalJSON() ([]byte, error) {
	switch t.Type {
	case responsesToolSearchType:
		return json.Marshal(struct {
			Type string `json:"type"`
		}{t.Type})
	case responsesNamespaceType:
		tools := t.Tools
		if tools == nil {
			tools = []responsesTool{}
		}
		return json.Marshal(struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Tools       []responsesTool `json:"tools"`
		}{t.Type, t.Name, t.Description, tools})
	}
	type wire responsesTool
	return json.Marshal(wire(t))
}

// --- SSE event payload types ---

// responsesOutputItem is a single item in the response.output array.
// Streaming events deliver these incrementally via response.output_item.added
// and response.output_item.done. Replay forwards the raw item JSON, so
// fields such as a reasoning item's encrypted_content are not modelled here.
type responsesOutputItem struct {
	Type      string                  `json:"type"` // "message" | "function_call" | "reasoning" | "tool_search_call" | "tool_search_output"
	ID        string                  `json:"id,omitempty"`
	CallID    string                  `json:"call_id,omitempty"`
	Name      string                  `json:"name,omitempty"`
	Arguments string                  `json:"arguments,omitempty"` // "function_call": a JSON string
	Namespace string                  `json:"namespace,omitempty"` // "function_call": set when the function was loaded by hosted search
	Status    string                  `json:"status,omitempty"`
	Phase     string                  `json:"phase,omitempty"`   // "message": "commentary" | "final_answer"
	Content   []responsesContentBlock `json:"content,omitempty"` // "message": output_text / refusal parts

	// SearchArguments is a tool_search_call item's arguments, a JSON
	// object rather than the string a function_call carries.
	SearchArguments json.RawMessage `json:"-"`
	// Tools is a tool_search_output item's loaded function and namespace
	// definitions.
	Tools json.RawMessage `json:"tools,omitempty"`
}

// UnmarshalJSON decodes arguments by item type: a tool_search_call keeps
// its arguments as raw JSON in SearchArguments, every other type decodes
// them as a string.
func (i *responsesOutputItem) UnmarshalJSON(data []byte) error {
	type plain responsesOutputItem
	wire := struct {
		*plain
		Arguments json.RawMessage `json:"arguments"`
	}{plain: (*plain)(i)}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if len(wire.Arguments) == 0 {
		return nil
	}
	if i.Type == "tool_search_call" {
		i.SearchArguments = wire.Arguments
		return nil
	}
	return json.Unmarshal(wire.Arguments, &i.Arguments)
}

// responsesUsage is the usage object on response.completed and
// response.incomplete. input_tokens already includes cached_tokens and
// cache_write_tokens; reasoning_tokens is a subset of output_tokens.
type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	InputTokensDetails struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens        int `json:"output_tokens"`
	OutputTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int `json:"total_tokens,omitempty"`
}

// applyTo copies the usage onto a message_complete event.
func (u responsesUsage) applyTo(ev *types.StreamEvent) {
	setEventUsage(ev, tokenReport{
		Input:      u.InputTokens,
		Output:     u.OutputTokens,
		CacheRead:  u.InputTokensDetails.CachedTokens,
		CacheWrite: u.InputTokensDetails.CacheWriteTokens,
		Reasoning:  u.OutputTokensDetails.ReasoningTokens,
	})
}

// responsesResponse is the response object delivered on response.completed
// and response.incomplete. Only the fields stirrup acts on are unmarshalled.
type responsesResponse struct {
	Model             string `json:"model,omitempty"`
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details,omitempty"`
	Output []responsesOutputItem `json:"output,omitempty"`
	Usage  *responsesUsage       `json:"usage,omitempty"`

	// RawOutput holds each output item as received, for replay capture.
	RawOutput []json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes the envelope once, keeping each output item's raw
// JSON beside its typed form.
func (r *responsesResponse) UnmarshalJSON(data []byte) error {
	type plain responsesResponse
	var wire struct {
		plain
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*r = responsesResponse(wire.plain)
	r.RawOutput = wire.Output
	r.Output = make([]responsesOutputItem, len(wire.Output))
	for i, raw := range wire.Output {
		if err := json.Unmarshal(raw, &r.Output[i]); err != nil {
			return fmt.Errorf("output[%d]: %w", i, err)
		}
	}
	return nil
}

// responsesErrorResponse is the error JSON returned for non-2xx responses.
// OpenAI uses the same envelope across Chat Completions and Responses.
type responsesErrorResponse struct {
	Error struct {
		Message string             `json:"message"`
		Type    string             `json:"type"`
		Code    responsesErrorCode `json:"code,omitempty"`
	} `json:"error"`
}

// responsesErrorCode decodes an error.code sent as a string or a number,
// as gateways send either. Any other JSON type decodes to "" rather than
// failing the surrounding error payload.
type responsesErrorCode string

func (c *responsesErrorCode) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*c = responsesErrorCode(s)
		return nil
	}
	var n json.Number
	if json.Unmarshal(b, &n) == nil {
		*c = responsesErrorCode(n.String())
		return nil
	}
	*c = ""
	return nil
}

// responsesErrorText renders an OpenAI error message with its error.code,
// which is what tells a policy stop (misalignment_policy_violation) or an
// exhausted quota apart from a transient failure.
func responsesErrorText(message string, code responsesErrorCode) string {
	switch {
	case code == "":
		return message
	case message == "":
		return "(code: " + string(code) + ")"
	default:
		return message + " (code: " + string(code) + ")"
	}
}

// responsesStreamError builds the error for a mid-stream failure event.
// server_error is a server-side fault, so it is marked retryable for the
// loop's turn retry; every other code (policy stops, quota, invalid
// prompts) is terminal.
func responsesStreamError(msg string, code responsesErrorCode) error {
	if code == "server_error" {
		return types.MarkRetryable(errors.New(msg))
	}
	return errors.New(msg)
}

// responsesCallState tracks an in-flight function call assembled across
// multiple SSE events. function_call_arguments.delta carries text fragments
// keyed by item_id (or output_index when item_id is absent on partner
// gateways); we accumulate them in argsBuf and flush once on the matching
// .done event.
type responsesCallState struct {
	itemID    string
	outputIdx int
	callID    string
	name      string
	argsBuf   strings.Builder
	emitted   bool // emitted at most once even if both done events fire
}

// --- Output replay ---

const (
	// responsesReplayKey is the adapter-owned Message.ReplayFields key that
	// carries one turn's ordered output items as a single JSON array. The
	// dotted form is not a threadable Chat Completions replay path, so the
	// openai-compatible adapter never echoes it.
	responsesReplayKey = "openai_responses.output"

	// responsesReplayOriginKey carries, as a JSON string, the
	// responsesReplayOrigin of the stored items. Items replay only to the
	// origin that produced them.
	responsesReplayOriginKey = "openai_responses.origin"

	// maxResponsesReplayBytes caps the output items stored per turn. It is
	// a guard, not a measurement: encrypted_content has no documented size.
	maxResponsesReplayBytes = 1 << 20
)

// Reasons a turn's output items are not replayed. They are logged; item
// content never is.
const (
	replayReasonUndecodable     = "output item failed to decode"
	replayReasonUnreplayable    = "unreplayable output item type"
	replayReasonNoEncrypted     = "reasoning item has no encrypted_content"
	replayReasonNoCallID        = "function_call item has no call_id"
	replayReasonNotAssistant    = "message item is not assistant output"
	replayReasonNotCompleted    = "output item is not completed"
	replayReasonEndsInReasoning = "turn ends with a reasoning item"
	replayReasonIncomplete      = "response is incomplete"
	replayReasonOverCap         = "output items exceed replay size cap"
)

// responsesReplayOrigin identifies the model and endpoint a turn's output
// items came from. The base URL is hashed so ReplayFields never carries it.
func responsesReplayOrigin(model, baseURL string) string {
	sum := sha256.Sum256([]byte(baseURL))
	return model + "@" + hex.EncodeToString(sum[:6])
}

// responsesReplayOriginFor returns the replay origin for a request, or ""
// when the resolved quirks do not replay output items, which disables both
// capture and replay.
func responsesReplayOriginFor(q quirks.ProviderQuirks, model, baseURL string) string {
	if !q.BehaviourFlags.OpenAIResponses.ReplayOutputItems {
		return ""
	}
	return responsesReplayOrigin(model, baseURL)
}

// replayableResponsesItem reports whether an output item type can be
// replayed verbatim as an input item. Other types (hosted-tool calls,
// program items, compaction) disable replay for their turn.
func replayableResponsesItem(itemType string) bool {
	switch itemType {
	case "reasoning", "message", "function_call", "tool_search_call", "tool_search_output":
		return true
	}
	return false
}

// responsesReplayItem is the part of an output item that decides whether
// it can be replayed.
type responsesReplayItem struct {
	responsesOutputItem
	Role             string `json:"role"`
	EncryptedContent string `json:"encrypted_content"`
}

// UnmarshalJSON decodes the embedded item with its own UnmarshalJSON,
// which would otherwise be promoted and leave Role and EncryptedContent
// unset.
func (r *responsesReplayItem) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &r.responsesOutputItem); err != nil {
		return err
	}
	var extra struct {
		Role             string `json:"role"`
		EncryptedContent string `json:"encrypted_content"`
	}
	if err := json.Unmarshal(data, &extra); err != nil {
		return err
	}
	r.Role, r.EncryptedContent = extra.Role, extra.EncryptedContent
	return nil
}

// checkResponsesReplayItems decodes a turn's output items and reports why
// they cannot be replayed, or an empty reason when they can. Capture and
// translation apply the same rules, so an edited stored array is refused.
func checkResponsesReplayItems(raws []json.RawMessage) (items []responsesReplayItem, reason, itemType string) {
	items = make([]responsesReplayItem, len(raws))
	for i, raw := range raws {
		item := &items[i]
		if err := json.Unmarshal(raw, item); err != nil {
			return nil, replayReasonUndecodable, ""
		}
		switch item.Type {
		case "reasoning":
			if item.EncryptedContent == "" {
				return nil, replayReasonNoEncrypted, item.Type
			}
		case "function_call":
			if item.CallID == "" {
				return nil, replayReasonNoCallID, item.Type
			}
		case "message":
			if item.Role != "assistant" {
				return nil, replayReasonNotAssistant, item.Type
			}
			for _, part := range item.Content {
				if part.Type != "output_text" && part.Type != "refusal" {
					return nil, replayReasonNotAssistant, item.Type
				}
			}
		case "tool_search_call", "tool_search_output":
		default:
			return nil, replayReasonUnreplayable, item.Type
		}
		if item.Status != "" && item.Status != "completed" {
			return nil, replayReasonNotCompleted, item.Type
		}
	}
	if n := len(items); n > 0 && items[n-1].Type == "reasoning" {
		return nil, replayReasonEndsInReasoning, ""
	}
	return items, "", ""
}

// stripResponsesCreatedBy drops created_by, an output-only field that the
// official SDK also removes before resending an item.
func stripResponsesCreatedBy(raw json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if _, ok := fields["created_by"]; !ok {
		return raw, nil
	}
	delete(fields, "created_by")
	return json.Marshal(fields)
}

// responsesReplayCapture collects one turn's output items for replay.
// Replay is all-or-nothing per turn, and logs carry sizes and item types
// only.
type responsesReplayCapture struct {
	logger *slog.Logger
	model  string
	origin string

	// warnedNoEncrypted is shared by every turn of one adapter, so a
	// missing encrypted_content is a WARN once and DEBUG afterwards.
	warnedNoEncrypted *atomic.Bool

	streamed []responsesStreamedItem
	size     int
	disabled bool
}

// responsesStreamedItem is one raw item from response.output_item.done.
type responsesStreamedItem struct {
	outputIdx int
	raw       json.RawMessage
}

// newResponsesReplayCapture returns the capture for one turn. An empty
// origin disables capture without logging.
func newResponsesReplayCapture(logger *slog.Logger, model, origin string, warnedNoEncrypted *atomic.Bool) *responsesReplayCapture {
	return &responsesReplayCapture{
		logger:            logger,
		model:             model,
		origin:            origin,
		warnedNoEncrypted: warnedNoEncrypted,
		disabled:          origin == "",
	}
}

// add records a completed output item from response.output_item.done.
func (c *responsesReplayCapture) add(ctx context.Context, outputIdx int, itemType string, raw json.RawMessage) {
	if c.disabled {
		return
	}
	if !replayableResponsesItem(itemType) {
		c.disable(ctx, replayReasonUnreplayable, slog.String("item_type", itemType))
		return
	}
	if c.size+len(raw) > maxResponsesReplayBytes {
		c.disable(ctx, replayReasonOverCap,
			slog.Int("bytes", c.size+len(raw)),
			slog.Int("limit_bytes", maxResponsesReplayBytes))
		return
	}
	c.size += len(raw)
	c.streamed = append(c.streamed, responsesStreamedItem{outputIdx: outputIdx, raw: raw})
}

// finalize returns the ReplayFields for the turn's message_complete event,
// or nil when the turn is not replayed. output is the terminal event's
// response.output array; when present it is authoritative (it carries the
// final encrypted_content), otherwise the streamed done items are used.
func (c *responsesReplayCapture) finalize(ctx context.Context, output []json.RawMessage, incomplete bool) map[string]json.RawMessage {
	if c.disabled {
		return nil
	}
	if incomplete {
		c.disable(ctx, replayReasonIncomplete)
		return nil
	}
	items := output
	if len(items) == 0 {
		sort.SliceStable(c.streamed, func(i, j int) bool {
			return c.streamed[i].outputIdx < c.streamed[j].outputIdx
		})
		items = make([]json.RawMessage, len(c.streamed))
		for i, s := range c.streamed {
			items[i] = s.raw
		}
	}
	if len(items) == 0 {
		return nil
	}

	if _, reason, itemType := checkResponsesReplayItems(items); reason != "" {
		var attrs []slog.Attr
		if itemType != "" {
			attrs = append(attrs, slog.String("item_type", itemType))
		}
		c.disable(ctx, reason, attrs...)
		return nil
	}

	stored := make([]json.RawMessage, len(items))
	size := 0
	for i, raw := range items {
		item, err := stripResponsesCreatedBy(raw)
		if err != nil {
			c.disable(ctx, replayReasonUndecodable)
			return nil
		}
		stored[i] = item
		size += len(item)
	}
	if size > maxResponsesReplayBytes {
		c.disable(ctx, replayReasonOverCap,
			slog.Int("bytes", size),
			slog.Int("limit_bytes", maxResponsesReplayBytes))
		return nil
	}

	var buf bytes.Buffer
	buf.Grow(size + len(stored) + 1)
	buf.WriteByte('[')
	for i, item := range stored {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(item)
	}
	buf.WriteByte(']')
	origin, err := json.Marshal(c.origin)
	if err != nil {
		c.disable(ctx, replayReasonUndecodable)
		return nil
	}

	c.logger.DebugContext(ctx, "openai-responses output replay captured",
		slog.String("provider.type", "openai-responses"),
		slog.String("provider.model", c.model),
		slog.Int("items", len(stored)),
		slog.Int("total_len", size),
	)
	return map[string]json.RawMessage{
		responsesReplayKey:       buf.Bytes(),
		responsesReplayOriginKey: origin,
	}
}

// disable turns replay off for the rest of the turn, logging once.
func (c *responsesReplayCapture) disable(ctx context.Context, reason string, attrs ...slog.Attr) {
	if c.disabled {
		return
	}
	c.disabled = true
	c.streamed = nil
	level := slog.LevelWarn
	if reason == replayReasonNoEncrypted && c.warnedNoEncrypted != nil && !c.warnedNoEncrypted.CompareAndSwap(false, true) {
		level = slog.LevelDebug
	}
	args := []any{
		slog.String("provider.type", "openai-responses"),
		slog.String("provider.model", c.model),
		slog.String("reason", reason),
	}
	for _, a := range attrs {
		args = append(args, a)
	}
	c.logger.Log(ctx, level, "openai-responses output replay disabled for turn; it will be reconstructed", args...)
}

// --- Message translation ---

// translateMessagesResponses converts stirrup's []types.Message format into
// the Responses API's typed input[] array. The system prompt is NOT placed
// in input[] — it goes into the top-level "instructions" field, which is
// returned separately.
//
// Tool calls and tool results become standalone function_call /
// function_call_output items (rather than being attached to the assistant
// message), matching the Responses API's model.
//
// When replayOrigin is non-empty, an assistant turn whose stored output
// items came from that origin and still match its content is replayed
// instead (see replayedResponsesItems); every other turn is reconstructed
// without item ids, so one turn never mixes the two.
//
// search is the request's hosted tool search, or nil when it has none.
// tool_search and tool_search_result blocks are reconstructed only under
// a search, and only as complete pairs (see reconstructableToolSearches);
// otherwise they are dropped. Under a search, a reconstructed call to a
// deferred or namespaced function carries its namespace.
func translateMessagesResponses(messages []types.Message, replayOrigin string, search *responsesToolSearch, nulls strictNullables) []responsesInput {
	var out []responsesInput

	for _, msg := range messages {
		switch msg.Role {
		case "assistant":
			if replayed := replayedResponsesItems(msg, replayOrigin, search, nulls); replayed != nil {
				out = append(out, replayed...)
				continue
			}

			var textParts []string
			var calls []responsesInput
			searches := reconstructableToolSearches(msg.Content, search)

			for _, block := range msg.Content {
				switch block.Type {
				case "text":
					textParts = append(textParts, block.Text)
				case "tool_use":
					args := string(types.NormalizeToolInput(block.Input))
					calls = append(calls, responsesInput{
						Type:      "function_call",
						CallID:    block.ID,
						Name:      block.Name,
						Arguments: args,
						Namespace: search.callNamespace(block.Name),
					})
				case "tool_search":
					if _, ok := searches[block.ID]; ok {
						calls = append(calls, responsesInput{
							Type:            "tool_search_call",
							SearchArguments: types.NormalizeToolInput(block.Input),
						})
					}
				case "tool_search_result":
					if loaded, ok := searches[block.ToolUseID]; ok {
						calls = append(calls, responsesInput{
							Type:        "tool_search_output",
							LoadedTools: loaded,
						})
					}
				}
			}

			if len(textParts) > 0 {
				out = append(out, responsesInput{
					Type: "message",
					Role: "assistant",
					Content: []responsesContentBlock{
						{Type: "output_text", Text: strings.Join(textParts, "")},
					},
				})
			}
			out = append(out, calls...)

		case "user":
			// function_call_output items are emitted in document order; text
			// blocks (the prompt, or mid-run user input injected after the
			// tool results) are batched into a single trailing input_text
			// item, since the Responses API prefers function_call_output
			// items ahead of the next turn's text.
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
					out = append(out, responsesInput{
						Type:   "function_call_output",
						CallID: block.ToolUseID,
						Output: content,
					})
				}
			}
			if len(textParts) > 0 {
				out = append(out, responsesInput{
					Type: "message",
					Role: "user",
					Content: []responsesContentBlock{
						{Type: "input_text", Text: strings.Join(textParts, "")},
					},
				})
			}
		}
	}

	return out
}

// responsesToolSearch is a request's hosted tool search as message
// translation needs it.
type responsesToolSearch struct {
	// definitions maps each function tool's wire name to its translated
	// definition, as the request's tools array carries it.
	definitions map[string]responsesTool
	// namespaces maps a namespace member's wire name to its namespace
	// entry, without the entry's tools.
	namespaces map[string]responsesTool
}

// newResponsesToolSearch indexes a request's translated function tools,
// including the members of each namespace.
func newResponsesToolSearch(tools []responsesTool) *responsesToolSearch {
	s := &responsesToolSearch{
		definitions: make(map[string]responsesTool, len(tools)),
		namespaces:  make(map[string]responsesTool),
	}
	for _, t := range tools {
		switch t.Type {
		case "function":
			s.definitions[t.Name] = t
		case responsesNamespaceType:
			group := responsesTool{Type: t.Type, Name: t.Name, Description: t.Description}
			for _, member := range t.Tools {
				s.definitions[member.Name] = member
				s.namespaces[member.Name] = group
			}
		}
	}
	return s
}

// loaded returns the entries a rebuilt tool_search_output carries for
// names: each definition flagged defer_loading, with namespace members
// grouped under their namespace where the namespace first appears. It
// returns false when a name has no definition in the request.
func (s *responsesToolSearch) loaded(names []string) ([]responsesTool, bool) {
	out := make([]responsesTool, 0, len(names))
	groupIndex := make(map[string]int)
	for _, name := range names {
		def, ok := s.definitions[name]
		if !ok {
			return nil, false
		}
		truthy := true
		def.DeferLoading = &truthy
		group, member := s.namespaces[name]
		if !member {
			out = append(out, def)
			continue
		}
		i, seen := groupIndex[group.Name]
		if !seen {
			i = len(out)
			groupIndex[group.Name] = i
			out = append(out, group)
		}
		out[i].Tools = append(out[i].Tools, def)
	}
	return out, true
}

// callNamespace returns the namespace a reconstructed function_call to
// name carries: its namespace for a namespace member, its own name for
// an ungrouped deferred function, and "" for an eager function or a
// request without a hosted search. The API resolves an eager function
// in its default namespace.
func (s *responsesToolSearch) callNamespace(name string) string {
	if s == nil {
		return ""
	}
	if group, ok := s.namespaces[name]; ok {
		return group.Name
	}
	if def, ok := s.definitions[name]; ok && def.DeferLoading != nil && *def.DeferLoading {
		return name
	}
	return ""
}

// reconstructableToolSearches returns, keyed by call ID, the loaded
// definitions of each provider-side search in blocks that can be rebuilt
// as a tool_search_call and tool_search_output pair. A search is left out,
// and both its blocks dropped, when the request has no hosted search, when
// either block is missing or repeated, when the search failed (no error
// shape is documented for hosted search output), when its input is not
// valid JSON, or when a loaded tool has no definition in the request.
func reconstructableToolSearches(blocks []types.ContentBlock, search *responsesToolSearch) map[string][]responsesTool {
	if search == nil {
		return nil
	}
	calls := make(map[string]int)
	results := make(map[string]int)
	invalidInput := make(map[string]bool)
	foreign := make(map[string]bool)
	for _, b := range blocks {
		switch b.Type {
		case "tool_search":
			calls[b.ID]++
			if b.Name != responsesToolSearchType {
				foreign[b.ID] = true
			}
			if !json.Valid(types.NormalizeToolInput(b.Input)) {
				invalidInput[b.ID] = true
			}
		case "tool_search_result":
			results[b.ToolUseID]++
		}
	}
	out := make(map[string][]responsesTool)
	for _, b := range blocks {
		id := b.ToolUseID
		if b.Type != "tool_search_result" || b.IsError || calls[id] != 1 || results[id] != 1 || invalidInput[id] || foreign[id] {
			continue
		}
		if loaded, ok := search.loaded(b.ToolReferences); ok {
			out[b.ToolUseID] = loaded
		}
	}
	return out
}

// replayedResponsesItems returns msg's stored output items as input items,
// or nil when the turn must be reconstructed. The stored items are used
// only when they came from origin, pass checkResponsesReplayItems, and
// still describe msg: each tool_use block must match a stored function_call
// by call_id, name and arguments (one to one), and the stored output_text
// must equal the text blocks. A message the harness has since rewritten
// therefore never replays stale items. Replayed items are semantically
// identical to the stored JSON; json.Marshal compacts and HTML-escapes them.
// Stored tool search items take no part in the match and replay in place,
// but only to a request with a hosted search (search non-nil). A stored
// function_call replays only under the namespace this request would give a
// reconstructed call, so a call loaded by hosted search never reaches a
// request without one. Stored arguments are compared without the synthetic
// nulls listed in nulls, which the stream parser dropped from the tool_use
// input.
func replayedResponsesItems(msg types.Message, origin string, search *responsesToolSearch, nulls strictNullables) []responsesInput {
	if origin == "" {
		return nil
	}
	var storedOrigin string
	if err := json.Unmarshal(msg.ReplayFields[responsesReplayOriginKey], &storedOrigin); err != nil || storedOrigin != origin {
		return nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(msg.ReplayFields[responsesReplayKey], &raws); err != nil || len(raws) == 0 {
		return nil
	}
	items, reason, _ := checkResponsesReplayItems(raws)
	if reason != "" {
		return nil
	}

	storedCalls := make(map[string]responsesReplayItem)
	var storedText strings.Builder
	for _, item := range items {
		switch item.Type {
		case "tool_search_call", "tool_search_output":
			if search == nil {
				return nil
			}
		case "function_call":
			if _, dup := storedCalls[item.CallID]; dup || item.Namespace != search.callNamespace(item.Name) {
				return nil
			}
			storedCalls[item.CallID] = item
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					storedText.WriteString(part.Text)
				}
			}
		}
	}

	matched := make(map[string]bool, len(storedCalls))
	var blockText strings.Builder
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			blockText.WriteString(block.Text)
		case "tool_use":
			call, ok := storedCalls[block.ID]
			if !ok || matched[block.ID] || call.Name != block.Name || !sameToolArguments(call, block.Input, nulls) {
				return nil
			}
			matched[block.ID] = true
		}
	}
	if len(matched) != len(storedCalls) || storedText.String() != blockText.String() {
		return nil
	}

	out := make([]responsesInput, len(raws))
	for i, raw := range raws {
		out[i] = responsesInput{Type: items[i].Type, Raw: raw}
	}
	return out
}

// sameToolArguments reports whether a stored function_call arguments string
// and a tool_use block's input hold the same JSON value once the stored
// arguments lose the synthetic nulls nulls lists for the call's tool.
func sameToolArguments(call responsesReplayItem, input json.RawMessage, nulls strictNullables) bool {
	stored := call.Arguments
	if stored == "" {
		stored = "{}"
	}
	var storedValue, inputValue any
	if json.Unmarshal([]byte(stored), &storedValue) != nil {
		return false
	}
	if args, ok := storedValue.(map[string]any); ok {
		nulls.dropFrom(call.Name, args)
	}
	if json.Unmarshal(types.NormalizeToolInput(input), &inputValue) != nil {
		return false
	}
	return reflect.DeepEqual(storedValue, inputValue)
}

// translateToolsResponses converts stirrup ToolDefinitions into the
// Responses API's flatter tool schema. Unlike Chat Completions, there is no
// nested "function" object — the name/description/parameters live directly
// on the tool item.
//
// strict / model / cache behave the same way as translateTools on the
// Chat Completions side: when strict is true, every tool's schema is
// rewritten by NormalizeStrictSchema and the wire entry carries
// strict=true; the cache memoises rewrites within the adapter's
// lifetime. A tool whose schema fails the lint is sent with its original
// schema and an explicit strict=false (an omitted strict on this surface
// means strict when possible); rejections the cache records for the first
// time are returned in fallbacks so the caller can log them. nulls records
// the optional properties of every tool sent strict:true, keyed by tool
// name (see strictNullables). An error returns no fallbacks: the request is
// not sent.
//
// When toolSearch is true, every Deferred tool carries defer_loading,
// tools sharing a Namespace are grouped into one namespace entry (see
// groupResponsesNamespaces), and the hosted tool search entry is appended
// last; otherwise Deferred and Namespace are ignored and every tool is
// sent loaded and ungrouped.
func translateToolsResponses(tools []types.ToolDefinition, strict, examples, toolSearch bool, model string, cache *strictSchemaCache) (out []responsesTool, fallbacks []*strictSchemaError, nulls strictNullables, err error) {
	if len(tools) == 0 {
		return nil, nil, nil, nil
	}
	out = make([]responsesTool, len(tools), len(tools)+1)
	for i, t := range tools {
		entry := responsesTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		}
		if toolSearch && t.Deferred {
			truthy := true
			entry.DeferLoading = &truthy
		}
		strictTool := strict
		if strict {
			schema, nullable, ok, fallback, err := strictToolSchema(cache, model, t.Name, t.InputSchema)
			if err != nil {
				return nil, nil, nil, err
			}
			entry.Parameters = schema
			entry.Strict = &ok
			strictTool = ok
			nulls = nulls.with(t.Name, nullable)
			if fallback != nil {
				fallbacks = append(fallbacks, fallback)
			}
		}
		if !strictTool && examples {
			// Fold worked examples into the schema, but only for
			// non-strict tools: the structured-outputs subset rejects the
			// `examples` keyword, identical to the Chat Completions side.
			merged, err := mergeSchemaExamples(entry.Parameters, toolInputExamples(t))
			if err != nil {
				return nil, nil, nil, fmt.Errorf("tool %q: merge examples: %w", t.Name, err)
			}
			entry.Parameters = merged
		}
		out[i] = entry
	}
	if toolSearch {
		out = groupResponsesNamespaces(tools, out)
		out = append(out, responsesTool{Type: responsesToolSearchType})
	}
	return out, fallbacks, nulls, nil
}

// groupResponsesNamespaces replaces the function entries of tools that
// share a Namespace with one namespace entry, placed where its first
// member was; entries are the translated tools, index for index. A
// namespace whose name is also a function's name is not formed, and its
// members stay ungrouped, so a search path never names both.
func groupResponsesNamespaces(tools []types.ToolDefinition, entries []responsesTool) []responsesTool {
	functionNames := make(map[string]bool, len(entries))
	for _, e := range entries {
		functionNames[e.Name] = true
	}
	out := make([]responsesTool, 0, len(entries)+1)
	groupIndex := make(map[string]int)
	for i, e := range entries {
		ns := tools[i].Namespace
		if ns == "" || functionNames[ns] {
			out = append(out, e)
			continue
		}
		g, seen := groupIndex[ns]
		if !seen {
			g = len(out)
			groupIndex[ns] = g
			out = append(out, responsesTool{
				Type:        responsesNamespaceType,
				Name:        ns,
				Description: tools[i].NamespaceDescription,
			})
		}
		out[g].Tools = append(out[g].Tools, e)
	}
	return out
}

// responsesToolSearchActive reports whether a request uses OpenAI's hosted
// tool search: the loop asked for it, the resolved quirks support it, and
// at least one tool is deferred.
func responsesToolSearchActive(params types.StreamParams, capability quirks.ToolSearchCapability) bool {
	if !params.ToolSearch || !capability.Supported {
		return false
	}
	return slices.ContainsFunc(params.Tools, func(t types.ToolDefinition) bool { return t.Deferred })
}

// buildResponsesRequest projects a StreamParams into the Responses API wire
// body. The Stream field is set by the streaming caller after this returns;
// the builder leaves it false so batch callers get an omitted field.
//
// q carries the resolved quirks for the (provider, model) pair; the
// Responses behaviour flags are threaded onto the request struct so
// MarshalJSON projects the body they selected. A tool whose schema fails
// the strict-mode lint is built with strict:false and reported through the
// request's strictFallbacks; only a malformed schema returns an error.
//
// replayOrigin is the caller's responsesReplayOriginFor value; stored
// output items from that origin are replayed, and "" reconstructs every
// turn.
//
// TODO(batch): consider returning json.RawMessage if endpoint-contract drift
// becomes a maintenance burden.
func buildResponsesRequest(params types.StreamParams, q quirks.ProviderQuirks, strictCache *strictSchemaCache, replayOrigin string) (responsesRequest, error) {
	toolSearch := responsesToolSearchActive(params, q.ToolSearch)
	tools, fallbacks, nulls, err := translateToolsResponses(params.Tools, q.BehaviourFlags.OpenAI.StrictMode, q.ToolExamples.Supported, toolSearch, params.Model, strictCache)
	if err != nil {
		return responsesRequest{}, err
	}
	var search *responsesToolSearch
	if toolSearch {
		search = newResponsesToolSearch(tools)
	}
	effort := projectReasoningEffort(params.ReasoningEffort, q.BehaviourFlags.OpenAIResponses.ReasoningEffortLevels)
	temperature := params.Temperature
	if responsesOmitsTemperature(q, effort) {
		temperature = nil
	}
	var summary string
	if effort != "" && types.ResolveReasoningSummary("openai-responses", params.ReasoningSummary) == types.ReasoningSummaryAuto {
		summary = responsesReasoningSummaryAuto
	}
	var cacheKey string
	if q.BehaviourFlags.OpenAIResponses.PromptCacheKey {
		cacheKey = params.CacheKey
	}
	return responsesRequest{
		Model:             params.Model,
		Instructions:      params.System,
		Input:             translateMessagesResponses(params.Messages, replayOrigin, search, nulls),
		Tools:             tools,
		strictFallbacks:   fallbacks,
		strictNulls:       nulls,
		MaxTokens:         params.MaxTokens,
		Temperature:       temperature,
		ParallelToolCalls: openAIParallelFromParams(params, q.ParallelToolCalls),
		ReasoningEffort:   effort,
		ReasoningSummary:  summary,
		PromptCacheKey:    cacheKey,
		TextFormat:        responsesTextFormatFromParams(params, q.OutputSchema),
		TokenField:        q.BehaviourFlags.OpenAIResponses.TokenField,
		StoreMode:         q.BehaviourFlags.OpenAIResponses.StoreMode,
		InputItemShape:    q.BehaviourFlags.OpenAIResponses.InputItemShape,

		IncludeEncryptedReasoning: q.BehaviourFlags.OpenAIResponses.IncludeEncryptedReasoning,
	}, nil
}

// responsesOmitsTemperature reports whether temperature stays off the wire:
// the model rejects sampling params outright, or a reasoning effort is sent
// (OpenAI documents removing temperature whenever effort is not none).
func responsesOmitsTemperature(q quirks.ProviderQuirks, projectedEffort string) bool {
	return q.BehaviourFlags.OpenAIResponses.OmitSamplingParams || projectedEffort != ""
}

// Stream sends a streaming request to the OpenAI Responses API and returns
// a channel of StreamEvents. The channel is closed when the stream ends or
// an error occurs. Cancelling the context terminates the stream.
func (o *OpenAIResponsesAdapter) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	start := time.Now()
	metricAttrs := metric.WithAttributes(
		attribute.String("provider.type", "openai-responses"),
		attribute.String("provider.model", params.Model),
	)

	registry := o.Registry
	if registry == nil {
		registry = quirks.DefaultRegistry()
	}
	q, appliedRules := registry.ResolveWithRules(o.quirksTarget(params.Model))

	logger := o.Logger
	if logger == nil {
		// The slog.Default() fallback bypasses the ScrubHandler-backed
		// logger the factory injects. On the production path the factory
		// always supplies a real logger; only direct struct-literal
		// construction reaches this branch, where the default handler is
		// unscrubbable.
		logger = slog.Default()
	}

	logger.DebugContext(ctx, "openai-responses quirks resolved",
		slog.String("provider.type", "openai-responses"),
		slog.String("provider.model", params.Model),
		slog.String("provider.endpoint_profile", o.EndpointProfile),
		slog.Any("rules", ruleDescriptions(appliedRules)),
	)

	// The Responses request body carries no tool_choice field, so a
	// non-auto ToolChoice requested against this adapter is silently
	// downgraded to auto. Warn once per Stream call so the downgrade is
	// observable. Only the static mode integer and the adapter / model
	// identifiers are logged — never message content or any secret-derived
	// value.
	if params.ToolChoice != types.ToolChoiceAuto && !q.ToolChoice.Supported {
		logger.WarnContext(ctx, "openai-responses tool-choice downgraded to auto: adapter does not support tool-choice",
			slog.String("provider.type", "openai-responses"),
			slog.String("provider.model", params.Model),
			slog.Int("tool_choice", int(params.ToolChoice)),
		)
	}

	if params.ToolSearch && !q.ToolSearch.Supported {
		logger.DebugContext(ctx, "openai-responses tool search not supported; sending every tool loaded",
			slog.String("provider.type", "openai-responses"),
			slog.String("provider.model", params.Model),
		)
	}

	if q.BehaviourFlags.OpenAI.StrictMode {

		logger.DebugContext(ctx, "openai-responses strict mode applied",
			slog.String("provider.type", "openai-responses"),
			slog.String("provider.model", params.Model),
			slog.Any("quirk.rules", ruleDescriptions(appliedRules)),
		)
	}

	projectedEffort := projectReasoningEffort(params.ReasoningEffort, q.BehaviourFlags.OpenAIResponses.ReasoningEffortLevels)
	if params.Temperature != nil && responsesOmitsTemperature(q, projectedEffort) {
		logger.WarnContext(ctx, "openai-responses quirks suppressed caller temperature",
			slog.String("provider.type", "openai-responses"),
			slog.String("provider.model", params.Model),
			slog.Any("quirk.rules", ruleDescriptions(appliedRules)),
		)
	}

	if err := validateReasoningEffort("openai-responses", params.ReasoningEffort, params.Model, q.BehaviourFlags.OpenAIResponses.ReasoningEffortLevels); err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, err
	}
	warnDroppedReasoningEffort(ctx, logger, "openai-responses", params.ReasoningEffort, params.Model, q.BehaviourFlags.OpenAIResponses.ReasoningEffortLevels)
	logDroppedOutputSchema(ctx, logger, "openai-responses", params.Model, params.OutputSchema, q.OutputSchema)

	replayOrigin := responsesReplayOriginFor(q, params.Model, o.baseURL)
	reqBody, err := buildResponsesRequest(params, q, o.strictSchemas, replayOrigin)
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		return nil, fmt.Errorf("build request: %w", err)
	}
	warnStrictFallbacks(ctx, logger, "openai-responses", params.Model, reqBody.strictFallbacks)
	reqBody.Stream = true

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

	requestURL, err := composeOpenAIURL(o.baseURL, "/responses", o.queryParams)
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
	req.Header.Set("Accept", "text/event-stream")
	setOpenAIAuthHeader(req, apiKey, o.apiKeyHeader)

	// DoWithRetry retries only this pre-stream call (connection errors, or a
	// 429/5xx on the initial response). A failure after streaming has begun
	// is never replayed here; it is classified for the agentic loop, which
	// retries the whole turn.
	resp, err := DoWithRetry(ctx, o.httpClient, req, RetryOptions{
		Policy:       o.RetryPolicy,
		Logger:       o.Logger,
		Metrics:      o.Metrics,
		ProviderType: "openai-responses",
		Model:        params.Model,
	})
	if err != nil {
		o.recordLatency(ctx, start, metricAttrs)
		// The composed URL carries o.baseURL and o.queryParams, either of
		// which may hold a credential (e.g. a gateway api_key query param);
		// unwrap the *url.Error so its embedded URL never leaks (CWE-532).
		return nil, classifyTransportError(fmt.Errorf("execute request: %w", security.UnwrapURLError(err)))
	}
	resp.Body = newIdleTimeoutBody(resp.Body, o.streamIdleTimeout)

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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var errResp responsesErrorResponse
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&errResp); err == nil && (errResp.Error.Message != "" || errResp.Error.Code != "") {
			err := fmt.Errorf("openai responses API returned status %d: %s", resp.StatusCode, responsesErrorText(errResp.Error.Message, errResp.Error.Code))
			if resp.StatusCode == http.StatusBadRequest && responsesContextOverflow(errResp.Error.Code, errResp.Error.Message) {
				err = types.MarkContextOverflow(err)
			}
			return nil, classifyStatusError(err, resp.StatusCode, body)
		}
		return nil, classifyStatusError(fmt.Errorf("openai responses API returned status %d", resp.StatusCode), resp.StatusCode, body)
	}

	ch := make(chan types.StreamEvent, 64)
	go func() {
		o.consumeSSE(ctx, resp, ch, start, metricAttrs, logger, params.Model, replayOrigin, reqBody.strictNulls)
		// Record latency on a background context: the caller's `ctx` may
		// already have been cancelled by the time the stream completes
		// (the agentic loop has moved on), and some OTel exporters drop
		// measurements on cancelled contexts. Synchronous error paths
		// above use the live `ctx` because the caller is still waiting.
		o.recordLatency(context.Background(), start, metricAttrs)
	}()
	return ch, nil
}

// recordLatency records the total provider request latency to the
// ProviderLatency histogram. Safe to call when Metrics is nil.
func (o *OpenAIResponsesAdapter) recordLatency(ctx context.Context, start time.Time, attrs metric.MeasurementOption) {
	if o.Metrics == nil {
		return
	}
	o.Metrics.ProviderLatency.Record(ctx, float64(time.Since(start).Milliseconds()), attrs)
}

// consumeSSE reads named-event SSE records from the response body and
// dispatches them. Records are separated by blank lines and may contain
// `event: <name>` and `data: <payload>` fields. Unlike the Chat Completions
// adapter (which only reads `data:` lines), Responses streaming relies on
// the event name to disambiguate payloads — there is no `[DONE]` sentinel.
func (o *OpenAIResponsesAdapter) consumeSSE(ctx context.Context, resp *http.Response, ch chan<- types.StreamEvent, streamStart time.Time, metricAttrs metric.MeasurementOption, logger *slog.Logger, model, replayOrigin string, strictNulls strictNullables) {
	defer close(ch)
	defer func() { _ = resp.Body.Close() }()

	ttfbRecorded := false
	// emitEvent forwards an event to the consumer, recording TTFB on the
	// first substantive event. Returns false if the consumer has gone
	// away (context cancelled) so the caller can unwind without leaking
	// the goroutine on the open HTTP body.
	emitEvent := func(ev types.StreamEvent) bool {
		// TTFB is meant to capture time-to-first-substantive-output; gate
		// it on event types that represent actual model inference output
		// so error-path zero latencies do not pollute the histogram.
		if !ttfbRecorded && o.Metrics != nil && (ev.Type == "text_delta" || ev.Type == "tool_call") {
			o.Metrics.RecordFirstChunk(ctx, metricAttrs, inferenceAttrs("openai-responses", model, o.baseURL), time.Since(streamStart))
			ttfbRecorded = true
		}
		select {
		case ch <- strictNulls.strip(ev):
			return true
		case <-ctx.Done():
			return false
		}
	}

	// Track in-flight function calls. Indexed by a stable key (item_id when
	// present, falling back to a stringified output_index). The value's
	// outputIdx field is preserved so we can flush in deterministic order.
	calls := make(map[string]*responsesCallState)
	replay := newResponsesReplayCapture(logger, model, replayOrigin, &o.replayWarnedNoEncrypted)
	summary := &responsesSummaryParts{logger: logger}
	searches := newResponsesToolSearches(logger)

	scanner := bufio.NewScanner(resp.Body)
	// Increase the buffer ceiling so a single SSE record carrying a large
	// JSON payload (e.g. a response.completed envelope) does not trip
	// bufio's default 64KB scanner limit.
	scanner.Buffer(make([]byte, 0, 64*1024), openaiMaxToolInputSize)

	var currentEvent string
	var dataParts []string
	var dataLen int // aggregate byte length of dataParts; capped to prevent OOM

	flushRecord := func() bool {
		// Reset event-record state on return; defer-style guard so any
		// early return below still leaves a clean slate. We do this
		// via explicit assignment because we need the captured values
		// inside the dispatch.
		data := strings.Join(dataParts, "\n")
		eventName := responsesRecordEvent(currentEvent, data)
		currentEvent = ""
		dataParts = dataParts[:0]
		dataLen = 0

		if eventName == "" || data == "" {
			return true
		}
		return o.dispatchEvent(ctx, eventName, data, calls, replay, summary, searches, emitEvent)
	}

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			emitEvent(types.StreamEvent{Type: "error", Error: ctx.Err()})
			return
		default:
		}
		// After a read error Scan still yields the partial last line; it is
		// not a complete record, so report the read error instead.
		if scanner.Err() != nil {
			break
		}

		line := scanner.Text()

		// Blank line terminates an SSE record. Process the accumulated
		// event/data and reset.
		if line == "" {
			if !flushRecord() {
				return
			}
			continue
		}

		// SSE comments start with ":" and must be ignored.
		if strings.HasPrefix(line, ":") {
			continue
		}

		// appendData stages a data payload chunk, enforcing the aggregate
		// size cap before allocating. Returns false if the cap was hit
		// (caller should stop reading the stream).
		appendData := func(chunk string) bool {
			if dataLen+len(chunk) > openaiMaxToolInputSize {
				emitEvent(types.StreamEvent{
					Type:  "error",
					Error: fmt.Errorf("SSE record data exceeds %d byte limit", openaiMaxToolInputSize),
				})
				return false
			}
			dataLen += len(chunk)
			dataParts = append(dataParts, chunk)
			return true
		}

		switch {
		case strings.HasPrefix(line, "event: "):
			currentEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "event:"):
			currentEvent = strings.TrimPrefix(line, "event:")
		case strings.HasPrefix(line, "data: "):
			if !appendData(strings.TrimPrefix(line, "data: ")) {
				return
			}
		case strings.HasPrefix(line, "data:"):
			if !appendData(strings.TrimPrefix(line, "data:")) {
				return
			}
		}
	}

	if err := scanner.Err(); err != nil {
		emitEvent(types.StreamEvent{Type: "error", Error: classifyTransportError(fmt.Errorf("read SSE stream: %w", err))})
		return
	}

	// Flush any trailing record without a terminating blank line. A
	// well-behaved server will not do this, but tolerating it avoids
	// dropped final events on premature EOF.
	if currentEvent != "" || len(dataParts) > 0 {
		flushRecord()
	}
}

// responsesSummaryParts tracks the reasoning-summary part the stream is in,
// so the first delta of each later part starts on a new paragraph rather
// than running on from the previous part's last sentence.
type responsesSummaryParts struct {
	logger *slog.Logger
	itemID string
	index  int
	seen   bool
}

func (s *responsesSummaryParts) text(itemID string, index int, delta string) string {
	if s.seen && (itemID != s.itemID || index != s.index) {
		delta = "\n\n" + delta
	}
	s.itemID, s.index, s.seen = itemID, index, true
	return delta
}

// responsesToolSearches turns hosted tool_search_call and
// tool_search_output items into tool_search and tool_search_result
// events. Hosted items carry a null call_id, so each output is paired with
// the earliest unanswered call by item id.
type responsesToolSearches struct {
	logger  *slog.Logger
	pending []string
}

// newResponsesToolSearches returns the search pairing state for one
// response. A nil logger discards its debug records.
func newResponsesToolSearches(logger *slog.Logger) *responsesToolSearches {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &responsesToolSearches{logger: logger}
}

// call returns the tool_search event for a completed tool_search_call.
func (s *responsesToolSearches) call(ctx context.Context, item responsesOutputItem, outputIdx int) types.StreamEvent {
	id := item.ID
	if id == "" {
		id = item.CallID
	}
	if id == "" {
		id = fmt.Sprintf("tool_search_%d", outputIdx)
	}
	s.pending = append(s.pending, id)
	return types.StreamEvent{
		Type:  "tool_search",
		ID:    id,
		Name:  responsesToolSearchType,
		Input: s.input(ctx, item.SearchArguments),
	}
}

// input decodes a tool_search_call's arguments, which hosted search sends
// as a JSON object. A string holding a JSON object is accepted too; any
// other value yields no input.
func (s *responsesToolSearches) input(ctx context.Context, raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var input map[string]any
	if json.Unmarshal(raw, &input) == nil {
		return input
	}
	var text string
	if json.Unmarshal(raw, &text) == nil && json.Unmarshal([]byte(text), &input) == nil {
		return input
	}
	s.logger.DebugContext(ctx, "openai-responses tool_search_call arguments are not a JSON object",
		slog.String("provider.type", "openai-responses"),
		slog.Int("arguments_len", len(raw)),
	)
	return nil
}

// output returns the tool_search_result event for a completed
// tool_search_output, naming every function it loaded.
func (s *responsesToolSearches) output(ctx context.Context, item responsesOutputItem) types.StreamEvent {
	var id string
	switch {
	case len(s.pending) > 0:
		id, s.pending = s.pending[0], s.pending[1:]
	case item.CallID != "":
		id = item.CallID
	default:
		id = item.ID
	}
	names, err := responsesLoadedToolNames(item.Tools)
	if err != nil {
		s.logger.DebugContext(ctx, "openai-responses tool_search_output tools undecodable; no tools loaded",
			slog.String("provider.type", "openai-responses"),
			slog.Int("tools_len", len(item.Tools)),
		)
	}
	return types.StreamEvent{Type: "tool_search_result", ID: id, ToolReferences: names}
}

// responsesLoadedTool is one entry of a tool_search_output's tools array:
// a function, or a namespace wrapping further entries.
type responsesLoadedTool struct {
	Type  string          `json:"type"`
	Name  string          `json:"name"`
	Tools json.RawMessage `json:"tools"`
}

// responsesMaxNamespaceDepth bounds how deep responsesLoadedToolNames
// descends: the top-level array is depth 1 and a namespace's tools are
// depth 2. Namespaces nested further are ignored without being decoded.
const responsesMaxNamespaceDepth = 2

// responsesLoadedToolNames returns the names of the functions in a
// tool_search_output's tools array, flattening namespaces up to
// responsesMaxNamespaceDepth, in order and without duplicates.
func responsesLoadedToolNames(raw json.RawMessage) ([]string, error) {
	var names []string
	seen := make(map[string]bool)
	var walk func(json.RawMessage, int) error
	walk = func(raw json.RawMessage, depth int) error {
		if len(raw) == 0 {
			return nil
		}
		var entries []responsesLoadedTool
		if err := json.Unmarshal(raw, &entries); err != nil {
			return err
		}
		for _, e := range entries {
			switch e.Type {
			case "function":
				if e.Name != "" && !seen[e.Name] {
					seen[e.Name] = true
					names = append(names, e.Name)
				}
			case "namespace":
				if depth < responsesMaxNamespaceDepth {
					if err := walk(e.Tools, depth+1); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if err := walk(raw, 1); err != nil {
		return nil, err
	}
	return names, nil
}

// responsesRecordEvent returns an SSE record's event name: its event
// field, or the payload's type for a server such as OpenRouter that sends
// data-only records.
func responsesRecordEvent(event, data string) string {
	if event != "" || data == "" {
		return event
	}
	var payload struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(data), &payload) != nil {
		return ""
	}
	return payload.Type
}

// dispatchEvent handles a single completed SSE record. It returns false to
// signal the caller to stop reading (terminal event or consumer cancelled),
// true to continue.
//
// `emit` returns false when the consumer has gone away (context cancelled);
// every emit call site propagates that to abandon the stream rather than
// pretending to keep going.
func (o *OpenAIResponsesAdapter) dispatchEvent(ctx context.Context, name, data string, calls map[string]*responsesCallState, replay *responsesReplayCapture, summary *responsesSummaryParts, searches *responsesToolSearches, emit func(types.StreamEvent) bool) bool {
	switch name {
	case "response.created":
		// Optional metadata; nothing to emit.
		return true

	case "response.output_item.added":
		var payload struct {
			OutputIndex int                 `json:"output_index"`
			Item        responsesOutputItem `json:"item"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse output_item.added: %w", err)})
			return false
		}
		if payload.Item.Type == "function_call" {
			key := callKey(payload.Item.ID, payload.OutputIndex)
			st, exists := calls[key]
			if !exists {
				st = &responsesCallState{
					itemID:    payload.Item.ID,
					outputIdx: payload.OutputIndex,
				}
				calls[key] = st
			}
			if payload.Item.CallID != "" {
				st.callID = payload.Item.CallID
			}
			if payload.Item.Name != "" {
				st.name = payload.Item.Name
			}
			// Some providers include the (complete) arguments string here
			// when the model emitted the call atomically. Seed argsBuf
			// from it so we still produce a tool_call on .done.
			if payload.Item.Arguments != "" {
				st.argsBuf.WriteString(payload.Item.Arguments)
			}
		}
		return true

	case "response.output_text.delta":
		var payload struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse output_text.delta: %w", err)})
			return false
		}
		if payload.Delta != "" {
			if !emit(types.StreamEvent{Type: "text_delta", Text: payload.Delta}) {
				return false
			}
		}
		return true

	case "response.reasoning_summary_text.delta":
		var payload struct {
			ItemID       string `json:"item_id"`
			SummaryIndex int    `json:"summary_index"`
			Delta        string `json:"delta"`
		}
		// Summary text is display-only, so a malformed delta is dropped
		// rather than failing the turn.
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			summary.logger.DebugContext(ctx, "openai-responses reasoning summary delta dropped", "error", err)
			return true
		}
		if payload.Delta != "" {
			text := summary.text(payload.ItemID, payload.SummaryIndex, payload.Delta)
			if !emit(types.StreamEvent{Type: "thinking_delta", Text: text}) {
				return false
			}
		}
		return true

	case "response.function_call_arguments.delta":
		var payload struct {
			ItemID      string `json:"item_id"`
			OutputIndex int    `json:"output_index"`
			Delta       string `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse function_call_arguments.delta: %w", err)})
			return false
		}
		key := callKey(payload.ItemID, payload.OutputIndex)
		st, exists := calls[key]
		if !exists {
			// .added not yet observed for this call (or was dropped);
			// create a state placeholder so we still accumulate.
			st = &responsesCallState{
				itemID:    payload.ItemID,
				outputIdx: payload.OutputIndex,
			}
			calls[key] = st
		}
		if st.argsBuf.Len()+len(payload.Delta) > openaiMaxToolInputSize {
			emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("tool arguments exceed %d byte limit", openaiMaxToolInputSize)})
			return false
		}
		st.argsBuf.WriteString(payload.Delta)
		return true

	case "response.function_call_arguments.done":
		var payload struct {
			ItemID      string `json:"item_id"`
			OutputIndex int    `json:"output_index"`
			Arguments   string `json:"arguments,omitempty"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse function_call_arguments.done: %w", err)})
			return false
		}
		key := callKey(payload.ItemID, payload.OutputIndex)
		st, exists := calls[key]
		if !exists {
			st = &responsesCallState{
				itemID:    payload.ItemID,
				outputIdx: payload.OutputIndex,
			}
			calls[key] = st
		}
		// If the .done event echoes the full arguments string and the
		// streamed deltas are missing, prefer the echoed copy.
		if st.argsBuf.Len() == 0 && payload.Arguments != "" {
			if len(payload.Arguments) > openaiMaxToolInputSize {
				emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("tool arguments exceed %d byte limit", openaiMaxToolInputSize)})
				return false
			}
			st.argsBuf.WriteString(payload.Arguments)
		}
		if !flushOneCall(st, emit) {
			return false
		}
		return true

	case "response.output_item.done":
		var payload struct {
			OutputIndex int             `json:"output_index"`
			Item        json.RawMessage `json:"item"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse output_item.done: %w", err)})
			return false
		}
		var item responsesOutputItem
		if len(payload.Item) > 0 {
			if err := json.Unmarshal(payload.Item, &item); err != nil {
				emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse output_item.done: %w", err)})
				return false
			}
		}
		replay.add(ctx, payload.OutputIndex, item.Type, payload.Item)
		switch item.Type {
		case "function_call":
		case "tool_search_call":
			return emit(searches.call(ctx, item, payload.OutputIndex))
		case "tool_search_output":
			return emit(searches.output(ctx, item))
		default:
			return true
		}
		key := callKey(item.ID, payload.OutputIndex)
		st, exists := calls[key]
		if !exists {
			st = &responsesCallState{
				itemID:    item.ID,
				outputIdx: payload.OutputIndex,
			}
			calls[key] = st
		}
		if item.CallID != "" {
			st.callID = item.CallID
		}
		if item.Name != "" {
			st.name = item.Name
		}
		// If the .done event carries the full arguments string and the
		// streamed deltas were never seen, prefer the echoed copy.
		if st.argsBuf.Len() == 0 && item.Arguments != "" {
			if len(item.Arguments) > openaiMaxToolInputSize {
				emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("tool arguments exceed %d byte limit", openaiMaxToolInputSize)})
				return false
			}
			st.argsBuf.WriteString(item.Arguments)
		}
		if !flushOneCall(st, emit) {
			return false
		}
		return true

	case "response.completed":
		// Flush any tool calls that never received a .done event (defensive).
		if !flushPendingCalls(calls, emit) {
			return false
		}
		var payload struct {
			Response responsesResponse `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse response.completed: %w", err)})
			return false
		}
		ev := types.StreamEvent{
			Type:          "message_complete",
			StopReason:    deriveStopReason(payload.Response),
			ReplayFields:  replay.finalize(ctx, payload.Response.RawOutput, false),
			ResponseModel: payload.Response.Model,
		}
		if payload.Response.Usage != nil {
			payload.Response.Usage.applyTo(&ev)
		}
		emit(ev)
		// Terminal event: signal caller to stop reading regardless of
		// whether the consumer accepted the message_complete event.
		return false

	case "response.incomplete":
		if !flushPendingCalls(calls, emit) {
			return false
		}
		var payload struct {
			Response responsesResponse `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			emit(types.StreamEvent{Type: "error", Error: fmt.Errorf("parse response.incomplete: %w", err)})
			return false
		}
		stop := "incomplete"
		if payload.Response.IncompleteDetails != nil {
			reason := payload.Response.IncompleteDetails.Reason
			// "max_output_tokens" / "max_tokens" both map to our existing
			// max_tokens stop reason. Anything else passes through verbatim
			// for diagnostic visibility.
			if reason == "max_output_tokens" || reason == "max_tokens" {
				stop = "max_tokens"
			} else if reason != "" {
				stop = reason
			}
		}
		ev := types.StreamEvent{
			Type:          "message_complete",
			StopReason:    stop,
			ReplayFields:  replay.finalize(ctx, payload.Response.RawOutput, true),
			ResponseModel: payload.Response.Model,
		}
		if payload.Response.Usage != nil {
			payload.Response.Usage.applyTo(&ev)
		}
		emit(ev)
		return false

	case "response.failed":
		var payload struct {
			Response struct {
				Error *struct {
					Message string             `json:"message"`
					Type    string             `json:"type"`
					Code    responsesErrorCode `json:"code"`
				} `json:"error"`
				Status string `json:"status"`
			} `json:"response"`
		}
		_ = json.Unmarshal([]byte(data), &payload)
		msg := "openai responses API: response failed"
		overflow := false
		var code responsesErrorCode
		if e := payload.Response.Error; e != nil && (e.Message != "" || e.Code != "") {
			message := e.Message
			if message == "" {
				message = "response failed"
			}
			msg = "openai responses API: " + responsesErrorText(message, e.Code)
			overflow = responsesContextOverflow(e.Code, e.Message)
			code = e.Code
		}
		err := responsesStreamError(msg, code)
		if overflow {
			err = types.MarkContextOverflow(err)
		}
		emit(types.StreamEvent{Type: "error", Error: err})
		return false

	case "error":
		var payload struct {
			Message string             `json:"message"`
			Type    string             `json:"type"`
			Code    responsesErrorCode `json:"code"`
		}
		_ = json.Unmarshal([]byte(data), &payload)
		msg := "openai responses API stream error"
		if payload.Message != "" || payload.Code != "" {
			msg += ": " + responsesErrorText(payload.Message, payload.Code)
		}
		err := responsesStreamError(msg, payload.Code)
		if responsesContextOverflow(payload.Code, payload.Message) {
			err = types.MarkContextOverflow(err)
		}
		emit(types.StreamEvent{Type: "error", Error: err})
		return false

	default:
		// Forward-compatible: unknown events (e.g. reasoning summary part
		// lifecycle, content_part lifecycle, partial-image deltas) are ignored. We
		// add a span event so production operators can spot a flood of
		// new event types from a future API revision instead of silently
		// dropping content.
		if span := oteltrace.SpanFromContext(ctx); span != nil && span.IsRecording() {
			span.AddEvent("openai_responses.unknown_sse_event",
				oteltrace.WithAttributes(attribute.String("event.type", name)),
			)
		}
		return true
	}
}

// callKey produces a stable identifier for a function call. item_id is
// preferred when the server provides one; falling back to output_index keeps
// us robust against partner gateways that omit it.
func callKey(itemID string, outputIndex int) string {
	if itemID != "" {
		return itemID
	}
	return fmt.Sprintf("idx:%d", outputIndex)
}

// flushOneCall emits a tool_call event for the supplied call state, parsing
// its accumulated arguments JSON. Returns false when the consumer has gone
// away (caller should stop reading the stream). Marks the call as emitted
// so a duplicate .done event does not fire it twice.
func flushOneCall(st *responsesCallState, emit func(types.StreamEvent) bool) bool {
	if st.emitted {
		return true
	}
	st.emitted = true
	return emit(toolCallEvent(st.callID, st.name, st.argsBuf.String()))
}

// flushPendingCalls emits any tool calls that were left in flight when the
// terminal response.completed / response.incomplete event arrived. Order is
// stable by output_index so multi-call responses are deterministic.
func flushPendingCalls(calls map[string]*responsesCallState, emit func(types.StreamEvent) bool) bool {
	pending := make([]*responsesCallState, 0, len(calls))
	for _, st := range calls {
		if !st.emitted {
			pending = append(pending, st)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].outputIdx < pending[j].outputIdx
	})
	for _, st := range pending {
		if !flushOneCall(st, emit) {
			return false
		}
	}
	return true
}

// deriveStopReason adapts the streaming Responses response shape to
// the shared deriveResponsesStopReason helper. Computes the
// hasTool / incomplete-reason inputs from resp and dispatches; the
// branch logic lives in batch.go so a new status arm is applied to
// the batch fabrication path simultaneously.
func deriveStopReason(resp responsesResponse) string {
	hasTool := false
	for _, item := range resp.Output {
		if item.Type == "function_call" {
			hasTool = true
			break
		}
	}
	incompleteReason := ""
	if resp.IncompleteDetails != nil {
		incompleteReason = resp.IncompleteDetails.Reason
	}
	return deriveResponsesStopReason(resp.Status, incompleteReason, hasTool)
}
