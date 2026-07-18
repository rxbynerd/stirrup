package provider

import "encoding/json"

// Go shapes for the Vertex AI Gemini adapter's request and SSE response
// wire format. Field tags follow Vertex AI's `:streamGenerateContent`
// REST documentation:
// https://cloud.google.com/vertex-ai/generative-ai/docs/model-reference/inference

// generateContentRequest is the JSON body sent to `:streamGenerateContent`.
// Pointer fields are used where unset must be wire-distinguishable from zero.
type generateContentRequest struct {
	Contents          []geminiContent         `json:"contents"`
	SystemInstruction *geminiContent          `json:"systemInstruction,omitempty"`
	Tools             []geminiTools           `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig       `json:"toolConfig,omitempty"`
	SafetySettings    []geminiSafetySetting   `json:"safetySettings,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
}

// geminiContent is one turn or one tool exchange in the Contents array.
// Roles: "user" (input), "model" (assistant output), "function" (tool
// results, on a separate Content entry from surrounding user text).
// SystemInstruction also uses Content, with role omitted.
type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

// geminiPart is a discriminated union over the shapes Vertex accepts in a
// content's parts array; exactly one of Text, FunctionCall, or
// FunctionResponse should be populated per part. InlineData / fileData /
// videoMetadata are intentionally unsupported — text-and-tools only.
//
// ThoughtSignature is an opaque blob Vertex emits on Gemini 3.x responses
// for cross-turn reasoning continuity; echoed back verbatim, never
// introspected. See docs/provider-quirks.md for the ReplayFields threading.
// The `thoughtSignature` JSON tag intentionally matches Vertex's camelCase
// wire format rather than the snake_case used elsewhere — do not "fix" it.
type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
	ThoughtSignature string                  `json:"thoughtSignature,omitempty"`
}

// geminiFunctionCall is a model-emitted call to one of the declared tools.
// PartialArgs and WillContinue are deserialise-only, populated only when
// streamFunctionCallArguments is enabled (see geminiToolConfig); the
// adapter leaves that flag off, but the parser stays tolerant of it.
type geminiFunctionCall struct {
	Name         string          `json:"name"`
	Args         json.RawMessage `json:"args,omitempty"`
	PartialArgs  json.RawMessage `json:"partialArgs,omitempty"`
	WillContinue bool            `json:"willContinue,omitempty"`
}

// geminiFunctionResponse delivers a tool execution result back to the model.
// Vertex matches it to the originating functionCall by Name (no ID echo),
// so the request builder maintains its own ID→name map (toolNameByID).
//
// Response is a json.RawMessage rather than a map[string]any so the
// request builder marshals a typed geminiFunctionResponseBody instead of
// an untyped map.
type geminiFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

// geminiFunctionResponseBody is the typed shape marshalled into
// geminiFunctionResponse.Response. Content is always present as a text
// fallback. Structured is emitted only when the resolved
// StructuredToolResults capability accepts the object-response shape; see
// docs/provider-quirks.md and the structured tool results section of
// docs/architecture.md.
type geminiFunctionResponseBody struct {
	Content    string          `json:"content"`
	Error      bool            `json:"error,omitempty"`
	Structured json.RawMessage `json:"structured,omitempty"`
	Kind       string          `json:"kind,omitempty"`
}

// geminiTools wraps a list of declarations. The harness always emits
// exactly one entry containing every declared function; Vertex's
// multi-entry form is reserved for built-in tools this adapter doesn't expose.
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
// streamFunctionCallArguments is deliberately left off; see
// docs/provider-quirks.md for why.
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
// one SSE `data:` event. The Candidates array updates incrementally — a
// chunk with the same candidate index appends to the previous chunk's parts.
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
