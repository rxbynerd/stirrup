package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// decodedRequest is a relaxed shape used to assert against the marshalled
// request body without coupling tests to the exact Go struct layout. Tests
// decode into this and inspect the relevant fields.
type decodedRequest struct {
	Contents          []decodedContent      `json:"contents"`
	SystemInstruction *decodedContent       `json:"systemInstruction"`
	Tools             []decodedTool         `json:"tools"`
	ToolConfig        *decodedToolConfig    `json:"toolConfig"`
	SafetySettings    []decodedSafetyEntry  `json:"safetySettings"`
	GenerationConfig  *decodedGenerationCfg `json:"generationConfig"`
}

type decodedContent struct {
	Role  string        `json:"role"`
	Parts []decodedPart `json:"parts"`
}

type decodedPart struct {
	Text             string               `json:"text,omitempty"`
	FunctionCall     *decodedFunctionCall `json:"functionCall,omitempty"`
	FunctionResponse *decodedFuncResponse `json:"functionResponse,omitempty"`
}

type decodedFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type decodedFuncResponse struct {
	Name     string                 `json:"name"`
	Response map[string]interface{} `json:"response"`
}

type decodedTool struct {
	FunctionDeclarations []decodedFuncDecl `json:"functionDeclarations"`
}

type decodedFuncDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type decodedToolConfig struct {
	FunctionCallingConfig struct {
		Mode                        string `json:"mode"`
		StreamFunctionCallArguments bool   `json:"streamFunctionCallArguments"`
	} `json:"functionCallingConfig"`
}

type decodedSafetyEntry struct {
	Category  string `json:"category"`
	Threshold string `json:"threshold"`
}

type decodedGenerationCfg struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens"`
}

func decodeGeminiRequest(t *testing.T, body []byte) decodedRequest {
	t.Helper()
	var dr decodedRequest
	if err := json.Unmarshal(body, &dr); err != nil {
		t.Fatalf("decode request body: %v\nbody=%s", err, body)
	}
	return dr
}

func TestBuildGenerateContentRequest_SystemInstructionFromParams(t *testing.T) {
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model:  "gemini-2.5-pro",
		System: "You are a helpful coding assistant.",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dr := decodeGeminiRequest(t, body)
	if dr.SystemInstruction == nil {
		t.Fatalf("expected systemInstruction, got nil")
	}
	if len(dr.SystemInstruction.Parts) != 1 || dr.SystemInstruction.Parts[0].Text != "You are a helpful coding assistant." {
		t.Errorf("systemInstruction.parts mismatch: %+v", dr.SystemInstruction.Parts)
	}
	if dr.SystemInstruction.Role != "" {
		t.Errorf("systemInstruction.role should be empty (Vertex ignores it), got %q", dr.SystemInstruction.Role)
	}
}

func TestBuildGenerateContentRequest_SingleUserText(t *testing.T) {
	body, toolNameByID, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "Hello"}}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(toolNameByID) != 0 {
		t.Errorf("toolNameByID should be empty, got %v", toolNameByID)
	}
	dr := decodeGeminiRequest(t, body)
	if len(dr.Contents) != 1 {
		t.Fatalf("expected 1 content, got %d: %+v", len(dr.Contents), dr.Contents)
	}
	if dr.Contents[0].Role != "user" || len(dr.Contents[0].Parts) != 1 || dr.Contents[0].Parts[0].Text != "Hello" {
		t.Errorf("user content mismatch: %+v", dr.Contents[0])
	}
}

