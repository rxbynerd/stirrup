package provider

import (
	"encoding/json"
	"fmt"
	"sort"
)

// ConvertSchema converts a JSON Schema (Draft 2020-12) document into the
// Gemini OpenAPI-3.0-flavoured Schema dialect used in
// tools[].functionDeclarations[].parameters. Pure — no I/O, no globals.
//
// Returns an error rather than silently dropping fields when an
// unsupported keyword is encountered. The caller (Stream) propagates
// the error so the run fails fast at request-build time.
//
// Supported transformations:
//   - JSON Schema lowercase type names → Gemini UPPERCASE.
//   - Type arrays of the form ["X","null"] → nullable: true with single type.
//   - Recursive descent into "properties" and "items".
//   - Pass-through of validation keywords (description, enum, required, etc.).
//   - Drop of metadata keywords ($schema, $id, $defs, definitions, $comment,
//     additionalProperties).
//
// Hard errors:
//   - "$ref" — Gemini does not resolve refs; caller must inline.
//   - "oneOf" / "anyOf" with more than one non-null branch.
//   - "allOf" — no merge logic.
//   - Type values not in the Gemini type table.
//   - Type arrays with more than two values, or two values where neither is
//     "null".
//
// Empty input ("" or "{}") returns an empty object schema. Unknown keywords
// are passed through verbatim — Vertex tolerates them, and silently dropping
// would mask future schema features.
func ConvertSchema(in json.RawMessage) (json.RawMessage, error) {
	if len(in) == 0 {
		return json.RawMessage("{}"), nil
	}
	var node any
	if err := json.Unmarshal(in, &node); err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	out, err := convertNode(node, "")
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// convertNode walks a single JSON Schema node, returning the Gemini-shaped
// equivalent. The path argument is used to enrich error messages with the
// schema location at which an unsupported keyword was found.
func convertNode(node any, path string) (any, error) {
	obj, ok := node.(map[string]any)
	if !ok {
		// Non-object values inside a schema position (e.g. an enum entry, a
		// default scalar) are passed through unchanged. The caller has
		// already routed us in via a known walking key.
		return node, nil
	}

	// Reject pinned-as-error keywords first, before any per-key pass-through
	// so that a malformed schema cannot smuggle through unsupported syntax.
	if _, has := obj["$ref"]; has {
		return nil, fmt.Errorf("schema at %s: $ref is not supported by Gemini; inline the referenced schema", pathOrRoot(path))
	}
	if _, has := obj["allOf"]; has {
		return nil, fmt.Errorf("schema at %s: allOf is not supported by Gemini", pathOrRoot(path))
	}

	// Drop metadata keywords that Gemini ignores. Listed explicitly so
	// extending the set is a single-line change. Done before union
	// collapsing so dropped keys cannot end up in the merged output by
	// accident.
	for _, drop := range []string{
		"$schema", "$id", "$comment", "$defs", "definitions",
		"additionalProperties", "unevaluatedProperties",
	} {
		delete(obj, drop)
	}

	// oneOf / anyOf collapse: a single non-null branch with an optional
	// "null" branch is treated as a nullable variant of the non-null branch.
	// Anything else is rejected. Done before the per-key type translation
	// below so the merged branch's `type` is already in Gemini form and is
	// not re-translated (which would fail with "unknown type STRING").
	for _, key := range []string{"oneOf", "anyOf"} {
		raw, has := obj[key]
		if !has {
			continue
		}
		branches, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("schema at %s: %s must be an array", pathOrRoot(path), key)
		}
		converted, nullable, err := collapseUnionBranches(branches, path, key)
		if err != nil {
			return nil, err
		}
		delete(obj, key)
		// Merge the collapsed branch into obj. Prefer obj's own keys
		// (caller-set sibling keywords) over the branch's keys for
		// deterministic precedence.
		if branchObj, ok := converted.(map[string]any); ok {
			for k, v := range branchObj {
				if _, exists := obj[k]; !exists {
					obj[k] = v
				}
			}
		}
		if nullable {
			obj["nullable"] = true
		}
		// The branch was already passed through convertNode, so its `type`
		// is already Gemini-shaped. Skip the type translation below by
		// returning here — there are no further walking targets at this
		// level (oneOf/anyOf branches don't carry their own properties or
		// items at this node).
		return obj, nil
	}

	// Type translation. JSON Schema accepts either a single string or an
	// array of strings; Gemini takes a single uppercase string plus an
	// optional `nullable` flag.
	if rawType, has := obj["type"]; has {
		switch t := rawType.(type) {
		case string:
			gemini, err := translateTypeName(t, path)
			if err != nil {
				return nil, err
			}
			obj["type"] = gemini
		case []any:
			gemini, nullable, err := translateTypeArray(t, path)
			if err != nil {
				return nil, err
			}
			obj["type"] = gemini
			if nullable {
				obj["nullable"] = true
			}
		default:
			return nil, fmt.Errorf("schema at %s: type must be string or array, got %T", pathOrRoot(path), rawType)
		}
	}

	// Recurse into known walking keys.
	if rawProps, has := obj["properties"]; has {
		props, ok := rawProps.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("schema at %s: properties must be an object", pathOrRoot(path))
		}
		// Sort keys for deterministic output ordering of the converted map.
		// Iteration order for map values does not affect JSON output (Go's
		// encoding/json sorts map keys), but sorted iteration here gives
		// deterministic error messages when one of many properties fails.
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			converted, err := convertNode(props[k], joinPath(path, "properties."+k))
			if err != nil {
				return nil, err
			}
			props[k] = converted
		}
	}

	if rawItems, has := obj["items"]; has {
		converted, err := convertNode(rawItems, joinPath(path, "items"))
		if err != nil {
			return nil, err
		}
		obj["items"] = converted
	}

	return obj, nil
}

