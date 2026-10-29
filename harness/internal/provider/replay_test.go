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
	must := require.New(t)
	ch, err := rp.Stream(context.Background(), types.StreamParams{})
	must.NoError(err)
	return collectEvents(t, ch)
}

// TestReplayProvider_ThinkingBlocks pins that recorded thinking and
// redacted_thinking blocks replay as their own events in recorded order,
// so a replayed turn rebuilds the same block sequence.
func TestReplayProvider_ThinkingBlocks(t *testing.T) {
	must := require.New(t)
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
	must.Equal(wantTypes, gotTypes)

	must.Equal("plan", events[0].Text)
	must.Equal("sig-A", events[0].ThoughtSignature)
	must.Equal("data-B", events[2].ThoughtSignature)
	must.Equal("tool_use", events[4].StopReason)
}

func TestReplayProvider_TextOnly(t *testing.T) {
	must := require.New(t)
	turns := []types.TurnRecord{
		{
			Turn: 1,
			ModelOutput: []types.ContentBlock{
				{Type: "text", Text: "Hello, world!"},
			},
		},
	}

	events := replayEvents(t, NewReplayProvider(turns))

	must.Len(events, 2, "events: %+v", events)
	must.Equal("text_delta", events[0].Type)
	must.Equal("Hello, world!", events[0].Text)
	must.Equal("message_complete", events[1].Type)
	must.Equal("end_turn", events[1].StopReason)
	must.Positive(events[1].OutputTokens)
}

func TestReplayProvider_ToolUseBlocks(t *testing.T) {
	must := require.New(t)
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

	must.Len(events, 3, "events: %+v", events)
	must.Equal("text_delta", events[0].Type)

	must.Equal("tool_call", events[1].Type)
	must.Equal("toolu_abc", events[1].ID)
	must.Equal("read_file", events[1].Name)
	must.Equal("main.go", events[1].Input["path"])

	must.Equal("message_complete", events[2].Type)
	must.Equal("tool_use", events[2].StopReason)
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
			must := require.New(t)
			events := replayEvents(t, rp)
			must.NotEmpty(events)
			must.Equal(want, events[0].Text)
		})
	}
}

func TestReplayProvider_StreamErrorsWhenTurnsExhausted(t *testing.T) {
	must := require.New(t)
	turns := []types.TurnRecord{
		{Turn: 1, ModelOutput: []types.ContentBlock{{Type: "text", Text: "Only turn."}}},
	}

	rp := NewReplayProvider(turns)
	replayEvents(t, rp)

	_, err := rp.Stream(context.Background(), types.StreamParams{})
	must.Error(err)
}

func TestReplayProvider_CancelledContextEmitsError(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	events := collectEvents(t, ch)

	must.True(slices.ContainsFunc(events, func(ev types.StreamEvent) bool {
		return ev.Type == "error" && ev.Error != nil
	}), "expected an error event from context cancellation")
}

func TestReplayProvider_EmptyModelOutput(t *testing.T) {
	must := require.New(t)
	turns := []types.TurnRecord{
		{Turn: 1, ModelOutput: nil},
	}

	events := replayEvents(t, NewReplayProvider(turns))

	must.Len(events, 1, "events: %+v", events)
	must.Equal("message_complete", events[0].Type)
	must.Equal("end_turn", events[0].StopReason)
}

// TestReplayProvider_ForwardsThoughtSignature pins that a tool_use
// ContentBlock's ThoughtSignature must surface on the emitted tool_call
// StreamEvent — load-bearing for live-continuation replay into a real
// provider, where dropping it would degrade cross-turn reasoning.
func TestReplayProvider_ForwardsThoughtSignature(t *testing.T) {
	must := require.New(t)
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
	must.NotNil(toolCall, "expected a tool_call event")
	must.Equal(sig, toolCall.ThoughtSignature)
}

// TestReplayProvider_ToolSearchBlocks pins that recorded search blocks
// replay as the events the loop persists them from, so a recorded call to a
// tool the search loaded is not rejected as unloaded.
func TestReplayProvider_ToolSearchBlocks(t *testing.T) {
	cases := []struct {
		name   string
		result types.ContentBlock
		want   types.StreamEvent
	}{
		{
			name:   "loaded",
			result: types.ContentBlock{Type: "tool_search_result", ToolUseID: "srvtoolu_1", ToolReferences: []string{"mcp_github_search_issues"}},
			want:   types.StreamEvent{Type: "tool_search_result", ID: "srvtoolu_1", ToolReferences: []string{"mcp_github_search_issues"}},
		},
		{
			name:   "error",
			result: types.ContentBlock{Type: "tool_search_result", ToolUseID: "srvtoolu_1", IsError: true, Name: "unavailable", Content: "upstream: timed out"},
			want:   types.StreamEvent{Type: "tool_search_result", ID: "srvtoolu_1", IsError: true, Name: "unavailable", Text: "upstream: timed out"},
		},
		{
			name:   "error without code or message",
			result: types.ContentBlock{Type: "tool_search_result", ToolUseID: "srvtoolu_1", IsError: true},
			want:   types.StreamEvent{Type: "tool_search_result", ID: "srvtoolu_1", IsError: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			turns := []types.TurnRecord{{
				Turn: 1,
				ModelOutput: []types.ContentBlock{
					{Type: "tool_search", ID: "srvtoolu_1", Name: "tool_search_tool_bm25", Input: json.RawMessage(`{"query":"github issues"}`)},
					tc.result,
					{Type: "future_block", Text: "ignored"},
					{Type: "tool_use", ID: "toolu_1", Name: "mcp_github_search_issues", Input: json.RawMessage(`{"q":"bug"}`)},
				},
			}}

			events := replayEvents(t, NewReplayProvider(turns))

			must.Equal([]string{"tool_search", "tool_search_result", "tool_call", "message_complete"}, eventTypes(events))
			must.Equal(types.StreamEvent{
				Type:  "tool_search",
				ID:    "srvtoolu_1",
				Name:  "tool_search_tool_bm25",
				Input: map[string]any{"query": "github issues"},
			}, events[0])
			must.Equal(tc.want, events[1])
			must.Equal("tool_use", events[3].StopReason)
		})
	}
}

func TestReplayProvider_ToolSearchBlockWithInvalidInputErrors(t *testing.T) {
	must := require.New(t)
	turns := []types.TurnRecord{{
		Turn:        1,
		ModelOutput: []types.ContentBlock{{Type: "tool_search", ID: "srvtoolu_1", Name: "tool_search_tool_bm25", Input: json.RawMessage(`{broken`)}},
	}}

	events := replayEvents(t, NewReplayProvider(turns))

	must.Equal([]string{"error"}, eventTypes(events))
	must.ErrorContains(events[0].Error, "invalid tool search input JSON")
}

// TestReplayProvider_ReplaysRecordedStopReason pins that a recorded stop
// reason is replayed as recorded, so an output-limit stop takes the same
// recovery path it took live, and that a turn without one still derives
// tool_use or end_turn from its blocks.
func TestReplayProvider_ReplaysRecordedStopReason(t *testing.T) {
	must := require.New(t)
	rp := NewReplayProvider([]types.TurnRecord{
		{StopReason: "max_tokens", ModelOutput: []types.ContentBlock{{Type: "text", Text: "cut off"}}},
		{ModelOutput: []types.ContentBlock{{Type: "text", Text: "done"}}},
	})

	first := replayEvents(t, rp)
	must.Equal("max_tokens", first[len(first)-1].StopReason)
	second := replayEvents(t, rp)
	must.Equal("end_turn", second[len(second)-1].StopReason)
}