func TestBuildGenerateContentRequest_MultiTurnWithToolUse(t *testing.T) {
	body, toolNameByID, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "Read main.go"}}},
			{Role: "assistant", Content: []types.ContentBlock{
				{Type: "text", Text: "Sure, reading it now."},
				{Type: "tool_use", ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
			}},
			{Role: "user", Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "call_1", Content: "package main"},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if toolNameByID["call_1"] != "read_file" {
		t.Errorf("toolNameByID[call_1] = %q, want read_file", toolNameByID["call_1"])
	}

	dr := decodeGeminiRequest(t, body)
	if len(dr.Contents) != 3 {
		t.Fatalf("expected 3 contents, got %d: %+v", len(dr.Contents), dr.Contents)
	}

	// 0: user text
	if dr.Contents[0].Role != "user" || dr.Contents[0].Parts[0].Text != "Read main.go" {
		t.Errorf("contents[0]: %+v", dr.Contents[0])
	}
	// 1: model with text + functionCall in one Content
	if dr.Contents[1].Role != "model" || len(dr.Contents[1].Parts) != 2 {
		t.Errorf("contents[1] should be model/2-parts, got %+v", dr.Contents[1])
	}
	if dr.Contents[1].Parts[0].Text != "Sure, reading it now." {
		t.Errorf("contents[1].parts[0].text = %q", dr.Contents[1].Parts[0].Text)
	}
	if dr.Contents[1].Parts[1].FunctionCall == nil ||
		dr.Contents[1].Parts[1].FunctionCall.Name != "read_file" {
		t.Errorf("contents[1].parts[1].functionCall: %+v", dr.Contents[1].Parts[1].FunctionCall)
	}
	// 2: function-role response
	if dr.Contents[2].Role != "function" || len(dr.Contents[2].Parts) != 1 {
		t.Errorf("contents[2] should be function/1-part, got %+v", dr.Contents[2])
	}
	resp := dr.Contents[2].Parts[0].FunctionResponse
	if resp == nil || resp.Name != "read_file" {
		t.Errorf("contents[2].parts[0].functionResponse: %+v", resp)
	}
	if resp.Response["content"] != "package main" {
		t.Errorf("response.content = %v", resp.Response["content"])
	}
	if _, hasError := resp.Response["error"]; hasError {
		t.Errorf("response.error should not be present on success: %+v", resp.Response)
	}
}

func TestBuildGenerateContentRequest_AssistantToolUseCoalescing(t *testing.T) {
	body, toolNameByID, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "list and read"}}},
			{Role: "assistant", Content: []types.ContentBlock{
				{Type: "tool_use", ID: "c1", Name: "list", Input: json.RawMessage(`{"path":"."}`)},
				{Type: "tool_use", ID: "c2", Name: "read_file", Input: json.RawMessage(`{"path":"x"}`)},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if toolNameByID["c1"] != "list" || toolNameByID["c2"] != "read_file" {
		t.Errorf("toolNameByID = %v", toolNameByID)
	}
	dr := decodeGeminiRequest(t, body)
	if len(dr.Contents) != 2 {
		t.Fatalf("expected 2 contents (one user, one model), got %d: %+v", len(dr.Contents), dr.Contents)
	}
	if dr.Contents[1].Role != "model" {
		t.Fatalf("contents[1].role = %q, want model", dr.Contents[1].Role)
	}
	if len(dr.Contents[1].Parts) != 2 {
		t.Fatalf("expected 2 parts in coalesced model content, got %d: %+v", len(dr.Contents[1].Parts), dr.Contents[1].Parts)
	}
	if dr.Contents[1].Parts[0].FunctionCall == nil || dr.Contents[1].Parts[0].FunctionCall.Name != "list" {
		t.Errorf("parts[0]: %+v", dr.Contents[1].Parts[0])
	}
	if dr.Contents[1].Parts[1].FunctionCall == nil || dr.Contents[1].Parts[1].FunctionCall.Name != "read_file" {
		t.Errorf("parts[1]: %+v", dr.Contents[1].Parts[1])
	}
}

func TestBuildGenerateContentRequest_ToolResultUnknownIDErrors(t *testing.T) {
	_, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "ghost", Content: "data"},
			}},
		},
	}, nil)
	if err == nil {
		t.Fatalf("expected error for unknown tool_use_id, got nil")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error should reference the unknown id, got %q", err.Error())
	}
}

