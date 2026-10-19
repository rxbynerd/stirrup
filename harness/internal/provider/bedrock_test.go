package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

// mockEventReader implements bedrockEventReader using a pre-loaded slice of
// events. Events are sent on a channel; Err() returns a configurable error
// after the channel drains.
type mockEventReader struct {
	ch       chan brtypes.ConverseStreamOutput
	closed   bool
	finalErr error
}

func newMockEventReader(events []brtypes.ConverseStreamOutput, err error) *mockEventReader {
	ch := make(chan brtypes.ConverseStreamOutput, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return &mockEventReader{ch: ch, finalErr: err}
}

func (m *mockEventReader) Events() <-chan brtypes.ConverseStreamOutput {
	return m.ch
}
func (m *mockEventReader) Close() error { m.closed = true; return nil }
func (m *mockEventReader) Err() error   { return m.finalErr }

// mockBedrockClient implements bedrockConverseStreamer. It captures the input
// and returns events from a mock event reader.
type mockBedrockClient struct {
	capturedInput *bedrockruntime.ConverseStreamInput
	apiErr        error // error from ConverseStream call itself
}

func (m *mockBedrockClient) ConverseStream(ctx context.Context, params *bedrockruntime.ConverseStreamInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error) {
	m.capturedInput = params
	if m.apiErr != nil {
		return nil, m.apiErr
	}
	// Tests exercising the full Stream() method use consumeBedrockStream
	// directly; this mock covers buildConverseStreamInput translation and
	// API error handling.
	return nil, fmt.Errorf("mock: use consumeBedrockStream directly for stream tests")
}

func TestBedrock_TextStreaming(t *testing.T) {
	events := []brtypes.ConverseStreamOutput{
		&brtypes.ConverseStreamOutputMemberContentBlockDelta{
			Value: brtypes.ContentBlockDeltaEvent{
				ContentBlockIndex: aws.Int32(0),
				Delta:             &brtypes.ContentBlockDeltaMemberText{Value: "Hello"},
			},
		},
		&brtypes.ConverseStreamOutputMemberContentBlockDelta{
			Value: brtypes.ContentBlockDeltaEvent{
				ContentBlockIndex: aws.Int32(0),
				Delta:             &brtypes.ContentBlockDeltaMemberText{Value: " world"},
			},
		},
		&brtypes.ConverseStreamOutputMemberMessageStop{
			Value: brtypes.MessageStopEvent{
				StopReason: brtypes.StopReasonEndTurn,
			},
		},
	}

	ch := make(chan types.StreamEvent, 64)
	reader := newMockEventReader(events, nil)
	go consumeBedrockStream(context.Background(), reader, ch)
	result := collectEvents(t, ch)

	require.Len(t, result, 3, "events: %+v", result)
	assert.Equal(t, "text_delta", result[0].Type, "event[0].Type")
	assert.Equal(t, "Hello", result[0].Text, "event[0].Text")
	assert.Equal(t, "text_delta", result[1].Type, "event[1].Type")
	assert.Equal(t, " world", result[1].Text, "event[1].Text")
	assert.Equal(t, "message_complete", result[2].Type, "event[2].Type")
	assert.Equal(t, "end_turn", result[2].StopReason, "event[2].StopReason")
}

func TestBedrock_ToolCallStreaming(t *testing.T) {
	events := []brtypes.ConverseStreamOutput{
		&brtypes.ConverseStreamOutputMemberContentBlockStart{
			Value: brtypes.ContentBlockStartEvent{
				ContentBlockIndex: aws.Int32(0),
				Start: &brtypes.ContentBlockStartMemberToolUse{
					Value: brtypes.ToolUseBlockStart{
						ToolUseId: aws.String("toolu_abc"),
						Name:      aws.String("read_file"),
					},
				},
			},
		},
		&brtypes.ConverseStreamOutputMemberContentBlockDelta{
			Value: brtypes.ContentBlockDeltaEvent{
				ContentBlockIndex: aws.Int32(0),
				Delta: &brtypes.ContentBlockDeltaMemberToolUse{
					Value: brtypes.ToolUseBlockDelta{
						Input: aws.String(`{"path":`),
					},
				},
			},
		},
		&brtypes.ConverseStreamOutputMemberContentBlockDelta{
			Value: brtypes.ContentBlockDeltaEvent{
				ContentBlockIndex: aws.Int32(0),
				Delta: &brtypes.ContentBlockDeltaMemberToolUse{
					Value: brtypes.ToolUseBlockDelta{
						Input: aws.String(`"main.go"}`),
					},
				},
			},
		},
		&brtypes.ConverseStreamOutputMemberContentBlockStop{
			Value: brtypes.ContentBlockStopEvent{
				ContentBlockIndex: aws.Int32(0),
			},
		},
		&brtypes.ConverseStreamOutputMemberMessageStop{
			Value: brtypes.MessageStopEvent{
				StopReason: brtypes.StopReasonToolUse,
			},
		},
	}

	ch := make(chan types.StreamEvent, 64)
	reader := newMockEventReader(events, nil)
	go consumeBedrockStream(context.Background(), reader, ch)
	result := collectEvents(t, ch)

	require.Len(t, result, 2, "events: %+v", result)

	tc := result[0]
	require.Equal(t, "tool_call", tc.Type, "event[0].Type")
	assert.Equal(t, "toolu_abc", tc.ID, "event[0].ID")
	assert.Equal(t, "read_file", tc.Name, "event[0].Name")
	assert.Equal(t, "main.go", tc.Input["path"], "event[0].Input[path]")

	assert.Equal(t, "message_complete", result[1].Type, "event[1].Type")
	assert.Equal(t, "tool_use", result[1].StopReason, "event[1].StopReason")
}

func TestBedrock_MixedTextAndToolCall(t *testing.T) {
	events := []brtypes.ConverseStreamOutput{
		// Text block at index 0.
		&brtypes.ConverseStreamOutputMemberContentBlockDelta{
			Value: brtypes.ContentBlockDeltaEvent{
				ContentBlockIndex: aws.Int32(0),
				Delta:             &brtypes.ContentBlockDeltaMemberText{Value: "Let me read that file."},
			},
		},
		&brtypes.ConverseStreamOutputMemberContentBlockStop{
			Value: brtypes.ContentBlockStopEvent{ContentBlockIndex: aws.Int32(0)},
		},
		// Tool use block at index 1.
		&brtypes.ConverseStreamOutputMemberContentBlockStart{
			Value: brtypes.ContentBlockStartEvent{
				ContentBlockIndex: aws.Int32(1),
				Start: &brtypes.ContentBlockStartMemberToolUse{
					Value: brtypes.ToolUseBlockStart{
						ToolUseId: aws.String("toolu_xyz"),
						Name:      aws.String("read_file"),
					},
				},
			},
		},
		&brtypes.ConverseStreamOutputMemberContentBlockDelta{
			Value: brtypes.ContentBlockDeltaEvent{
				ContentBlockIndex: aws.Int32(1),
				Delta: &brtypes.ContentBlockDeltaMemberToolUse{
					Value: brtypes.ToolUseBlockDelta{
						Input: aws.String(`{"path":"test.go"}`),
					},
				},
			},
		},
		&brtypes.ConverseStreamOutputMemberContentBlockStop{
			Value: brtypes.ContentBlockStopEvent{ContentBlockIndex: aws.Int32(1)},
		},
		&brtypes.ConverseStreamOutputMemberMessageStop{
			Value: brtypes.MessageStopEvent{StopReason: brtypes.StopReasonToolUse},
		},
	}

	ch := make(chan types.StreamEvent, 64)
	reader := newMockEventReader(events, nil)
	go consumeBedrockStream(context.Background(), reader, ch)
	result := collectEvents(t, ch)

	require.Len(t, result, 3, "events: %+v", result)
	assert.Equal(t, "text_delta", result[0].Type, "event[0].Type")
	assert.Equal(t, "tool_call", result[1].Type, "event[1].Type")
	assert.Equal(t, "read_file", result[1].Name, "event[1].Name")
	assert.Equal(t, "message_complete", result[2].Type, "event[2].Type")
}

func TestBedrock_StopReasonMapping(t *testing.T) {
	tests := []struct {
		name   string
		reason brtypes.StopReason
		want   string
	}{
		{"end turn", brtypes.StopReasonEndTurn, "end_turn"},
		{"tool use", brtypes.StopReasonToolUse, "tool_use"},
		{"max tokens", brtypes.StopReasonMaxTokens, "max_tokens"},
		{"stop sequence", brtypes.StopReasonStopSequence, "stop_sequence"},
		{"unknown reason passes through", brtypes.StopReason("unknown_reason"), "unknown_reason"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, mapStopReason(tt.reason))
		})
	}
}

