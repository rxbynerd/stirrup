package security

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateJSONSchema_AcceptsValidInput(t *testing.T) {
	must := require.New(t)
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"},
			"age": {"type": "integer"}
		},
		"required": ["name"]
	}`)
	input := json.RawMessage(`{"name": "Alice", "age": 30}`)

	must.NoError(ValidateJSONSchema(input, schema))
}

func TestValidateJSONSchema_RejectsMissingRequired(t *testing.T) {
	must := require.New(t)
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		},
		"required": ["name"]
	}`)
	input := json.RawMessage(`{}`)

	must.Error(ValidateJSONSchema(input, schema))
}

func TestValidateJSONSchema_RejectsWrongType(t *testing.T) {
	must := require.New(t)
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"count": {"type": "integer"}
		}
	}`)
	input := json.RawMessage(`{"count": "not-a-number"}`)

	must.Error(ValidateJSONSchema(input, schema))
}

func TestValidateJSONSchema_RejectsFractionForInteger(t *testing.T) {
	must := require.New(t)
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"count": {"type": "integer"}
		}
	}`)
	input := json.RawMessage(`{"count": 3.7}`)

	must.Error(ValidateJSONSchema(input, schema))
}

func TestValidateJSONSchema_RejectsAdditionalPropertiesWhenFalse(t *testing.T) {
	must := require.New(t)
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		},
		"additionalProperties": false
	}`)
	input := json.RawMessage(`{"name": "Alice", "extra": true}`)

	must.Error(ValidateJSONSchema(input, schema))
}

func TestValidateJSONSchema_AllowsAdditionalPropertiesWhenTrue(t *testing.T) {
	must := require.New(t)
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		},
		"additionalProperties": true
	}`)
	input := json.RawMessage(`{"name": "Alice", "extra": true}`)

	must.NoError(ValidateJSONSchema(input, schema))
}

func TestValidateJSONSchema_StripsProtoKeys(t *testing.T) {
	must := require.New(t)
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		},
		"additionalProperties": false
	}`)
	// __proto__ should be stripped before validation, so this should pass.
	input := json.RawMessage(`{"name": "Alice", "__proto__": {"admin": true}}`)

	must.NoError(ValidateJSONSchema(input, schema))
}

func TestValidateJSONSchema_StripsConstructorKeys(t *testing.T) {
	must := require.New(t)
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		},
		"additionalProperties": false
	}`)
	input := json.RawMessage(`{"name": "Alice", "constructor": "bad"}`)

	must.NoError(ValidateJSONSchema(input, schema))
}

func TestValidateJSONSchema_AcceptsNestedProperties(t *testing.T) {
	must := require.New(t)
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"config": {
				"type": "object",
				"properties": {
					"enabled": {"type": "boolean"}
				},
				"required": ["enabled"]
			}
		}
	}`)
	input := json.RawMessage(`{"config": {"enabled": true}}`)

	must.NoError(ValidateJSONSchema(input, schema))
}

func TestValidateJSONSchema_RejectsNestedMissingRequired(t *testing.T) {
	must := require.New(t)
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"config": {
				"type": "object",
				"properties": {
					"enabled": {"type": "boolean"}
				},
				"required": ["enabled"]
			}
		}
	}`)
	input := json.RawMessage(`{"config": {}}`)

	must.Error(ValidateJSONSchema(input, schema))
}

func TestValidateJSONSchema_RejectsTopLevelTypeMismatch(t *testing.T) {
	must := require.New(t)
	schema := json.RawMessage(`{"type": "object"}`)
	input := json.RawMessage(`"just a string"`)

	must.Error(ValidateJSONSchema(input, schema))
}

func TestValidateJSONSchema_RejectsInvalidJSON(t *testing.T) {
	must := require.New(t)
	schema := json.RawMessage(`{"type": "object"}`)
	input := json.RawMessage(`{invalid`)

	must.Error(ValidateJSONSchema(input, schema))
}

func TestValidateJSONSchema_TypeKeyword(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		input  string
		ok     bool
	}{
		{"string ok", `{"type":"string"}`, `"hello"`, true},
		{"string bad", `{"type":"string"}`, `42`, false},
		{"number ok", `{"type":"number"}`, `3.14`, true},
		{"number bad", `{"type":"number"}`, `"nope"`, false},
		{"boolean ok", `{"type":"boolean"}`, `true`, true},
		{"boolean bad", `{"type":"boolean"}`, `1`, false},
		{"array ok", `{"type":"array"}`, `[1,2]`, true},
		{"array bad", `{"type":"array"}`, `{}`, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			err := ValidateJSONSchema(json.RawMessage(tt.input), json.RawMessage(tt.schema))
			if tt.ok {
				must.NoError(err)
			} else {
				must.Error(err)
			}
		})
	}
}