func TestBuildGenerateContentRequest_ErrorToolResultIncludesErrorFlag(t *testing.T) {
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "assistant", Content: []types.ContentBlock{
				{Type: "tool_use", ID: "c1", Name: "read_file", Input: json.RawMessage(`{"path":"missing"}`)},
			}},
			{Role: "user", Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "c1", Content: "ENOENT", IsError: true},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dr := decodeGeminiRequest(t, body)
	if len(dr.Contents) < 2 {
		t.Fatalf("expected >=2 contents, got %d", len(dr.Contents))
	}
	resp := dr.Contents[len(dr.Contents)-1].Parts[0].FunctionResponse
	if resp == nil {
		t.Fatalf("expected functionResponse on last part")
	}
	if resp.Response["content"] != "ENOENT" {
		t.Errorf("response.content = %v, want ENOENT", resp.Response["content"])
	}
	if errFlag, ok := resp.Response["error"]; !ok || errFlag != true {
		t.Errorf("response.error = %v, want true", errFlag)
	}
}

func TestBuildGenerateContentRequest_DefaultSafetySettings(t *testing.T) {
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dr := decodeGeminiRequest(t, body)
	if len(dr.SafetySettings) != 5 {
		t.Fatalf("expected 5 default safety settings, got %d: %+v", len(dr.SafetySettings), dr.SafetySettings)
	}
	expectedCategories := map[string]bool{
		"HARM_CATEGORY_HATE_SPEECH":       false,
		"HARM_CATEGORY_HARASSMENT":        false,
		"HARM_CATEGORY_DANGEROUS_CONTENT": false,
		"HARM_CATEGORY_SEXUALLY_EXPLICIT": false,
		"HARM_CATEGORY_CIVIC_INTEGRITY":   false,
	}
	for _, s := range dr.SafetySettings {
		if _, ok := expectedCategories[s.Category]; !ok {
			t.Errorf("unexpected category %q", s.Category)
			continue
		}
		expectedCategories[s.Category] = true
		if s.Threshold != "BLOCK_NONE" {
			t.Errorf("category %q threshold = %q, want BLOCK_NONE", s.Category, s.Threshold)
		}
	}
	for c, seen := range expectedCategories {
		if !seen {
			t.Errorf("missing default safety setting for %q", c)
		}
	}
}

func TestBuildGenerateContentRequest_CustomSafetySettings(t *testing.T) {
	custom := []types.GeminiSafetySetting{
		{Category: "HARM_CATEGORY_DANGEROUS_CONTENT", Threshold: "BLOCK_ONLY_HIGH"},
		{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "BLOCK_MEDIUM_AND_ABOVE"},
	}
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}, custom)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dr := decodeGeminiRequest(t, body)
	if len(dr.SafetySettings) != 2 {
		t.Fatalf("expected 2 custom safety settings, got %d", len(dr.SafetySettings))
	}
	if dr.SafetySettings[0].Category != "HARM_CATEGORY_DANGEROUS_CONTENT" || dr.SafetySettings[0].Threshold != "BLOCK_ONLY_HIGH" {
		t.Errorf("safetySettings[0] mismatch: %+v", dr.SafetySettings[0])
	}
	if dr.SafetySettings[1].Category != "HARM_CATEGORY_HATE_SPEECH" || dr.SafetySettings[1].Threshold != "BLOCK_MEDIUM_AND_ABOVE" {
		t.Errorf("safetySettings[1] mismatch: %+v", dr.SafetySettings[1])
	}
}

