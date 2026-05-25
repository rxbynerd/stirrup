package provider

import "encoding/json"

// This file declares the Go shapes used by the Vertex AI Gemini adapter for
// both request marshalling (Wave 3) and SSE consumption (Wave 4). Keeping
// them in a shared file means Wave 4 does not need to retype any of the
// response/chunk structs.
//
// All field tags follow Vertex AI's REST documentation for
// `:streamGenerateContent`:
//   https://cloud.google.com/vertex-ai/generative-ai/docs/model-reference/inference

// generateContentRequest is the JSON body sent to
// `:streamGenerateContent`. Field names match the API exactly. Pointer
// types are used where unset must be wire-distinguishable from zero
// (Temperature, SystemInstruction, ToolConfig, GenerationConfig).
type generateContentRequest struct {
	Contents          []geminiContent         `json:"contents"`
	SystemInstruction *geminiContent          `json:"systemInstruction,omitempty"`
	Tools             []geminiTools           `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig       `json:"toolConfig,omitempty"`
	SafetySettings    []geminiSafetySetting   `json:"safetySettings,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
}

// geminiContent is one turn or one tool exchange in the Contents array.
// Roles recognised by Vertex AI:
//   - "user"     — operator-side input (text and tool results)
//   - "model"    — assistant output (text and functionCall parts)
//   - "function" — tool result delivery; required role for functionResponse
//     parts. Note: function responses live on a separate Content entry from
//     surrounding user text. The systemInstruction field uses Content too,
//     but its role is omitted (Vertex ignores it there).
type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

// geminiPart is a discriminated-union over the four shapes Vertex accepts in
// a content's parts array. Exactly one of Text, FunctionCall, or
// FunctionResponse should be populated per part. (InlineData /
// fileData / videoMetadata exist in the spec but are intentionally not
// supported by this adapter — the harness is text-and-tools only.)
//
// ThoughtSignature is the one non-discriminator field on this struct: it is
// an opaque blob Vertex emits alongside `text` or `functionCall` parts on
// Gemini 3.x responses (the model's encrypted chain-of-thought for
// cross-turn reasoning continuity). The harness echoes it back unchanged on
// the corresponding part of the assistant's content the next time the same
// history is rendered so the model can resume its prior reasoning. We do
// not introspect, log, or mutate the value — it is provider-private state
// that just happens to be threaded through the message history.
//
// The `thoughtSignature` JSON tag matches the Vertex AI wire format
// directly (camelCase); this differs from the snake_case convention on
// types.ContentBlock and types.StreamEvent (`thought_signature`). Do not
// "fix" this tag to snake_case — Vertex will not recognise it and the
// round-trip will silently break for Gemini 3.x model turns.
type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
	ThoughtSignature string                  `json:"thoughtSignature,omitempty"`
}

// geminiFunctionCall is a model-emitted call to one of the declared tools.
// Args is the argument object the model produced. PartialArgs and
// WillContinue are deserialise-only fields that appear on streamed chunks
// when streamFunctionCallArguments is enabled — the request marshaller
// never emits them. The adapter currently leaves streamFunctionCallArguments
// off (see geminiToolConfig), so these fields are not exercised by the
// happy path; they are retained so the parser remains tolerant of a future
// wire-format reversion or an operator who flips the flag back on.
type geminiFunctionCall struct {
	Name         string          `json:"name"`
	Args         json.RawMessage `json:"args,omitempty"`
	PartialArgs  json.RawMessage `json:"partialArgs,omitempty"`
	WillContinue bool            `json:"willContinue,omitempty"`
}

// geminiFunctionResponse delivers a tool execution result back to the model.
// Vertex matches functionResponse to the originating functionCall by Name —
// there is no ID echo, so the request builder maintains its own ID→name
// map (toolNameByID) to populate this field correctly.
//
// Response is a free-form JSON object. The harness convention is
// {"content": <result-string>}; an additional {"error": true} key is set
// when the tool call failed so the model can react.
type geminiFunctionResponse struct {
	Name     string                 `json:"name"`
	Response map[string]interface{} `json:"response"`
}

// geminiTools wraps a list of declarations. Vertex accepts multiple Tools
// entries, but the harness emits exactly one entry containing every
// declared function. (Multi-entry Tools is reserved for built-in
// retrieval / code-execution tools, which this adapter does not expose.)
type geminiTools struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations"`
}

