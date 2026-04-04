package security

import (
	"encoding/json"
	"testing"
)

func TestValidateJSONSchema_ValidInput(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"},
			"age": {"type": "integer"}
		},
		"required": ["name"]
	}`)
	input := json.RawMessage(`{"name": "Alice", "age": 30}`)

	if err := ValidateJSONSchema(input, schema); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateJSONSchema_MissingRequired(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		},
		"required": ["name"]
	}`)
	input := json.RawMessage(`{}`)

	err := ValidateJSONSchema(input, schema)
	if err == nil {
		t.Fatal("expected error for missing required field, got nil")
	}
}

func TestValidateJSONSchema_WrongType(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"count": {"type": "integer"}
		}
	}`)
	input := json.RawMessage(`{"count": "not-a-number"}`)

	err := ValidateJSONSchema(input, schema)
	if err == nil {
		t.Fatal("expected error for wrong type, got nil")
	}
}

func TestValidateJSONSchema_IntegerWithFraction(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"count": {"type": "integer"}
		}
	}`)
	input := json.RawMessage(`{"count": 3.7}`)

	err := ValidateJSONSchema(input, schema)
	if err == nil {
		t.Fatal("expected error for non-integer number, got nil")
	}
}

func TestValidateJSONSchema_AdditionalPropertiesFalse(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		},
		"additionalProperties": false
	}`)
	input := json.RawMessage(`{"name": "Alice", "extra": true}`)

	err := ValidateJSONSchema(input, schema)
	if err == nil {
		t.Fatal("expected error for additional properties, got nil")
	}
}

func TestValidateJSONSchema_AdditionalPropertiesTrue(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		},
		"additionalProperties": true
	}`)
	input := json.RawMessage(`{"name": "Alice", "extra": true}`)

	if err := ValidateJSONSchema(input, schema); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateJSONSchema_StripsProtoKeys(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		},
		"additionalProperties": false
	}`)
	// __proto__ should be stripped before validation, so this should pass.
	input := json.RawMessage(`{"name": "Alice", "__proto__": {"admin": true}}`)

	if err := ValidateJSONSchema(input, schema); err != nil {
		t.Fatalf("unexpected error (proto should be stripped): %v", err)
	}
}

func TestValidateJSONSchema_StripsConstructorKeys(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string"}
		},
		"additionalProperties": false
	}`)
	input := json.RawMessage(`{"name": "Alice", "constructor": "bad"}`)

	if err := ValidateJSONSchema(input, schema); err != nil {
		t.Fatalf("unexpected error (constructor should be stripped): %v", err)
	}
}

func TestValidateJSONSchema_NestedProperties(t *testing.T) {
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

	if err := ValidateJSONSchema(input, schema); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateJSONSchema_NestedMissingRequired(t *testing.T) {
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

	err := ValidateJSONSchema(input, schema)
	if err == nil {
		t.Fatal("expected error for nested missing required, got nil")
	}
}

func TestValidateJSONSchema_TopLevelTypeMismatch(t *testing.T) {
	schema := json.RawMessage(`{"type": "object"}`)
	input := json.RawMessage(`"just a string"`)

	err := ValidateJSONSchema(input, schema)
	if err == nil {
		t.Fatal("expected error for top-level type mismatch, got nil")
	}
}

func TestValidateJSONSchema_InvalidJSON(t *testing.T) {
	schema := json.RawMessage(`{"type": "object"}`)
	input := json.RawMessage(`{invalid`)

	err := ValidateJSONSchema(input, schema)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestValidateJSONSchema_AllTypes(t *testing.T) {
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
			err := ValidateJSONSchema(json.RawMessage(tt.input), json.RawMessage(tt.schema))
			if tt.ok && err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
			if !tt.ok && err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestValidateJSONSchema_OneOf(t *testing.T) {
	schema := json.RawMessage(`{
		"oneOf": [
			{"type": "string"},
			{"type": "integer"}
		]
	}`)

	t.Run("matches string", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`"hello"`), schema); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("matches integer", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`42`), schema); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("matches neither", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`true`), schema); err == nil {
			t.Fatal("expected error for value matching neither oneOf branch")
		}
	})
	t.Run("matches both fails", func(t *testing.T) {
		// A number is both "number" and satisfies no constraints, so use
		// two schemas that an integer would match both of.
		bothSchema := json.RawMessage(`{
			"oneOf": [
				{"type": "number"},
				{"type": "number"}
			]
		}`)
		if err := ValidateJSONSchema(json.RawMessage(`42`), bothSchema); err == nil {
			t.Fatal("expected error when value matches more than one oneOf branch")
		}
	})
}

func TestValidateJSONSchema_AnyOf(t *testing.T) {
	schema := json.RawMessage(`{
		"anyOf": [
			{"type": "string"},
			{"type": "number"}
		]
	}`)

	t.Run("matches first", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`"hello"`), schema); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("matches second", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`3.14`), schema); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("matches none", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`true`), schema); err == nil {
			t.Fatal("expected error for value matching no anyOf branch")
		}
	})
}