// TestBuildGenerateContentRequest_StreamFunctionCallArgumentsFalseWhenToolsPresent
// pins the request shape: when tools are declared the adapter sets
// functionCallingConfig.mode="AUTO" and streamFunctionCallArguments=false.
// The flag stays off because Gemini 3.x's streamed-args wire format
// (JSON-path deltas with name only on the first chunk) would otherwise
// break the parser — see geminiToolConfig in gemini_types.go for the full
// rationale.
func TestBuildGenerateContentRequest_StreamFunctionCallArgumentsFalseWhenToolsPresent(t *testing.T) {
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
		Tools: []types.ToolDefinition{
			{
				Name:        "read_file",
				Description: "read a file",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dr := decodeGeminiRequest(t, body)
	if dr.ToolConfig == nil {
		t.Fatalf("expected toolConfig to be set")
	}
	if dr.ToolConfig.FunctionCallingConfig.Mode != "AUTO" {
		t.Errorf("mode = %q, want AUTO", dr.ToolConfig.FunctionCallingConfig.Mode)
	}
	if dr.ToolConfig.FunctionCallingConfig.StreamFunctionCallArguments {
		t.Errorf("streamFunctionCallArguments should be false")
	}
	if len(dr.Tools) != 1 || len(dr.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("tool declarations not as expected: %+v", dr.Tools)
	}
	decl := dr.Tools[0].FunctionDeclarations[0]
	if decl.Name != "read_file" {
		t.Errorf("decl.name = %q", decl.Name)
	}
	// Parameters must be Gemini-shaped (uppercase types).
	var params map[string]any
	if err := json.Unmarshal(decl.Parameters, &params); err != nil {
		t.Fatalf("decode parameters: %v", err)
	}
	if params["type"] != "OBJECT" {
		t.Errorf("parameters.type = %v, want OBJECT", params["type"])
	}
}

func TestBuildGenerateContentRequest_NoToolConfigWhenNoTools(t *testing.T) {
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dr := decodeGeminiRequest(t, body)
	if dr.ToolConfig != nil {
		t.Errorf("toolConfig should be nil when no tools, got %+v", dr.ToolConfig)
	}
	if len(dr.Tools) != 0 {
		t.Errorf("tools should be empty, got %+v", dr.Tools)
	}
}

func TestBuildGenerateContentRequest_BadToolSchemaErrors(t *testing.T) {
	_, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
		Tools: []types.ToolDefinition{
			{
				Name:        "bad_tool",
				Description: "uses $ref",
				InputSchema: json.RawMessage(`{"$ref":"#/$defs/Foo"}`),
			},
		},
	}, nil)
	if err == nil {
		t.Fatalf("expected error for tool schema with $ref")
	}
	if !strings.Contains(err.Error(), "bad_tool") {
		t.Errorf("error should mention the failing tool name, got %q", err.Error())
	}
}

func TestBuildGenerateContentRequest_MultipleSystemMessagesConcatenated(t *testing.T) {
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model:  "gemini-2.5-pro",
		System: "Base system prompt.",
		Messages: []types.Message{
			{Role: "system", Content: []types.ContentBlock{{Type: "text", Text: "Extra rule A."}}},
			{Role: "system", Content: []types.ContentBlock{{Type: "text", Text: "Extra rule B."}}},
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dr := decodeGeminiRequest(t, body)
	if dr.SystemInstruction == nil {
		t.Fatalf("expected systemInstruction")
	}
	got := dr.SystemInstruction.Parts[0].Text
	want := "Base system prompt.\n\nExtra rule A.\n\nExtra rule B."
	if got != want {
		t.Errorf("systemInstruction text mismatch:\n got: %q\nwant: %q", got, want)
	}
	// The system messages should not also appear in Contents.
	for _, c := range dr.Contents {
		if c.Role == "system" {
			t.Errorf("system message leaked into contents: %+v", c)
		}
	}
}

func TestBuildGenerateContentRequest_AssistantToolResultRejected(t *testing.T) {
	_, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "assistant", Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "x", Content: "y"},
			}},
		},
	}, nil)
	if err == nil {
		t.Fatalf("expected error for tool_result on assistant message")
	}
	if !strings.Contains(err.Error(), "tool_result") {
		t.Errorf("error should mention tool_result, got %q", err.Error())
	}
}

func TestBuildGenerateContentRequest_GenerationConfig(t *testing.T) {
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model:       "gemini-2.5-pro",
		MaxTokens:   2048,
		Temperature: types.Float64Ptr(0.2),
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dr := decodeGeminiRequest(t, body)
	if dr.GenerationConfig == nil {
		t.Fatalf("expected generationConfig")
	}
	if dr.GenerationConfig.MaxOutputTokens != 2048 {
		t.Errorf("maxOutputTokens = %d, want 2048", dr.GenerationConfig.MaxOutputTokens)
	}
	if dr.GenerationConfig.Temperature == nil || *dr.GenerationConfig.Temperature != 0.2 {
		t.Errorf("temperature = %v, want 0.2", dr.GenerationConfig.Temperature)
	}
}