func TestBedrock_MetadataTokens(t *testing.T) {
	outputTokens := int32(150)
	events := []brtypes.ConverseStreamOutput{
		&brtypes.ConverseStreamOutputMemberMessageStop{
			Value: brtypes.MessageStopEvent{StopReason: brtypes.StopReasonEndTurn},
		},
		&brtypes.ConverseStreamOutputMemberMetadata{
			Value: brtypes.ConverseStreamMetadataEvent{
				Usage: &brtypes.TokenUsage{
					OutputTokens: &outputTokens,
				},
			},
		},
	}

	ch := make(chan types.StreamEvent, 64)
	reader := newMockEventReader(events, nil)
	go consumeBedrockStream(context.Background(), reader, ch)
	result := collectEvents(t, ch)

	require.Len(t, result, 2, "events: %+v", result)
	assert.Equal(t, "end_turn", result[0].StopReason, "event[0].StopReason")
	assert.Equal(t, 150, result[1].OutputTokens, "event[1].OutputTokens")
}

func TestBedrock_StreamError(t *testing.T) {
	events := []brtypes.ConverseStreamOutput{
		&brtypes.ConverseStreamOutputMemberContentBlockDelta{
			Value: brtypes.ContentBlockDeltaEvent{
				ContentBlockIndex: aws.Int32(0),
				Delta:             &brtypes.ContentBlockDeltaMemberText{Value: "partial"},
			},
		},
	}

	ch := make(chan types.StreamEvent, 64)
	reader := newMockEventReader(events, fmt.Errorf("connection reset"))
	go consumeBedrockStream(context.Background(), reader, ch)
	result := collectEvents(t, ch)

	require.Len(t, result, 2, "events: %+v", result)
	assert.Equal(t, "text_delta", result[0].Type, "event[0].Type")
	require.Equal(t, "error", result[1].Type, "event[1].Type")
	require.ErrorContains(t, result[1].Error, "connection reset")
}

