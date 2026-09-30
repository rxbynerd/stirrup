package types

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestIsThinkingBlock(t *testing.T) {
	cases := map[string]bool{
		"thinking":          true,
		"redacted_thinking": true,
		"text":              false,
		"tool_use":          false,
		"tool_result":       false,
		"":                  false,
	}
	for typ, want := range cases {
		if got := IsThinkingBlock(ContentBlock{Type: typ}); got != want {
			t.Errorf("IsThinkingBlock(%q) = %v, want %v", typ, got, want)
		}
	}
}

func TestStripThinkingBlocks_RemovesBothTypesAndKeepsOrder(t *testing.T) {
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
	if !reflect.DeepEqual(got, want) {
		t.Errorf("StripThinkingBlocks mismatch\n got: %+v\nwant: %+v", got, want)
	}
	if len(in[1].Content) != 4 || in[1].Content[0].Type != "thinking" {
		t.Errorf("input history was mutated: %+v", in[1].Content)
	}
}

func TestStripThinkingBlocks_NoThinkingReturnsInput(t *testing.T) {
	in := []Message{
		{Role: "user", Content: []ContentBlock{{Type: "text", Text: "go"}}},
		{Role: "assistant", Content: []ContentBlock{{Type: "text", Text: "done"}}},
	}
	got := StripThinkingBlocks(in)
	if len(got) != len(in) || &got[0] != &in[0] {
		t.Errorf("history without thinking blocks should be returned as-is")
	}
	if StripThinkingBlocks(nil) != nil {
		t.Errorf("nil history should stay nil")
	}
}

// TestStripThinkingBlocks_ThinkingOnlyMessageLeftForAdapter pins that the
// helper removes blocks without reshaping the history: an emptied message
// stays in place for the adapter to handle (the Anthropic adapter's wire
// shape is pinned by TestBuildAnthropicRequest_ThinkingOnlyTurnOmitted).
func TestStripThinkingBlocks_ThinkingOnlyMessageLeftForAdapter(t *testing.T) {
	in := []Message{
		{Role: "user", Content: []ContentBlock{{Type: "text", Text: "go"}}},
		{Role: "assistant", Content: []ContentBlock{{Type: "thinking", ThoughtSignature: "sig"}}},
	}
	got := StripThinkingBlocks(in)
	if len(got) != 2 || got[1].Role != "assistant" {
		t.Fatalf("got %+v, want both messages in their original positions", got)
	}
	if containsThinking(got[1].Content) {
		t.Errorf("thinking block survived: %+v", got[1].Content)
	}
}
