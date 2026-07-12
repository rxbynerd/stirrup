package provider

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// structuredToolResultMessages returns a multi-turn history whose final user
// message carries a tool_result block with a structured envelope. The same
// history is reused across the adapter tests so a single fixture pins both
// the structured-on and the text-fallback shapes.
func structuredToolResultMessages() []types.Message {
	return []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "run it"}}},
		{Role: "assistant", Content: []types.ContentBlock{
			{Type: "tool_use", ID: "call_1", Name: "run_command", Input: json.RawMessage(`{"command":"echo hi"}`)},
		}},
		{Role: "user", Content: []types.ContentBlock{
			{
				Type:       "tool_result",
				ToolUseID:  "call_1",
				Content:    "hi\n(exit 0)",
				Structured: json.RawMessage(`{"stdout":"hi\n","stderr":"","exit_code":0}`),
				Kind:       "command_result",
			},
		}},
	}
}

// textOnlyToolResultMessages is structuredToolResultMessages with the
// envelope stripped — the pre-#231 shape. A capability-off serialisation of
// the structured history must equal the serialisation of this history, which
// is how the tests prove "text-by-default is byte-identical to today".
func textOnlyToolResultMessages() []types.Message {
	msgs := structuredToolResultMessages()
	last := msgs[len(msgs)-1].Content
	last[0].Structured = nil
	last[0].Kind = ""
	return msgs
}