func TestBedrock_ContextCancellation(t *testing.T) {
	// Create a reader with events that will block.
	blockingCh := make(chan brtypes.ConverseStreamOutput)
	reader := &mockEventReader{ch: blockingCh}

	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan types.StreamEvent, 64)

	go func() {
		// Send one event, then cancel.
		blockingCh <- &brtypes.ConverseStreamOutputMemberContentBlockDelta{
			Value: brtypes.ContentBlockDeltaEvent{
				ContentBlockIndex: aws.Int32(0),
				Delta:             &brtypes.ContentBlockDeltaMemberText{Value: "before cancel"},
			},
		}
		cancel()
		// Send another event that should trigger the ctx.Done() check.
		blockingCh <- &brtypes.ConverseStreamOutputMemberContentBlockDelta{
			Value: brtypes.ContentBlockDeltaEvent{
				ContentBlockIndex: aws.Int32(0),
				Delta:             &brtypes.ContentBlockDeltaMemberText{Value: "after cancel"},
			},
		}
		close(blockingCh)
	}()

	go consumeBedrockStream(ctx, reader, ch)
	result := collectEvents(t, ch)

	hasError := slices.ContainsFunc(result, func(ev types.StreamEvent) bool {
		return ev.Type == "error" && ev.Error != nil
	})
	require.True(t, hasError, "expected an error event from context cancellation")
}

