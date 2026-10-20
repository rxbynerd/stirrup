package types

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsThinkingBlock_ClassifiesBlockTypes(t *testing.T) {
	cases := []struct {
		name string
		typ  string
		want bool
	}{
		{name: "thinking", typ: "thinking", want: true},
		{name: "redacted thinking", typ: "redacted_thinking", want: true},
		{name: "text", typ: "text", want: false},
		{name: "tool use", typ: "tool_use", want: false},
		{name: "tool result", typ: "tool_result", want: false},
		{name: "empty type", typ: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, IsThinkingBlock(ContentBlock{Type: tc.typ}), "block type %q", tc.typ)
		})
	}
}

func TestStripThinkingBlocks_RemovesBothTypesAndKeepsOrder(t *testing.T) {
	must := require.New(t)
	in := []Message{
		{Role: "user", Content: []ContentBlock{{Type: "text", Text: "go"}}},
		{
			Role: "assistant",
			Content: []ContentBlock{
				{Type: "thinking", Text: "", ThoughtSignature: "sig-1"},
				{Type: "text", Text: "reading"},
				{Type: "redacted_thinking", ThoughtSignature: "data-1"},
				{Type: "tool_use", ID: "t1", Name: "read_file", Input: json.RawMessage(`{}`)},
			},
			ReplayFields: map[string]json.RawMessage{"k": json.RawMessage(`"v"`)},
		},
		{Role: "user", Content: []ContentBlock{{Type: "tool_result", ToolUseID: "t1", Content: "ok"}}},
	}

	got := StripThinkingBlocks(in)

	want := []Message{
		in[0],
		{
			Role: "assistant",
			Content: []ContentBlock{
				{Type: "text", Text: "reading"},
				{Type: "tool_use", ID: "t1", Name: "read_file", Input: json.RawMessage(`{}`)},
			},
			ReplayFields: in[1].ReplayFields,
		},
		in[2],
	}
	must.Equal(want, got)

	must.Len(in[1].Content, 4, "input history was mutated: %+v", in[1].Content)
	must.Equal("thinking", in[1].Content[0].Type, "input history was mutated")
}

func TestStripThinkingBlocks_NoThinkingReturnsInput(t *testing.T) {
	must := require.New(t)
	in := []Message{
		{Role: "user", Content: []ContentBlock{{Type: "text", Text: "go"}}},
		{Role: "assistant", Content: []ContentBlock{{Type: "text", Text: "done"}}},
	}
	got := StripThinkingBlocks(in)
	must.Len(got, len(in))
	must.Same(&in[0], &got[0], "history without thinking blocks should be returned as-is")

	must.Nil(StripThinkingBlocks(nil), "nil history should stay nil")
}

// TestStripThinkingBlocks_ThinkingOnlyMessageLeftForAdapter pins that the
// helper removes blocks without reshaping the history: an emptied message
// stays in place for the adapter to handle (the Anthropic adapter's wire
// shape is pinned by TestBuildAnthropicRequest_ThinkingOnlyTurnOmitted).
func TestStripThinkingBlocks_ThinkingOnlyMessageLeftForAdapter(t *testing.T) {
	must := require.New(t)
	in := []Message{
		{Role: "user", Content: []ContentBlock{{Type: "text", Text: "go"}}},
		{Role: "assistant", Content: []ContentBlock{{Type: "thinking", ThoughtSignature: "sig"}}},
	}
	got := StripThinkingBlocks(in)
	must.Len(got, 2, "both messages should keep their original positions: %+v", got)
	must.Equal("assistant", got[1].Role)
	must.False(containsThinking(got[1].Content), "thinking block survived: %+v", got[1].Content)
}
