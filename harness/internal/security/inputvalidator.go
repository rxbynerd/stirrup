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

// ValidateJSONSchema validates input against a JSON Schema Draft 2020-12
// document. Dangerous keys (__proto__, constructor) are stripped from input
// before validation to prevent prototype pollution attacks. Use
// StripDangerousKeysFromInput when callers need to learn that keys were
// stripped (e.g. to emit a SecurityLogger event).
func ValidateJSONSchema(input json.RawMessage, schema json.RawMessage) error {
	// No schema — accept any input.
	if len(schema) == 0 {
		return nil
	}

	var inputVal any
	if err := json.Unmarshal(input, &inputVal); err != nil {
		return fmt.Errorf("invalid input JSON: %w", err)
	}

	inputVal = stripDangerousKeys(inputVal)

	sch, err := compileJSONSchema(schema)
	if err != nil {
		return err
	}

	if err := sch.Validate(inputVal); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	return nil
}

// CompileJSONSchema reports whether schema is a self-contained JSON Schema
// that compiles, without validating any instance against it.
func CompileJSONSchema(schema json.RawMessage) error {
	_, err := compileJSONSchema(schema)
	return err
}

// schemaResourceURL is absolute so the compiler does not resolve it
// against the working directory, whose path would otherwise appear in
// validation errors returned to the model.
const schemaResourceURL = "urn:stirrup:schema"

func compileJSONSchema(schema json.RawMessage) (*jsonschema.Schema, error) {
	schemaVal, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return nil, fmt.Errorf("invalid schema JSON: %w", err)
	}

	c := jsonschema.NewCompiler()
	// Blocks external resource loading (file://, http://, $ref) — schemas
	// must be self-contained.
	c.UseLoader(noopLoader{})
	if err := c.AddResource(schemaResourceURL, schemaVal); err != nil {
		return nil, fmt.Errorf("failed to add schema resource: %w", err)
	}

	sch, err := c.Compile(schemaResourceURL)
	if err != nil {
		return nil, fmt.Errorf("failed to compile schema: %w", err)
	}
	return sch, nil
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
