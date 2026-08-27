package types

import (
	"encoding/json"
	"testing"
)

func TestNormalizeToolInput(t *testing.T) {
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
			if got := string(NormalizeToolInput(tt.in)); got != tt.want {
				t.Errorf("NormalizeToolInput(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// A zero-argument tool call decodes to a nil map, which re-encodes as JSON
// null. Normalizing the marshalled bytes is what keeps the stored tool_use
// block a valid object.
func TestNormalizeToolInput_NilMapMarshalsToObject(t *testing.T) {
	var input map[string]any
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal nil map: %v", err)
	}
	if string(raw) != "null" {
		t.Fatalf("precondition: nil map marshals to %q, want null", raw)
	}
	if got := string(NormalizeToolInput(raw)); got != "{}" {
		t.Errorf("NormalizeToolInput(marshalled nil map) = %q, want {}", got)
	}
}
