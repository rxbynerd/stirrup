package types

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateRunConfig_OutputSchema(t *testing.T) {
	cases := []struct {
		name    string
		schema  string
		wantErr string
	}{
		{"invalid_json", `{"type":`, "outputSchema is not valid JSON"},
		{"array_root", `[{"type":"object"}]`, "outputSchema must be a JSON Schema object"},
		{"boolean_root", `true`, "outputSchema must be a JSON Schema object"},
		{"null_root", `null`, "outputSchema must be a JSON Schema object"},
		{"external_ref_http", `{"$ref":"https://example.com/schema.json"}`, `reference "https://example.com/schema.json" is not a local fragment`},
		{"external_ref_file", `{"type":"object","properties":{"a":{"$ref":"file:///etc/passwd"}}}`, `reference "file:///etc/passwd" is not a local fragment`},
		{"relative_ref", `{"allOf":[{"$ref":"other.json#/x"}]}`, `reference "other.json#/x" is not a local fragment`},
		{"external_dynamic_ref", `{"$dynamicRef":"https://example.com/meta"}`, `reference "https://example.com/meta" is not a local fragment`},
		{"oversized", `{"description":"` + strings.Repeat("x", maxOutputSchemaBytes) + `"}`, "over the 65536-byte limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.OutputSchema = json.RawMessage(tc.schema)
			err := ValidateRunConfig(c)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateRunConfig error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateRunConfig_OutputSchemaAccepted(t *testing.T) {
	cases := map[string]string{
		"object_schema": `{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`,
		"local_refs":    `{"$defs":{"label":{"enum":["bug","feature"]}},"type":"object","properties":{"label":{"$ref":"#/$defs/label"}}}`,
		"empty_object":  `{}`,
	}
	for name, schema := range cases {
		t.Run(name, func(t *testing.T) {
			c := validConfig()
			c.OutputSchema = json.RawMessage(schema)
			if err := ValidateRunConfig(c); err != nil {
				t.Fatalf("ValidateRunConfig error = %v, want nil", err)
			}
		})
	}

	t.Run("read_only_mode", func(t *testing.T) {
		c := validConfig()
		c.Mode = "review"
		c.Tools.BuiltIn = []string{"read_file"}
		c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
		c.OutputSchema = json.RawMessage(`{"type":"object"}`)
		if err := ValidateRunConfig(c); err != nil {
			t.Fatalf("ValidateRunConfig error = %v, want nil for a read-only run", err)
		}
	})
}

func TestRunConfig_OutputSchemaJSONRoundTrip(t *testing.T) {
	c := validConfig()
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "outputSchema") {
		t.Errorf("unset outputSchema serialised: %s", out)
	}

	c.OutputSchema = json.RawMessage(`{"type":"object","required":["title"]}`)
	out, err = json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back RunConfig
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(back.OutputSchema) != `{"type":"object","required":["title"]}` {
		t.Errorf("OutputSchema round trip = %s", back.OutputSchema)
	}
}
