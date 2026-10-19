package types

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateRunConfig_OutputSchema_RejectsInvalid(t *testing.T) {
	cases := []struct {
		name    string
		schema  string
		wantErr string
	}{
		{"invalid json", `{"type":`, "outputSchema is not valid JSON"},
		{"array root", `[{"type":"object"}]`, "outputSchema must be a JSON Schema object"},
		{"boolean root", `true`, "outputSchema must be a JSON Schema object"},
		{"null root", `null`, "outputSchema must be a JSON Schema object"},
		{"external http ref", `{"$ref":"https://example.com/schema.json"}`, `reference "https://example.com/schema.json" is not a local fragment`},
		{"external file ref", `{"type":"object","properties":{"a":{"$ref":"file:///etc/passwd"}}}`, `reference "file:///etc/passwd" is not a local fragment`},
		{"relative ref", `{"allOf":[{"$ref":"other.json#/x"}]}`, `reference "other.json#/x" is not a local fragment`},
		{"external dynamic ref", `{"$dynamicRef":"https://example.com/meta"}`, `reference "https://example.com/meta" is not a local fragment`},
		{"oversized", `{"description":"` + strings.Repeat("x", maxOutputSchemaBytes) + `"}`, "over the 65536-byte limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.OutputSchema = json.RawMessage(tc.schema)
			require.ErrorContains(t, ValidateRunConfig(c), tc.wantErr)
		})
	}
}

func TestValidateRunConfig_OutputSchema_AcceptsValid(t *testing.T) {
	cases := map[string]string{
		"object schema": `{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`,
		"local refs":    `{"$defs":{"label":{"enum":["bug","feature"]}},"type":"object","properties":{"label":{"$ref":"#/$defs/label"}}}`,
		"empty object":  `{}`,
	}
	for name, schema := range cases {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			c.OutputSchema = json.RawMessage(schema)
			require.NoError(t, ValidateRunConfig(c))
		})
	}

	t.Run("read only mode", func(t *testing.T) {
		c := validConfig()
		c.Mode = "review"
		c.Tools.BuiltIn = []string{"read_file"}
		c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
		c.OutputSchema = json.RawMessage(`{"type":"object"}`)
		require.NoError(t, ValidateRunConfig(c))
	})
}

func TestRunConfig_OutputSchema_JSONRoundTrip(t *testing.T) {
	t.Run("unset field is omitted", func(t *testing.T) {
		out, err := json.Marshal(validConfig())
		require.NoError(t, err)
		require.NotContains(t, string(out), "outputSchema")
	})

	t.Run("set field survives a round trip", func(t *testing.T) {
		c := validConfig()
		c.OutputSchema = json.RawMessage(`{"type":"object","required":["title"]}`)
		out, err := json.Marshal(c)
		require.NoError(t, err)

		var back RunConfig
		require.NoError(t, json.Unmarshal(out, &back))
		require.JSONEq(t, `{"type":"object","required":["title"]}`, string(back.OutputSchema))
	})
}
