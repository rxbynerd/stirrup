package trace

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
}

func intAttr(t *testing.T, span tracetest.SpanStub, key string) int64 {
	t.Helper()
	for _, attr := range span.Attributes {
		if string(attr.Key) == key {
			return attr.Value.AsInt64()
		}
	}
	t.Errorf("attribute %q not found on span %q", key, span.Name)
	return 0
}

func TestStartChatSpan_NilBeforeStart(t *testing.T) {
	emitter, _ := newTestOTelEmitter()
	if h := emitter.StartChatSpan(context.Background(), ChatSpanStart{Model: "m"}); h != nil {
		t.Error("StartChatSpan before Start returned a handle")
	}
	if h := emitter.StartToolSpan(context.Background(), ToolSpanStart{Name: "read_file", Known: true}); h != nil {
		t.Error("StartToolSpan before Start returned a handle")
	}
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
	if span.SpanKind != oteltrace.SpanKindClient {
		t.Errorf("kind = %v, want client", span.SpanKind)
	}
	root := findSpan(t, exporter, "invoke_agent stirrup")
	if span.Parent.SpanID() != root.SpanContext.SpanID() {
		t.Error("chat span is not a child of the run span")
	}
	assertAttribute(t, span, genAIOperationNameKey, "chat")
	assertAttribute(t, span, genAIProviderNameKey, "aws.bedrock")
	assertAttribute(t, span, genAIRequestModelKey, "claude-haiku-4-5")
	assertAttribute(t, span, serverAddressKey, "bedrock-runtime.us-east-1.amazonaws.com")
	if got := intAttr(t, span, serverPortKey); got != 443 {
		t.Errorf("server.port = %d", got)
	}
	if got := intAttr(t, span, "turn.number"); got != 3 {
		t.Errorf("turn.number = %d", got)
	}
	if got := intAttr(t, span, genAIRequestMaxTokensKey); got != 8192 {
		t.Errorf("max_tokens = %d", got)
	}
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

func TestChatSpan_EndAttributes(t *testing.T) {
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
	for key, want := range map[string]int64{
		genAIUsageInputTokens:           100,
		genAIUsageOutputTokens:          20,
		genAIUsageCacheReadInputTokens:  50,
		genAIUsageReasoningOutputTokens: 7,
		"turn.tool_calls":               3,
		"turn.duration_ms":              1234,
		"turn.retries":                  2,
	} {
		if got := intAttr(t, span, key); got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
	assertAttribute(t, span, genAIResponseModelKey, "m-2026")
	assertAttributesAbsent(t, span, genAIUsageCacheCreationInputTokens, errorTypeKey)
	for _, attr := range span.Attributes {
		if string(attr.Key) == genAIFinishReasonsKey {
			if got := attr.Value.AsStringSlice(); len(got) != 1 || got[0] != "tool_use" {
				t.Errorf("finish_reasons = %v", got)
			}
		}
	}
	if span.Status.Code != codes.Unset {
		t.Errorf("status = %v, want Unset", span.Status.Code)
	}
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
	if span.Status.Code != codes.Error {
		t.Errorf("status = %v, want Error", span.Status.Code)
	}
	if strings.Contains(span.Status.Description, liveSecret) || !strings.Contains(span.Status.Description, "stream reset") {
		t.Errorf("status description = %q, want scrubbed readable text", span.Status.Description)
	}
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
	if got := intAttr(t, span, genAIUsageInputTokens); got != 90 {
		t.Errorf("input tokens = %d, want 90", got)
	}
	if got := intAttr(t, span, genAIUsageOutputTokens); got != 12 {
		t.Errorf("output tokens = %d, want 12", got)
	}
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
	if !ok || !strings.Contains(input, "list files") {
		t.Errorf("input messages = %q, want the request that failed", input)
	}
	if strings.Contains(input, liveSecret) {
		t.Error("input messages leak the secret")
	}
	assertAttributesAbsent(t, span, genAIOutputMessagesKey)
}

func TestChatSpan_ContentCapture(t *testing.T) {
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
	if !ok || !strings.Contains(input, "list files") {
		t.Errorf("input messages = %q", input)
	}
	output, _ := spanAttrString(parent, genAIOutputMessagesKey)
	if !strings.Contains(output, `"finish_reason":"tool_use"`) {
		t.Errorf("output messages = %q, want the summary's finish reason", output)
	}
	if _, ok := spanAttrString(parent, genAISystemInstructionsKey); !ok {
		t.Error("top-level chat span is missing system instructions")
	}
	assertAttributesAbsent(t, findSpan(t, exporter, "chat child"), genAISystemInstructionsKey)
	for _, s := range exporter.GetSpans() {
		for _, attr := range s.Attributes {
			if strings.Contains(attr.Value.String(), liveSecret) {
				t.Errorf("%s: %s leaks the secret", s.Name, attr.Key)
			}
		}
	}

	root := findSpan(t, exporter, "invoke_agent stirrup")
	rootInput, _ := spanAttrString(root, genAIInputMessagesKey)
	if !strings.Contains(rootInput, "list files") {
		t.Errorf("root input = %q, want the top-level run's seed prompt", rootInput)
	}
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

func TestToolSpan_Attributes(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	start := time.Now()
	tool := emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{CallID: "tu-1", Name: "read_file", Known: true, InputSize: 17})
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
	if got := intAttr(t, span, "tool.input_size"); got != 17 {
		t.Errorf("tool.input_size = %d", got)
	}
	if got := intAttr(t, span, "tool.output_size"); got != 40 {
		t.Errorf("tool.output_size = %d", got)
	}
	if !span.EndTime.Equal(endAt) {
		t.Errorf("end time = %v, want %v", span.EndTime, endAt)
	}
	assertAttributesAbsent(t, span, "tool.name", "tool.failure_category", genAIToolCallArgumentsKey, genAIToolCallResultKey)
}

func TestToolSpan_UnknownToolNameBounded(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{CallID: "tu-x", Name: "model_invented_name"}).End(ToolSpanEnd{
		Summary: &types.ToolCallTrace{Name: "model_invented_name", ErrorCategory: "unknown_tool"},
	})
	finishLiveRun(t, emitter)
	span := findSpan(t, exporter, "execute_tool")
	assertAttribute(t, span, genAIToolNameKey, "model_invented_name")
	assertAttribute(t, span, errorTypeKey, "unknown_tool")
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
	if span.Status.Code != codes.Error || strings.Contains(span.Status.Description, liveSecret) || !strings.Contains(span.Status.Description, "exit 1") {
		t.Errorf("status = %v %q, want scrubbed Error", span.Status.Code, span.Status.Description)
	}
	bare := findSpan(t, exporter, "execute_tool write_file")
	if bare.Status.Description != "tool call failed" {
		t.Errorf("status description = %q, want the literal fallback", bare.Status.Description)
	}
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
	if !ok || strings.Contains(args, liveSecret) || !json.Valid([]byte(args)) {
		t.Errorf("arguments = %q, want scrubbed JSON", args)
	}
	result, ok := spanAttrString(span, genAIToolCallResultKey)
	if !ok || strings.Contains(result, liveSecret) || !strings.Contains(result, "contents") {
		t.Errorf("result = %q, want scrubbed text", result)
	}
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

	if n := len(exporter.GetSpans()); n != 3 {
		t.Fatalf("exported %d spans, want 3", n)
	}
	assertAttributesAbsent(t, findSpan(t, exporter, "chat m"), errorTypeKey)
}

func TestFinish_EndsAbandonedHandles(t *testing.T) {
	emitter, exporter := startLiveRun(t, false)
	emitter.StartChatSpan(emitter.RootContext(), ChatSpanStart{Model: "m"})
	emitter.StartToolSpan(emitter.RootContext(), ToolSpanStart{Name: "read_file", Known: true})
	finishLiveRun(t, emitter)

	for _, name := range []string{"chat m", "execute_tool read_file"} {
		span := findSpan(t, exporter, name)
		found := false
		for _, attr := range span.Attributes {
			if string(attr.Key) == spanAbandonedKey && attr.Value.AsBool() {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: missing %s=true", name, spanAbandonedKey)
		}
		if span.Status.Code != codes.Error {
			t.Errorf("%s: status = %v, want Error", name, span.Status.Code)
		}
	}
}
