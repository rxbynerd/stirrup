package trace

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/types"
)

const liveSecret = "sk-ant-api03-livespansecret"

func startLiveRun(t *testing.T, capture bool) (*OTelTraceEmitter, *tracetest.InMemoryExporter) {
	t.Helper()
	var emitter *OTelTraceEmitter
	var exporter *tracetest.InMemoryExporter
	if capture {
		emitter, exporter = newTestOTelEmitterWithCapture()
	} else {
		emitter, exporter = newTestOTelEmitter()
	}
	emitter.Start("run-live", &types.RunConfig{
		RunID:       "run-live",
		Mode:        "execution",
		Provider:    types.ProviderConfig{Type: "anthropic"},
		ModelRouter: types.ModelRouterConfig{Model: "claude-sonnet-4-6"},
	})
	return emitter, exporter
}

func finishLiveRun(t *testing.T, emitter *OTelTraceEmitter) {
	t.Helper()
	finishLiveRunWithOutcome(t, emitter, "success")
}

func finishLiveRunWithOutcome(t *testing.T, emitter *OTelTraceEmitter, outcome string) {
	t.Helper()
	_, err := emitter.Finish(context.Background(), outcome)
	require.NoError(t, err)
}

// lookupAttr returns the value of key on span and whether the span carries it.
func lookupAttr(span tracetest.SpanStub, key string) (attribute.Value, bool) {
	for _, attr := range span.Attributes {
		if string(attr.Key) == key {
			return attr.Value, true
		}
	}
	return attribute.Value{}, false
}

func intAttr(t *testing.T, span tracetest.SpanStub, key string) int64 {
	t.Helper()
	v, ok := lookupAttr(span, key)
	require.True(t, ok, "attribute %q not found on span %q", key, span.Name)
	return v.AsInt64()
}

func TestStartChatSpan_NilBeforeStart(t *testing.T) {
	emitter, _ := newTestOTelEmitter()
	require.Nil(t, emitter.StartChatSpan(context.Background(), ChatSpanStart{Model: "m"}),
		"StartChatSpan before Start returned a handle")
	require.Nil(t, emitter.StartToolSpan(context.Background(), ToolSpanStart{Name: "read_file", Known: true}),
		"StartToolSpan before Start returned a handle")
}

func TestChatSpan_StartAttributesAndKind(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	temp := 0.2
	chat := emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{
		Turn: 3, ProviderType: "bedrock", Model: "claude-haiku-4-5",
		ServerAddress: "bedrock-runtime.us-east-1.amazonaws.com", ServerPort: 443,
		MaxTokens: 8192, Temperature: &temp,
	})
	chat.End(ChatSpanEnd{Summary: &types.TurnTrace{Turn: 3, StopReason: "end_turn"}})
	finishLiveRun(t, emitter)

	span := findSpan(t, exporter, "chat claude-haiku-4-5")
	require.Equal(t, oteltrace.SpanKindClient, span.SpanKind)
	root := findSpan(t, exporter, "invoke_agent stirrup")
	require.Equal(t, root.SpanContext.SpanID(), span.Parent.SpanID(), "chat span should be a child of the run span")
	assertAttribute(t, span, genAIOperationNameKey, "chat")
	assertAttribute(t, span, genAIProviderNameKey, "aws.bedrock")
	assertAttribute(t, span, genAIRequestModelKey, "claude-haiku-4-5")
	assertAttribute(t, span, serverAddressKey, "bedrock-runtime.us-east-1.amazonaws.com")
	require.Equal(t, int64(443), intAttr(t, span, serverPortKey))
	require.Equal(t, int64(3), intAttr(t, span, "turn.number"))
	require.Equal(t, int64(8192), intAttr(t, span, genAIRequestMaxTokensKey))
}

func TestChatSpan_NameFallsBackToRunModel(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{}).End(ChatSpanEnd{})
	finishLiveRun(t, emitter)
	span := findSpan(t, exporter, "chat claude-sonnet-4-6")
	assertAttribute(t, span, genAIProviderNameKey, "anthropic")

	bare, bareExporter := newTestOTelEmitter()
	bare.Start("run-bare", nil)
	bare.StartChatSpan(bare.RootContext(), ChatSpanStart{}).End(ChatSpanEnd{})
	finishLiveRun(t, bare)
	assertAttributesAbsent(t, findSpan(t, bareExporter, "chat"), genAIRequestModelKey, genAIProviderNameKey)
}