func TestValidateJSONSchema_AllOf(t *testing.T) {
	schema := json.RawMessage(`{
		"allOf": [
			{"type": "object", "required": ["name"]},
			{"type": "object", "required": ["age"]}
		]
	}`)

	t.Run("satisfies all", func(t *testing.T) {
		input := json.RawMessage(`{"name": "Alice", "age": 30}`)
		if err := ValidateJSONSchema(input, schema); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("missing from second", func(t *testing.T) {
		input := json.RawMessage(`{"name": "Alice"}`)
		if err := ValidateJSONSchema(input, schema); err == nil {
			t.Fatal("expected error when allOf constraint is not fully satisfied")
		}
	})
}

func TestValidateJSONSchema_Enum(t *testing.T) {
	schema := json.RawMessage(`{"enum": ["red", "green", "blue"]}`)

	t.Run("valid value", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`"green"`), schema); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("invalid value", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`"yellow"`), schema); err == nil {
			t.Fatal("expected error for value not in enum")
		}
	})
}

func TestValidateJSONSchema_Pattern(t *testing.T) {
	schema := json.RawMessage(`{"type": "string", "pattern": "^[a-z]+$"}`)

	t.Run("matches pattern", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`"hello"`), schema); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("does not match", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`"Hello123"`), schema); err == nil {
			t.Fatal("expected error for string not matching pattern")
		}
	})
}

func TestValidateJSONSchema_MinMax(t *testing.T) {
	schema := json.RawMessage(`{"type": "number", "minimum": 0, "maximum": 100}`)

	t.Run("in range", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`50`), schema); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("at minimum", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`0`), schema); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("at maximum", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`100`), schema); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("below minimum", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`-1`), schema); err == nil {
			t.Fatal("expected error for value below minimum")
		}
	})
	t.Run("above maximum", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`101`), schema); err == nil {
			t.Fatal("expected error for value above maximum")
		}
	})
}

func TestValidateJSONSchema_ArrayItems(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "array",
		"items": {"type": "string"}
	}`)

	t.Run("valid items", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`["a", "b", "c"]`), schema); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("empty array", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`[]`), schema); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("invalid item type", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`["a", 42, "c"]`), schema); err == nil {
			t.Fatal("expected error for array item with wrong type")
		}
	})
}

func TestValidateJSONSchema_DefsAndRef(t *testing.T) {
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
		input := json.RawMessage(`{"address": {"street": "123 Main St", "city": "Springfield"}}`)
		if err := ValidateJSONSchema(input, schema); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("invalid ref missing field", func(t *testing.T) {
		input := json.RawMessage(`{"address": {"street": "123 Main St"}}`)
		if err := ValidateJSONSchema(input, schema); err == nil {
			t.Fatal("expected error for missing required field in $ref schema")
		}
	})
	t.Run("invalid ref wrong type", func(t *testing.T) {
		input := json.RawMessage(`{"address": "not an object"}`)
		if err := ValidateJSONSchema(input, schema); err == nil {
			t.Fatal("expected error for wrong type against $ref schema")
		}
	})
}

func TestValidateJSONSchema_MinMaxLength(t *testing.T) {
	schema := json.RawMessage(`{"type": "string", "minLength": 2, "maxLength": 5}`)

	t.Run("valid length", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`"abc"`), schema); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("at minLength", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`"ab"`), schema); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("at maxLength", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`"abcde"`), schema); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("too short", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`"a"`), schema); err == nil {
			t.Fatal("expected error for string below minLength")
		}
	})
	t.Run("too long", func(t *testing.T) {
		if err := ValidateJSONSchema(json.RawMessage(`"abcdef"`), schema); err == nil {
			t.Fatal("expected error for string above maxLength")
		}
	})
}

func TestValidateJSONSchema_ExternalRefBlocked(t *testing.T) {
	t.Run("file ref blocked", func(t *testing.T) {
		schema := json.RawMessage(`{"$ref": "file:///etc/passwd"}`)
		err := ValidateJSONSchema(json.RawMessage(`{}`), schema)
		if err == nil {
			t.Fatal("expected error for external file:// $ref")
		}
	})
	t.Run("http ref blocked", func(t *testing.T) {
		schema := json.RawMessage(`{"$ref": "http://attacker.com/schema.json"}`)
		err := ValidateJSONSchema(json.RawMessage(`{}`), schema)
		if err == nil {
			t.Fatal("expected error for external http:// $ref")
		}
	})
	t.Run("inline ref still works", func(t *testing.T) {
		schema := json.RawMessage(`{
			"type": "object",
			"properties": {"val": {"$ref": "#/$defs/pos"}},
			"$defs": {"pos": {"type": "integer", "minimum": 0}}
		}`)
		if err := ValidateJSONSchema(json.RawMessage(`{"val": 5}`), schema); err != nil {
			t.Fatalf("unexpected error for inline $ref: %v", err)
		}
	})
}

func TestValidateJSONSchema_NilAndEmptySchema(t *testing.T) {
	input := json.RawMessage(`{"any": "input"}`)

	t.Run("nil schema accepts any input", func(t *testing.T) {
		if err := ValidateJSONSchema(input, nil); err != nil {
			t.Fatalf("unexpected error for nil schema: %v", err)
		}
	})
	t.Run("empty schema accepts any input", func(t *testing.T) {
		if err := ValidateJSONSchema(input, json.RawMessage("")); err != nil {
			t.Fatalf("unexpected error for empty schema: %v", err)
		}
	})
	t.Run("invalid schema JSON returns error", func(t *testing.T) {
		if err := ValidateJSONSchema(input, json.RawMessage("{bad")); err == nil {
			t.Fatal("expected error for invalid schema JSON")
		}
	})
}
