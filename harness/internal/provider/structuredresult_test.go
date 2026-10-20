package provider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

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
// envelope stripped. A capability-off serialisation of the structured
// history must equal the serialisation of this history.
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

// TestAnthropicStructuredToolResult_EmitsArrayFormWhenCapabilityOn pins that the Anthropic
// builder emits the content-block array form (canonical text + structured
// JSON text part) for a structured tool result when the capability accepts
// it. The default registry resolves the capability on for every Anthropic
// model.
func TestAnthropicStructuredToolResult_EmitsArrayFormWhenCapabilityOn(t *testing.T) {
	must := require.New(t)
	q := quirks.DefaultRegistry().Resolve("anthropic", "claude-sonnet-4-5")
	must.True(q.StructuredToolResults.Supported, "precondition: anthropic structured capability resolved off")

	body := buildAnthropicRequest(types.StreamParams{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 1024,
		Tools:     anthropicTools(),
		Messages:  structuredToolResultMessages(),
	}, true, q)
	raw := mustMarshal(t, body)

	var decoded struct {
		Messages []struct {
			Content []struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	must.NoError(json.Unmarshal(raw, &decoded))

	// The third message is the user tool_result turn.
	trBlock := decoded.Messages[2].Content[0]
	must.Equal("tool_result", trBlock.Type)

	var parts []anthropicToolResultPart
	must.NoError(json.Unmarshal(trBlock.Content, &parts),
		"tool_result content must be an array when the capability is on: %s", trBlock.Content)
	must.Len(parts, 2, "expected text and structured parts, got %+v", parts)

	must.Equal("text", parts[0].Type)
	must.Equal("hi\n(exit 0)", parts[0].Text, "part[0] must be the canonical text fallback")

	must.Equal("text", parts[1].Type)
	must.True(json.Valid([]byte(parts[1].Text)), "part[1].text must be a JSON payload: %q", parts[1].Text)
	var sp map[string]any
	must.NoError(json.Unmarshal([]byte(parts[1].Text), &sp), "structured part must decode")
	must.InDelta(0, sp["exit_code"], 0)
}

// TestAnthropicStructuredToolResult_TextOnlyWhenCapabilityOff pins the no-regression
// guarantee: with the capability unset (zero ProviderQuirks) the structured
// history serialises byte-identically to the same history with the envelope
// stripped. The structured envelope must not leak onto the wire when the
// provider has not opted in.
func TestAnthropicStructuredToolResult_TextOnlyWhenCapabilityOff(t *testing.T) {
	must := require.New(t)
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

	must.Equal(string(textOnly), string(withStructured),
		"capability-off body must be byte-identical to text-only history")
	must.NotContains(string(withStructured), "exit_code",
		"structured payload leaked onto the wire with capability off")
}

// TestAnthropicToolResultContent_EmptyTextWithStructuredReturnsNil pins that an
// empty canonical text with a non-empty structured payload does NOT emit the
// array form with a meaningless empty-string first part. It falls through to
// a nil return (the omitempty shape for an empty-content tool result)
// regardless of the structured envelope.
func TestAnthropicToolResultContent_EmptyTextWithStructuredReturnsNil(t *testing.T) {
	must := require.New(t)
	cap := quirks.StructuredToolResultCapability{Supported: true, ContentBlockArray: true}
	block := types.ContentBlock{
		Type:       "tool_result",
		ToolUseID:  "call_1",
		Content:    "",
		Structured: json.RawMessage(`{"exit_code":0}`),
		Kind:       "command_result",
	}
	got := anthropicToolResultContent(block, cap)
	must.Nil(got, "empty content with structured must return nil, not an empty-string array part: %s", got)
}

// TestAnthropicToolResultContent_IdenticalStructuredCollapsesToString pins that a
// tool result whose structured envelope is byte-identical to its canonical
// text (read_command_output renders its JSON payload as both) is sent once
// as the plain string form: the array form would put the same bytes on the
// wire twice, doubling a large page's token cost.
func TestAnthropicToolResultContent_IdenticalStructuredCollapsesToString(t *testing.T) {
	must := require.New(t)
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
	want := mustMarshal(t, payload)
	must.Equal(string(want), string(got),
		"identical structured and text must collapse to the single string form")
}

// TestGeminiStructuredToolResult_EmbedsEnvelopeWhenCapabilityOn pins that the Gemini builder
// embeds the structured envelope under functionResponse.response.structured
// (with kind) alongside the canonical content when the capability accepts it.
func TestGeminiStructuredToolResult_EmbedsEnvelopeWhenCapabilityOn(t *testing.T) {
	must := require.New(t)
	q := quirks.DefaultRegistry().Resolve("gemini", "gemini-2.5-pro")
	must.True(q.StructuredToolResults.ObjectResponse, "precondition: gemini object-response capability resolved off")

	body, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model:    "gemini-2.5-pro",
		Tools:    anthropicTools(),
		Messages: structuredToolResultMessages(),
	}, nil, q)
	must.NoError(err)

	resp := geminiFunctionResponseFromBody(t, body)
	var got struct {
		Content    string          `json:"content"`
		Error      bool            `json:"error"`
		Structured json.RawMessage `json:"structured"`
		Kind       string          `json:"kind"`
	}
	must.NoError(json.Unmarshal(resp, &got), "response object: %s", resp)
	must.Equal("hi\n(exit 0)", got.Content, "response.content must be the canonical text")
	must.Equal("command_result", got.Kind)
	must.NotEmpty(got.Structured, "response.structured must be present with capability on")

	var sp map[string]any
	must.NoError(json.Unmarshal(got.Structured, &sp), "structured must decode")
	must.InDelta(0, sp["exit_code"], 0)
}