func TestChatSpan_EndRecordsUsageAndFinishReason(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Model: "m"}).End(ChatSpanEnd{
		Summary: &types.TurnTrace{
			Turn:          0,
			Tokens:        types.TokenUsage{Input: 100, Output: 20, CacheRead: 50, Reasoning: 7},
			InputReported: true,
			StopReason:    "tool_use",
			DurationMs:    1234,
			ResponseModel: "m-2026",
			Retries:       2,
			ToolCalls:     3,
		},
	})
	finishLiveRun(t, emitter)

	span := findSpan(t, exporter, "chat m")
	for _, tc := range []struct {
		name string
		key  string
		want int64
	}{
		{"input tokens", genAIUsageInputTokens, 100},
		{"output tokens", genAIUsageOutputTokens, 20},
		{"cache read tokens", genAIUsageCacheReadInputTokens, 50},
		{"reasoning tokens", genAIUsageReasoningOutputTokens, 7},
		{"tool calls", "turn.tool_calls", 3},
		{"duration", "turn.duration_ms", 1234},
		{"retries", "turn.retries", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, intAttr(t, span, tc.key))
		})
	}
	assertAttribute(t, span, genAIResponseModelKey, "m-2026")
	assertAttributesAbsent(t, span, genAIUsageCacheCreationInputTokens, errorTypeKey)
	reasons, ok := lookupAttr(span, genAIFinishReasonsKey)
	require.True(t, ok, "finish_reasons missing")
	require.Equal(t, []string{"tool_use"}, reasons.AsStringSlice())
	require.Equal(t, codes.Unset, span.Status.Code)
}

// TestChatSpan_OutputLimitRecoveryAttribute pins that only a turn the
// loop continued past an output-limit stop carries the recovery attribute.
func TestChatSpan_OutputLimitRecoveryAttribute(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Model: "a"}).End(ChatSpanEnd{
		Summary: &types.TurnTrace{StopReason: "max_tokens", OutputLimitRecovery: true},
	})
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Model: "b"}).End(ChatSpanEnd{
		Summary: &types.TurnTrace{StopReason: "end_turn"},
	})
	finishLiveRun(t, emitter)

	recovered, ok := lookupAttr(findSpan(t, exporter, "chat a"), "turn.output_limit_recovery")
	require.True(t, ok && recovered.AsBool(), "turn.output_limit_recovery should be true on the recovered turn")
	assertAttributesAbsent(t, findSpan(t, exporter, "chat b"), "turn.output_limit_recovery")
}

// TestChatSpan_WindDownAttribute pins that only the turn that carried the
// wind-down instruction names its trigger on the chat span.
func TestChatSpan_WindDownAttribute(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Model: "a"}).End(ChatSpanEnd{
		Summary: &types.TurnTrace{StopReason: "tool_use", WindDown: "tokens"},
	})
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Model: "b"}).End(ChatSpanEnd{
		Summary: &types.TurnTrace{StopReason: "end_turn"},
	})
	finishLiveRun(t, emitter)

	assertAttribute(t, findSpan(t, exporter, "chat a"), "turn.wind_down", "tokens")
	assertAttributesAbsent(t, findSpan(t, exporter, "chat b"), "turn.wind_down")
}

func TestChatSpan_ErrorScrubbed(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Model: "m"}).End(ChatSpanEnd{
		Summary:   &types.TurnTrace{StopReason: "error"},
		ErrorType: "provider_stream_failed",
		ErrorText: "stream reset, key " + liveSecret,
	})
	finishLiveRun(t, emitter)

	span := findSpan(t, exporter, "chat m")
	assertAttribute(t, span, errorTypeKey, "provider_stream_failed")
	require.Equal(t, codes.Error, span.Status.Code)
	require.NotContains(t, span.Status.Description, liveSecret, "status description leaks the secret")
	require.Contains(t, span.Status.Description, "stream reset")
}

func TestChatSpan_FailedRequestWithoutUsage(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Model: "m"}).End(ChatSpanEnd{
		Summary:   &types.TurnTrace{StopReason: "error", DurationMs: 40},
		ErrorType: "provider_request_failed",
		ErrorText: "connection refused",
	})
	finishLiveRun(t, emitter)

	span := findSpan(t, exporter, "chat m")
	assertAttribute(t, span, errorTypeKey, "provider_request_failed")
	assertAttributesAbsent(t, span, genAIUsageInputTokens, genAIUsageOutputTokens, genAIFinishReasonsKey)
}

