package types

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOutputSchema_Validate pins the two checks adapters rely on before
// emitting a schema: the name obeys the OpenAI schema-name grammar and the
// schema is a JSON object.
func TestOutputSchema_Validate(t *testing.T) {
	t.Run("accepts a named object schema", func(t *testing.T) {
		must := require.New(t)
		s := &OutputSchema{Name: "verdict_v1", Schema: json.RawMessage(`{"type":"object"}`)}
		must.NoError(s.Validate())
	})

	t.Run("rejects nil", func(t *testing.T) {
		must := require.New(t)
		var s *OutputSchema
		must.Error(s.Validate())
	})

	badNames := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"space", "has space"},
		{"non-ascii", "ünïcode"},
		{"slash", "a/b"},
		{"65 characters", strings.Repeat("a", 65)},
	}
	for _, tc := range badNames {
		t.Run("rejects name "+tc.name, func(t *testing.T) {
			must := require.New(t)
			s := &OutputSchema{Name: tc.value, Schema: json.RawMessage(`{"type":"object"}`)}
			must.Error(s.Validate())
		})
	}

	badSchemas := []struct {
		name string
		raw  string
	}{
		{"empty", ``},
		{"null", `null`},
		{"array", `[]`},
		{"string", `"string"`},
		{"truncated", `{"type":`},
	}
	for _, tc := range badSchemas {
		t.Run("rejects schema "+tc.name, func(t *testing.T) {
			must := require.New(t)
			s := &OutputSchema{Name: "verdict", Schema: json.RawMessage(tc.raw)}
			must.Error(s.Validate())
		})
	}
}

// TestStreamParams_MarshalOmitsNilOutputSchema pins that the outputSchema
// key is omitted when no schema is set and carries name and schema when
// one is.
func TestStreamParams_MarshalOmitsNilOutputSchema(t *testing.T) {
	must := require.New(t)
	b, err := json.Marshal(StreamParams{Model: "m"})
	must.NoError(err)
	must.NotContains(string(b), "outputSchema")

	b, err = json.Marshal(StreamParams{Model: "m", OutputSchema: &OutputSchema{Name: "v", Schema: json.RawMessage(`{}`)}})
	must.NoError(err)
	must.Contains(string(b), `"outputSchema":{"name":"v","schema":{}}`)
}
