package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/rxbynerd/stirrup/types"
)

// ---------------------------------------------------------------------------
// Mock infrastructure
// ---------------------------------------------------------------------------

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

// mockConverseStreamOutput wraps a mockEventReader to look like the SDK's
// ConverseStreamOutput.GetStream() return value. We need this because
// BedrockAdapter.Stream calls output.GetStream() which returns a
// *bedrockruntime.ConverseStreamEventStream. Instead of fighting that, our
// tests call consumeBedrockStream directly with a mockEventReader.

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
	// We cannot easily construct a real ConverseStreamOutput with an injected
	// event stream. Instead, tests that need to exercise the full Stream()
	// method will use consumeBedrockStream directly. This mock is used for
	// testing the buildConverseStreamInput translation and API error handling.
	return nil, fmt.Errorf("mock: use consumeBedrockStream directly for stream tests")
}

// ---------------------------------------------------------------------------
// Stream consumption tests
// ---------------------------------------------------------------------------

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

	if len(result) != 3 {
		t.Fatalf("expected 3 events, got %d: %+v", len(result), result)
	}
	if result[0].Type != "text_delta" || result[0].Text != "Hello" {
		t.Errorf("event[0] = %+v, want text_delta/Hello", result[0])
	}
	if result[1].Type != "text_delta" || result[1].Text != " world" {
		t.Errorf("event[1] = %+v, want text_delta/ world", result[1])
	}
	if result[2].Type != "message_complete" || result[2].StopReason != "end_turn" {
		t.Errorf("event[2] = %+v, want message_complete/end_turn", result[2])
	}
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

	if len(result) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(result), result)
	}

	tc := result[0]
	if tc.Type != "tool_call" {
		t.Fatalf("event[0].Type = %q, want tool_call", tc.Type)
	}
	if tc.ID != "toolu_abc" {
		t.Errorf("event[0].ID = %q, want toolu_abc", tc.ID)
	}
	if tc.Name != "read_file" {
		t.Errorf("event[0].Name = %q, want read_file", tc.Name)
	}
	if tc.Input["path"] != "main.go" {
		t.Errorf("event[0].Input[path] = %v, want main.go", tc.Input["path"])
	}

	if result[1].Type != "message_complete" || result[1].StopReason != "tool_use" {
		t.Errorf("event[1] = %+v, want message_complete/tool_use", result[1])
	}
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

	// Expect: text_delta, tool_call, message_complete.
	if len(result) != 3 {
		t.Fatalf("expected 3 events, got %d: %+v", len(result), result)
	}
	if result[0].Type != "text_delta" {
		t.Errorf("event[0].Type = %q, want text_delta", result[0].Type)
	}
	if result[1].Type != "tool_call" || result[1].Name != "read_file" {
		t.Errorf("event[1] = %+v, want tool_call/read_file", result[1])
	}
	if result[2].Type != "message_complete" {
		t.Errorf("event[2].Type = %q, want message_complete", result[2].Type)
	}
}

func TestBedrock_StopReasonMapping(t *testing.T) {
	tests := []struct {
		reason brtypes.StopReason
		want   string
	}{
		{brtypes.StopReasonEndTurn, "end_turn"},
		{brtypes.StopReasonToolUse, "tool_use"},
		{brtypes.StopReasonMaxTokens, "max_tokens"},
		{brtypes.StopReasonStopSequence, "stop_sequence"},
		{brtypes.StopReason("unknown_reason"), "unknown_reason"},
	}
	for _, tt := range tests {
		t.Run(string(tt.reason), func(t *testing.T) {
			got := mapStopReason(tt.reason)
			if got != tt.want {
				t.Errorf("mapStopReason(%q) = %q, want %q", tt.reason, got, tt.want)
			}
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

	if len(result) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(result), result)
	}
	if result[0].StopReason != "end_turn" {
		t.Errorf("event[0].StopReason = %q, want end_turn", result[0].StopReason)
	}
	if result[1].OutputTokens != 150 {
		t.Errorf("event[1].OutputTokens = %d, want 150", result[1].OutputTokens)
	}
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

	// Expect: text_delta("partial"), error.
	if len(result) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(result), result)
	}
	if result[0].Type != "text_delta" {
		t.Errorf("event[0].Type = %q, want text_delta", result[0].Type)
	}
	if result[1].Type != "error" || result[1].Error == nil {
		t.Fatalf("event[1] = %+v, want error event", result[1])
	}
	if !strings.Contains(result[1].Error.Error(), "connection reset") {
		t.Errorf("error = %q, want to contain 'connection reset'", result[1].Error)
	}
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

	// Should get the first text_delta and then an error from cancellation.
	foundError := false
	for _, ev := range result {
		if ev.Type == "error" && ev.Error != nil {
			foundError = true
		}
	}
	if !foundError {
		t.Error("expected an error event from context cancellation")
	}
}

