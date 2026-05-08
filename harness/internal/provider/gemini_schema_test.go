package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestConvertSchema_Cases exercises the JSON Schema → Gemini Schema converter
// across all supported and rejected shapes. Each case either pins an expected
// JSON shape on the converted output (decoded into a generic map for ordered-
// independent comparison) or expects a specific error substring.
func TestConvertSchema_Cases(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		want      string // expected canonical JSON; "" when expectErr is set
		expectErr string // substring of expected error; "" when success
	}{
		{
			name: "trivial string",
			in:   `{"type":"string"}`,
			want: `{"type":"STRING"}`,
		},
		{
			name: "object with two properties and required",
			in: `{
				"type":"object",
				"properties":{
					"path":{"type":"string","description":"file path"},
					"count":{"type":"integer"}
				},
				"required":["path"]
			}`,
			want: `{"type":"OBJECT","properties":{"path":{"type":"STRING","description":"file path"},"count":{"type":"INTEGER"}},"required":["path"]}`,
		},
		{
			name: "array of strings",
			in:   `{"type":"array","items":{"type":"string"}}`,
			want: `{"type":"ARRAY","items":{"type":"STRING"}}`,
		},
		{
			name: "nested object three levels",
			in: `{
				"type":"object",
				"properties":{
					"outer":{
						"type":"object",
						"properties":{
							"inner":{
								"type":"object",
								"properties":{
									"leaf":{"type":"boolean"}
								}
							}
						}
					}
				}
			}`,
			want: `{"type":"OBJECT","properties":{"outer":{"type":"OBJECT","properties":{"inner":{"type":"OBJECT","properties":{"leaf":{"type":"BOOLEAN"}}}}}}}`,
		},
		{
			name: "enum passes through",
			in:   `{"type":"string","enum":["a","b","c"]}`,
			want: `{"type":"STRING","enum":["a","b","c"]}`,
		},
		{
			name: "type array with null marks nullable",
			in:   `{"type":["string","null"]}`,
			want: `{"type":"STRING","nullable":true}`,
		},
		{
			name: "type array null-first marks nullable",
			in:   `{"type":["null","integer"]}`,
			want: `{"type":"INTEGER","nullable":true}`,
		},
		{
			name: "anyOf with one branch plus null is nullable",
			in:   `{"anyOf":[{"type":"string"},{"type":"null"}]}`,
			want: `{"type":"STRING","nullable":true}`,
		},
		{
			name: "additionalProperties dropped silently",
			in:   `{"type":"object","additionalProperties":false,"properties":{"x":{"type":"string"}}}`,
			want: `{"type":"OBJECT","properties":{"x":{"type":"STRING"}}}`,
		},
		{
			name: "metadata keywords dropped",
			in:   `{"$schema":"https://json-schema.org/draft/2020-12/schema","$id":"foo","$comment":"hi","type":"string"}`,
			want: `{"type":"STRING"}`,
		},
		{
			name: "validation keywords pass through",
			in:   `{"type":"string","minLength":1,"maxLength":80,"pattern":"^[a-z]+$"}`,
			want: `{"type":"STRING","minLength":1,"maxLength":80,"pattern":"^[a-z]+$"}`,
		},
		{
			name: "unknown keyword passes through",
			in:   `{"type":"string","x-vertex-extension":true}`,
			want: `{"type":"STRING","x-vertex-extension":true}`,
		},
		{
			name: "empty input produces empty object",
			in:   ``,
			want: `{}`,
		},
		{
			name:      "three-element type array errors",
			in:        `{"type":["string","integer","null"]}`,
			expectErr: "exactly two elements",
		},
		{
			name:      "type array with two non-null errors",
			in:        `{"type":["string","integer"]}`,
			expectErr: "exactly one \"null\"",
		},
		{
			name:      "oneOf two non-null branches errors",
			in:        `{"oneOf":[{"type":"string"},{"type":"integer"}]}`,
			expectErr: "discriminated unions",
		},
		{
			// Mirror of the anyOf nullable-collapse case for the oneOf
			// keyword. Both keywords share the same collapse logic
			// (collapseUnionBranches), but exercising both keeps the
			// converter symmetric and guards against a future refactor
			// that splits one branch from the other.
			name: "oneOf with one branch plus null is nullable",
			in:   `{"oneOf":[{"type":"string"},{"type":"null"}]}`,
			want: `{"type":"STRING","nullable":true}`,
		},
		{
			name:      "ref errors",
			in:        `{"$ref":"#/$defs/Foo"}`,
			expectErr: "$ref is not supported",
		},
		{
			name:      "allOf errors",
			in:        `{"allOf":[{"type":"object"}]}`,
			expectErr: "allOf is not supported",
		},
		{
			name:      "type null errors",
			in:        `{"type":"null"}`,
			expectErr: "type \"null\" is not supported",
		},
		{
			name:      "unknown type errors",
			in:        `{"type":"date"}`,
			expectErr: "unknown type",
		},
		{
			name:      "anyOf empty branches errors",
			in:        `{"anyOf":[]}`,
			expectErr: "no non-null branches",
		},
		{
			name:      "anyOf only null errors",
			in:        `{"anyOf":[{"type":"null"}]}`,
			expectErr: "no non-null branches",
		},
		{
			name:      "malformed JSON errors",
			in:        `{"type":`,
			expectErr: "parse schema",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ConvertSchema(json.RawMessage(tt.in))
			if tt.expectErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (output=%s)", tt.expectErr, got)
				}
				if !strings.Contains(err.Error(), tt.expectErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.expectErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertJSONEqual(t, got, tt.want)
		})
	}
}