func TestValidateJSONSchema_OneOfMatchesExactlyOneBranch(t *testing.T) {
	schema := json.RawMessage(`{
		"oneOf": [
			{"type": "string"},
			{"type": "integer"}
		]
	}`)

	t.Run("matches string", func(t *testing.T) {
		must := require.New(t)
		must.NoError(ValidateJSONSchema(json.RawMessage(`"hello"`), schema))
	})
	t.Run("matches integer", func(t *testing.T) {
		must := require.New(t)
		must.NoError(ValidateJSONSchema(json.RawMessage(`42`), schema))
	})
	t.Run("rejects value matching no branch", func(t *testing.T) {
		must := require.New(t)
		must.Error(ValidateJSONSchema(json.RawMessage(`true`), schema))
	})
	t.Run("rejects value matching two branches", func(t *testing.T) {
		must := require.New(t)
		bothSchema := json.RawMessage(`{
			"oneOf": [
				{"type": "number"},
				{"type": "number"}
			]
		}`)
		must.Error(ValidateJSONSchema(json.RawMessage(`42`), bothSchema))
	})
}

func TestValidateJSONSchema_AnyOfMatchesAnyBranch(t *testing.T) {
	schema := json.RawMessage(`{
		"anyOf": [
			{"type": "string"},
			{"type": "number"}
		]
	}`)

	t.Run("matches first", func(t *testing.T) {
		must := require.New(t)
		must.NoError(ValidateJSONSchema(json.RawMessage(`"hello"`), schema))
	})
	t.Run("matches second", func(t *testing.T) {
		must := require.New(t)
		must.NoError(ValidateJSONSchema(json.RawMessage(`3.14`), schema))
	})
	t.Run("rejects value matching no branch", func(t *testing.T) {
		must := require.New(t)
		must.Error(ValidateJSONSchema(json.RawMessage(`true`), schema))
	})
}

func TestValidateJSONSchema_AllOfRequiresEveryBranch(t *testing.T) {
	schema := json.RawMessage(`{
		"allOf": [
			{"type": "object", "required": ["name"]},
			{"type": "object", "required": ["age"]}
		]
	}`)

	t.Run("satisfies all", func(t *testing.T) {
		must := require.New(t)
		input := json.RawMessage(`{"name": "Alice", "age": 30}`)
		must.NoError(ValidateJSONSchema(input, schema))
	})
	t.Run("missing from second", func(t *testing.T) {
		must := require.New(t)
		input := json.RawMessage(`{"name": "Alice"}`)
		must.Error(ValidateJSONSchema(input, schema))
	})
}

func TestValidateJSONSchema_EnumRejectsValuesOutsideList(t *testing.T) {
	schema := json.RawMessage(`{"enum": ["red", "green", "blue"]}`)

	t.Run("valid value", func(t *testing.T) {
		must := require.New(t)
		must.NoError(ValidateJSONSchema(json.RawMessage(`"green"`), schema))
	})
	t.Run("invalid value", func(t *testing.T) {
		must := require.New(t)
		must.Error(ValidateJSONSchema(json.RawMessage(`"yellow"`), schema))
	})
}

func TestValidateJSONSchema_PatternRejectsNonMatchingStrings(t *testing.T) {
	schema := json.RawMessage(`{"type": "string", "pattern": "^[a-z]+$"}`)

	t.Run("matches pattern", func(t *testing.T) {
		must := require.New(t)
		must.NoError(ValidateJSONSchema(json.RawMessage(`"hello"`), schema))
	})
	t.Run("does not match", func(t *testing.T) {
		must := require.New(t)
		must.Error(ValidateJSONSchema(json.RawMessage(`"Hello123"`), schema))
	})
}

