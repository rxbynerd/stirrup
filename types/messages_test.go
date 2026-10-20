package types

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestToolResult_TextOnlyWireBytesUnchanged pins that the
// additive Structured field is invisible on the wire when unset: a
// text-only ToolResult serialises byte-for-byte as before the field
// existed.
func TestToolResult_TextOnlyWireBytesUnchanged(t *testing.T) {
	must := require.New(t)
	r := ToolResult{ToolUseID: "tu_1", Content: "hello", IsError: false}
	got, err := json.Marshal(r)
	must.NoError(err, "marshal")

	must.Equal(`{"tool_use_id":"tu_1","content":"hello"}`, string(got), "text-only ToolResult JSON changed") //nolint:testifylint // byte-exact wire form is the behaviour under test
	t.Run("structured omitted when nil", func(t *testing.T) {
		must := require.New(t)
		must.NotContains(string(got), "structured")
	})
	t.Run("kind omitted when empty", func(t *testing.T) {
		must := require.New(t)
		must.NotContains(string(got), "kind")
	})
}

func TestToolResult_StructuredRoundTrip(t *testing.T) {
	must := require.New(t)
	payload := json.RawMessage(`{"exit_code":0,"stdout":"ok"}`)
	r := ToolResult{ToolUseID: "tu_2", Content: "ok", Structured: payload, Kind: "command_result"}
	raw, err := json.Marshal(r)
	must.NoError(err, "marshal")
	var back ToolResult
	must.NoError(json.Unmarshal(raw, &back), "unmarshal")

	t.Run("text fallback kept", func(t *testing.T) {
		must := require.New(t)
		must.Equal("ok", back.Content)
	})
	t.Run("structured payload preserved", func(t *testing.T) {
		must := require.New(t)
		must.Equal(string(payload), string(back.Structured))
	})
	t.Run("kind preserved", func(t *testing.T) {
		must := require.New(t)
		must.Equal("command_result", back.Kind)
	})
}

// TestToolCallRecord_StructuredAndKindOmittedWhenUnset mirrors the invariant for the trace
// record so an existing text-only trace line is unchanged.
func TestToolCallRecord_StructuredAndKindOmittedWhenUnset(t *testing.T) {
	must := require.New(t)
	rec := ToolCallRecord{ID: "id", Name: "read_file", Input: json.RawMessage(`{}`), Output: "x", Success: true}
	got, err := json.Marshal(rec)
	must.NoError(err, "marshal")

	t.Run("structured omitted when nil", func(t *testing.T) {
		must := require.New(t)
		must.NotContains(string(got), "structured")
	})
	t.Run("kind omitted when empty", func(t *testing.T) {
		must := require.New(t)
		must.NotContains(string(got), "kind")
	})
}

// TestToolCall_InternalNameOmittedOnWire pins that under the default
// profile InternalName is empty and the "internalName" key is
// physically absent from marshalled JSON across every tool-call trace
// shape. The positive case confirms the key appears when set.
func TestToolCall_InternalNameOmittedOnWire(t *testing.T) {
	t.Run("summary omits empty internal name", func(t *testing.T) {
		must := require.New(t)
		b, err := json.Marshal(ToolCallSummary{Name: "grep_files", Success: true})
		must.NoError(err, "marshal")
		must.NotContains(string(b), "internalName")
	})
	t.Run("trace omits empty internal name", func(t *testing.T) {
		must := require.New(t)
		b, err := json.Marshal(ToolCallTrace{Name: "grep_files", Success: true})
		must.NoError(err, "marshal")
		must.NotContains(string(b), "internalName")
	})
	t.Run("record omits empty internal name", func(t *testing.T) {
		must := require.New(t)
		b, err := json.Marshal(ToolCallRecord{ID: "id", Name: "grep_files", Input: json.RawMessage(`{}`), Output: "x", Success: true})
		must.NoError(err, "marshal")
		must.NotContains(string(b), "internalName")
	})
	t.Run("emits set internal name", func(t *testing.T) {
		must := require.New(t)
		b, err := json.Marshal(ToolCallTrace{Name: "grep", InternalName: "grep_files", Success: true})
		must.NoError(err, "marshal")
		must.Contains(string(b), `"internalName":"grep_files"`)
	})
}

// TestToolDefinition_PresentationAbsentFromDefaultJSON pins that the additive
// Presentation bundle is invisible on the default JSON encoding: a
// tool definition with no Presentation serialises byte-for-byte as
// before the field existed, and a tool WITH Presentation still omits
// it (json:"-") so the Anthropic verbatim path never leaks unknown
// keys onto the wire.
func TestToolDefinition_PresentationAbsentFromDefaultJSON(t *testing.T) {
	must := require.New(t)
	schema := json.RawMessage(`{"type":"object"}`)
	bare := ToolDefinition{Name: "demo", Description: "d", InputSchema: schema}
	bareBytes, err := json.Marshal(bare)
	must.NoError(err, "marshal bare")

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
	must.NoError(err, "marshal with presentation")

	t.Run("presentation does not change encoding", func(t *testing.T) {
		must := require.New(t)
		must.Equal(string(bareBytes), string(withBytes), "Presentation leaked onto the wire")
	})
	t.Run("presentation keys absent", func(t *testing.T) {
		must := require.New(t)
		for _, key := range []string{"Presentation", "inputExamples", "annotations"} {
			must.NotContains(string(withBytes), key, "Presentation field %q must not appear on the default encoding", key)
		}
	})
}

// TestToolPresentation_RoundTrip confirms ToolPresentation and ToolAnnotations
// marshal and unmarshal losslessly when serialised directly (e.g. by an
// adapter that opts to project them), including the *bool unset-vs-false
// distinction on the annotation hints.
func TestToolPresentation_RoundTrip(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err, "marshal")
	var got ToolPresentation
	must.NoError(json.Unmarshal(b, &got), "unmarshal")
	must.NotNil(got.Annotations, "annotations did not round-trip")

	t.Run("read-only hint round-trips", func(t *testing.T) {
		must := require.New(t)
		must.NotNil(got.Annotations.ReadOnlyHint)
		must.True(*got.Annotations.ReadOnlyHint)
	})
	t.Run("unset destructive hint stays nil", func(t *testing.T) {
		must := require.New(t)
		must.Nil(got.Annotations.DestructiveHint)
	})
	t.Run("input examples round-trip", func(t *testing.T) {
		must := require.New(t)
		must.Len(got.InputExamples, 1)
		must.JSONEq(`{"x":"y"}`, string(got.InputExamples[0]))
	})
}