func TestBedrock_MalformedToolInput_MarksToolCall(t *testing.T) {
	events := []brtypes.ConverseStreamOutput{
		&brtypes.ConverseStreamOutputMemberContentBlockStart{
			Value: brtypes.ContentBlockStartEvent{
				ContentBlockIndex: aws.Int32(0),
				Start: &brtypes.ContentBlockStartMemberToolUse{
					Value: brtypes.ToolUseBlockStart{
						ToolUseId: aws.String("toolu_bad"),
						Name:      aws.String("read_file"),
					},
				},
			},
		},
		&brtypes.ConverseStreamOutputMemberContentBlockDelta{
			Value: brtypes.ContentBlockDeltaEvent{
				ContentBlockIndex: aws.Int32(0),
				Delta: &brtypes.ContentBlockDeltaMemberToolUse{
					Value: brtypes.ToolUseBlockDelta{
						Input: aws.String(`{"path": INVALID`),
					},
				},
			},
		},
		&brtypes.ConverseStreamOutputMemberContentBlockStop{
			Value: brtypes.ContentBlockStopEvent{
				ContentBlockIndex: aws.Int32(0),
			},
		},
		&brtypes.ConverseStreamOutputMemberMessageStop{
			Value: brtypes.MessageStopEvent{StopReason: brtypes.StopReasonToolUse},
		},
	}

	ch := make(chan types.StreamEvent, 64)
	reader := newMockEventReader(events, nil)
	go consumeBedrockStream(context.Background(), reader, ch)
	requireMarkedToolCall(t, collectEvents(t, ch), "toolu_bad", "read_file", `{"path": INVALID`)
}

func TestBedrock_TranslateMessages(t *testing.T) {
	msgs := []types.Message{
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "text", Text: "Hello"},
			},
		},
		{
			Role: "assistant",
			Content: []types.ContentBlock{
				{Type: "text", Text: "Hi there"},
				{Type: "tool_use", ID: "toolu_1", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
			},
		},
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "toolu_1", Content: "file contents here"},
			},
		},
	}

	result, err := bedrockTranslateMessages(msgs)
	require.NoError(t, err)
	require.Len(t, result, 3)

	assert.Equal(t, brtypes.ConversationRoleUser, result[0].Role, "msg[0].Role")
	require.Len(t, result[0].Content, 1, "msg[0].Content")
	require.IsType(t, &brtypes.ContentBlockMemberText{}, result[0].Content[0], "msg[0].Content[0]")
	text := result[0].Content[0].(*brtypes.ContentBlockMemberText)
	assert.Equal(t, "Hello", text.Value, "msg[0].Content[0].Value")

	assert.Equal(t, brtypes.ConversationRoleAssistant, result[1].Role, "msg[1].Role")
	require.Len(t, result[1].Content, 2, "msg[1].Content")
	require.IsType(t, &brtypes.ContentBlockMemberToolUse{}, result[1].Content[1], "msg[1].Content[1]")
	toolUse := result[1].Content[1].(*brtypes.ContentBlockMemberToolUse)
	assert.Equal(t, "read_file", aws.ToString(toolUse.Value.Name), "tool_use.Name")
	assert.Equal(t, "toolu_1", aws.ToString(toolUse.Value.ToolUseId), "tool_use.ToolUseId")

	require.Len(t, result[2].Content, 1, "msg[2].Content")
	require.IsType(t, &brtypes.ContentBlockMemberToolResult{}, result[2].Content[0], "msg[2].Content[0]")
	toolResult := result[2].Content[0].(*brtypes.ContentBlockMemberToolResult)
	assert.Equal(t, "toolu_1", aws.ToString(toolResult.Value.ToolUseId), "tool_result.ToolUseId")
}

func TestBedrock_TranslateMessages_RejectsUnknownRole(t *testing.T) {
	msgs := []types.Message{
		{Role: "system", Content: []types.ContentBlock{{Type: "text", Text: "nope"}}},
	}
	_, err := bedrockTranslateMessages(msgs)
	require.ErrorContains(t, err, "unsupported message role")
}

func TestBedrock_TranslateTools(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
	tools := []types.ToolDefinition{
		{Name: "read_file", Description: "Read a file", InputSchema: schema},
	}

	result, err := bedrockTranslateTools(tools)
	require.NoError(t, err)
	require.Len(t, result, 1)

	require.IsType(t, &brtypes.ToolMemberToolSpec{}, result[0])
	toolSpec := result[0].(*brtypes.ToolMemberToolSpec)
	assert.Equal(t, "read_file", aws.ToString(toolSpec.Value.Name), "tool.Name")
	assert.Equal(t, "Read a file", aws.ToString(toolSpec.Value.Description), "tool.Description")
}