func anthropicTools() []types.ToolDefinition {
	return []types.ToolDefinition{{
		Name:        "run_command",
		Description: "Run a shell command",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`),
	}}
}

// TestAnthropicStructuredToolResult_CapabilityOn pins that the Anthropic
// builder emits the content-block array form (canonical text + structured
// JSON text part) for a structured tool result when the capability accepts
// it. The default registry resolves the capability on for every Anthropic
// model.
func TestAnthropicStructuredToolResult_CapabilityOn(t *testing.T) {
	q := quirks.DefaultRegistry().Resolve("anthropic", "claude-sonnet-4-5")
	if !q.StructuredToolResults.Supported {
		t.Fatalf("precondition: anthropic structured capability resolved off")
	}
	body := buildAnthropicRequest(types.StreamParams{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 1024,
		Tools:     anthropicTools(),
		Messages:  structuredToolResultMessages(),
	}, true, q)

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded struct {
		Messages []struct {
			Content []struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// The third message is the user tool_result turn.
	trBlock := decoded.Messages[2].Content[0]
	if trBlock.Type != "tool_result" {
		t.Fatalf("expected tool_result block, got %q", trBlock.Type)
	}
	var parts []anthropicToolResultPart
	if err := json.Unmarshal(trBlock.Content, &parts); err != nil {
		t.Fatalf("tool_result content is not an array (capability-on must emit array form): %v\ncontent: %s", err, trBlock.Content)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 content parts (text + structured), got %d: %+v", len(parts), parts)
	}
	if parts[0].Type != "text" || parts[0].Text != "hi\n(exit 0)" {
		t.Errorf("part[0] = %+v, want canonical text fallback", parts[0])
	}
	if parts[1].Type != "text" {
		t.Errorf("part[1].type = %q, want text", parts[1].Type)
	}
	if !json.Valid([]byte(parts[1].Text)) {
		t.Errorf("part[1].text is not valid JSON (structured payload): %q", parts[1].Text)
	}
	var sp map[string]any
	if err := json.Unmarshal([]byte(parts[1].Text), &sp); err != nil {
		t.Fatalf("structured part not decodable: %v", err)
	}
	if sp["exit_code"] != float64(0) {
		t.Errorf("structured exit_code = %v, want 0", sp["exit_code"])
	}
}

// TestAnthropicStructuredToolResult_TextByDefault pins the no-regression
// guarantee: with the capability unset (zero ProviderQuirks) the structured
// history serialises byte-identically to the same history with the envelope
// stripped. The structured envelope must not leak onto the wire when the
// provider has not opted in.
func TestAnthropicStructuredToolResult_TextByDefault(t *testing.T) {
	var zero quirks.ProviderQuirks

	withStructured := mustMarshal(t, buildAnthropicRequest(types.StreamParams{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 1024,
		Tools:     anthropicTools(),
		Messages:  structuredToolResultMessages(),
	}, true, zero))

	textOnly := mustMarshal(t, buildAnthropicRequest(types.StreamParams{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 1024,
		Tools:     anthropicTools(),
		Messages:  textOnlyToolResultMessages(),
	}, true, zero))

	if !bytes.Equal(withStructured, textOnly) {
		t.Errorf("capability-off body must be byte-identical to text-only history\n with-structured: %s\n text-only:       %s", withStructured, textOnly)
	}
	if bytes.Contains(withStructured, []byte("exit_code")) {
		t.Errorf("structured payload leaked onto the wire with capability off: %s", withStructured)
	}
}

// TestAnthropicToolResultContent_EmptyContentCapabilityOn (REC-1) pins that an
// empty canonical text with a non-empty structured payload does NOT emit the
// array form with a meaningless empty-string first part. It falls through to a
// nil return — the pre-#231 omitempty shape for an empty-content tool result —
// regardless of the structured envelope.
func TestAnthropicToolResultContent_EmptyContentCapabilityOn(t *testing.T) {
	cap := quirks.StructuredToolResultCapability{Supported: true, ContentBlockArray: true}
	block := types.ContentBlock{
		Type:       "tool_result",
		ToolUseID:  "call_1",
		Content:    "",
		Structured: json.RawMessage(`{"exit_code":0}`),
		Kind:       "command_result",
	}
	got := anthropicToolResultContent(block, cap)
	if got != nil {
		t.Errorf("empty content with structured must return nil (no empty-string array part), got %s", got)
	}
}

// TestAnthropicToolResultContent_IdenticalStructuredCollapses pins that a
// tool result whose structured envelope is byte-identical to its canonical
// text (read_command_output renders its JSON payload as both) is sent once
// as the plain string form: the array form would put the same bytes on the
// wire twice, doubling a large page's token cost.
func TestAnthropicToolResultContent_IdenticalStructuredCollapses(t *testing.T) {
	cap := quirks.StructuredToolResultCapability{Supported: true, ContentBlockArray: true}
	payload := `{"reference":"stirrup://command-output/abc/stdout","content":"chunk bytes"}`
	block := types.ContentBlock{
		Type:       "tool_result",
		ToolUseID:  "call_1",
		Content:    payload,
		Structured: json.RawMessage(payload),
		Kind:       "command_output_chunk",
	}
	got := anthropicToolResultContent(block, cap)
	want, _ := json.Marshal(payload)
	if !bytes.Equal(got, want) {
		t.Errorf("identical structured+text must collapse to the single string form\n got:  %s\n want: %s", got, want)
	}
}

// TestGeminiStructuredToolResult_CapabilityOn pins that the Gemini builder
// embeds the structured envelope under functionResponse.response.structured
// (with kind) alongside the canonical content when the capability accepts it.
func TestGeminiStructuredToolResult_CapabilityOn(t *testing.T) {
	q := quirks.DefaultRegistry().Resolve("gemini", "gemini-2.5-pro")
	if !q.StructuredToolResults.ObjectResponse {
		t.Fatalf("precondition: gemini object-response capability resolved off")
	}
	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model:    "gemini-2.5-pro",
		Tools:    anthropicTools(),
		Messages: structuredToolResultMessages(),
	}, nil, q)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	resp := geminiFunctionResponseFromBody(t, body)
	var got struct {
		Content    string          `json:"content"`
		Error      bool            `json:"error"`
		Structured json.RawMessage `json:"structured"`
		Kind       string          `json:"kind"`
	}
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("decode response object: %v\nresponse: %s", err, resp)
	}
	if got.Content != "hi\n(exit 0)" {
		t.Errorf("response.content = %q, want canonical text", got.Content)
	}
	if got.Kind != "command_result" {
		t.Errorf("response.kind = %q, want command_result", got.Kind)
	}
	if len(got.Structured) == 0 {
		t.Fatalf("response.structured missing with capability on")
	}
	var sp map[string]any
	if err := json.Unmarshal(got.Structured, &sp); err != nil {
		t.Fatalf("structured not decodable: %v", err)
	}
	if sp["exit_code"] != float64(0) {
		t.Errorf("structured exit_code = %v, want 0", sp["exit_code"])
	}
}

// TestGeminiStructuredToolResult_TextByDefault pins the no-regression
// guarantee for Gemini: capability-off serialisation equals the text-only
// history, and the structured payload never appears on the wire.
func TestGeminiStructuredToolResult_TextByDefault(t *testing.T) {
	var zero quirks.ProviderQuirks

	withStructured, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model:    "gemini-2.5-pro",
		Tools:    anthropicTools(),
		Messages: structuredToolResultMessages(),
	}, nil, zero)
	if err != nil {
		t.Fatalf("build with-structured: %v", err)
	}
	textOnly, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model:    "gemini-2.5-pro",
		Tools:    anthropicTools(),
		Messages: textOnlyToolResultMessages(),
	}, nil, zero)
	if err != nil {
		t.Fatalf("build text-only: %v", err)
	}
	if !bytes.Equal(withStructured, textOnly) {
		t.Errorf("capability-off body must be byte-identical to text-only history\n with-structured: %s\n text-only:       %s", withStructured, textOnly)
	}
	if bytes.Contains(withStructured, []byte("exit_code")) {
		t.Errorf("structured payload leaked onto the wire with capability off: %s", withStructured)
	}
}

// TestOpenAIStructuredToolResult_AlwaysText pins that the OpenAI Chat
// Completions adapter is text-only regardless of a structured envelope:
// OpenAI has no structured capability rule, and a `tool` message's content is
// a plain string on the wire. The structured and text-only histories must
// serialise identically and the payload must never leak.
func TestOpenAIStructuredToolResult_AlwaysText(t *testing.T) {
	q := quirks.DefaultRegistry().Resolve("openai-compatible", "gpt-4o")
	if q.StructuredToolResults.Supported {
		t.Fatalf("precondition: openai must not resolve a structured capability")
	}

	withStructured, err := buildOpenAIRequest(types.StreamParams{
		Model:    "gpt-4o",
		Tools:    anthropicTools(),
		Messages: structuredToolResultMessages(),
	}, true, q, nil)
	if err != nil {
		t.Fatalf("build with-structured: %v", err)
	}
	textOnly, err := buildOpenAIRequest(types.StreamParams{
		Model:    "gpt-4o",
		Tools:    anthropicTools(),
		Messages: textOnlyToolResultMessages(),
	}, true, q, nil)
	if err != nil {
		t.Fatalf("build text-only: %v", err)
	}

	a := mustMarshal(t, withStructured)
	b := mustMarshal(t, textOnly)
	if !bytes.Equal(a, b) {
		t.Errorf("openai body must be byte-identical regardless of structured envelope\n with-structured: %s\n text-only:       %s", a, b)
	}
	if bytes.Contains(a, []byte("exit_code")) {
		t.Errorf("structured payload leaked into the openai request: %s", a)
	}
}

// TestOpenAIResponsesStructuredToolResult_AlwaysText is the Responses-API
// analogue: function_call_output.output is a plain string, so the adapter
// stays text-only and the payload never reaches the wire.
func TestOpenAIResponsesStructuredToolResult_AlwaysText(t *testing.T) {
	q := quirks.DefaultRegistry().Resolve("openai-compatible", "gpt-4o")

	withStructured, err := buildResponsesRequest(types.StreamParams{
		Model:    "gpt-4o",
		Tools:    anthropicTools(),
		Messages: structuredToolResultMessages(),
	}, q, nil)
	if err != nil {
		t.Fatalf("build with-structured: %v", err)
	}
	textOnly, err := buildResponsesRequest(types.StreamParams{
		Model:    "gpt-4o",
		Tools:    anthropicTools(),
		Messages: textOnlyToolResultMessages(),
	}, q, nil)
	if err != nil {
		t.Fatalf("build text-only: %v", err)
	}

	a := mustMarshal(t, withStructured)
	b := mustMarshal(t, textOnly)
	if !bytes.Equal(a, b) {
		t.Errorf("responses body must be byte-identical regardless of structured envelope\n with-structured: %s\n text-only:       %s", a, b)
	}
	if bytes.Contains(a, []byte("exit_code")) {
		t.Errorf("structured payload leaked into the responses request: %s", a)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// geminiFunctionResponseFromBody extracts the functionResponse.response raw
// object from the first function-role content in a serialised Gemini request
// body. Fails the test if the shape is not present.
func geminiFunctionResponseFromBody(t *testing.T, body []byte) json.RawMessage {
	t.Helper()
	var decoded struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				FunctionResponse *struct {
					Name     string          `json:"name"`
					Response json.RawMessage `json:"response"`
				} `json:"functionResponse"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal gemini body: %v", err)
	}
	for _, c := range decoded.Contents {
		if c.Role != "function" {
			continue
		}
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				return p.FunctionResponse.Response
			}
		}
	}
	t.Fatalf("no functionResponse in body: %s", body)
	return nil
}