func TestValidateJSONSchema_MinimumAndMaximumAreInclusive(t *testing.T) {
	schema := json.RawMessage(`{"type": "number", "minimum": 0, "maximum": 100}`)

	t.Run("in range", func(t *testing.T) {
		must := require.New(t)
		must.NoError(ValidateJSONSchema(json.RawMessage(`50`), schema))
	})
	t.Run("at minimum", func(t *testing.T) {
		must := require.New(t)
		must.NoError(ValidateJSONSchema(json.RawMessage(`0`), schema))
	})
	t.Run("at maximum", func(t *testing.T) {
		must := require.New(t)
		must.NoError(ValidateJSONSchema(json.RawMessage(`100`), schema))
	})
	t.Run("below minimum", func(t *testing.T) {
		must := require.New(t)
		must.Error(ValidateJSONSchema(json.RawMessage(`-1`), schema))
	})
	t.Run("above maximum", func(t *testing.T) {
		must := require.New(t)
		must.Error(ValidateJSONSchema(json.RawMessage(`101`), schema))
	})
}

func TestValidateJSONSchema_ItemsChecksEachElement(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "array",
		"items": {"type": "string"}
	}`)

	t.Run("valid items", func(t *testing.T) {
		must := require.New(t)
		must.NoError(ValidateJSONSchema(json.RawMessage(`["a", "b", "c"]`), schema))
	})
	t.Run("empty array", func(t *testing.T) {
		must := require.New(t)
		must.NoError(ValidateJSONSchema(json.RawMessage(`[]`), schema))
	})
	t.Run("invalid item type", func(t *testing.T) {
		must := require.New(t)
		must.Error(ValidateJSONSchema(json.RawMessage(`["a", 42, "c"]`), schema))
	})
}

func TestValidateJSONSchema_ResolvesLocalRefs(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"address": {"$ref": "#/$defs/address"}
		},
		"$defs": {
			"address": {
				"type": "object",
				"properties": {
					"street": {"type": "string"},
					"city": {"type": "string"}
				},
				"required": ["street", "city"]
			}
		}
	}`)

	t.Run("valid ref", func(t *testing.T) {
		must := require.New(t)
		input := json.RawMessage(`{"address": {"street": "123 Main St", "city": "Springfield"}}`)
		must.NoError(ValidateJSONSchema(input, schema))
	})
	t.Run("invalid ref missing field", func(t *testing.T) {
		must := require.New(t)
		input := json.RawMessage(`{"address": {"street": "123 Main St"}}`)
		must.Error(ValidateJSONSchema(input, schema))
	})
	t.Run("invalid ref wrong type", func(t *testing.T) {
		must := require.New(t)
		input := json.RawMessage(`{"address": "not an object"}`)
		must.Error(ValidateJSONSchema(input, schema))
	})
}

func TestValidateJSONSchema_MinLengthAndMaxLengthAreInclusive(t *testing.T) {
	schema := json.RawMessage(`{"type": "string", "minLength": 2, "maxLength": 5}`)

	t.Run("valid length", func(t *testing.T) {
		must := require.New(t)
		must.NoError(ValidateJSONSchema(json.RawMessage(`"abc"`), schema))
	})
	t.Run("at minLength", func(t *testing.T) {
		must := require.New(t)
		must.NoError(ValidateJSONSchema(json.RawMessage(`"ab"`), schema))
	})
	t.Run("at maxLength", func(t *testing.T) {
		must := require.New(t)
		must.NoError(ValidateJSONSchema(json.RawMessage(`"abcde"`), schema))
	})
	t.Run("too short", func(t *testing.T) {
		must := require.New(t)
		must.Error(ValidateJSONSchema(json.RawMessage(`"a"`), schema))
	})
	t.Run("too long", func(t *testing.T) {
		must := require.New(t)
		must.Error(ValidateJSONSchema(json.RawMessage(`"abcdef"`), schema))
	})
}

func TestValidateJSONSchema_BlocksExternalRefs(t *testing.T) {
	t.Run("file ref blocked", func(t *testing.T) {
		must := require.New(t)
		schema := json.RawMessage(`{"$ref": "file:///etc/passwd"}`)
		must.Error(ValidateJSONSchema(json.RawMessage(`{}`), schema))
	})
	t.Run("http ref blocked", func(t *testing.T) {
		must := require.New(t)
		schema := json.RawMessage(`{"$ref": "http://attacker.com/schema.json"}`)
		must.Error(ValidateJSONSchema(json.RawMessage(`{}`), schema))
	})
	t.Run("inline ref still works", func(t *testing.T) {
		must := require.New(t)
		schema := json.RawMessage(`{
			"type": "object",
			"properties": {"val": {"$ref": "#/$defs/pos"}},
			"$defs": {"pos": {"type": "integer", "minimum": 0}}
		}`)
		must.NoError(ValidateJSONSchema(json.RawMessage(`{"val": 5}`), schema))
	})
}

