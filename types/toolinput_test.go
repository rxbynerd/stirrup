package types

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeToolInput_NormalizesEmptyAndKeepsObjects(t *testing.T) {
	tests := []struct {
		name string
		in   json.RawMessage
		want string
	}{
		{"nil", nil, "{}"},
		{"empty", json.RawMessage(``), "{}"},
		{"literal null", json.RawMessage(`null`), "{}"},
		{"padded null", json.RawMessage("  null\n"), "{}"},
		{"whitespace only", json.RawMessage("  \n\t"), "{}"},
		{"empty object preserved", json.RawMessage(`{}`), `{}`},
		{"populated object preserved", json.RawMessage(`{"path":"main.go"}`), `{"path":"main.go"}`},
		{"null-valued property preserved", json.RawMessage(`{"path":null}`), `{"path":null}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			got := string(NormalizeToolInput(tt.in))
			must.Equal(tt.want, got)
		})
	}
}

// A zero-argument tool call decodes to a nil map, which re-encodes as JSON
// null. Normalizing the marshalled bytes is what keeps the stored tool_use
// block a valid object.
func TestNormalizeToolInput_NilMapMarshalsToObject(t *testing.T) {
	must := require.New(t)
	var input map[string]any
	raw, err := json.Marshal(input)
	must.NoError(err)
	must.Equal("null", string(raw), "precondition: nil map marshals to null")

	got := string(NormalizeToolInput(raw))
	must.Equal("{}", got)
}