// TestConvertSchema_RealisticToolSchema exercises the converter on a shape
// representative of the harness's built-in tool schemas: an object with
// optional fields, an array of strings, and pass-through validation
// keywords. This guards against regressions caused by reshuffling the
// internal walking order.
func TestConvertSchema_RealisticToolSchema(t *testing.T) {
	in := `{
		"type":"object",
		"properties":{
			"path":{"type":"string","description":"target path","minLength":1},
			"recursive":{"type":"boolean","default":false},
			"include":{"type":"array","items":{"type":"string"},"maxItems":50}
		},
		"required":["path"],
		"additionalProperties":false
	}`
	got, err := ConvertSchema(json.RawMessage(in))
	if err != nil {
		t.Fatalf("ConvertSchema returned error: %v", err)
	}
	want := `{
		"type":"OBJECT",
		"properties":{
			"path":{"type":"STRING","description":"target path","minLength":1},
			"recursive":{"type":"BOOLEAN","default":false},
			"include":{"type":"ARRAY","items":{"type":"STRING"},"maxItems":50}
		},
		"required":["path"]
	}`
	assertJSONEqual(t, got, want)
}

// TestConvertSchema_NullableNestedProperty asserts that nullability is
// applied at the leaf level rather than bubbling up to the parent schema —
// a common failure mode when the type-array translation accidentally writes
// to the wrong node.
func TestConvertSchema_NullableNestedProperty(t *testing.T) {
	in := `{
		"type":"object",
		"properties":{
			"label":{"type":["string","null"]}
		}
	}`
	got, err := ConvertSchema(json.RawMessage(in))
	if err != nil {
		t.Fatalf("ConvertSchema returned error: %v", err)
	}
	want := `{"type":"OBJECT","properties":{"label":{"type":"STRING","nullable":true}}}`
	assertJSONEqual(t, got, want)
}

// assertJSONEqual decodes both the actual and expected JSON into generic
// values and compares them, eliminating any field-ordering differences.
func assertJSONEqual(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var gotV, wantV any
	if err := json.Unmarshal(got, &gotV); err != nil {
		t.Fatalf("decode got: %v\nbody=%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &wantV); err != nil {
		t.Fatalf("decode want: %v\nbody=%s", err, want)
	}
	gotCanon, err := json.Marshal(gotV)
	if err != nil {
		t.Fatalf("re-marshal got: %v", err)
	}
	wantCanon, err := json.Marshal(wantV)
	if err != nil {
		t.Fatalf("re-marshal want: %v", err)
	}
	if string(gotCanon) != string(wantCanon) {
		t.Errorf("schema mismatch:\n got: %s\nwant: %s", gotCanon, wantCanon)
	}
}
