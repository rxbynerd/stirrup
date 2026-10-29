package provider

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

func TestBedrockQuirksTarget_MapsModelIDs(t *testing.T) {
	anthropicOnBedrock := func(model string) quirks.Target {
		return quirks.Target{ProviderType: "anthropic", Endpoint: BedrockEndpoint, Model: model}
	}
	bedrock := func(model string) quirks.Target {
		return quirks.Target{ProviderType: "bedrock", Model: model}
	}
	cases := []struct {
		name  string
		model string
		want  quirks.Target
	}{
		{"bare anthropic id", "anthropic.claude-opus-5-5", anthropicOnBedrock("claude-opus-5-5")},
		{"versioned anthropic id", "anthropic.claude-sonnet-4-6-v1:0", anthropicOnBedrock("claude-sonnet-4-6-v1:0")},
		{"us cross-region profile", "us.anthropic.claude-sonnet-5-5", anthropicOnBedrock("claude-sonnet-5-5")},
		{"global cross-region profile", "global.anthropic.claude-fable-5-1", anthropicOnBedrock("claude-fable-5-1")},
		{"us-gov cross-region profile", "us-gov.anthropic.claude-haiku-5-5", anthropicOnBedrock("claude-haiku-5-5")},
		{"foundation model ARN", "arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-opus-5-5", anthropicOnBedrock("claude-opus-5-5")},
		{"system inference profile ARN", "arn:aws:bedrock:eu-west-1:111122223333:inference-profile/eu.anthropic.claude-opus-5-5", anthropicOnBedrock("claude-opus-5-5")},
		{"application inference profile ARN", "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/a1b2c3d4", bedrock("arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/a1b2c3d4")},
		{"non-anthropic id", "meta.llama3-1-70b-instruct-v1:0", bedrock("meta.llama3-1-70b-instruct-v1:0")},
		{"non-anthropic cross-region profile", "us.amazon.nova-pro-v1:0", bedrock("us.amazon.nova-pro-v1:0")},
		{"first-party id without provider prefix", "claude-opus-5-5", bedrock("claude-opus-5-5")},
		{"anthropic prefix with no model", "anthropic.", bedrock("anthropic.")},
		{"two region labels", "us.eu.anthropic.claude-opus-5-5", bedrock("us.eu.anthropic.claude-opus-5-5")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, BedrockQuirksTarget(tc.model))
		})
	}
}

// TestBedrock_Stream_SamplingParamsFollowAnthropicRules pins that the
// Bedrock request drops temperature exactly where the anthropic rules say
// the Claude model rejects it, and warns when it does.
func TestBedrock_Stream_SamplingParamsFollowAnthropicRules(t *testing.T) {
	cases := []struct {
		model    string
		wantTemp bool
	}{
		{model: "anthropic.claude-sonnet-4-6-v1:0", wantTemp: true},
		{model: "anthropic.claude-opus-4-7", wantTemp: false},
		{model: "anthropic.claude-opus-5-5", wantTemp: false},
		{model: "us.anthropic.claude-sonnet-5-5", wantTemp: false},
		{model: "eu.anthropic.claude-fable-5-1", wantTemp: false},
		{model: "anthropic.claude-haiku-5-5", wantTemp: false},
		{model: "meta.llama3-1-70b-instruct-v1:0", wantTemp: true},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			client := &mockBedrockClient{apiErr: fmt.Errorf("mock")}
			adapter := &BedrockAdapter{client: client, AdapterDeps: AdapterDeps{Logger: logger}}

			_, _ = adapter.Stream(context.Background(), types.StreamParams{
				Model:       tc.model,
				MaxTokens:   1024,
				Temperature: types.Float64Ptr(0.1),
				Messages: []types.Message{
					{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
				},
			})

			must.NotNil(client.capturedInput, "client never reached")
			must.NotNil(client.capturedInput.InferenceConfig)
			check.Equal(int32(1024), aws.ToInt32(client.capturedInput.InferenceConfig.MaxTokens), "MaxTokens")
			if tc.wantTemp {
				check.NotNil(client.capturedInput.InferenceConfig.Temperature, "temperature should be forwarded")
				check.NotContains(buf.String(), "bedrock quirks suppressed caller temperature")
				return
			}
			check.Nil(client.capturedInput.InferenceConfig.Temperature, "temperature should be omitted")
			check.Contains(buf.String(), "bedrock quirks suppressed caller temperature")
			check.Contains(buf.String(), "omit sampling params", "log should name the rule that dropped temperature")
			check.NotContains(buf.String(), "prompt caching", "log should not name rules the Converse request does not honour")
		})
	}
}

func TestBedrock_Stream_ReasoningEffortWarnsIgnored(t *testing.T) {
	check := assert.New(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	adapter := &BedrockAdapter{client: &mockBedrockClient{apiErr: fmt.Errorf("mock")}, AdapterDeps: AdapterDeps{Logger: logger}}

	_, _ = adapter.Stream(context.Background(), types.StreamParams{
		Model:           "anthropic.claude-opus-5-5",
		MaxTokens:       1024,
		ReasoningEffort: "high",
	})

	check.Contains(buf.String(), "bedrock reasoningEffort ignored")
	check.Contains(buf.String(), `"reasoning_effort":"high"`)
}

func TestResolveBedrockQuirks_KeepsOnlyWhatConverseHonours(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	q, rules := ResolveBedrockQuirks(quirks.DefaultRegistry(), "us.anthropic.claude-opus-5-5")

	check.True(q.BehaviourFlags.Anthropic.OmitSamplingParams)
	check.Empty(q.BehaviourFlags.Anthropic.EffortLevels, "Converse carries no effort control")
	check.False(q.BehaviourFlags.Anthropic.PromptCaching, "Converse carries no cache_control")
	check.False(q.ToolChoice.Supported, "Converse projection sends no tool_choice")
	check.False(q.ToolSearch.Supported, "Converse projection sends no tool search")
	check.False(q.StructuredToolResults.Supported, "Converse projection sends text tool results")
	must.NotEmpty(rules)
	for _, rule := range rules {
		check.Contains(rule.Description, "omit sampling params")
	}
}

func TestNewBedrockAdapter_UsesDefaultRegistryAndRecordsRetryPolicy(t *testing.T) {
	must := require.New(t)
	adapter, err := NewBedrockAdapter("us-east-1", "", nil, RetryPolicy{MaxAttempts: 3})
	must.NoError(err)
	must.Same(quirks.DefaultRegistry(), adapter.Registry)
	must.Equal(3, adapter.RetryPolicy.MaxAttempts)
}
