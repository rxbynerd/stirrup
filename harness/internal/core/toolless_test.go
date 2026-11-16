package core

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

func TestBuildLoop_NoBuiltInClassificationRunOffersNoTools(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	t.Setenv("TEST_OPENAI_KEY", "test-key")

	var bodies []string
	server := newOpenAIServer(t, nil, []string{
		openAIChunk(`{"id":"c-1","choices":[{"index":0,"delta":{"content":"{\"label\":\"spam\"}"},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"c-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n",
	}, &bodies)
	defer server.Close()

	timeout := 30
	config := &types.RunConfig{
		RunID:           "toolless-classification",
		Mode:            "review",
		Prompt:          "Classify the submission.",
		Provider:        types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:     types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "gpt-5.5"},
		PromptBuilder:   types.PromptBuilderConfig{Type: "default"},
		ContextStrategy: types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:        types.ExecutorConfig{Type: "none"},
		Verifier:        types.VerifierConfig{Type: "none"},
		GitStrategy:     types.GitStrategyConfig{Type: "none"},
		TraceEmitter:    types.TraceEmitterConfig{Type: "jsonl"},
		Tools:           types.ToolsConfig{NoBuiltIn: true},
		OutputSchema:    json.RawMessage(`{"type":"object","properties":{"label":{"type":"string"}},"required":["label"],"additionalProperties":false}`),
		MaxTurns:        2,
		Timeout:         &timeout,
	}

	tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	loop, err := BuildLoopWithTransport(context.Background(), config, tp)
	must.NoError(err)
	defer func() { _ = loop.Close() }()
	must.Empty(loop.Tools.List(), "no tool is registered")

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)
	must.JSONEq(`{"label":"spam"}`, string(runTrace.StructuredOutput))
	check.Empty(runTrace.ToolCalls)

	must.Len(bodies, 1, "a tool-less classification is a single provider call")
	var body map[string]any
	must.NoError(json.Unmarshal([]byte(bodies[0]), &body))
	check.NotContains(body, "tools")
	check.NotContains(body, "tool_choice")
	check.NotContains(body, "parallel_tool_calls")
}