func TestBedrock_TranslateToolResult_MarksErrorStatus(t *testing.T) {
	msgs := []types.Message{
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "toolu_err", Content: "something failed", IsError: true},
			},
		},
	}

	result, err := bedrockTranslateMessages(msgs)
	require.NoError(t, err)

	require.IsType(t, &brtypes.ContentBlockMemberToolResult{}, result[0].Content[0])
	trBlock := result[0].Content[0].(*brtypes.ContentBlockMemberToolResult)
	require.Equal(t, brtypes.ToolResultStatusError, trBlock.Value.Status)
}

func TestBedrock_BuildConverseStreamInput(t *testing.T) {
	params := types.StreamParams{
		Model:       "anthropic.claude-sonnet-4-6-v1",
		System:      "You are helpful.",
		MaxTokens:   4096,
		Temperature: types.Float64Ptr(0.1),
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "Hello"}}},
		},
		Tools: []types.ToolDefinition{
			{Name: "read_file", Description: "Read", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}

	input, err := buildConverseStreamInput(params)
	require.NoError(t, err)

	assert.Equal(t, "anthropic.claude-sonnet-4-6-v1", aws.ToString(input.ModelId), "ModelId")
	require.Len(t, input.System, 1)
	require.Len(t, input.Messages, 1)
	require.NotNil(t, input.InferenceConfig)
	assert.Equal(t, int32(4096), aws.ToInt32(input.InferenceConfig.MaxTokens), "MaxTokens")
	assert.InDelta(t, 0.1, aws.ToFloat32(input.InferenceConfig.Temperature), 1e-6, "Temperature")
	require.NotNil(t, input.ToolConfig)
	assert.Len(t, input.ToolConfig.Tools, 1, "ToolConfig.Tools")
}

// TestBedrock_TemperatureWireShape pins the unset-vs-explicit-zero
// semantics for StreamParams.Temperature on the Bedrock adapter. A nil
// pointer leaves InferenceConfig.Temperature nil, which the AWS SDK omits
// from the Converse request. A non-nil pointer (including an explicit 0.0
// for greedy decoding) flows through verbatim.
func TestBedrock_TemperatureWireShape(t *testing.T) {
	cases := []struct {
		name        string
		temperature *float64
		wantSet     bool
		wantValue   float32
	}{
		{name: "nil leaves temperature unset", temperature: nil, wantSet: false},
		{name: "explicit zero forwarded", temperature: types.Float64Ptr(0.0), wantSet: true, wantValue: 0.0},
		{name: "non-zero forwarded", temperature: types.Float64Ptr(0.5), wantSet: true, wantValue: 0.5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, err := buildConverseStreamInput(types.StreamParams{
				Model:       "anthropic.claude-sonnet-4-6-v1",
				MaxTokens:   1024,
				Temperature: tc.temperature,
				Messages: []types.Message{
					{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
				},
			})
			require.NoError(t, err)
			require.NotNil(t, input.InferenceConfig)
			got := input.InferenceConfig.Temperature
			if tc.wantSet {
				require.NotNil(t, got, "Temperature should be set")
				require.InDelta(t, tc.wantValue, *got, 1e-6)
			} else {
				require.Nil(t, got, "Temperature should be unset")
			}
		})
	}
}

func TestBedrock_BuildConverseStreamInput_OmitsEmptySystem(t *testing.T) {
	params := types.StreamParams{
		Model:     "anthropic.claude-sonnet-4-6-v1",
		MaxTokens: 1024,
	}
	input, err := buildConverseStreamInput(params)
	require.NoError(t, err)
	require.Nil(t, input.System, "system prompt should be omitted when empty")
}

func TestBedrock_Stream_ReturnsAPIError(t *testing.T) {
	client := &mockBedrockClient{apiErr: fmt.Errorf("access denied")}
	adapter := &BedrockAdapter{client: client}

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "anthropic.claude-sonnet-4-6-v1",
		MaxTokens: 1024,
	})
	require.ErrorContains(t, err, "access denied")
}

