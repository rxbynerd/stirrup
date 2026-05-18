package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func TestReplayProvider_TextOnly(t *testing.T) {
	turns := []types.TurnRecord{
		{
			Turn: 1,
			ModelOutput: []types.ContentBlock{
				{Type: "text", Text: "Hello, world!"},
			},
		},
	}

	rp := NewReplayProvider(turns)
	ch, err := rp.Stream(context.Background(), types.StreamParams{})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(events), events)
	}
	if events[0].Type != "text_delta" || events[0].Text != "Hello, world!" {
		t.Errorf("event[0] = %+v, want text_delta/Hello, world!", events[0])
	}
	if events[1].Type != "message_complete" {
		t.Errorf("event[1].Type = %q, want message_complete", events[1].Type)
	}
	if events[1].StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", events[1].StopReason)
	}
	if events[1].OutputTokens <= 0 {
		t.Errorf("OutputTokens = %d, want > 0", events[1].OutputTokens)
	}
}

func TestReplayProvider_ToolUseBlocks(t *testing.T) {
	turns := []types.TurnRecord{
		{
			Turn: 1,
			ModelOutput: []types.ContentBlock{
				{Type: "text", Text: "Let me read that file."},
				{
					Type:  "tool_use",
					ID:    "toolu_abc",
					Name:  "read_file",
					Input: json.RawMessage(`{"path":"main.go"}`),
				},
			},
		},
	}

	rp := NewReplayProvider(turns)
	ch, err := rp.Stream(context.Background(), types.StreamParams{})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d: %+v", len(events), events)
	}

	if events[0].Type != "text_delta" {
		t.Errorf("event[0].Type = %q, want text_delta", events[0].Type)
	}

	if events[1].Type != "tool_call" {
		t.Fatalf("event[1].Type = %q, want tool_call", events[1].Type)
	}
	if events[1].ID != "toolu_abc" {
		t.Errorf("event[1].ID = %q, want toolu_abc", events[1].ID)
	}
	if events[1].Name != "read_file" {
		t.Errorf("event[1].Name = %q, want read_file", events[1].Name)
	}
	if events[1].Input["path"] != "main.go" {
		t.Errorf("event[1].Input[path] = %v, want main.go", events[1].Input["path"])
	}

	if events[2].Type != "message_complete" {
		t.Errorf("event[2].Type = %q, want message_complete", events[2].Type)
	}
	if events[2].StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", events[2].StopReason)
	}
}

func TestReplayProvider_MultipleTurns(t *testing.T) {
	turns := []types.TurnRecord{
		{
			Turn:        1,
			ModelOutput: []types.ContentBlock{{Type: "text", Text: "First turn."}},
		},
		{
			Turn:        2,
			ModelOutput: []types.ContentBlock{{Type: "text", Text: "Second turn."}},
		},
		{
			Turn:        3,
			ModelOutput: []types.ContentBlock{{Type: "text", Text: "Third turn."}},
		},
	}

	rp := NewReplayProvider(turns)

	for i, want := range []string{"First turn.", "Second turn.", "Third turn."} {
		ch, err := rp.Stream(context.Background(), types.StreamParams{})
		if err != nil {
			t.Fatalf("Stream() turn %d error: %v", i+1, err)
		}
		events := collectEvents(t, ch)
		if len(events) < 1 {
			t.Fatalf("turn %d: expected at least 1 event", i+1)
		}
		if events[0].Text != want {
			t.Errorf("turn %d: text = %q, want %q", i+1, events[0].Text, want)
		}
	}
}

func TestReplayProvider_TurnsExhausted(t *testing.T) {
	turns := []types.TurnRecord{
		{Turn: 1, ModelOutput: []types.ContentBlock{{Type: "text", Text: "Only turn."}}},
	}

	rp := NewReplayProvider(turns)

	// First call succeeds.
	ch, err := rp.Stream(context.Background(), types.StreamParams{})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	for range ch {
	}

	// Second call should fail.
	_, err = rp.Stream(context.Background(), types.StreamParams{})
	if err == nil {
		t.Fatal("expected error when turns exhausted, got nil")
	}
}

func TestReplayProvider_ContextCancellation(t *testing.T) {
	turns := []types.TurnRecord{
		{
			Turn: 1,
			ModelOutput: []types.ContentBlock{
				{Type: "text", Text: "block 1"},
				{Type: "text", Text: "block 2"},
				{Type: "text", Text: "block 3"},
			},
		},
	}

	rp := NewReplayProvider(turns)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	ch, err := rp.Stream(ctx, types.StreamParams{})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	// Should get an error event from context cancellation at some point.
	foundError := false
	for _, ev := range events {
		if ev.Type == "error" && ev.Error != nil {
			foundError = true
		}
	}
	if !foundError {
		t.Error("expected an error event from context cancellation")
	}
}

func TestReplayProvider_EmptyModelOutput(t *testing.T) {
	turns := []types.TurnRecord{
		{Turn: 1, ModelOutput: nil},
	}

	rp := NewReplayProvider(turns)
	ch, err := rp.Stream(context.Background(), types.StreamParams{})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)

	// Should still get a message_complete event.
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %+v", len(events), events)
	}
	if events[0].Type != "message_complete" {
		t.Errorf("event[0].Type = %q, want message_complete", events[0].Type)
	}
	if events[0].StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", events[0].StopReason)
	}
}

// TestReplayProvider_ForwardsThoughtSignature pins the issue #194
// follow-on behaviour: a tool_use ContentBlock in a TurnRecord that
// carries a ThoughtSignature must surface that value on the emitted
// tool_call StreamEvent. The load-bearing path is live-continuation
// (mineFailureTasks) where ReplayProvider seeds the history of a run
// that subsequently calls a real Vertex provider — dropping the
// signature there would silently degrade cross-turn reasoning
// continuity for exactly the multi-turn agentic sequences likely to
// appear in a failure recording.
func TestReplayProvider_ForwardsThoughtSignature(t *testing.T) {
	const sig = "REPLAYED-SIG=="
	turns := []types.TurnRecord{
		{
			Turn: 1,
			ModelOutput: []types.ContentBlock{
				{
					Type:             "tool_use",
					ID:               "toolu_abc",
					Name:             "read_file",
					Input:            json.RawMessage(`{"path":"main.go"}`),
					ThoughtSignature: sig,
				},
			},
		},
	}

	rp := NewReplayProvider(turns)
	ch, err := rp.Stream(context.Background(), types.StreamParams{})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	events := collectEvents(t, ch)
	var toolCall *types.StreamEvent
	for i := range events {
		if events[i].Type == "tool_call" {
			toolCall = &events[i]
		}
	}
	if toolCall == nil {
		t.Fatal("expected a tool_call event")
	}
	if toolCall.ThoughtSignature != sig {
		t.Errorf("tool_call.ThoughtSignature = %q, want %q", toolCall.ThoughtSignature, sig)
	}
}