// geminiFunctionDeclaration is one tool. Parameters is a Gemini-OpenAPI
// schema produced by ConvertSchema; see gemini_schema.go.
type geminiFunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// geminiToolConfig pins tool-calling behaviour for the request.
// streamFunctionCallArguments is deliberately left off for this adapter.
// When the flag is true, Gemini 3.x streams a function call across
// multiple SSE chunks where only the first carries `name` and subsequent
// chunks deliver `partialArgs` as an array of JSON-path delta records
// ({jsonPath, stringValue, willContinue}) rather than the cumulative
// JSON-object snapshots emitted by the 2.x format. Supporting that shape
// would require index-keyed slot correlation plus JSON-path synthesis on
// the receive side, with no upside for this harness — the trace is
// emitted post-turn, not mid-stream, so per-chunk argument visibility is
// not load-bearing. With the flag off, both 2.5 and 3.x emit a single
// functionCall part with `name` and `args` populated in one chunk, which
// the parser already handles uniformly.
type geminiToolConfig struct {
	FunctionCallingConfig geminiFunctionCallingConfig `json:"functionCallingConfig"`
}

// geminiFunctionCallingConfig selects the tool-calling mode and streaming
// behaviour. Mode "AUTO" lets the model choose whether to call a tool;
// "ANY" forces a tool call, "NONE" forbids one. AllowedFunctionNames
// restricts an ANY-mode turn to a specific subset — the harness uses it
// to express a single-named-tool choice (StreamParams.ToolChoiceTool).
// It is omitempty so AUTO/NONE and an unrestricted ANY emit no array.
type geminiFunctionCallingConfig struct {
	Mode                        string   `json:"mode"`
	AllowedFunctionNames        []string `json:"allowedFunctionNames,omitempty"`
	StreamFunctionCallArguments bool     `json:"streamFunctionCallArguments,omitempty"`
}

// geminiSafetySetting is the wire-format struct (HARM_CATEGORY_*,
// BLOCK_*) sent to Vertex. Validated by types.GeminiSafetySetting before
// it reaches this layer; we only translate the field names.
type geminiSafetySetting struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

// geminiGenerationConfig carries the per-request inference knobs. Pointer
// types so the JSON encoder can omit unset values (omitempty alone is
// ambiguous for zero floats).
type geminiGenerationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
}

// generateContentChunk is a partial GenerateContentResponse delivered as
// one SSE `data:` event. Vertex's streamGenerateContent emits the same
// schema as the non-streaming endpoint, but the Candidates array is
// updated incrementally — a chunk with the same candidate index appends
// to the previous chunk's parts. Declared here alongside the request
// shapes so Wave 4 (the SSE stream consumer in gemini.go) does not need
// to retype it.
type generateContentChunk struct {
	Candidates     []geminiCandidate     `json:"candidates,omitempty"`
	UsageMetadata  *geminiUsageMetadata  `json:"usageMetadata,omitempty"`
	PromptFeedback *geminiPromptFeedback `json:"promptFeedback,omitempty"`
}

// geminiPromptFeedback is set on a response (or terminal SSE chunk)
// when Vertex blocks the *prompt itself* on safety policy — the model
// never produced any candidates, so the chunk has no Candidates array
// and the only signal is BlockReason ("SAFETY", "OTHER", etc.). The
// adapter surfaces these as a synthetic message_complete with
// StopReason="safety_blocked" so the agentic loop terminates cleanly
// rather than seeing a silent empty stream.
type geminiPromptFeedback struct {
	BlockReason string `json:"blockReason,omitempty"`
}

// geminiCandidate is one branch of the model's output. The harness only
// uses Index 0; multi-candidate responses are not requested. SafetyRatings
// is opaque (RawMessage) — they are surfaced verbatim to the trace and
// never decoded by the adapter.
type geminiCandidate struct {
	Content       *geminiContent   `json:"content,omitempty"`
	FinishReason  string           `json:"finishReason,omitempty"`
	SafetyRatings *json.RawMessage `json:"safetyRatings,omitempty"`
	Index         int              `json:"index,omitempty"`
}

// geminiUsageMetadata mirrors Vertex's usageMetadata. The harness reports
// CandidatesTokenCount as the OutputTokens for stop-event compatibility
// with the other adapters.
type geminiUsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount int `json:"candidatesTokenCount,omitempty"`
	TotalTokenCount      int `json:"totalTokenCount,omitempty"`
}
