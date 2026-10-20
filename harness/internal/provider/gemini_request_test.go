package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
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
	ThoughtSignature string               `json:"thoughtSignature,omitempty"`
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
		Mode                        string   `json:"mode"`
		AllowedFunctionNames        []string `json:"allowedFunctionNames"`
		StreamFunctionCallArguments bool     `json:"streamFunctionCallArguments"`
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
	must := require.New(t)
	var dr decodedRequest
	must.NoError(json.Unmarshal(body, &dr), "body=%s", body)
	return dr
}

func TestBuildGenerateContentRequest_SystemInstructionFromParams(t *testing.T) {
	must := require.New(t)
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model:  "gemini-2.5-pro",
		System: "You are a helpful coding assistant.",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}, nil, quirks.ProviderQuirks{})
	must.NoError(err)
	dr := decodeGeminiRequest(t, body)
	must.NotNil(dr.SystemInstruction, "expected systemInstruction")
	must.Len(dr.SystemInstruction.Parts, 1)
	must.Equal("You are a helpful coding assistant.", dr.SystemInstruction.Parts[0].Text)
	must.Empty(dr.SystemInstruction.Role, "Vertex ignores systemInstruction.role")
}

func TestBuildGenerateContentRequest_SingleUserText(t *testing.T) {
	must := require.New(t)
	body, toolNameByID, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "Hello"}}},
		},
	}, nil, quirks.ProviderQuirks{})
	must.NoError(err)
	must.Empty(toolNameByID)
	dr := decodeGeminiRequest(t, body)
	must.Len(dr.Contents, 1)
	must.Equal("user", dr.Contents[0].Role)
	must.Len(dr.Contents[0].Parts, 1)
	must.Equal("Hello", dr.Contents[0].Parts[0].Text)
}

func TestBuildGenerateContentRequest_MultiTurnWithToolUse(t *testing.T) {
	must := require.New(t)
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
	}, nil, quirks.ProviderQuirks{})
	must.NoError(err)
	must.Equal("read_file", toolNameByID["call_1"])

	dr := decodeGeminiRequest(t, body)
	must.Len(dr.Contents, 3)

	// User text.
	must.Equal("user", dr.Contents[0].Role)
	must.Len(dr.Contents[0].Parts, 1)
	must.Equal("Read main.go", dr.Contents[0].Parts[0].Text)

	// Model turn: text and functionCall share one Content.
	must.Equal("model", dr.Contents[1].Role)
	must.Len(dr.Contents[1].Parts, 2)
	must.Equal("Sure, reading it now.", dr.Contents[1].Parts[0].Text)
	must.NotNil(dr.Contents[1].Parts[1].FunctionCall)
	must.Equal("read_file", dr.Contents[1].Parts[1].FunctionCall.Name)

	// Function-role response.
	must.Equal("function", dr.Contents[2].Role)
	must.Len(dr.Contents[2].Parts, 1)
	resp := dr.Contents[2].Parts[0].FunctionResponse
	must.NotNil(resp)
	must.Equal("read_file", resp.Name)
	must.Equal("package main", resp.Response["content"])
	must.NotContains(resp.Response, "error", "response.error should not be present on success")
}

func TestBuildGenerateContentRequest_AssistantToolUseCoalescing(t *testing.T) {
	must := require.New(t)
	body, toolNameByID, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "list and read"}}},
			{Role: "assistant", Content: []types.ContentBlock{
				{Type: "tool_use", ID: "c1", Name: "list", Input: json.RawMessage(`{"path":"."}`)},
				{Type: "tool_use", ID: "c2", Name: "read_file", Input: json.RawMessage(`{"path":"x"}`)},
			}},
		},
	}, nil, quirks.ProviderQuirks{})
	must.NoError(err)
	must.Equal("list", toolNameByID["c1"])
	must.Equal("read_file", toolNameByID["c2"])
	dr := decodeGeminiRequest(t, body)
	must.Len(dr.Contents, 2, "expected one user and one model content")
	must.Equal("model", dr.Contents[1].Role)
	must.Len(dr.Contents[1].Parts, 2, "expected both tool calls coalesced into one model content")
	must.NotNil(dr.Contents[1].Parts[0].FunctionCall)
	must.Equal("list", dr.Contents[1].Parts[0].FunctionCall.Name)
	must.NotNil(dr.Contents[1].Parts[1].FunctionCall)
	must.Equal("read_file", dr.Contents[1].Parts[1].FunctionCall.Name)
}

