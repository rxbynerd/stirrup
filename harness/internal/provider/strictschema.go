package provider

import (
	"encoding/json"
	"fmt"
	"sort"
)

// strictSchemaError carries a tool name plus the schema field path the
// normaliser rejected. The exported message names the tool and path only
// — never the schema's description or enum values, because those may
// contain operator-supplied or model-supplied content that should not
// land in a log or trace at error level. The reason string names the
// JSON Schema keyword or structural issue and is safe to surface.
type strictSchemaError struct {
	tool   string
	path   string
	reason string
}

func (e *strictSchemaError) Error() string {
	loc := e.path
	if loc == "" {
		loc = "<root>"
	}
	if e.tool == "" {
		return fmt.Sprintf("strict-mode schema lint failed at %s: %s", loc, e.reason)
	}
	return fmt.Sprintf("strict-mode schema lint failed for tool %q at %s: %s", e.tool, loc, e.reason)
}

// NormalizeStrictSchema rewrites a JSON Schema document to satisfy OpenAI's
// structured-outputs strict-mode contract: every property is listed in
// `required` (optionals become nullable via `["type","null"]`) and every
// object carries `additionalProperties: false`. The rewrite is faithful —
// no field is deleted, no type narrowed beyond nullability.
//
// When the input contains a construct strict mode cannot express (`$ref`,
// `oneOf`, `anyOf`, `allOf`, `patternProperties`, a tuple-form `items`, a
// property with no `type`/`enum`), it returns a *strictSchemaError before
// producing any bytes, naming only the tool and field path — never the
// schema's description or enum values, which may carry content unsafe to
// surface in logs. toolName is informational only; pass "" when the caller
// wraps its own context.
//
// Empty input ("" or "{}") returns a well-formed empty-object schema
// rather than being rejected for a missing `type`.
//
// The schemas this function accepts must originate from first-party tool
// registrations or the structured MCP import path; the recursion has no
// depth cap because that input surface is bounded by design.
func NormalizeStrictSchema(toolName string, in json.RawMessage) (json.RawMessage, error) {
	if len(in) == 0 || string(in) == "{}" {
		return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false,"required":[]}`), nil
	}
	var node any
	if err := json.Unmarshal(in, &node); err != nil {
		return nil, &strictSchemaError{tool: toolName, path: "", reason: fmt.Sprintf("parse schema: %v", err)}
	}
	out, err := normalizeStrictNode(toolName, node, "")
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// normalizeStrictNode is the recursive worker. It returns a fresh node so
// the caller's input is not mutated (the registry's schema bytes are
// shared across turns).
func normalizeStrictNode(toolName string, node any, path string) (any, error) {
	obj, ok := node.(map[string]any)
	if !ok {
		// Reached via a known walking key; the caller only descends into
		// schema positions, so a non-map here is a scalar/array leaf.
		return node, nil
	}

	// Reject features strict mode cannot express, naming the keyword and
	// field path so the operator sees what blocked normalisation.
	for _, key := range []string{"$ref", "oneOf", "anyOf", "allOf", "patternProperties", "dependentSchemas", "if", "then", "else", "not"} {
		if _, has := obj[key]; has {
			return nil, &strictSchemaError{
				tool:   toolName,
				path:   path,
				reason: fmt.Sprintf("strict mode does not support the %q keyword", key),
			}
		}
	}

	// Copy the input so the rewrite does not mutate the caller's map.
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		out[k] = v
	}

	// Nullability is applied at the parent level when this node sits in
	// an optional `properties` slot, not here — an object's own `type`
	// is passed through as the caller declared it.
	typeStr, typeIsString := out["type"].(string)

	switch {
	case typeIsString && typeStr == "object":
		if err := normalizeStrictObject(toolName, out, path); err != nil {
			return nil, err
		}
	case typeIsString && typeStr == "array":
		if err := normalizeStrictArray(toolName, out, path); err != nil {
			return nil, err
		}
	default:
		// Non-object/non-array leaves pass through unchanged: strict mode
		// silently ignores constraint keywords like enum/description/
		// minimum/pattern rather than rejecting them, so retaining them
		// keeps the rewrite faithful to the canonical schema.
	}

	return out, nil
}

// normalizeStrictObject rewrites an `object`-typed node in place on out:
// sets additionalProperties=false, recurses into each property, and
// rewrites the required list to cover every property (optionals are
// nullable-wrapped). The caller has already type-asserted out["type"]
// equals "object".
func normalizeStrictObject(toolName string, out map[string]any, path string) error {
	out["additionalProperties"] = false

	rawProps, hasProps := out["properties"]
	if !hasProps {
		// An object with no properties is well-formed in strict mode; pin
		// the empty shape so the wire body always carries the canonical
		// fields.
		out["properties"] = map[string]any{}
		out["required"] = []any{}
		return nil
	}
	props, ok := rawProps.(map[string]any)
	if !ok {
		return &strictSchemaError{
			tool:   toolName,
			path:   path,
			reason: "properties must be an object",
		}
	}

	// Determine the original required set (string slice → set). Missing
	// `required` means everything is optional, so the set is empty.
	currentRequired := map[string]bool{}
	if rawReq, has := out["required"]; has {
		arr, ok := rawReq.([]any)
		if !ok {
			return &strictSchemaError{
				tool:   toolName,
				path:   path,
				reason: "required must be an array of strings",
			}
		}
		for i, v := range arr {
			s, ok := v.(string)
			if !ok {
				return &strictSchemaError{
					tool:   toolName,
					path:   path,
					reason: fmt.Sprintf("required[%d] must be a string", i),
				}
			}
			currentRequired[s] = true
		}
	}

	// Walk properties in sorted order so error messages and the
	// resulting required list are deterministic. Strict mode's required
	// array is order-significant for API validation; sorting keeps it
	// stable across rewrites and across Go versions whose map iteration
	// would otherwise reshuffle the order.
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	newProps := make(map[string]any, len(props))
	allRequired := make([]any, 0, len(props))
	for _, k := range keys {
		childPath := joinPath(path, "properties."+k)
		converted, err := normalizeStrictNode(toolName, props[k], childPath)
		if err != nil {
			return err
		}
		// Optional properties become nullable; see makeNullable.
		if !currentRequired[k] {
			converted, err = makeNullable(toolName, converted, childPath)
			if err != nil {
				return err
			}
		}
		newProps[k] = converted
		allRequired = append(allRequired, k)
	}

	out["properties"] = newProps
	out["required"] = allRequired
	return nil
}

// normalizeStrictArray rewrites an `array`-typed node in place on out:
// recurses into `items`. Tuple-form items (an array of schemas) is not
// supported in strict mode, so the function rejects it here rather than
// letting the parent silently emit an unrepresentable shape.
func normalizeStrictArray(toolName string, out map[string]any, path string) error {
	rawItems, has := out["items"]
	if !has {
		return nil
	}
	switch items := rawItems.(type) {
	case map[string]any:
		converted, err := normalizeStrictNode(toolName, items, joinPath(path, "items"))
		if err != nil {
			return err
		}
		out["items"] = converted
	case []any:
		return &strictSchemaError{
			tool:   toolName,
			path:   path,
			reason: "strict mode does not support tuple-form items (items as an array of schemas)",
		}
	default:
		return &strictSchemaError{
			tool:   toolName,
			path:   path,
			reason: "items must be a schema object",
		}
	}
	return nil
}

// makeNullable wraps a property schema's `type` so it admits JSON null:
// a string type "X" becomes ["X","null"]; an array type gets "null"
// appended unless already present. A typeless property (e.g. enum-only)
// is rejected — strict mode cannot model a typeless nullable.
func makeNullable(toolName string, node any, path string) (any, error) {
	obj, ok := node.(map[string]any)
	if !ok {
		// A scalar in a property slot is a malformed schema; reject it
		// with a clear path rather than silently passing through.
		return nil, &strictSchemaError{
			tool:   toolName,
			path:   path,
			reason: "property schema must be an object",
		}
	}
	rawType, has := obj["type"]
	if !has {
		return nil, &strictSchemaError{
			tool:   toolName,
			path:   path,
			reason: "optional property has no type — strict mode requires an explicit type to model nullability",
		}
	}
	switch t := rawType.(type) {
	case string:
		obj["type"] = []any{t, "null"}
	case []any:
		hasNull := false
		for _, v := range t {
			if s, ok := v.(string); ok && s == "null" {
				hasNull = true
				break
			}
		}
		if !hasNull {
			obj["type"] = append(append([]any{}, t...), "null")
		}
	default:
		return nil, &strictSchemaError{
			tool:   toolName,
			path:   path,
			reason: fmt.Sprintf("type must be a string or array of strings, got %T", rawType),
		}
	}
	return obj, nil
}