func TestChatSpan_FailedRequestKeepsDiscardedUsage(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Model: "m"}).End(ChatSpanEnd{
		Summary:   &types.TurnTrace{StopReason: "error", Tokens: types.TokenUsage{Input: 90, Output: 12}},
		ErrorType: "provider_stream_failed",
	})
	finishLiveRun(t, emitter)

	span := findSpan(t, exporter, "chat m")
	require.Equal(t, int64(90), intAttr(t, span, genAIUsageInputTokens))
	require.Equal(t, int64(12), intAttr(t, span, genAIUsageOutputTokens))
	assertAttributesAbsent(t, span, genAIFinishReasonsKey)
}

func TestChatSpan_FailedRequestCarriesScrubbedInputOnly(t *testing.T) {
	emitter, exporter := startLiveRun(t, true)
	record := captureTurnRecord()
	record.ModelInput.Messages[0].Content[0].Text = "list files, key " + liveSecret
	record.ModelOutput = nil
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Model: "m"}).End(ChatSpanEnd{
		Summary:   &types.TurnTrace{StopReason: "error"},
		Record:    &record,
		ErrorType: "provider_request_failed",
	})
	finishLiveRun(t, emitter)

	span := findSpan(t, exporter, "chat m")
	input, ok := spanAttrString(span, genAIInputMessagesKey)
	require.True(t, ok, "input messages missing")
	require.Contains(t, input, "list files", "input messages should hold the request that failed")
	require.NotContains(t, input, liveSecret, "input messages leak the secret")
	assertAttributesAbsent(t, span, genAIOutputMessagesKey)
}

func TestChatSpan_ContentCaptureRecordsScrubbedMessages(t *testing.T) {
	emitter, exporter := startLiveRun(t, true)
	emitter.RecordSystemInstructions("system prompt with " + liveSecret)
	record := captureTurnRecord()
	record.ModelInput.Messages[0].Content[0].Text = "list files, key " + liveSecret
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Model: "m"}).End(ChatSpanEnd{
		Summary: &types.TurnTrace{StopReason: "tool_use"},
		Record:  &record,
	})
	child := captureTurnRecord()
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Model: "child", RunID: "run-child"}).End(ChatSpanEnd{
		Summary: &types.TurnTrace{StopReason: "end_turn"},
		Record:  &child,
	})
	finishLiveRun(t, emitter)

	parent := findSpan(t, exporter, "chat m")
	input, ok := spanAttrString(parent, genAIInputMessagesKey)
	require.True(t, ok, "input messages missing")
	require.Contains(t, input, "list files")
	output, _ := spanAttrString(parent, genAIOutputMessagesKey)
	require.Contains(t, output, `"finish_reason":"tool_use"`, "output messages should carry the summary's finish reason")
	_, ok = spanAttrString(parent, genAISystemInstructionsKey)
	require.True(t, ok, "top-level chat span is missing system instructions")
	assertAttributesAbsent(t, findSpan(t, exporter, "chat child"), genAISystemInstructionsKey)
	for _, s := range exporter.GetSpans() {
		for _, attr := range s.Attributes {
			require.NotContains(t, attr.Value.String(), liveSecret, "%s: %s leaks the secret", s.Name, attr.Key)
		}
	}

	root := findSpan(t, exporter, "invoke_agent stirrup")
	rootInput, _ := spanAttrString(root, genAIInputMessagesKey)
	require.Contains(t, rootInput, "list files", "root input should hold the top-level run's seed prompt")
}

func TestChatSpan_NoContentWithoutCapture(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	emitter.RecordSystemInstructions("system")
	record := captureTurnRecord()
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Model: "m"}).End(ChatSpanEnd{
		Summary: &types.TurnTrace{StopReason: "end_turn"},
		Record:  &record,
	})
	finishLiveRun(t, emitter)
	for _, s := range exporter.GetSpans() {
		assertAttributesAbsent(t, s, genAIInputMessagesKey, genAIOutputMessagesKey, genAISystemInstructionsKey)
	}
}