func TestBuildGenerateContentRequest_ToolResultUnknownIDErrors(t *testing.T) {
	must := require.New(t)
	_, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "ghost", Content: "data"},
			}},
		},
	}, nil, quirks.ProviderQuirks{})
	must.ErrorContains(err, "ghost")
}

func TestBuildGenerateContentRequest_ErrorToolResultIncludesErrorFlag(t *testing.T) {
	must := require.New(t)
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
	}, nil, quirks.ProviderQuirks{})
	must.NoError(err)
	dr := decodeGeminiRequest(t, body)
	must.GreaterOrEqual(len(dr.Contents), 2)
	resp := dr.Contents[len(dr.Contents)-1].Parts[0].FunctionResponse
	must.NotNil(resp, "expected functionResponse on last part")
	must.Equal("ENOENT", resp.Response["content"])
	must.Contains(resp.Response, "error")
	must.Equal(true, resp.Response["error"])
}

func TestBuildGenerateContentRequest_DefaultSafetySettings(t *testing.T) {
	must := require.New(t)
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}, nil, quirks.ProviderQuirks{})
	must.NoError(err)
	dr := decodeGeminiRequest(t, body)
	must.Len(dr.SafetySettings, 5)

	gotCategories := make([]string, 0, len(dr.SafetySettings))
	for _, s := range dr.SafetySettings {
		gotCategories = append(gotCategories, s.Category)
		must.Equal("BLOCK_NONE", s.Threshold, "category %s", s.Category)
	}
	must.ElementsMatch([]string{
		"HARM_CATEGORY_HATE_SPEECH",
		"HARM_CATEGORY_HARASSMENT",
		"HARM_CATEGORY_DANGEROUS_CONTENT",
		"HARM_CATEGORY_SEXUALLY_EXPLICIT",
		"HARM_CATEGORY_CIVIC_INTEGRITY",
	}, gotCategories)
}

func TestBuildGenerateContentRequest_CustomSafetySettings(t *testing.T) {
	must := require.New(t)
	custom := []types.GeminiSafetySetting{
		{Category: "HARM_CATEGORY_DANGEROUS_CONTENT", Threshold: "BLOCK_ONLY_HIGH"},
		{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "BLOCK_MEDIUM_AND_ABOVE"},
	}
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}, custom, quirks.ProviderQuirks{})
	must.NoError(err)
	dr := decodeGeminiRequest(t, body)
	must.Equal([]decodedSafetyEntry{
		{Category: "HARM_CATEGORY_DANGEROUS_CONTENT", Threshold: "BLOCK_ONLY_HIGH"},
		{Category: "HARM_CATEGORY_HATE_SPEECH", Threshold: "BLOCK_MEDIUM_AND_ABOVE"},
	}, dr.SafetySettings)
}

// TestBuildGenerateContentRequest_StreamFunctionCallArgsFalseWithTools
// pins the request shape: when tools are declared the adapter sets
// functionCallingConfig.mode="AUTO" and streamFunctionCallArguments=false.
// See geminiToolConfig in gemini_types.go for why the flag stays off.
func TestBuildGenerateContentRequest_StreamFunctionCallArgsFalseWithTools(t *testing.T) {
	must := require.New(t)
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
	}, nil, quirks.ProviderQuirks{})
	must.NoError(err)
	dr := decodeGeminiRequest(t, body)
	must.NotNil(dr.ToolConfig, "expected toolConfig to be set")
	must.Equal("AUTO", dr.ToolConfig.FunctionCallingConfig.Mode)
	must.False(dr.ToolConfig.FunctionCallingConfig.StreamFunctionCallArguments)
	must.Len(dr.Tools, 1)
	must.Len(dr.Tools[0].FunctionDeclarations, 1)
	decl := dr.Tools[0].FunctionDeclarations[0]
	must.Equal("read_file", decl.Name)

	// Parameters must be Gemini-shaped (uppercase types).
	var params map[string]any
	must.NoError(json.Unmarshal(decl.Parameters, &params))
	must.Equal("OBJECT", params["type"])
}

