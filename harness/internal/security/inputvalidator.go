package security

import (
	"encoding/json"
	"fmt"
	"strings"
)

// dangerousKeys are stripped from input objects to prevent prototype pollution.
var dangerousKeys = map[string]bool{
	"__proto__":   true,
	"constructor": true,
}

// ValidateJSONSchema validates input against a JSON Schema document.
//
// This is a simplified Phase 1 implementation that checks:
//   - required fields are present
//   - type constraints (string, number, integer, boolean, object, array)
//   - additionalProperties: false rejects unexpected fields
//   - strips __proto__ and constructor keys (prototype pollution protection)
//
// TODO: Replace with github.com/santhosh-tekuri/jsonschema for full
// JSON Schema Draft 2020-12 support including $ref, patternProperties,
// oneOf/anyOf/allOf, format validation, etc.
func ValidateJSONSchema(input json.RawMessage, schema json.RawMessage) error {
	var inputVal any
	if err := json.Unmarshal(input, &inputVal); err != nil {
		return fmt.Errorf("invalid input JSON: %w", err)
	}

	var schemaDef map[string]any
	if err := json.Unmarshal(schema, &schemaDef); err != nil {
		return fmt.Errorf("invalid schema JSON: %w", err)
	}

	// Strip dangerous keys from the input before validation.
	inputVal = stripDangerousKeys(inputVal)

	return validateValue(inputVal, schemaDef, "")
}

func validateValue(value any, schema map[string]any, path string) error {
	if schemaType, ok := schema["type"].(string); ok {
		if err := checkType(value, schemaType, path); err != nil {
			return err
		}
	}

	if schemaType, _ := schema["type"].(string); schemaType == "object" {
		obj, ok := value.(map[string]any)
		if !ok {
			// Type mismatch already caught by checkType; if type wasn't specified,
			// we can only validate object constraints against actual objects.
			return nil
		}

		if err := checkRequired(obj, schema, path); err != nil {
			return err
		}

		if err := checkAdditionalProperties(obj, schema, path); err != nil {
			return err
		}

		if err := checkProperties(obj, schema, path); err != nil {
			return err
		}
	}

	return nil
}

func checkType(value any, expected string, path string) error {
	prefix := "value"
	if path != "" {
		prefix = path
	}

	switch expected {
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s: expected string, got %T", prefix, value)
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("%s: expected number, got %T", prefix, value)
		}
	case "integer":
		f, ok := value.(float64)
		if !ok {
			return fmt.Errorf("%s: expected integer, got %T", prefix, value)
		}
		if f != float64(int64(f)) {
			return fmt.Errorf("%s: expected integer, got float", prefix)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: expected boolean, got %T", prefix, value)
		}
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("%s: expected object, got %T", prefix, value)
		}
	case "array":
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("%s: expected array, got %T", prefix, value)
		}
	case "null":
		if value != nil {
			return fmt.Errorf("%s: expected null, got %T", prefix, value)
		}
	}

	return nil
}

func checkRequired(obj map[string]any, schema map[string]any, path string) error {
	required, ok := schema["required"].([]any)
	if !ok {
		return nil
	}

	var missing []string
	for _, r := range required {
		field, ok := r.(string)
		if !ok {
			continue
		}
		if _, exists := obj[field]; !exists {
			missing = append(missing, field)
		}
	}

	if len(missing) > 0 {
		prefix := "object"
		if path != "" {
			prefix = path
		}
		return fmt.Errorf("%s: missing required fields: %s", prefix, strings.Join(missing, ", "))
	}

	return nil
}

func checkAdditionalProperties(obj map[string]any, schema map[string]any, path string) error {
	ap, exists := schema["additionalProperties"]
	if !exists {
		return nil
	}

	allowed, ok := ap.(bool)
	if !ok || allowed {
		return nil
	}

	// additionalProperties: false — only properties defined in "properties" are allowed.
	props, _ := schema["properties"].(map[string]any)
	var extra []string
	for key := range obj {
		if props != nil {
			if _, defined := props[key]; defined {
				continue
			}
		}
		extra = append(extra, key)
	}

	if len(extra) > 0 {
		prefix := "object"
		if path != "" {
			prefix = path
		}
		return fmt.Errorf("%s: unexpected fields: %s", prefix, strings.Join(extra, ", "))
	}

	return nil
}

func checkProperties(obj map[string]any, schema map[string]any, path string) error {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil
	}

	for key, val := range obj {
		propSchema, exists := props[key]
		if !exists {
			continue
		}
		ps, ok := propSchema.(map[string]any)
		if !ok {
			continue
		}
		fieldPath := key
		if path != "" {
			fieldPath = path + "." + key
		}
		if err := validateValue(val, ps, fieldPath); err != nil {
			return err
		}
	}

	return nil
}

func stripDangerousKeys(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, item := range val {
			if dangerousKeys[k] {
				continue
			}
			out[k] = stripDangerousKeys(item)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = stripDangerousKeys(item)
		}
		return out
	default:
		return v
	}
}