// collapseUnionBranches converts a oneOf/anyOf branch list into a single
// schema node plus a nullable flag. Returns an error when the union shape is
// not supported (zero non-null branches, or more than one).
func collapseUnionBranches(branches []any, path, key string) (any, bool, error) {
	nullable := false
	var nonNull []any
	for i, b := range branches {
		bm, ok := b.(map[string]any)
		if !ok {
			return nil, false, fmt.Errorf("schema at %s: %s[%d] must be an object", pathOrRoot(path), key, i)
		}
		if isNullBranch(bm) {
			nullable = true
			continue
		}
		nonNull = append(nonNull, bm)
	}
	if len(nonNull) == 0 {
		return nil, false, fmt.Errorf("schema at %s: %s has no non-null branches; Gemini requires a concrete type", pathOrRoot(path), key)
	}
	if len(nonNull) > 1 {
		return nil, false, fmt.Errorf("schema at %s: %s has %d non-null branches; Gemini Schema does not support discriminated unions", pathOrRoot(path), key, len(nonNull))
	}
	converted, err := convertNode(nonNull[0], joinPath(path, key+"[0]"))
	if err != nil {
		return nil, false, err
	}
	return converted, nullable, nil
}

// isNullBranch reports whether a union branch matches {"type":"null"}, which
// is the JSON Schema idiom for nullability inside oneOf/anyOf.
func isNullBranch(b map[string]any) bool {
	t, ok := b["type"]
	if !ok {
		return false
	}
	s, ok := t.(string)
	if !ok {
		return false
	}
	return s == "null"
}

// translateTypeName maps a single JSON Schema type name to its Gemini
// UPPERCASE equivalent. Returns an error for "null" (callers must use a
// type-array or oneOf form to express nullability) and for unknown values.
func translateTypeName(t, path string) (string, error) {
	switch t {
	case "object":
		return "OBJECT", nil
	case "string":
		return "STRING", nil
	case "number":
		return "NUMBER", nil
	case "integer":
		return "INTEGER", nil
	case "boolean":
		return "BOOLEAN", nil
	case "array":
		return "ARRAY", nil
	case "null":
		return "", fmt.Errorf("schema at %s: type \"null\" is not supported; use {\"type\":[\"X\",\"null\"]} or anyOf", pathOrRoot(path))
	default:
		return "", fmt.Errorf("schema at %s: unknown type %q", pathOrRoot(path), t)
	}
}

// translateTypeArray handles the JSON Schema 2020-12 form where `type` is an
// array, e.g. ["string","null"]. Gemini Schema is single-typed, so the only
// supported shape is exactly two elements where one is "null".
func translateTypeArray(arr []any, path string) (string, bool, error) {
	if len(arr) != 2 {
		return "", false, fmt.Errorf("schema at %s: type array must have exactly two elements (one being \"null\"); got %d", pathOrRoot(path), len(arr))
	}
	var nonNull string
	nullable := false
	for i, v := range arr {
		s, ok := v.(string)
		if !ok {
			return "", false, fmt.Errorf("schema at %s: type array element %d must be a string", pathOrRoot(path), i)
		}
		if s == "null" {
			nullable = true
			continue
		}
		nonNull = s
	}
	if !nullable || nonNull == "" {
		return "", false, fmt.Errorf("schema at %s: type array must contain exactly one \"null\" and one concrete type", pathOrRoot(path))
	}
	gemini, err := translateTypeName(nonNull, path)
	if err != nil {
		return "", false, err
	}
	return gemini, true, nil
}

// pathOrRoot returns the supplied path or "<root>" when empty, so error
// messages always identify a location.
func pathOrRoot(p string) string {
	if p == "" {
		return "<root>"
	}
	return p
}

// joinPath appends a segment to a dotted schema path.
func joinPath(parent, segment string) string {
	if parent == "" {
		return segment
	}
	return parent + "." + segment
}