// TestBuildGenerateContentRequest_ToolChoice pins the
// functionCallingConfig.mode projection gated on the resolved
// capability. Gemini's base rule advertises every mode, so:
//   - ToolChoiceAuto (zero) keeps mode AUTO with no allowedFunctionNames;
//   - ToolChoiceRequired emits mode ANY;
//   - ToolChoiceNone emits mode NONE;
//   - ToolChoiceTool emits mode ANY restricted to the named tool;
//   - ToolChoiceTool with no name degrades to AUTO.
func TestBuildGenerateContentRequest_ToolChoice(t *testing.T) {
	q := quirks.DefaultRegistry().Resolve("gemini", "gemini-2.5-pro")
	baseTools := []types.ToolDefinition{
		{Name: "read_file", Description: "read a file", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
	}

	cases := []struct {
		name        string
		choice      types.ToolChoiceMode
		toolName    string
		wantMode    string
		wantAllowed []string
	}{
		{"auto", types.ToolChoiceAuto, "", "AUTO", nil},
		{"required", types.ToolChoiceRequired, "", "ANY", nil},
		{"none", types.ToolChoiceNone, "", "NONE", nil},
		{"named tool", types.ToolChoiceTool, "read_file", "ANY", []string{"read_file"}},
		{"named tool without name degrades to auto", types.ToolChoiceTool, "", "AUTO", nil},
		{"named tool with invalid name degrades to auto", types.ToolChoiceTool, "bad name!", "AUTO", nil},
		{"named tool with over-length name degrades to auto", types.ToolChoiceTool, strings.Repeat("a", 65), "AUTO", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			body, _, err := BuildGenerateContentRequest(types.StreamParams{
				Model:          "gemini-2.5-pro",
				ToolChoice:     tc.choice,
				ToolChoiceName: tc.toolName,
				Messages:       []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
				Tools:          baseTools,
			}, nil, q)
			must.NoError(err)
			dr := decodeGeminiRequest(t, body)
			must.NotNil(dr.ToolConfig, "expected toolConfig to be set")
			must.Equal(tc.wantMode, dr.ToolConfig.FunctionCallingConfig.Mode)
			must.Equal(tc.wantAllowed, dr.ToolConfig.FunctionCallingConfig.AllowedFunctionNames)
		})
	}
}

// TestBuildGenerateContentRequest_ToolChoiceUnsupportedCapability pins
// the graceful no-op: a zero-value capability leaves the mode at AUTO
// even for a ToolChoiceRequired request.
func TestBuildGenerateContentRequest_ToolChoiceUnsupportedCapability(t *testing.T) {
	must := require.New(t)
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model:      "gemini-2.5-pro",
		ToolChoice: types.ToolChoiceRequired,
		Messages:   []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools: []types.ToolDefinition{
			{Name: "read_file", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}, nil, quirks.ProviderQuirks{})
	must.NoError(err)
	dr := decodeGeminiRequest(t, body)
	must.NotNil(dr.ToolConfig, "expected toolConfig to be set")
	must.Equal("AUTO", dr.ToolConfig.FunctionCallingConfig.Mode)
}