func TestBedrock_MalformedToolInputJSON(t *testing.T) {
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
			Value: brtypes.MessageStopEvent{StopReason: brtypes.StopReasonEndTurn},
		},
	}

	ch := make(chan types.StreamEvent, 64)
	reader := newMockEventReader(events, nil)
	go consumeBedrockStream(context.Background(), reader, ch)
	result := collectEvents(t, ch)

	foundError := false
	for _, ev := range result {
		if ev.Type == "error" && ev.Error != nil && strings.Contains(ev.Error.Error(), "tool input JSON") {
			foundError = true
		}
	}
	if !foundError {
		t.Error("expected an error event for malformed tool input JSON")
	}
}

// ---------------------------------------------------------------------------
// Message/tool translation tests
// ---------------------------------------------------------------------------

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
	if err != nil {
		t.Fatalf("bedrockTranslateMessages() error: %v", err)
	}

	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result))
	}

	// First message: user text.
	if result[0].Role != brtypes.ConversationRoleUser {
		t.Errorf("msg[0].Role = %q, want user", result[0].Role)
	}
	if len(result[0].Content) != 1 {
		t.Fatalf("msg[0] has %d content blocks, want 1", len(result[0].Content))
	}
	if textBlock, ok := result[0].Content[0].(*brtypes.ContentBlockMemberText); !ok {
		t.Errorf("msg[0].Content[0] type = %T, want ContentBlockMemberText", result[0].Content[0])
	} else if textBlock.Value != "Hello" {
		t.Errorf("msg[0].Content[0].Value = %q, want Hello", textBlock.Value)
	}

	// Second message: assistant with text + tool_use.
	if result[1].Role != brtypes.ConversationRoleAssistant {
		t.Errorf("msg[1].Role = %q, want assistant", result[1].Role)
	}
	if len(result[1].Content) != 2 {
		t.Fatalf("msg[1] has %d content blocks, want 2", len(result[1].Content))
	}
	if toolBlock, ok := result[1].Content[1].(*brtypes.ContentBlockMemberToolUse); !ok {
		t.Errorf("msg[1].Content[1] type = %T, want ContentBlockMemberToolUse", result[1].Content[1])
	} else {
		if aws.ToString(toolBlock.Value.Name) != "read_file" {
			t.Errorf("tool_use.Name = %q, want read_file", aws.ToString(toolBlock.Value.Name))
		}
		if aws.ToString(toolBlock.Value.ToolUseId) != "toolu_1" {
			t.Errorf("tool_use.ToolUseId = %q, want toolu_1", aws.ToString(toolBlock.Value.ToolUseId))
		}
	}

	// Third message: user with tool_result.
	if len(result[2].Content) != 1 {
		t.Fatalf("msg[2] has %d content blocks, want 1", len(result[2].Content))
	}
	if trBlock, ok := result[2].Content[0].(*brtypes.ContentBlockMemberToolResult); !ok {
		t.Errorf("msg[2].Content[0] type = %T, want ContentBlockMemberToolResult", result[2].Content[0])
	} else {
		if aws.ToString(trBlock.Value.ToolUseId) != "toolu_1" {
			t.Errorf("tool_result.ToolUseId = %q, want toolu_1", aws.ToString(trBlock.Value.ToolUseId))
		}
	}
}

