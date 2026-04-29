package security

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// dangerousKeys are stripped from input objects to prevent prototype pollution.
var dangerousKeys = map[string]bool{
	"__proto__":   true,
	"constructor": true,
}

// StripDangerousKeysFromInput parses input JSON, recursively strips
// __proto__ and constructor keys, and returns the cleaned JSON alongside the
// list of dropped key names (paths are not tracked — only the keys
// themselves, deduplicated by occurrence). Returns an error if input is not
// valid JSON.
//
// Callers can use this before ValidateJSONSchema to learn whether dangerous
// keys were stripped, so they can emit a SecurityLogger event without
// changing the existing ValidateJSONSchema signature (which still strips
// silently for backward compatibility).
func StripDangerousKeysFromInput(input json.RawMessage) (json.RawMessage, []string, error) {
	if len(input) == 0 {
		return input, nil, nil
	}
	var v any
	if err := json.Unmarshal(input, &v); err != nil {
		return input, nil, fmt.Errorf("invalid input JSON: %w", err)
	}
	stripped, dropped := stripDangerousKeysWithReport(v)
	if len(dropped) == 0 {
		// No changes — return the original bytes unchanged so callers don't
		// see spurious whitespace differences from re-marshalling.
		return input, nil, nil
	}
	out, err := json.Marshal(stripped)
	if err != nil {
		return input, dropped, fmt.Errorf("re-marshal stripped input: %w", err)
	}
	return out, dropped, nil
}

// ValidateJSONSchema validates input against a JSON Schema document.
//
// This uses santhosh-tekuri/jsonschema v6 for full JSON Schema Draft 2020-12
// support including $ref, $defs, patternProperties, oneOf/anyOf/allOf,
// format validation, enum, pattern, minimum/maximum, array items, etc.
//
// Dangerous keys (__proto__, constructor) are stripped from input before
// validation to prevent prototype pollution attacks. Use
// StripDangerousKeysFromInput when callers need to learn that keys were
// stripped (e.g. to emit a SecurityLogger event).
func ValidateJSONSchema(input json.RawMessage, schema json.RawMessage) error {
	// No schema to validate against — accept any input.
	if len(schema) == 0 {
		return nil
	}

	var inputVal any
	if err := json.Unmarshal(input, &inputVal); err != nil {
		return fmt.Errorf("invalid input JSON: %w", err)
	}

	// Strip dangerous keys from the input before validation.
	inputVal = stripDangerousKeys(inputVal)

	// Unmarshal the schema into the format the compiler expects.
	schemaVal, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return fmt.Errorf("invalid schema JSON: %w", err)
	}

	c := jsonschema.NewCompiler()
	// Block all external resource loading (file://, http://, etc.) to prevent
	// local file read or SSRF via $ref in untrusted schemas from MCP servers.
	c.UseLoader(noopLoader{})
	if err := c.AddResource("schema.json", schemaVal); err != nil {
		return fmt.Errorf("failed to add schema resource: %w", err)
	}

	sch, err := c.Compile("schema.json")
	if err != nil {
		return fmt.Errorf("failed to compile schema: %w", err)
	}

	if err := sch.Validate(inputVal); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	return nil
}

// noopLoader blocks all external resource loading. Schemas must be
// self-contained (inline $defs/$ref only). This prevents local file reads
// and SSRF when validating against untrusted schemas from MCP servers.
type noopLoader struct{}

func (noopLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema loading is disabled: %s", url)
}

func stripDangerousKeys(v any) any {
	out, _ := stripDangerousKeysWithReport(v)
	return out
}

// stripDangerousKeysWithReport recursively strips __proto__ and constructor
// from any nested map[string]any, returning the stripped value alongside the
// distinct key names that were dropped. Order of dropped keys is unspecified.
func stripDangerousKeysWithReport(v any) (any, []string) {
	dropped := map[string]struct{}{}
	stripped := stripWithDropped(v, dropped)
	if len(dropped) == 0 {
		return stripped, nil
	}
	keys := make([]string, 0, len(dropped))
	for k := range dropped {
		keys = append(keys, k)
	}
	return stripped, keys
}

func stripWithDropped(v any, dropped map[string]struct{}) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, item := range val {
			if dangerousKeys[k] {
				dropped[k] = struct{}{}
				continue
			}
			out[k] = stripWithDropped(item, dropped)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = stripWithDropped(item, dropped)
		}
		return out
	default:
		return v
	}
}