// TestBuildGenerateContentRequest_ToolChoice_PartialCapability asserts
// Supported=true with a specific mode disabled falls back to mode AUTO
// rather than emitting the disallowed mode.
func TestBuildGenerateContentRequest_ToolChoice_PartialCapability(t *testing.T) {
	tools := []types.ToolDefinition{
		{Name: "read_file", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	cases := []struct {
		name   string
		choice types.ToolChoiceMode
		cap    quirks.ToolChoiceCapability
	}{
		{"required disabled", types.ToolChoiceRequired, quirks.ToolChoiceCapability{Supported: true, Required: false}},
		{"none disabled", types.ToolChoiceNone, quirks.ToolChoiceCapability{Supported: true, None: false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			body, _, err := BuildGenerateContentRequest(types.StreamParams{
				Model:      "gemini-2.5-pro",
				ToolChoice: tc.choice,
				Messages:   []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
				Tools:      tools,
			}, nil, quirks.ProviderQuirks{ToolChoice: tc.cap})
			must.NoError(err)
			dr := decodeGeminiRequest(t, body)
			must.NotNil(dr.ToolConfig, "expected toolConfig to be set")
			must.Equal("AUTO", dr.ToolConfig.FunctionCallingConfig.Mode)
		})
	}
}

// TestStreamFunctionCallArgsFromQuirks_ShapeMapsToWireFlag pins the projection
// from the resolved GeminiStreamArgsShape onto the wire boolean: the
// default (StreamArgsOff) keeps the flag false; V2/V3 flip it to true.
// The `default` arm of the switch is a forward-compatibility safety net
// and is not exercised here (unreachable without an out-of-range enum).
func TestStreamFunctionCallArgsFromQuirks_ShapeMapsToWireFlag(t *testing.T) {
	cases := []struct {
		name  string
		shape quirks.GeminiStreamArgsShape
		want  bool
	}{
		{name: "off baseline", shape: quirks.StreamArgsOff, want: false},
		{name: "v2 snapshot opts in", shape: quirks.StreamArgsV2Snapshot, want: true},
		{name: "v3 deltas opts in", shape: quirks.StreamArgsV3Deltas, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			q := quirks.ProviderQuirks{
				BehaviourFlags: quirks.ProviderBehaviourFlags{
					Gemini: quirks.GeminiBehaviourFlags{
						StreamFunctionCallArgsShape: tc.shape,
					},
				},
			}
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
			}, nil, q)
			must.NoError(err)
			dr := decodeGeminiRequest(t, body)
			must.NotNil(dr.ToolConfig, "expected toolConfig to be present when tools are declared")
			must.Equal(tc.want, dr.ToolConfig.FunctionCallingConfig.StreamFunctionCallArguments)
		})
	}
}

func TestBuildGenerateContentRequest_NoToolConfigWhenNoTools(t *testing.T) {
	must := require.New(t)
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}, nil, quirks.ProviderQuirks{})
	must.NoError(err)
	dr := decodeGeminiRequest(t, body)
	must.Nil(dr.ToolConfig, "toolConfig should be omitted when no tools are declared")
	must.Empty(dr.Tools)
}

func TestBuildGenerateContentRequest_BadToolSchemaErrors(t *testing.T) {
	must := require.New(t)
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
	}, nil, quirks.ProviderQuirks{})
	must.ErrorContains(err, "bad_tool")
}

func TestBuildGenerateContentRequest_MultipleSystemMessagesConcatenated(t *testing.T) {
	must := require.New(t)
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model:  "gemini-2.5-pro",
		System: "Base system prompt.",
		Messages: []types.Message{
			{Role: "system", Content: []types.ContentBlock{{Type: "text", Text: "Extra rule A."}}},
			{Role: "system", Content: []types.ContentBlock{{Type: "text", Text: "Extra rule B."}}},
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}, nil, quirks.ProviderQuirks{})
	must.NoError(err)
	dr := decodeGeminiRequest(t, body)
	must.NotNil(dr.SystemInstruction, "expected systemInstruction")
	must.NotEmpty(dr.SystemInstruction.Parts)
	must.Equal("Base system prompt.\n\nExtra rule A.\n\nExtra rule B.", dr.SystemInstruction.Parts[0].Text)

	// System messages are folded into systemInstruction, not sent as contents.
	for _, c := range dr.Contents {
		must.NotEqual("system", c.Role)
	}
}

func TestBuildGenerateContentRequest_AssistantToolResultRejected(t *testing.T) {
	must := require.New(t)
	_, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "assistant", Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "x", Content: "y"},
			}},
		},
	}, nil, quirks.ProviderQuirks{})
	must.ErrorContains(err, "tool_result")
}

func TestBuildGenerateContentRequest_GenerationConfig(t *testing.T) {
	must := require.New(t)
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model:       "gemini-2.5-pro",
		MaxTokens:   2048,
		Temperature: types.Float64Ptr(0.2),
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}, nil, quirks.ProviderQuirks{})
	must.NoError(err)
	dr := decodeGeminiRequest(t, body)
	must.NotNil(dr.GenerationConfig, "expected generationConfig")
	must.Equal(2048, dr.GenerationConfig.MaxOutputTokens)
	must.NotNil(dr.GenerationConfig.Temperature)
	must.InDelta(0.2, *dr.GenerationConfig.Temperature, 1e-9)
}