// TestGeminiStructuredToolResult_TextOnlyWhenCapabilityOff pins the no-regression
// guarantee for Gemini: capability-off serialisation equals the text-only
// history, and the structured payload never appears on the wire.
func TestGeminiStructuredToolResult_TextOnlyWhenCapabilityOff(t *testing.T) {
	must := require.New(t)
	var zero quirks.ProviderQuirks

	withStructured, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model:    "gemini-2.5-pro",
		Tools:    anthropicTools(),
		Messages: structuredToolResultMessages(),
	}, nil, zero)
	must.NoError(err, "build with structured envelope")

	textOnly, _, err := BuildGenerateContentRequest(types.StreamParams{
		Model:    "gemini-2.5-pro",
		Tools:    anthropicTools(),
		Messages: textOnlyToolResultMessages(),
	}, nil, zero)
	must.NoError(err, "build text-only history")

	must.Equal(string(textOnly), string(withStructured),
		"capability-off body must be byte-identical to text-only history")
	must.NotContains(string(withStructured), "exit_code",
		"structured payload leaked onto the wire with capability off")
}

// TestOpenAIStructuredToolResult_AlwaysText pins that the OpenAI Chat
// Completions adapter is text-only regardless of a structured envelope:
// OpenAI has no structured capability rule, and a `tool` message's content is
// a plain string on the wire. The structured and text-only histories must
// serialise identically and the payload must never leak.
func TestOpenAIStructuredToolResult_AlwaysText(t *testing.T) {
	must := require.New(t)
	q := quirks.DefaultRegistry().Resolve("openai-compatible", "gpt-4o")
	must.False(q.StructuredToolResults.Supported, "precondition: openai must not resolve a structured capability")

	withStructured, err := buildOpenAIRequest(types.StreamParams{
		Model:    "gpt-4o",
		Tools:    anthropicTools(),
		Messages: structuredToolResultMessages(),
	}, true, q, nil)
	must.NoError(err, "build with structured envelope")

	textOnly, err := buildOpenAIRequest(types.StreamParams{
		Model:    "gpt-4o",
		Tools:    anthropicTools(),
		Messages: textOnlyToolResultMessages(),
	}, true, q, nil)
	must.NoError(err, "build text-only history")

	withStructuredJSON := mustMarshal(t, withStructured)
	textOnlyJSON := mustMarshal(t, textOnly)
	must.Equal(string(textOnlyJSON), string(withStructuredJSON), //nolint:testifylint // byte identity is the contract
		"openai body must be byte-identical regardless of structured envelope")
	must.NotContains(string(withStructuredJSON), "exit_code",
		"structured payload leaked into the openai request")
}

// TestOpenAIResponsesStructuredToolResult_AlwaysText is the Responses-API
// analogue: function_call_output.output is a plain string, so the adapter
// stays text-only and the payload never reaches the wire.
func TestOpenAIResponsesStructuredToolResult_AlwaysText(t *testing.T) {
	must := require.New(t)
	q := quirks.DefaultRegistry().Resolve("openai-compatible", "gpt-4o")

	withStructured, err := buildResponsesRequest(types.StreamParams{
		Model:    "gpt-4o",
		Tools:    anthropicTools(),
		Messages: structuredToolResultMessages(),
	}, q, nil, "")
	must.NoError(err, "build with structured envelope")

	textOnly, err := buildResponsesRequest(types.StreamParams{
		Model:    "gpt-4o",
		Tools:    anthropicTools(),
		Messages: textOnlyToolResultMessages(),
	}, q, nil, "")
	must.NoError(err, "build text-only history")

	withStructuredJSON := mustMarshal(t, withStructured)
	textOnlyJSON := mustMarshal(t, textOnly)
	must.Equal(string(textOnlyJSON), string(withStructuredJSON), //nolint:testifylint // byte identity is the contract
		"responses body must be byte-identical regardless of structured envelope")
	must.NotContains(string(withStructuredJSON), "exit_code",
		"structured payload leaked into the responses request")
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	must := require.New(t)
	b, err := json.Marshal(v)
	must.NoError(err)
	return b
}

// geminiFunctionResponseFromBody extracts the functionResponse.response raw
// object from the first function-role content in a serialised Gemini request
// body. Fails the test if the shape is not present.
func geminiFunctionResponseFromBody(t *testing.T, body []byte) json.RawMessage {
	t.Helper()
	must := require.New(t)
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
	must.NoError(json.Unmarshal(body, &decoded), "decode gemini body")
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
	must.FailNow("no functionResponse in gemini body", "body: %s", body)
	return nil
}