// TestBedrock_ToolChoiceNonAuto_WarnsDowngrade pins that Bedrock's
// ConverseStream request carries no tool-choice field, so a non-auto
// ToolChoice is silently downgraded to auto. The warn must fire (so the
// downgrade is observable) and must NOT leak message content or any
// secret-derived value — only the static mode integer and the adapter /
// model identifiers. The mock client's deliberate error is irrelevant:
// the warn is emitted before the build/client call.
func TestBedrock_ToolChoiceNonAuto_WarnsDowngrade(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	client := &mockBedrockClient{apiErr: fmt.Errorf("mock")}
	adapter := &BedrockAdapter{client: client, Logger: logger}

	// Tools are supplied so buildConverseStreamInput populates ToolConfig.
	// Without a tool the ToolConfig is nil and the wire-body assertion below
	// would pass vacuously, never exercising the "ToolChoice not projected"
	// invariant it is meant to pin.
	_, _ = adapter.Stream(context.Background(), types.StreamParams{
		Model:      "anthropic.claude-sonnet-4-6-v1",
		MaxTokens:  1024,
		ToolChoice: types.ToolChoiceRequired,
		Tools: []types.ToolDefinition{
			{
				Name:        "read_file",
				Description: "Read a file",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
			},
		},
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "secret-prompt-text"}}},
		},
	})

	out := buf.String()
	assert.Contains(t, out, "bedrock tool-choice downgraded to auto", "expected tool-choice downgrade warn")
	assert.Contains(t, out, "anthropic.claude-sonnet-4-6-v1", "warn log missing model identifier")
	assert.NotContains(t, out, "secret-prompt-text", "warn log leaked message content")

	// The downgrade is log-only: the request body must never carry a tool
	// choice, mirroring the openai-responses wire-body guard. The mock
	// captures the input before its apiErr fires, so capturedInput reflects
	// the body the adapter built; with Tools present ToolConfig is non-nil,
	// so the ToolChoice check below is exercised rather than skipped.
	input := client.capturedInput
	require.NotNil(t, input, "client never reached: buildConverseStreamInput must succeed so the wire body can be inspected")
	require.NotNil(t, input.ToolConfig, "ToolConfig must be populated (Tools were supplied) for the ToolChoice assertion to be meaningful")
	require.Nil(t, input.ToolConfig.ToolChoice, "request must not project ToolChoice onto ConverseStreamInput")
}

// TestBedrock_ToolChoiceAuto_NoWarn pins the negative: the zero-value
// ToolChoiceAuto must not emit the downgrade warn (the auto case is the
// always-satisfied no-op).
func TestBedrock_ToolChoiceAuto_NoWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	client := &mockBedrockClient{apiErr: fmt.Errorf("mock")}
	adapter := &BedrockAdapter{client: client, Logger: logger}

	_, _ = adapter.Stream(context.Background(), types.StreamParams{
		Model:      "anthropic.claude-sonnet-4-6-v1",
		MaxTokens:  1024,
		ToolChoice: types.ToolChoiceAuto,
	})

	require.NotContains(t, buf.String(), "tool-choice downgraded", "auto tool-choice must not warn")
}

func TestBedrock_APIError_RecordsLatency(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	metrics, err := observability.NewMetricsForTesting(provider)
	require.NoError(t, err, "NewMetricsForTesting")

	client := &mockBedrockClient{apiErr: fmt.Errorf("boom")}
	adapter := &BedrockAdapter{client: client, Metrics: metrics}

	_, err = adapter.Stream(context.Background(), types.StreamParams{
		Model:     "anthropic.claude-sonnet-4-6-v1",
		MaxTokens: 1024,
	})
	require.Error(t, err)
	assert.EqualValues(t, 1, providerHistogramTotalCount(t, reader, "stirrup.harness.provider_latency"), "error path still records latency")
	assert.EqualValues(t, 0, providerHistogramTotalCount(t, reader, "stirrup.harness.provider_ttfb"), "error path records no time to first byte")
}