// findModelContent returns the first model-role content in the decoded request, or nil.
func findModelContent(dr decodedRequest) *decodedContent {
	for i := range dr.Contents {
		if dr.Contents[i].Role == "model" {
			return &dr.Contents[i]
		}
	}
	return nil
}

// TestBuildGenerateContentRequest_TemperatureWireShape pins the unset-vs-
// explicit-zero semantics for StreamParams.Temperature: the adapter emits
// generationConfig.temperature only when the pointer is non-nil, so an
// explicit Float64Ptr(0.0) transmits "temperature":0 (greedy decoding).
func TestBuildGenerateContentRequest_TemperatureWireShape(t *testing.T) {
	messages := []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
	}

	cases := []struct {
		name              string
		maxTokens         int
		temperature       *float64
		wantTemperature   bool
		wantTempSubstring string
		wantMaxOutTokens  bool
	}{
		{name: "nil omitted", maxTokens: 1024, temperature: nil, wantTemperature: false, wantMaxOutTokens: true},
		{name: "explicit zero serialised", maxTokens: 1024, temperature: types.Float64Ptr(0.0), wantTemperature: true, wantTempSubstring: `"temperature":0`, wantMaxOutTokens: true},
		{name: "non-zero serialised", maxTokens: 1024, temperature: types.Float64Ptr(0.5), wantTemperature: true, wantTempSubstring: `"temperature":0.5`, wantMaxOutTokens: true},
		// maxOutputTokens must be omitted entirely: emitting "maxOutputTokens":0
		// produces a validation error or a hard zero-output cap on Vertex AI.
		{name: "zero maxtokens omits maxOutputTokens", maxTokens: 0, temperature: types.Float64Ptr(0.0), wantTemperature: true, wantTempSubstring: `"temperature":0`, wantMaxOutTokens: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			body, _, err := BuildGenerateContentRequest(types.StreamParams{
				Model:       "gemini-2.5-pro",
				MaxTokens:   tc.maxTokens,
				Temperature: tc.temperature,
				Messages:    messages,
			}, nil, quirks.ProviderQuirks{})
			must.NoError(err)
			bs := string(body)
			if tc.wantTemperature {
				must.Contains(bs, `"temperature"`, "non-nil pointer must emit temperature")
			} else {
				must.NotContains(bs, `"temperature"`, "nil pointer must omit temperature (omitempty)")
			}
			if tc.wantTempSubstring != "" {
				must.Contains(bs, tc.wantTempSubstring)
			}
			if tc.wantMaxOutTokens {
				must.Contains(bs, `"maxOutputTokens"`, "non-zero MaxTokens must emit maxOutputTokens")
			} else {
				must.NotContains(bs, `"maxOutputTokens"`, "zero MaxTokens must omit maxOutputTokens")
			}
		})
	}
}

func TestBuildGenerateContentRequest_AssistantToolUseEmptyInputBecomesEmptyObject(t *testing.T) {
	must := require.New(t)
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "assistant", Content: []types.ContentBlock{
				{Type: "tool_use", ID: "c1", Name: "noop", Input: nil},
			}},
		},
	}, nil, quirks.ProviderQuirks{})
	must.NoError(err)
	dr := decodeGeminiRequest(t, body)
	must.Len(dr.Contents, 1)
	must.Len(dr.Contents[0].Parts, 1)
	fc := dr.Contents[0].Parts[0].FunctionCall
	must.NotNil(fc, "expected functionCall on the part")
	// Args should serialise as "{}" not be omitted entirely (Vertex
	// requires a present args object on functionCall).
	var argsObj map[string]any
	must.NoError(json.Unmarshal(fc.Args, &argsObj), "raw args=%s", fc.Args)
	must.Empty(argsObj, "args should serialise as an empty object")
}