// TestBuildGenerateContentRequest_TemperatureWireShape pins the unset-vs-
// explicit-zero semantics for StreamParams.Temperature on the Gemini
// adapter (issue #200). The adapter emits a generationConfig.temperature
// only when the upstream pointer is non-nil; an explicit Float64Ptr(0.0)
// transmits "temperature":0 (caller-requested greedy decoding).
func TestBuildGenerateContentRequest_TemperatureWireShape(t *testing.T) {
	messages := []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
	}

	cases := []struct {
		name              string
		temperature       *float64
		wantTemperature   bool
		wantTempSubstring string
	}{
		{name: "nil omitted", temperature: nil, wantTemperature: false},
		{name: "explicit zero serialised", temperature: types.Float64Ptr(0.0), wantTemperature: true, wantTempSubstring: `"temperature":0`},
		{name: "non-zero serialised", temperature: types.Float64Ptr(0.5), wantTemperature: true, wantTempSubstring: `"temperature":0.5`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _, err := BuildGenerateContentRequest(types.StreamParams{
				Model:       "gemini-2.5-pro",
				MaxTokens:   1024,
				Temperature: tc.temperature,
				Messages:    messages,
			}, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			bs := string(body)
			hasKey := strings.Contains(bs, `"temperature"`)
			if tc.wantTemperature && !hasKey {
				t.Errorf("missing 'temperature' for non-nil pointer: %s", bs)
			}
			if !tc.wantTemperature && hasKey {
				t.Errorf("contains 'temperature' for nil pointer (omitempty broken): %s", bs)
			}
			if tc.wantTempSubstring != "" && !strings.Contains(bs, tc.wantTempSubstring) {
				t.Errorf("missing %q in body: %s", tc.wantTempSubstring, bs)
			}
		})
	}
}

func TestBuildGenerateContentRequest_AssistantToolUseEmptyInputBecomesEmptyObject(t *testing.T) {
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "assistant", Content: []types.ContentBlock{
				{Type: "tool_use", ID: "c1", Name: "noop", Input: nil},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dr := decodeGeminiRequest(t, body)
	if len(dr.Contents) != 1 || len(dr.Contents[0].Parts) != 1 {
		t.Fatalf("unexpected contents: %+v", dr.Contents)
	}
	fc := dr.Contents[0].Parts[0].FunctionCall
	if fc == nil {
		t.Fatalf("expected functionCall")
	}
	// Args should serialise as "{}" not be omitted entirely (Vertex
	// requires a present args object on functionCall).
	var argsObj map[string]any
	if err := json.Unmarshal(fc.Args, &argsObj); err != nil {
		t.Fatalf("decode args: %v (raw=%s)", err, fc.Args)
	}
	if len(argsObj) != 0 {
		t.Errorf("args = %v, want empty object", argsObj)
	}
}

// TestBuildGenerateContentRequest_BodyIsValidJSON guards against accidental
// non-JSON output (e.g. from an Encoder that wraps in newlines, or a
// programmer error introducing a Stringer that breaks marshalling).
func TestBuildGenerateContentRequest_BodyIsValidJSON(t *testing.T) {
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody=%s", err, body)
	}
}

// TestBuildGenerateContentRequest_UserTextAndToolResultOrdering pins the
// per-message ordering: function-response Contents are emitted before the
// trailing user-text Content within the same message. Reordering would
// silently change wire output for callers that combine text and
// tool_result blocks in one user message.
func TestBuildGenerateContentRequest_UserTextAndToolResultOrdering(t *testing.T) {
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "assistant", Content: []types.ContentBlock{
				{Type: "tool_use", ID: "c1", Name: "read_file", Input: json.RawMessage(`{"path":"x"}`)},
			}},
			{Role: "user", Content: []types.ContentBlock{
				{Type: "text", Text: "follow-up note"},
				{Type: "tool_result", ToolUseID: "c1", Content: "ok"},
			}},
		},
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dr := decodeGeminiRequest(t, body)
	// contents: [model(call), function(response), user(text)]
	if len(dr.Contents) != 3 {
		t.Fatalf("expected 3 contents, got %d: %+v", len(dr.Contents), dr.Contents)
	}
	if dr.Contents[1].Role != "function" {
		t.Errorf("contents[1].role = %q, want function", dr.Contents[1].Role)
	}
	if dr.Contents[2].Role != "user" || dr.Contents[2].Parts[0].Text != "follow-up note" {
		t.Errorf("contents[2] mismatch: %+v", dr.Contents[2])
	}
}
