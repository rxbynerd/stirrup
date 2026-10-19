package provider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// Thinking blocks are Anthropic-owned history. Every other adapter must
// drop them by construction, so a history carrying them serialises exactly
// like the same history with them stripped and no signature leaves the
// harness towards a different provider.

const (
	foreignThinkingSignature = "anthropic-thinking-signature"
	foreignRedactedData      = "anthropic-redacted-data"
)

func historyWithThinking() []types.Message {
	return []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "read main.go"}}},
		{Role: "assistant", Content: []types.ContentBlock{
			{Type: "thinking", Text: "plan the read", ThoughtSignature: foreignThinkingSignature},
			{Type: "text", Text: "Reading it."},
			{Type: "redacted_thinking", ThoughtSignature: foreignRedactedData},
			{Type: "tool_use", ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
		}},
		{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: "call_1", Content: "package main"}}},
		{Role: "assistant", Content: []types.ContentBlock{
			{Type: "thinking", ThoughtSignature: foreignThinkingSignature},
		}},
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "continue"}}},
	}
}

// assertThinkingDropped fails when the two serialisations differ or the
// thinking-bearing one carries any thinking payload.
func assertThinkingDropped(t *testing.T, adapter string, with, without []byte) {
	t.Helper()
	require.Equal(t, string(without), string(with),
		"%s: history with thinking blocks serialises differently", adapter)
	for _, leaked := range []string{foreignThinkingSignature, foreignRedactedData, "plan the read"} {
		require.NotContains(t, string(with), leaked,
			"%s: request carries thinking payload", adapter)
	}
}

func TestOpenAICompatibleRequest_DropsThinkingBlocks(t *testing.T) {
	q := quirks.DefaultRegistry().Resolve("openai-compatible", "gpt-4o")
	marshal := func(messages []types.Message) []byte {
		req, err := buildOpenAIRequest(types.StreamParams{Model: "gpt-4o", MaxTokens: 64, Messages: messages}, true, q, nil)
		require.NoError(t, err)
		body, err := json.Marshal(req)
		require.NoError(t, err)
		return body
	}
	assertThinkingDropped(t, "openai-compatible",
		marshal(historyWithThinking()),
		marshal(types.StripThinkingBlocks(historyWithThinking())))
}

func TestOpenAIResponsesRequest_DropsThinkingBlocks(t *testing.T) {
	q := quirks.DefaultRegistry().Resolve("openai-responses", "gpt-4o")
	marshal := func(messages []types.Message) []byte {
		req, err := buildResponsesRequest(types.StreamParams{Model: "gpt-4o", MaxTokens: 64, Messages: messages}, q, nil, "")
		require.NoError(t, err)
		body, err := json.Marshal(req)
		require.NoError(t, err)
		return body
	}
	assertThinkingDropped(t, "openai-responses",
		marshal(historyWithThinking()),
		marshal(types.StripThinkingBlocks(historyWithThinking())))
}

func TestGeminiRequest_DropsThinkingBlocks(t *testing.T) {
	q := quirks.DefaultRegistry().Resolve("gemini", "gemini-2.5-pro")
	marshal := func(messages []types.Message) []byte {
		body, _, err := BuildGenerateContentRequest(types.StreamParams{Model: "gemini-2.5-pro", MaxTokens: 64, Messages: messages}, nil, q)
		require.NoError(t, err)
		return body
	}
	assertThinkingDropped(t, "gemini",
		marshal(historyWithThinking()),
		marshal(types.StripThinkingBlocks(historyWithThinking())))
}

func TestBedrockConverseRequest_DropsThinkingBlocks(t *testing.T) {
	params := func(messages []types.Message) types.StreamParams {
		return types.StreamParams{Model: "anthropic.claude-sonnet-4-5", MaxTokens: 64, Messages: messages}
	}
	with, err := buildConverseStreamInput(params(historyWithThinking()))
	require.NoError(t, err, "build with thinking blocks")
	without, err := buildConverseStreamInput(params(types.StripThinkingBlocks(historyWithThinking())))
	require.NoError(t, err, "build without thinking blocks")
	require.Equal(t, without, with, "bedrock: history with thinking blocks translates differently")
}
