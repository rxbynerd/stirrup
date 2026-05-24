package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToolResult_StructuredOmitemptyByteIdentical pins the issue #231
// invariant that the additive Structured field is invisible on the wire when
// unset: a text-only ToolResult must serialise byte-for-byte the way it did
// before the field existed.
func TestToolResult_StructuredOmitemptyByteIdentical(t *testing.T) {
	r := ToolResult{ToolUseID: "tu_1", Content: "hello", IsError: false}
	got, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"tool_use_id":"tu_1","content":"hello"}`
	if string(got) != want {
		t.Errorf("text-only ToolResult JSON changed\n got: %s\nwant: %s", got, want)
	}
	if strings.Contains(string(got), "structured") {
		t.Errorf("nil Structured must be omitted, got: %s", got)
	}
}

func TestToolResult_StructuredRoundTrip(t *testing.T) {
	payload := json.RawMessage(`{"exit_code":0,"stdout":"ok"}`)
	r := ToolResult{ToolUseID: "tu_2", Content: "ok", Structured: payload}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ToolResult
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Content != "ok" {
		t.Errorf("text fallback lost: %q", back.Content)
	}
	if string(back.Structured) != string(payload) {
		t.Errorf("structured payload not preserved\n got: %s\nwant: %s", back.Structured, payload)
	}
}

// TestToolCallRecord_StructuredOmitempty mirrors the invariant for the trace
// record so an existing text-only trace line is unchanged.
func TestToolCallRecord_StructuredOmitempty(t *testing.T) {
	rec := ToolCallRecord{ID: "id", Name: "read_file", Input: json.RawMessage(`{}`), Output: "x", Success: true}
	got, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(got), "structured") {
		t.Errorf("nil Structured must be omitted from ToolCallRecord, got: %s", got)
	}
}
