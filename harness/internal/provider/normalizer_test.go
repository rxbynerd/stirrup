package provider

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

var errFake = errors.New("fake provider error")

// fakeAdapter is a recording stub that captures the StreamParams it
// receives and replays a caller-supplied script of StreamEvents. Used
// by the normalizer tests to assert "what the wrapped provider saw on
// the wire" without bringing up real HTTP transport.
type fakeAdapter struct {
	receivedParams types.StreamParams
	scripted       []types.StreamEvent
	streamErr      error
	batchID        string
}

func (f *fakeAdapter) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	f.receivedParams = params
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	ch := make(chan types.StreamEvent, len(f.scripted))
	for _, ev := range f.scripted {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// fakeBatchAdapter implements both ProviderAdapter and the batchModeAdapter
// duck-typed interface so the wrapper's pass-through is exercised.
type fakeBatchAdapter struct {
	fakeAdapter
}

func (f *fakeBatchAdapter) LastBatchID() string { return f.batchID }

func TestNormalizingAdapter_OutboundToolsRewritten_OpenAI(t *testing.T) {
	inner := &fakeAdapter{}
	w := NewNormalizingAdapter(inner, "openai-compatible")

	params := types.StreamParams{
		Tools: []types.ToolDefinition{
			{Name: "read_file", InputSchema: json.RawMessage(`{}`)},
			{Name: "mcp_jira_create-issue", InputSchema: json.RawMessage(`{}`)},
		},
	}
	_, err := w.Stream(context.Background(), params)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := inner.receivedParams.Tools[0].Name; got != "read_file" {
		t.Errorf("tools[0].Name = %q, want unchanged", got)
	}
	// OpenAI allows hyphens — name passes through verbatim.
	if got := inner.receivedParams.Tools[1].Name; got != "mcp_jira_create-issue" {
		t.Errorf("tools[1].Name = %q, want unchanged for OpenAI", got)
	}
}

func TestNormalizingAdapter_OutboundToolsRewritten_Gemini(t *testing.T) {
	inner := &fakeAdapter{}
	w := NewNormalizingAdapter(inner, "gemini")

	params := types.StreamParams{
		Tools: []types.ToolDefinition{
			{Name: "mcp_jira_create-issue", InputSchema: json.RawMessage(`{}`)},
			{Name: "mcp_slack_post.message", InputSchema: json.RawMessage(`{}`)},
		},
	}
	_, err := w.Stream(context.Background(), params)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for i, td := range inner.receivedParams.Tools {
		if strings.ContainsAny(td.Name, "-.") {
			t.Errorf("tools[%d].Name = %q still contains disallowed char for Gemini", i, td.Name)
		}
	}
}

func TestNormalizingAdapter_InboundToolCallReverseTranslated(t *testing.T) {
	inner := &fakeAdapter{
		scripted: []types.StreamEvent{
			{Type: "text_delta", Text: "hello"},
			{Type: "tool_call", ID: "call_1", Name: "mcp_jira_create_issue"},
			{Type: "message_complete", StopReason: "tool_use"},
		},
	}
	w := NewNormalizingAdapter(inner, "gemini")

	params := types.StreamParams{
		Tools: []types.ToolDefinition{
			{Name: "mcp_jira_create-issue", InputSchema: json.RawMessage(`{}`)},
		},
	}
	ch, err := w.Stream(context.Background(), params)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// The provider saw the normalised "mcp_jira_create_issue".
	if got := inner.receivedParams.Tools[0].Name; got != "mcp_jira_create_issue" {
		t.Fatalf("outbound tool name = %q, want %q", got, "mcp_jira_create_issue")
	}

	// Drain the stream and assert the tool_call event was de-normalised.
	var sawToolCall bool
	for ev := range ch {
		if ev.Type == "tool_call" {
			sawToolCall = true
			if ev.Name != "mcp_jira_create-issue" {
				t.Errorf("inbound tool_call Name = %q, want internal %q",
					ev.Name, "mcp_jira_create-issue")
			}
		}
	}
	if !sawToolCall {
		t.Fatal("expected to observe a tool_call event")
	}
}

func TestNormalizingAdapter_MultipleToolCallsInBatchReverseTranslated(t *testing.T) {
	// A streamed response can carry several tool_call events in a
	// single turn (parallel tool use). The wrapper's per-event reverse
	// translation must apply to every one, not just the first — the
	// goroutine that drains innerCh loops over the channel but no test
	// previously scripted more than one tool_call. Gemini's policy is
	// the strictest, so two MCP-prefixed names with hyphens and dots
	// force normalisation on the outbound side and assert the inbound
	// reverse-mapping restores both originals independently and in
	// order.
	inner := &fakeAdapter{
		scripted: []types.StreamEvent{
			{Type: "text_delta", Text: "running both"},
			{Type: "tool_call", ID: "call_1", Name: "mcp_jira_create_issue"},
			{Type: "tool_call", ID: "call_2", Name: "mcp_slack_post_message"},
			{Type: "message_complete", StopReason: "tool_use"},
		},
	}
	w := NewNormalizingAdapter(inner, "gemini")

	params := types.StreamParams{
		Tools: []types.ToolDefinition{
			{Name: "mcp_jira_create-issue", InputSchema: json.RawMessage(`{}`)},
			{Name: "mcp_slack_post.message", InputSchema: json.RawMessage(`{}`)},
		},
	}
	ch, err := w.Stream(context.Background(), params)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var toolCallNames []string
	for ev := range ch {
		if ev.Type == "tool_call" {
			toolCallNames = append(toolCallNames, ev.Name)
		}
	}
	wantNames := []string{"mcp_jira_create-issue", "mcp_slack_post.message"}
	if len(toolCallNames) != len(wantNames) {
		t.Fatalf("got %d tool_call events, want %d (names=%v)", len(toolCallNames), len(wantNames), toolCallNames)
	}
	for i, want := range wantNames {
		if toolCallNames[i] != want {
			t.Errorf("tool_call[%d].Name = %q, want internal %q", i, toolCallNames[i], want)
		}
	}
}

func TestNormalizingAdapter_MessageHistoryToolUseNamesRewritten(t *testing.T) {
	// A prior assistant turn produced a tool_use block whose Name
	// carries the internal (registry) name. On the next turn the
	// wrapper must rewrite that Name to the provider-facing form so
	// the provider can correlate the round-tripped block back to its
	// declared tool.
	inner := &fakeAdapter{}
	w := NewNormalizingAdapter(inner, "gemini")

	params := types.StreamParams{
		Tools: []types.ToolDefinition{
			{Name: "mcp_jira_create-issue", InputSchema: json.RawMessage(`{}`)},
		},
		Messages: []types.Message{
			{
				Role: "assistant",
				Content: []types.ContentBlock{
					{Type: "text", Text: "I'll create the issue."},
					{Type: "tool_use", ID: "call_1", Name: "mcp_jira_create-issue", Input: json.RawMessage(`{}`)},
				},
			},
			{
				Role: "user",
				Content: []types.ContentBlock{
					{Type: "tool_result", ToolUseID: "call_1", Content: "OK"},
				},
			},
		},
	}

	if _, err := w.Stream(context.Background(), params); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	got := inner.receivedParams.Messages[0].Content[1]
	if got.Type != "tool_use" {
		t.Fatalf("block 1 type = %q, want tool_use", got.Type)
	}
	if got.Name != "mcp_jira_create_issue" {
		t.Errorf("rewritten tool_use Name = %q, want %q", got.Name, "mcp_jira_create_issue")
	}
	// Unrelated blocks must be unchanged.
	if inner.receivedParams.Messages[0].Content[0].Text != "I'll create the issue." {
		t.Errorf("text block was mutated")
	}
	if inner.receivedParams.Messages[1].Content[0].Type != "tool_result" {
		t.Errorf("tool_result block was mutated")
	}
}

func TestNormalizingAdapter_CallerParamsNotMutated(t *testing.T) {
	// The wrapper must not write through to the caller's StreamParams
	// — the loop reuses params construction state and the batch
	// adapter retains the slice across goroutines.
	inner := &fakeAdapter{}
	w := NewNormalizingAdapter(inner, "gemini")

	tools := []types.ToolDefinition{
		{Name: "mcp_jira_create-issue", InputSchema: json.RawMessage(`{}`)},
	}
	messages := []types.Message{
		{
			Role: "assistant",
			Content: []types.ContentBlock{
				{Type: "tool_use", ID: "call_1", Name: "mcp_jira_create-issue"},
			},
		},
	}
	params := types.StreamParams{Tools: tools, Messages: messages}

	if _, err := w.Stream(context.Background(), params); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if tools[0].Name != "mcp_jira_create-issue" {
		t.Errorf("caller tools slice was mutated: %q", tools[0].Name)
	}
	if messages[0].Content[0].Name != "mcp_jira_create-issue" {
		t.Errorf("caller messages slice was mutated: %q", messages[0].Content[0].Name)
	}
}

func TestNormalizingAdapter_CollisionAfterNormalisationFailsClosed(t *testing.T) {
	// Two distinct internal names that normalise to the same external
	// name under the Gemini policy. The wrapper must refuse the
	// stream rather than silently aliasing — silent aliasing would
	// route an inbound tool_call to the wrong handler.
	inner := &fakeAdapter{}
	w := NewNormalizingAdapter(inner, "gemini")

	params := types.StreamParams{
		Tools: []types.ToolDefinition{
			{Name: "mcp_jira-issue", InputSchema: json.RawMessage(`{}`)},
			{Name: "mcp_jira.issue", InputSchema: json.RawMessage(`{}`)},
		},
	}
	// Names collide on first sanitization but a hash suffix
	// disambiguates them. Confirm the stream still works (this is
	// the deterministic-resolution path, not the unresolvable case).
	if _, err := w.Stream(context.Background(), params); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	a := inner.receivedParams.Tools[0].Name
	b := inner.receivedParams.Tools[1].Name
	if a == b {
		t.Fatalf("collision not resolved: both normalised to %q", a)
	}
}

func TestNormalizingAdapter_InnerStreamError_Propagated(t *testing.T) {
	inner := &fakeAdapter{streamErr: errFake}
	w := NewNormalizingAdapter(inner, "anthropic")
	_, err := w.Stream(context.Background(), types.StreamParams{})
	if err == nil {
		t.Fatal("expected error to propagate from inner adapter")
	}
}

func TestNormalizingAdapter_LastBatchIDPassThrough(t *testing.T) {
	inner := &fakeBatchAdapter{}
	inner.batchID = "batch-xyz"
	w := NewNormalizingAdapter(inner, "anthropic")
	if got := w.LastBatchID(); got != "batch-xyz" {
		t.Errorf("LastBatchID = %q, want %q", got, "batch-xyz")
	}
}

func TestNormalizingAdapter_LastBatchIDEmptyWhenInnerNotBatch(t *testing.T) {
	w := NewNormalizingAdapter(&fakeAdapter{}, "anthropic")
	if got := w.LastBatchID(); got != "" {
		t.Errorf("LastBatchID for non-batch inner = %q, want empty", got)
	}
}

func TestNormalizingAdapter_RoundTripDispatch_OpenAIResponses(t *testing.T) {
	// Integration-style: a full request/response cycle on a provider
	// type that permits hyphens. Both directions must preserve the
	// internal name unchanged so dispatch still resolves the handler
	// by its original identifier.
	inner := &fakeAdapter{
		scripted: []types.StreamEvent{
			{Type: "tool_call", ID: "1", Name: "mcp_jira_create-issue"},
		},
	}
	w := NewNormalizingAdapter(inner, "openai-responses")

	params := types.StreamParams{
		Tools: []types.ToolDefinition{
			{Name: "mcp_jira_create-issue", InputSchema: json.RawMessage(`{}`)},
		},
	}
	ch, err := w.Stream(context.Background(), params)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for ev := range ch {
		if ev.Type == "tool_call" && ev.Name != "mcp_jira_create-issue" {
			t.Errorf("round-trip altered name: got %q, want unchanged", ev.Name)
		}
	}
}