func TestToolSpan_RecordsAttributesAndEndTime(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	start := time.Now()
	tool := emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{CallID: "tu-1", Name: "read_file", Known: true, InputSize: 17, Turn: 2})
	endAt := start.Add(5 * time.Millisecond)
	tool.End(ToolSpanEnd{
		Summary: &types.ToolCallTrace{ID: "tu-1", Name: "read_file", Success: true, DurationMs: 5, OutputSize: 40},
		EndTime: endAt,
	})
	finishLiveRun(t, emitter)

	span := findSpan(t, exporter, "execute_tool read_file")
	assertAttribute(t, span, genAIOperationNameKey, "execute_tool")
	assertAttribute(t, span, genAIToolNameKey, "read_file")
	assertAttribute(t, span, genAIToolCallIDKey, "tu-1")
	assertAttribute(t, span, genAIToolTypeKey, "function")
	require.Equal(t, int64(17), intAttr(t, span, "tool.input_size"))
	require.Equal(t, int64(2), intAttr(t, span, "turn.number"))
	require.Equal(t, int64(40), intAttr(t, span, "tool.output_size"))
	require.True(t, span.EndTime.Equal(endAt), "end time = %v, want %v", span.EndTime, endAt)
	assertAttributesAbsent(t, span, "tool.name", "tool.failure_category", "tool.queue_ms", genAIToolCallArgumentsKey, genAIToolCallResultKey)
}

func TestToolSpan_RecordsQueueTime(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{Name: "control_plane_tool", Known: true}).End(ToolSpanEnd{
		Summary: &types.ToolCallTrace{Name: "control_plane_tool", Success: true, DurationMs: 5},
		Queued:  42 * time.Millisecond,
	})
	finishLiveRun(t, emitter)
	span := findSpan(t, exporter, "execute_tool control_plane_tool")
	require.Equal(t, int64(42), intAttr(t, span, "tool.queue_ms"))
}

func TestToolSpan_ShortUnknownToolNameRecorded(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{CallID: "tu-x", Name: "model_invented_name"}).End(ToolSpanEnd{
		Summary: &types.ToolCallTrace{Name: "model_invented_name", ErrorCategory: "unknown_tool"},
	})
	finishLiveRun(t, emitter)
	span := findSpan(t, exporter, "execute_tool")
	assertAttribute(t, span, genAIToolNameKey, "model_invented_name")
	assertAttribute(t, span, errorTypeKey, "unknown_tool")
}

func TestToolSpan_UnknownToolNameScrubbedAndCut(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	name := liveSecret + strings.Repeat("é", 200)
	emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{Name: name}).End(ToolSpanEnd{
		Summary: &types.ToolCallTrace{Name: name, ErrorCategory: "unknown_tool"},
	})
	finishLiveRun(t, emitter)
	span := findSpan(t, exporter, "execute_tool")
	got, ok := spanAttrString(span, genAIToolNameKey)
	require.True(t, ok, "gen_ai.tool.name missing")
	require.NotContains(t, got, liveSecret, "gen_ai.tool.name carries the secret")
	require.LessOrEqual(t, len(got), maxUnknownToolNameBytes)
	require.True(t, utf8.ValidString(got), "gen_ai.tool.name is not valid UTF-8")
}

func TestToolSpan_FailureScrubbed(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{Name: "run_command", Known: true}).End(ToolSpanEnd{
		Summary: &types.ToolCallTrace{Name: "run_command", ErrorCategory: "handler_error", ErrorReason: "exit 1: " + liveSecret},
	})
	emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{Name: "write_file", Known: true}).End(ToolSpanEnd{
		Summary: &types.ToolCallTrace{Name: "write_file"},
	})
	finishLiveRun(t, emitter)

	span := findSpan(t, exporter, "execute_tool run_command")
	assertAttribute(t, span, errorTypeKey, "handler_error")
	require.Equal(t, codes.Error, span.Status.Code)
	require.NotContains(t, span.Status.Description, liveSecret, "status description leaks the secret")
	require.Contains(t, span.Status.Description, "exit 1")
	bare := findSpan(t, exporter, "execute_tool write_file")
	require.Equal(t, "tool call failed", bare.Status.Description, "status description should be the literal fallback")
}

