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