// TestBuildGenerateContentRequest_BodyIsValidJSON guards against the request
// body ceasing to be a JSON object, for example from a programmer error that
// breaks marshalling.
func TestBuildGenerateContentRequest_BodyIsValidJSON(t *testing.T) {
	must := require.New(t)
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}, nil, quirks.ProviderQuirks{})
	must.NoError(err)
	var generic map[string]any
	must.NoError(json.Unmarshal(body, &generic), "body=%s", body)
}

// TestBuildGenerateContentRequest_UserTextAndToolResultOrdering pins the
// per-message ordering: function-response Contents are emitted before the
// trailing user-text Content within the same message. Reordering would
// silently change wire output for callers that combine text and
// tool_result blocks in one user message.
func TestBuildGenerateContentRequest_UserTextAndToolResultOrdering(t *testing.T) {
	must := require.New(t)
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
	}, nil, quirks.ProviderQuirks{})
	must.NoError(err)
	dr := decodeGeminiRequest(t, body)
	// contents: [model(call), function(response), user(text)]
	must.Len(dr.Contents, 3)
	must.Equal("function", dr.Contents[1].Role)
	must.Equal("user", dr.Contents[2].Role)
	must.NotEmpty(dr.Contents[2].Parts)
	must.Equal("follow-up note", dr.Contents[2].Parts[0].Text)
}

// TestBuildGenerateContentRequest_ThoughtSignatureRoundTrip asserts that
// when an assistant `tool_use` block carries a ThoughtSignature, the
// marshalled request emits `thoughtSignature` on the corresponding part.
func TestBuildGenerateContentRequest_ThoughtSignatureRoundTrip(t *testing.T) {
	must := require.New(t)
	const sig = "AY89a18t+D98lADcFYKgjMgoHS7rOPAQUE=="
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-3.1-pro-preview",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "Read main.go"}}},
			{Role: "assistant", Content: []types.ContentBlock{
				{
					Type:             "tool_use",
					ID:               "call_1",
					Name:             "read_file",
					Input:            json.RawMessage(`{"path":"main.go"}`),
					ThoughtSignature: sig,
				},
			}},
			{Role: "user", Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "call_1", Content: "package main"},
			}},
		},
	}, nil, quirks.ProviderQuirks{})
	must.NoError(err)
	dr := decodeGeminiRequest(t, body)

	model := findModelContent(dr)
	must.NotNil(model, "expected a model-role content in the request body")
	must.Len(model.Parts, 1)
	must.NotNil(model.Parts[0].FunctionCall, "expected functionCall on the model part")
	must.Equal(sig, model.Parts[0].ThoughtSignature)
}

// TestBuildGenerateContentRequest_ThoughtSignatureRoundTripOnText asserts
// assistant `text` blocks that carry a ThoughtSignature also re-emit it
// on the part. Receive-side capture on text parts is not yet implemented
// (see TODO in gemini.go), but the send-side path supports the field.
func TestBuildGenerateContentRequest_ThoughtSignatureRoundTripOnText(t *testing.T) {
	must := require.New(t)
	const sig = "TEXTPARTSIGNATURE=="
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-3.1-pro-preview",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: []types.ContentBlock{
				{Type: "text", Text: "hello!", ThoughtSignature: sig},
			}},
		},
	}, nil, quirks.ProviderQuirks{})
	must.NoError(err)
	dr := decodeGeminiRequest(t, body)
	model := findModelContent(dr)
	must.NotNil(model, "expected a model-role content in the request body")
	must.Len(model.Parts, 1)
	must.Equal("hello!", model.Parts[0].Text)
	must.Equal(sig, model.Parts[0].ThoughtSignature)
}

// TestBuildGenerateContentRequest_NoThoughtSignatureWhenAbsent confirms
// assistant blocks without a ThoughtSignature do NOT emit the field on
// the wire (`omitempty` keeps parity with the pre-3.x request shape).
func TestBuildGenerateContentRequest_NoThoughtSignatureWhenAbsent(t *testing.T) {
	must := require.New(t)
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model: "gemini-2.5-pro",
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: []types.ContentBlock{
				{Type: "tool_use", ID: "c1", Name: "read_file", Input: json.RawMessage(`{"path":"x"}`)},
			}},
		},
	}, nil, quirks.ProviderQuirks{})
	must.NoError(err)
	must.NotContains(string(body), "thoughtSignature", "no signature was set, so the field must be omitted")
}

