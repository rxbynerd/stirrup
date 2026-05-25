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
	if strings.Contains(string(got), "kind") {
		t.Errorf("empty Kind must be omitted, got: %s", got)
	}
}

func TestToolResult_StructuredRoundTrip(t *testing.T) {
	payload := json.RawMessage(`{"exit_code":0,"stdout":"ok"}`)
	r := ToolResult{ToolUseID: "tu_2", Content: "ok", Structured: payload, Kind: "command_result"}
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
	if back.Kind != "command_result" {
		t.Errorf("kind not preserved: %q", back.Kind)
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
	if strings.Contains(string(got), "kind") {
		t.Errorf("empty Kind must be omitted from ToolCallRecord, got: %s", got)
	}
}

// TestToolCall_InternalNameOmittedOnWire pins the issue #234 back-compat
// guarantee at the WIRE level (not just in memory): under the default
// profile InternalName is empty and the "internalName" key must be
// physically absent from the marshalled JSON of every tool-call trace
// shape, so a default-profile trace is byte-compatible with pre-profile
// consumers. The positive case confirms the key appears when set.
func TestToolCall_InternalNameOmittedOnWire(t *testing.T) {
	t.Run("summary", func(t *testing.T) {
		b, err := json.Marshal(ToolCallSummary{Name: "grep_files", Success: true})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(b), "internalName") {
			t.Errorf("empty InternalName must be omitted from ToolCallSummary, got: %s", b)
		}
	})
	t.Run("trace", func(t *testing.T) {
		b, err := json.Marshal(ToolCallTrace{Name: "grep_files", Success: true})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(b), "internalName") {
			t.Errorf("empty InternalName must be omitted from ToolCallTrace, got: %s", b)
		}
	})
	t.Run("record", func(t *testing.T) {
		b, err := json.Marshal(ToolCallRecord{ID: "id", Name: "grep_files", Input: json.RawMessage(`{}`), Output: "x", Success: true})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(b), "internalName") {
			t.Errorf("empty InternalName must be omitted from ToolCallRecord, got: %s", b)
		}
	})
	t.Run("present_when_set", func(t *testing.T) {
		b, err := json.Marshal(ToolCallTrace{Name: "grep", InternalName: "grep_files", Success: true})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(b), `"internalName":"grep_files"`) {
			t.Errorf("a set InternalName must appear on the wire, got: %s", b)
		}
	})
}