func TestValidateJSONSchema_NilOrEmptySchemaAcceptsAnyInput(t *testing.T) {
	input := json.RawMessage(`{"any": "input"}`)

	t.Run("nil schema accepts any input", func(t *testing.T) {
		must := require.New(t)
		must.NoError(ValidateJSONSchema(input, nil))
	})
	t.Run("empty schema accepts any input", func(t *testing.T) {
		must := require.New(t)
		must.NoError(ValidateJSONSchema(input, json.RawMessage("")))
	})
	t.Run("invalid schema JSON returns error", func(t *testing.T) {
		must := require.New(t)
		must.Error(ValidateJSONSchema(input, json.RawMessage("{bad")))
	})
}

func TestStripDangerousKeysFromInput_ReportsDroppedKeys(t *testing.T) {
	t.Run("clean input is unchanged and reports nothing", func(t *testing.T) {
		must := require.New(t)
		in := json.RawMessage(`{"a":1,"b":[2,3]}`)
		out, dropped, err := StripDangerousKeysFromInput(in)
		must.NoError(err)
		must.Empty(dropped)
		must.Equal(string(in), string(out))
	})

	t.Run("reports top-level __proto__", func(t *testing.T) {
		must := require.New(t)
		in := json.RawMessage(`{"name":"alice","__proto__":{"admin":true}}`)
		_, dropped, err := StripDangerousKeysFromInput(in)
		must.NoError(err)
		must.Contains(dropped, "__proto__")
	})

	t.Run("reports nested constructor", func(t *testing.T) {
		must := require.New(t)
		in := json.RawMessage(`{"outer":{"constructor":"x","keep":1}}`)
		_, dropped, err := StripDangerousKeysFromInput(in)
		must.NoError(err)
		must.Contains(dropped, "constructor")
	})

	t.Run("reports both dangerous keys", func(t *testing.T) {
		must := require.New(t)
		in := json.RawMessage(`{"__proto__":1,"constructor":2,"x":3}`)
		_, dropped, err := StripDangerousKeysFromInput(in)
		must.NoError(err)
		must.Contains(dropped, "__proto__")
		must.Contains(dropped, "constructor")
	})

	t.Run("invalid JSON returns error", func(t *testing.T) {
		must := require.New(t)
		_, _, err := StripDangerousKeysFromInput(json.RawMessage(`{bad`))
		must.Error(err)
	})

	t.Run("empty input reports nothing and returns nil", func(t *testing.T) {
		must := require.New(t)
		out, dropped, err := StripDangerousKeysFromInput(nil)
		must.NoError(err)
		must.Empty(dropped)
		must.Nil(out)
	})
}

func TestCompileJSONSchema_ValidatesSchemas(t *testing.T) {
	cases := []struct {
		name    string
		schema  string
		wantErr string
	}{
		{"valid", `{"type":"object","properties":{"a":{"$ref":"#/$defs/a"}},"$defs":{"a":{"type":"string"}}}`, ""},
		{"invalid json", `{"type":`, "invalid schema JSON"},
		{"unknown type name", `{"type":"strng"}`, "failed to compile schema"},
		{"wrong keyword type", `{"type":"object","required":"title"}`, "failed to compile schema"},
		{"external ref", `{"$ref":"https://example.com/schema.json"}`, "failed to compile schema"},
		{"dangling local ref", `{"$ref":"#/$defs/missing"}`, "failed to compile schema"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			err := CompileJSONSchema(json.RawMessage(tc.schema))
			if tc.wantErr == "" {
				must.NoError(err)
				return
			}
			must.ErrorContains(err, tc.wantErr)
		})
	}
}

func TestValidateJSONSchema_ErrorOmitsWorkingDirectory(t *testing.T) {
	must := require.New(t)
	wd, err := os.Getwd()
	must.NoError(err)

	err = ValidateJSONSchema(json.RawMessage(`{}`), json.RawMessage(`{"type":"object","required":["title"]}`))
	must.ErrorContains(err, "missing property 'title'")
	must.NotContains(err.Error(), wd)
	must.NotContains(err.Error(), "file://")
}
