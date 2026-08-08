package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestToolResult_StructuredOmitemptyByteIdentical pins that the
// additive Structured field is invisible on the wire when unset: a
// text-only ToolResult serialises byte-for-byte as before the field
// existed.
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

// TestToolCall_InternalNameOmittedOnWire pins that under the default
// profile InternalName is empty and the "internalName" key is
// physically absent from marshalled JSON across every tool-call trace
// shape. The positive case confirms the key appears when set.
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

// TestToolDefinition_PresentationByteIdentical pins that the additive
// Presentation bundle is invisible on the default JSON encoding: a
// tool definition with no Presentation serialises byte-for-byte as
// before the field existed, and a tool WITH Presentation still omits
// it (json:"-") so the Anthropic verbatim path never leaks unknown
// keys onto the wire.
func TestToolDefinition_PresentationByteIdentical(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	bare := ToolDefinition{Name: "demo", Description: "d", InputSchema: schema}
	bareBytes, err := json.Marshal(bare)
	if err != nil {
		t.Fatalf("marshal bare: %v", err)
	}

	withPres := ToolDefinition{
		Name:        "demo",
		Description: "d",
		InputSchema: schema,
		Presentation: &ToolPresentation{
			InputExamples: []json.RawMessage{json.RawMessage(`{"a":1}`)},
			Annotations:   &ToolAnnotations{Title: "Demo"},
		},
	}
	withBytes, err := json.Marshal(withPres)
	if err != nil {
		t.Fatalf("marshal with presentation: %v", err)
	}

	if string(bareBytes) != string(withBytes) {
		t.Errorf("Presentation leaked onto the wire:\n bare = %s\n with = %s", bareBytes, withBytes)
	}
	if strings.Contains(string(withBytes), "Presentation") || strings.Contains(string(withBytes), "inputExamples") || strings.Contains(string(withBytes), "annotations") {
		t.Errorf("Presentation fields must not appear on the default encoding, got: %s", withBytes)
	}
}

// TestToolPresentation_RoundTrip confirms ToolPresentation and ToolAnnotations
// marshal and unmarshal losslessly when serialised directly (e.g. by an
// adapter that opts to project them), including the *bool unset-vs-false
// distinction on the annotation hints.
func TestToolPresentation_RoundTrip(t *testing.T) {
	readOnly := true
	pres := ToolPresentation{
		InputExamples: []json.RawMessage{json.RawMessage(`{"x":"y"}`)},
		Annotations: &ToolAnnotations{
			Title:        "Title",
			ReadOnlyHint: &readOnly,
			// DestructiveHint deliberately left unset.
		},
	}
	b, err := json.Marshal(pres)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got ToolPresentation
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Annotations == nil || got.Annotations.ReadOnlyHint == nil || !*got.Annotations.ReadOnlyHint {
		t.Errorf("ReadOnlyHint did not round-trip: %+v", got.Annotations)
	}
	if got.Annotations.DestructiveHint != nil {
		t.Errorf("unset DestructiveHint must stay nil, got %v", *got.Annotations.DestructiveHint)
	}
	if len(got.InputExamples) != 1 || string(got.InputExamples[0]) != `{"x":"y"}` {
		t.Errorf("InputExamples did not round-trip: %v", got.InputExamples)
	}
}
