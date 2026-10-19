package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// replayEvents streams the next turn from rp and returns every event it emits.
func replayEvents(t *testing.T, rp *ReplayProvider) []types.StreamEvent {
	t.Helper()
	ch, err := rp.Stream(context.Background(), types.StreamParams{})
	require.NoError(t, err)
	return collectEvents(t, ch)
}

// TestReplayProvider_ThinkingBlocks pins that recorded thinking and
// redacted_thinking blocks replay as their own events in recorded order,
// so a replayed turn rebuilds the same block sequence.
func TestReplayProvider_ThinkingBlocks(t *testing.T) {
	turns := []types.TurnRecord{{
		Turn: 1,
		ModelOutput: []types.ContentBlock{
			{Type: "thinking", Text: "plan", ThoughtSignature: "sig-A"},
			{Type: "text", Text: "Reading."},
			{Type: "redacted_thinking", ThoughtSignature: "data-B"},
			{Type: "tool_use", ID: "toolu_1", Name: "read_file", Input: json.RawMessage(`{"path":"a"}`)},
		},
	}}

	events := replayEvents(t, NewReplayProvider(turns))

	gotTypes := make([]string, 0, len(events))
	for _, ev := range events {
		gotTypes = append(gotTypes, ev.Type)
	}
	wantTypes := []string{"thinking", "text_delta", "redacted_thinking", "tool_call", "message_complete"}
	require.Equal(t, wantTypes, gotTypes)

	require.Equal(t, "plan", events[0].Text)
	require.Equal(t, "sig-A", events[0].ThoughtSignature)
	require.Equal(t, "data-B", events[2].ThoughtSignature)
	require.Equal(t, "tool_use", events[4].StopReason)
}

func TestReplayProvider_TextOnly(t *testing.T) {
	turns := []types.TurnRecord{
		{
			Turn: 1,
			ModelOutput: []types.ContentBlock{
				{Type: "text", Text: "Hello, world!"},
			},
		},
	}

	events := replayEvents(t, NewReplayProvider(turns))

	require.Len(t, events, 2, "events: %+v", events)
	require.Equal(t, "text_delta", events[0].Type)
	require.Equal(t, "Hello, world!", events[0].Text)
	require.Equal(t, "message_complete", events[1].Type)
	require.Equal(t, "end_turn", events[1].StopReason)
	require.Positive(t, events[1].OutputTokens)
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

	events := replayEvents(t, NewReplayProvider(turns))

	require.Len(t, events, 3, "events: %+v", events)
	require.Equal(t, "text_delta", events[0].Type)

	require.Equal(t, "tool_call", events[1].Type)
	require.Equal(t, "toolu_abc", events[1].ID)
	require.Equal(t, "read_file", events[1].Name)
	require.Equal(t, "main.go", events[1].Input["path"])

	require.Equal(t, "message_complete", events[2].Type)
	require.Equal(t, "tool_use", events[2].StopReason)
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
		t.Run(fmt.Sprintf("turn %d", i+1), func(t *testing.T) {
			events := replayEvents(t, rp)
			require.NotEmpty(t, events)
			require.Equal(t, want, events[0].Text)
		})
	}
}

func TestReplayProvider_StreamErrorsWhenTurnsExhausted(t *testing.T) {
	turns := []types.TurnRecord{
		{Turn: 1, ModelOutput: []types.ContentBlock{{Type: "text", Text: "Only turn."}}},
	}

	rp := NewReplayProvider(turns)
	replayEvents(t, rp)

	_, err := rp.Stream(context.Background(), types.StreamParams{})
	require.Error(t, err)
}

func TestReplayProvider_CancelledContextEmitsError(t *testing.T) {
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
	cancel()

	ch, err := rp.Stream(ctx, types.StreamParams{})
	require.NoError(t, err)

	events := collectEvents(t, ch)

	require.True(t, slices.ContainsFunc(events, func(ev types.StreamEvent) bool {
		return ev.Type == "error" && ev.Error != nil
	}), "expected an error event from context cancellation")
}

func TestReplayProvider_EmptyModelOutput(t *testing.T) {
	turns := []types.TurnRecord{
		{Turn: 1, ModelOutput: nil},
	}

	events := replayEvents(t, NewReplayProvider(turns))

	require.Len(t, events, 1, "events: %+v", events)
	require.Equal(t, "message_complete", events[0].Type)
	require.Equal(t, "end_turn", events[0].StopReason)
}

// TestReplayProvider_ForwardsThoughtSignature pins that a tool_use
// ContentBlock's ThoughtSignature must surface on the emitted tool_call
// StreamEvent — load-bearing for live-continuation replay into a real
// provider, where dropping it would degrade cross-turn reasoning.
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

	events := replayEvents(t, NewReplayProvider(turns))

	var toolCall *types.StreamEvent
	for i := range events {
		if events[i].Type == "tool_call" {
			toolCall = &events[i]
		}
	}
	require.NotNil(t, toolCall, "expected a tool_call event")
	require.Equal(t, sig, toolCall.ThoughtSignature)
}