func TestToolSpan_ContentCaptureScrubbed(t *testing.T) {
	emitter, exporter := startLiveRun(t, true)
	emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{CallID: "tu-1", Name: "read_file", Known: true}).End(ToolSpanEnd{
		Summary: &types.ToolCallTrace{Name: "read_file", Success: true},
		Record: &types.ToolCallRecord{
			ID:     "tu-1",
			Name:   "read_file",
			Input:  json.RawMessage(`{"path":"` + liveSecret + `"}`),
			Output: "contents " + liveSecret,
		},
	})
	finishLiveRun(t, emitter)

	span := findSpan(t, exporter, "execute_tool read_file")
	args, ok := spanAttrString(span, genAIToolCallArgumentsKey)
	require.True(t, ok, "tool call arguments missing")
	require.NotContains(t, args, liveSecret, "arguments leak the secret")
	require.True(t, json.Valid([]byte(args)), "arguments = %q, want JSON", args)
	result, ok := spanAttrString(span, genAIToolCallResultKey)
	require.True(t, ok, "tool call result missing")
	require.NotContains(t, result, liveSecret, "result leaks the secret")
	require.Contains(t, result, "contents")
}

func TestLiveSpan_EndIsIdempotent(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	chat := emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Model: "m"})
	chat.End(ChatSpanEnd{Summary: &types.TurnTrace{StopReason: "end_turn"}})
	chat.End(ChatSpanEnd{ErrorType: "cancelled"})
	tool := emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{Name: "read_file", Known: true})
	tool.End(ToolSpanEnd{Summary: &types.ToolCallTrace{Success: true}})
	tool.End(ToolSpanEnd{})
	finishLiveRun(t, emitter)

	require.Len(t, exporter.GetSpans(), 3)
	assertAttributesAbsent(t, findSpan(t, exporter, "chat m"), errorTypeKey)
}

func TestFinish_EndsAbandonedHandles(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Model: "m"})
	emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{Name: "read_file", Known: true})
	finishLiveRun(t, emitter)

	for _, name := range []string{"chat m", "execute_tool read_file"} {
		t.Run(name, func(t *testing.T) {
			span := findSpan(t, exporter, name)
			abandoned, ok := lookupAttr(span, spanAbandonedKey)
			require.True(t, ok && abandoned.AsBool(), "missing %s=true", spanAbandonedKey)
			require.Equal(t, codes.Error, span.Status.Code)
		})
	}
}

func failChat(emitter *OTelTraceEmitter, errorType string) {
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Model: "m"}).End(ChatSpanEnd{
		Summary:   &types.TurnTrace{StopReason: "error"},
		ErrorType: errorType,
	})
}

func TestRootSpan_FailedRunNamesFailingRequestAndReason(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	failChat(emitter, "provider_request_failed")
	emitter.RecordRunError("provider anthropic (m): connection refused, key " + liveSecret)
	finishLiveRunWithOutcome(t, emitter, "error")

	root := findSpan(t, exporter, "invoke_agent stirrup")
	assertAttribute(t, root, errorTypeKey, "provider_request_failed")
	assertAttribute(t, root, "run.outcome", "error")
	require.Equal(t, codes.Error, root.Status.Code)
	require.NotContains(t, root.Status.Description, liveSecret, "status description leaks the secret")
	require.Contains(t, root.Status.Description, "connection refused")
}

func TestRootSpan_ErrorTypeIgnoresEarlierRecoveredAndSubAgentFailures(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	failChat(emitter, "context_overflow")
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Model: "m"}).End(ChatSpanEnd{
		Summary: &types.TurnTrace{StopReason: "end_turn"},
	})
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Model: "m", RunID: "run-child"}).End(ChatSpanEnd{
		Summary:   &types.TurnTrace{StopReason: "error"},
		ErrorType: "provider_stream_failed",
	})
	finishLiveRunWithOutcome(t, emitter, "error")

	root := findSpan(t, exporter, "invoke_agent stirrup")
	assertAttribute(t, root, errorTypeKey, "error")
	require.Equal(t, "error", root.Status.Description, "status description should be the outcome when no run error was recorded")
}

func TestRootSpan_OtherOutcomesKeepOutcomeAsErrorType(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	failChat(emitter, "provider_stream_failed")
	emitter.RecordRunError("reached the turn limit (maxTurns 5)")
	finishLiveRunWithOutcome(t, emitter, "max_turns")

	root := findSpan(t, exporter, "invoke_agent stirrup")
	assertAttribute(t, root, errorTypeKey, "max_turns")
	require.Equal(t, "reached the turn limit (maxTurns 5)", root.Status.Description)
}
