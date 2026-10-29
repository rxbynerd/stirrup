package types

import (
	"encoding/json"
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

	t.Run("rejects an empty or malformed name", func(t *testing.T) {
		for _, name := range []string{"", "has space", "ünïcode", "a/b", string(make([]byte, 65))} {
			must := require.New(t)
			s := &OutputSchema{Name: name, Schema: json.RawMessage(`{"type":"object"}`)}
			must.Error(s.Validate(), "name %q", name)
		}
	})

	t.Run("rejects a non-object schema", func(t *testing.T) {
		for _, raw := range []string{``, `null`, `[]`, `"string"`, `{"type":`} {
			must := require.New(t)
			s := &OutputSchema{Name: "verdict", Schema: json.RawMessage(raw)}
			must.Error(s.Validate(), "schema %q", raw)
		}
	})
}

// TestStreamParams_MarshalOmitsNilOutputSchema pins that a request without
// a schema serialises byte-identically to before the field existed.
func TestStreamParams_MarshalOmitsNilOutputSchema(t *testing.T) {
	must := require.New(t)
	b, err := json.Marshal(StreamParams{Model: "m"})
	must.NoError(err)
	must.NotContains(string(b), "outputSchema")

	b, err = json.Marshal(StreamParams{Model: "m", OutputSchema: &OutputSchema{Name: "v", Schema: json.RawMessage(`{}`)}})
	must.NoError(err)
	must.Contains(string(b), `"outputSchema":{"name":"v","schema":{}}`)
}