func TestBedrock_TranslateMessages_BadRole(t *testing.T) {
	msgs := []types.Message{
		{Role: "system", Content: []types.ContentBlock{{Type: "text", Text: "nope"}}},
	}
	_, err := bedrockTranslateMessages(msgs)
	if err == nil {
		t.Fatal("expected error for unsupported role, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported message role") {
		t.Errorf("error = %q, want to contain 'unsupported message role'", err)
	}
}

func TestBedrock_TranslateTools(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
	tools := []types.ToolDefinition{
		{Name: "read_file", Description: "Read a file", InputSchema: schema},
	}

	result, err := bedrockTranslateTools(tools)
	if err != nil {
		t.Fatalf("bedrockTranslateTools() error: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(result))
	}
	toolSpec, ok := result[0].(*brtypes.ToolMemberToolSpec)
	if !ok {
		t.Fatalf("tool type = %T, want ToolMemberToolSpec", result[0])
	}
	if aws.ToString(toolSpec.Value.Name) != "read_file" {
		t.Errorf("tool.Name = %q, want read_file", aws.ToString(toolSpec.Value.Name))
	}
	if aws.ToString(toolSpec.Value.Description) != "Read a file" {
		t.Errorf("tool.Description = %q, want 'Read a file'", aws.ToString(toolSpec.Value.Description))
	}
}

func TestBedrock_TranslateToolResult_Error(t *testing.T) {
	msgs := []types.Message{
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "tool_result", ToolUseID: "toolu_err", Content: "something failed", IsError: true},
			},
		},
	}

	result, err := bedrockTranslateMessages(msgs)
	if err != nil {
		t.Fatalf("bedrockTranslateMessages() error: %v", err)
	}

	trBlock, ok := result[0].Content[0].(*brtypes.ContentBlockMemberToolResult)
	if !ok {
		t.Fatalf("content type = %T, want ContentBlockMemberToolResult", result[0].Content[0])
	}
	if trBlock.Value.Status != brtypes.ToolResultStatusError {
		t.Errorf("tool_result.Status = %q, want error", trBlock.Value.Status)
	}
}

func TestBedrock_BuildConverseStreamInput(t *testing.T) {
	params := types.StreamParams{
		Model:       "anthropic.claude-sonnet-4-6-v1",
		System:      "You are helpful.",
		MaxTokens:   4096,
		Temperature: 0.1,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "Hello"}}},
		},
		Tools: []types.ToolDefinition{
			{Name: "read_file", Description: "Read", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}

	input, err := buildConverseStreamInput(params)
	if err != nil {
		t.Fatalf("buildConverseStreamInput() error: %v", err)
	}

	if aws.ToString(input.ModelId) != "anthropic.claude-sonnet-4-6-v1" {
		t.Errorf("ModelId = %q, want anthropic.claude-sonnet-4-6-v1", aws.ToString(input.ModelId))
	}
	if len(input.System) != 1 {
		t.Fatalf("System has %d blocks, want 1", len(input.System))
	}
	if len(input.Messages) != 1 {
		t.Fatalf("Messages has %d entries, want 1", len(input.Messages))
	}
	if input.InferenceConfig == nil {
		t.Fatal("InferenceConfig is nil")
	}
	if input.InferenceConfig.MaxTokens == nil || *input.InferenceConfig.MaxTokens != 4096 {
		t.Errorf("MaxTokens = %v, want 4096", input.InferenceConfig.MaxTokens)
	}
	if input.InferenceConfig.Temperature == nil || *input.InferenceConfig.Temperature != 0.1 {
		t.Errorf("Temperature = %v, want 0.1", input.InferenceConfig.Temperature)
	}
	if input.ToolConfig == nil || len(input.ToolConfig.Tools) != 1 {
		t.Errorf("ToolConfig has %d tools, want 1", len(input.ToolConfig.Tools))
	}
}

func TestBedrock_BuildConverseStreamInput_NoSystem(t *testing.T) {
	params := types.StreamParams{
		Model:     "anthropic.claude-sonnet-4-6-v1",
		MaxTokens: 1024,
	}
	input, err := buildConverseStreamInput(params)
	if err != nil {
		t.Fatalf("buildConverseStreamInput() error: %v", err)
	}
	if input.System != nil {
		t.Errorf("System = %v, want nil for empty system prompt", input.System)
	}
}

func TestBedrock_APIError(t *testing.T) {
	client := &mockBedrockClient{apiErr: fmt.Errorf("access denied")}
	adapter := &BedrockAdapter{client: client}

	_, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "anthropic.claude-sonnet-4-6-v1",
		MaxTokens: 1024,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("error = %q, want to contain 'access denied'", err)
	}
}