// TestGeminiThoughtSignature_FullRoundTrip confirms JSON decode of the
// Vertex wire type and serialisation by BuildGenerateContentRequest agree:
// parse a Gemini 3.x response chunk containing a functionCall with a
// thoughtSignature, hand-build the ContentBlock the harness would persist,
// and assert the next request body emits the same signature back to
// Vertex. Does not drive consumeSSE or streamEventsToResult directly;
// those paths are covered by TestGeminiAdapter_ThoughtSignatureCapturedOnToolCall
// and TestStreamEventsToResult_ThoughtSignaturePropagatedToBlock.
func TestGeminiThoughtSignature_FullRoundTrip(t *testing.T) {
	must := require.New(t)
	const sig = "AY89a18t+D98lADcFYKgjMgoHS7rOPAQUE=="

	chunkJSON := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{"path":"docs/safety-rings.md"}},"thoughtSignature":"` + sig + `"}]},"finishReason":"STOP"}]}`
	var chunk generateContentChunk
	must.NoError(json.Unmarshal([]byte(chunkJSON), &chunk), "decode chunk")
	must.NotEmpty(chunk.Candidates, "decoded chunk has no candidates")
	must.NotNil(chunk.Candidates[0].Content, "decoded chunk has no content")
	must.NotEmpty(chunk.Candidates[0].Content.Parts, "decoded chunk has no parts")
	gotSig := chunk.Candidates[0].Content.Parts[0].ThoughtSignature
	must.Equal(sig, gotSig, "decoded thoughtSignature")

	messages := []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "Read it"}}},
		{Role: "assistant", Content: []types.ContentBlock{
			{
				Type:             "tool_use",
				ID:               "call_1",
				Name:             "read_file",
				Input:            json.RawMessage(`{"path":"docs/safety-rings.md"}`),
				ThoughtSignature: gotSig,
			},
		}},
		{Role: "user", Content: []types.ContentBlock{
			{Type: "tool_result", ToolUseID: "call_1", Content: "..."},
		}},
	}

	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model:    "gemini-3.1-pro-preview",
		Messages: messages,
	}, nil, quirks.ProviderQuirks{})
	must.NoError(err, "BuildGenerateContentRequest")
	must.Contains(string(body), `"thoughtSignature":"`+sig+`"`, "round-tripped request body")
}

// TestGeminiToolResultResponse_MarshalErrorOnInvalidStructured asserts a
// malformed Structured payload surfaces as an error rather than emitting
// a broken functionResponse body.
func TestGeminiToolResultResponse_MarshalErrorOnInvalidStructured(t *testing.T) {
	must := require.New(t)
	block := types.ContentBlock{
		Type:       "tool_result",
		ToolUseID:  "c1",
		Content:    "ok",
		Structured: json.RawMessage(`{not json`),
		Kind:       "file_excerpt",
	}
	capability := quirks.StructuredToolResultCapability{Supported: true, ObjectResponse: true}

	_, err := geminiToolResultResponse(block, capability)
	must.Error(err, "invalid Structured payload must fail to marshal")
}

// TestGeminiToolResultResponse_TextOnlyWhenCapabilityWithholdsStructured
// asserts an invalid Structured payload does not reach json.Marshal when
// the capability withholds the object-response shape: the capability
// gate, not the payload, decides whether Structured is embedded.
func TestGeminiToolResultResponse_TextOnlyWhenCapabilityWithholdsStructured(t *testing.T) {
	must := require.New(t)
	block := types.ContentBlock{
		Type:       "tool_result",
		ToolUseID:  "c1",
		Content:    "ok",
		Structured: json.RawMessage(`{not json`),
		Kind:       "file_excerpt",
	}
	raw, err := geminiToolResultResponse(block, quirks.StructuredToolResultCapability{})
	must.NoError(err)
	var body map[string]interface{}
	must.NoError(json.Unmarshal(raw, &body), "text-only body must be valid JSON: raw=%s", raw)
	must.Equal("ok", body["content"])
	must.NotContains(body, "structured", "capability withholds structured, so it must be absent")
}