func TestBedrock_ConsumeStreamMetered_RecordsTTFB(t *testing.T) {
	events := []brtypes.ConverseStreamOutput{
		&brtypes.ConverseStreamOutputMemberContentBlockDelta{
			Value: brtypes.ContentBlockDeltaEvent{
				ContentBlockIndex: aws.Int32(0),
				Delta:             &brtypes.ContentBlockDeltaMemberText{Value: "Hi"},
			},
		},
		&brtypes.ConverseStreamOutputMemberMessageStop{
			Value: brtypes.MessageStopEvent{StopReason: brtypes.StopReasonEndTurn},
		},
	}
	ch := make(chan types.StreamEvent, 64)
	stream := newMockEventReader(events, nil)

	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	metrics, err := observability.NewMetricsForTesting(provider)
	require.NoError(t, err, "NewMetricsForTesting")

	attrs := metric.WithAttributes(
		attribute.String("provider.type", "bedrock"),
		attribute.String("provider.model", "anthropic.claude-sonnet-4-6-v1"),
	)
	inference := inferenceAttrs("bedrock", "anthropic.claude-sonnet-4-6-v1", "")
	inference.ServerAddress, inference.ServerPort = (&BedrockAdapter{region: "us-east-1"}).ServerEndpoint()
	go consumeBedrockStreamMetered(context.Background(), stream, ch, metrics, time.Now(), attrs, inference)
	for range ch {
	}

	assert.EqualValues(t, 1, providerHistogramTotalCount(t, reader, "stirrup.harness.provider_ttfb"), "provider_ttfb count")
	assert.EqualValues(t, 1, providerHistogramTotalCount(t, reader, "gen_ai.client.operation.time_to_first_chunk"), "time_to_first_chunk count")

	ttfc, _ := providerHistogramFinder(t, reader, "gen_ai.client.operation.time_to_first_chunk")
	serverAddress, _ := ttfc.DataPoints[0].Attributes.Value("server.address")
	assert.Equal(t, "bedrock-runtime.us-east-1.amazonaws.com", serverAddress.AsString(), "server.address should be the regional Bedrock Runtime host")
	providerName, _ := ttfc.DataPoints[0].Attributes.Value("gen_ai.provider.name")
	assert.Equal(t, "aws.bedrock", providerName.AsString(), "gen_ai.provider.name")
}

// TestNewBedrockAdapter_RetryPolicyWiring covers the one adapter that
// cannot reuse DoWithRetry: ConverseStream goes through the AWS SDK's own
// transport, not a raw *http.Client, so the shared RetryPolicy is mapped
// onto the SDK's own Standard retryer (or aws.NopRetryer when retries are
// disabled) at construction time. This only exercises that
// NewBedrockAdapter accepts the policy and returns a usable adapter for
// both cases — the retryer itself is internal to the AWS SDK client and
// not inspectable from here.
func TestNewBedrockAdapter_RetryPolicyWiring(t *testing.T) {
	for name, policy := range map[string]RetryPolicy{
		"enabled":  {MaxAttempts: 5, InitialDelay: 200 * time.Millisecond, MaxDelay: 10 * time.Second},
		"disabled": {},
	} {
		t.Run(name, func(t *testing.T) {
			adapter, err := NewBedrockAdapter("", "", nil, policy)
			require.NoError(t, err, "NewBedrockAdapter rejected the retry policy")
			require.NotNil(t, adapter, "NewBedrockAdapter returned a nil adapter with no error")
		})
	}
}

// TestNewBedrockAdapter_ReadTimeout pins the idle-read bound on the SDK
// client, which the SDK does not apply to Bedrock Runtime by default.
func TestNewBedrockAdapter_ReadTimeout(t *testing.T) {
	adapter, err := NewBedrockAdapter("", "", nil, RetryPolicy{})
	require.NoError(t, err, "NewBedrockAdapter")

	require.IsType(t, &bedrockruntime.Client{}, adapter.client, "adapter client type")
	client := adapter.client.(*bedrockruntime.Client)

	httpClient := client.Options().HTTPClient
	require.IsType(t, &awshttp.BuildableClient{}, httpClient, "SDK HTTP client type")
	buildable := httpClient.(*awshttp.BuildableClient)

	readTimeout, set := buildable.GetReadTimeout()
	require.True(t, set, "read timeout must be set on the SDK client")
	assert.Equal(t, defaultStreamIdleTimeout, readTimeout, "read timeout")
	assert.Zero(t, buildable.GetTimeout(), "total timeout would cap long ConverseStream turns")
}
