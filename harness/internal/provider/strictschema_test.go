package provider

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestNormalizeStrictSchema_AllPropertiesRequiredAndNullable pins the
// core strict-mode rewrite contract: every property ends up in the
// required array (in sorted order for determinism), optionals are
// nullable-wrapped via ["type","null"], and additionalProperties is set
// to false at every object level.
func TestNormalizeStrictSchema_AllPropertiesRequiredAndNullable(t *testing.T) {
	// read_file's actual schema (path required, start_line + limit optional).
	in := json.RawMessage(`{
		"type": "object",
		"properties": {
			"path":       {"type": "string", "description": "p"},
			"start_line": {"type": "integer", "minimum": 1},
			"limit":      {"type": "integer", "minimum": 1, "maximum": 5000}
		},
		"required": ["path"],
		"additionalProperties": false
	}`)

	out, err := NormalizeStrictSchema("read_file", in)
	if err != nil {
		t.Fatalf("NormalizeStrictSchema: %v", err)
	}
	var got map[string]any
	if uerr := json.Unmarshal(out, &got); uerr != nil {
		t.Fatalf("re-unmarshal: %v", uerr)
	}

	// required is the sorted full property name list.
	wantReq := []any{"limit", "path", "start_line"}
	if !reflect.DeepEqual(got["required"], wantReq) {
		t.Errorf("required = %v, want %v", got["required"], wantReq)
	}

	// additionalProperties at the top level.
	if got["additionalProperties"] != false {
		t.Errorf("additionalProperties = %v, want false", got["additionalProperties"])
	}

	props := got["properties"].(map[string]any)

	// path was required → not nullable-wrapped.
	pathSchema := props["path"].(map[string]any)
	if s, ok := pathSchema["type"].(string); !ok || s != "string" {
		t.Errorf("path.type = %v, want string", pathSchema["type"])
	}

	// start_line was optional → nullable-wrapped.
	startSchema := props["start_line"].(map[string]any)
	startType, _ := startSchema["type"].([]any)
	if !containsString(startType, "integer") || !containsString(startType, "null") {
		t.Errorf("start_line.type = %v, want [integer,null]", startSchema["type"])
	}

	// limit was optional → nullable-wrapped, and minimum/maximum preserved.
	limitSchema := props["limit"].(map[string]any)
	limitType, _ := limitSchema["type"].([]any)
	if !containsString(limitType, "integer") || !containsString(limitType, "null") {
		t.Errorf("limit.type = %v, want [integer,null]", limitSchema["type"])
	}
	if _, has := limitSchema["minimum"]; !has {
		t.Errorf("limit.minimum dropped: %v", limitSchema)
	}
	if _, has := limitSchema["maximum"]; !has {
		t.Errorf("limit.maximum dropped: %v", limitSchema)
	}
}

// TestNormalizeStrictSchema_NestedObjects pins the recursive rewrite:
// nested objects also get additionalProperties=false and their own
// properties expand into a sorted full-required list. The brief calls
// out "nested objects" as an explicit test case.
func TestNormalizeStrictSchema_NestedObjects(t *testing.T) {
	in := json.RawMessage(`{
		"type": "object",
		"properties": {
			"outer": {
				"type": "object",
				"properties": {
					"inner_req": {"type": "string"},
					"inner_opt": {"type": "integer"}
				},
				"required": ["inner_req"]
			}
		},
		"required": ["outer"]
	}`)

	out, err := NormalizeStrictSchema("nested_tool", in)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var got map[string]any
	if uerr := json.Unmarshal(out, &got); uerr != nil {
		t.Fatalf("re-unmarshal: %v", uerr)
	}
	outer := got["properties"].(map[string]any)["outer"].(map[string]any)
	if outer["additionalProperties"] != false {
		t.Errorf("nested additionalProperties = %v, want false", outer["additionalProperties"])
	}
	outerReq := outer["required"].([]any)
	wantOuterReq := []any{"inner_opt", "inner_req"}
	if !reflect.DeepEqual(outerReq, wantOuterReq) {
		t.Errorf("nested required = %v, want %v", outerReq, wantOuterReq)
	}
	innerOpt := outer["properties"].(map[string]any)["inner_opt"].(map[string]any)
	innerOptType := innerOpt["type"].([]any)
	if !containsString(innerOptType, "null") {
		t.Errorf("nested inner_opt should be nullable, got %v", innerOpt["type"])
	}
}

// TestNormalizeStrictSchema_ArraysOfObjects pins that array items
// schemas are normalised too: an array's items.type = object expands
// just like a top-level object.
func TestNormalizeStrictSchema_ArraysOfObjects(t *testing.T) {
	in := json.RawMessage(`{
		"type": "object",
		"properties": {
			"events": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"name":  {"type": "string"},
						"count": {"type": "integer"}
					},
					"required": ["name"]
				}
			}
		},
		"required": ["events"]
	}`)

	out, err := NormalizeStrictSchema("events_tool", in)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	events := got["properties"].(map[string]any)["events"].(map[string]any)
	items := events["items"].(map[string]any)
	if items["additionalProperties"] != false {
		t.Errorf("items.additionalProperties = %v, want false", items["additionalProperties"])
	}
	itemsReq := items["required"].([]any)
	wantReq := []any{"count", "name"}
	if !reflect.DeepEqual(itemsReq, wantReq) {
		t.Errorf("items.required = %v, want %v", itemsReq, wantReq)
	}
}

// TestNormalizeStrictSchema_PreservesEnumAndDescription pins the
// no-field-deletion contract: enum and description on each property
// survive the rewrite verbatim. Operations like multiSchema's enum on
// the `operation` property carry the strict-mode validation semantic
// the model relies on.
func TestNormalizeStrictSchema_PreservesEnumAndDescription(t *testing.T) {
	in := json.RawMessage(`{
		"type": "object",
		"properties": {
			"op": {
				"type": "string",
				"enum": ["a", "b", "c"],
				"description": "Pick one."
			}
		},
		"required": ["op"]
	}`)
	out, err := NormalizeStrictSchema("enum_tool", in)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	op := got["properties"].(map[string]any)["op"].(map[string]any)
	if op["description"] != "Pick one." {
		t.Errorf("description not preserved: %v", op)
	}
	enums := op["enum"].([]any)
	if len(enums) != 3 || enums[0] != "a" {
		t.Errorf("enum not preserved: %v", op["enum"])
	}
}

// TestNormalizeStrictSchema_RejectsUnsupportedKeywords pins the
// fail-closed contract for $ref, oneOf, anyOf, allOf, etc. Each
// rejection must name the offending field path so the operator can
// locate it; the error MUST NOT include the schema's description or
// enum content.
func TestNormalizeStrictSchema_RejectsUnsupportedKeywords(t *testing.T) {
	cases := []struct {
		name        string
		schema      string
		wantPath    string
		wantKeyword string
	}{
		{
			name:        "top-level $ref",
			schema:      `{"$ref":"#/$defs/X"}`,
			wantPath:    "<root>",
			wantKeyword: "$ref",
		},
		{
			name:        "top-level oneOf",
			schema:      `{"oneOf":[{"type":"string"},{"type":"integer"}]}`,
			wantPath:    "<root>",
			wantKeyword: "oneOf",
		},
		{
			name:        "nested anyOf",
			schema:      `{"type":"object","properties":{"x":{"anyOf":[{"type":"string"},{"type":"integer"}]}},"required":["x"]}`,
			wantPath:    "properties.x",
			wantKeyword: "anyOf",
		},
		{
			name:        "allOf",
			schema:      `{"allOf":[{"type":"string"}]}`,
			wantPath:    "<root>",
			wantKeyword: "allOf",
		},
		{
			name:        "patternProperties",
			schema:      `{"type":"object","patternProperties":{"^foo":{"type":"string"}}}`,
			wantPath:    "<root>",
			wantKeyword: "patternProperties",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeStrictSchema("test_tool", json.RawMessage(tc.schema))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			var schemaErr *strictSchemaError
			if !errors.As(err, &schemaErr) {
				t.Fatalf("expected *strictSchemaError, got %T: %v", err, err)
			}
			if schemaErr.tool != "test_tool" {
				t.Errorf("tool = %q, want %q", schemaErr.tool, "test_tool")
			}
			if !strings.Contains(err.Error(), tc.wantKeyword) {
				t.Errorf("error %q does not name the offending keyword %q", err, tc.wantKeyword)
			}
			if !strings.Contains(err.Error(), tc.wantPath) {
				t.Errorf("error %q does not name the offending path %q", err, tc.wantPath)
			}
		})
	}
}

// TestNormalizeStrictSchema_RejectsTypelessProperty pins that a property
// with no type (e.g. an enum-only property) is rejected when optional:
// strict mode cannot represent a nullable schema without a concrete type.
func TestNormalizeStrictSchema_RejectsTypelessProperty(t *testing.T) {
	in := json.RawMessage(`{
		"type": "object",
		"properties": {
			"x": {"enum": ["a", "b"]}
		}
	}`)
	_, err := NormalizeStrictSchema("typeless_tool", in)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no type") {
		t.Errorf("error %q does not name the typeless-property reason", err)
	}
	if !strings.Contains(err.Error(), "properties.x") {
		t.Errorf("error %q does not name the offending property path", err)
	}
}

// TestNormalizeStrictSchema_RejectsTupleItems pins the rejection of
// items-as-array (tuple validation), which strict mode does not model.
func TestNormalizeStrictSchema_RejectsTupleItems(t *testing.T) {
	in := json.RawMessage(`{
		"type": "object",
		"properties": {
			"pair": {
				"type": "array",
				"items": [{"type":"string"}, {"type":"integer"}]
			}
		}
	}`)
	_, err := NormalizeStrictSchema("tuple_tool", in)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "tuple") {
		t.Errorf("error %q should mention tuple-form items", err)
	}
}

// TestNormalizeStrictSchema_EmptySchema pins the empty-input shortcut:
// an empty string or "{}" yields a well-formed empty object schema so
// a no-arg tool serialises cleanly under strict mode.
func TestNormalizeStrictSchema_EmptySchema(t *testing.T) {
	for _, in := range []string{"", "{}"} {
		out, err := NormalizeStrictSchema("noarg_tool", json.RawMessage(in))
		if err != nil {
			t.Errorf("empty schema %q: unexpected error %v", in, err)
			continue
		}
		var got map[string]any
		if uerr := json.Unmarshal(out, &got); uerr != nil {
			t.Errorf("re-unmarshal: %v", uerr)
			continue
		}
		if got["type"] != "object" {
			t.Errorf("empty schema %q: type=%v, want object", in, got["type"])
		}
		if got["additionalProperties"] != false {
			t.Errorf("empty schema %q: additionalProperties=%v, want false", in, got["additionalProperties"])
		}
		if _, has := got["required"]; !has {
			t.Errorf("empty schema %q: required missing", in)
		}
	}
}

// TestNormalizeStrictSchema_GrepFilesActualSchema is the integration
// canary: the real grep_files schema (Wave 3 Step A) must round-trip
// through the strict-mode normaliser without error, and its optional
// fields (path, include, exclude, max_results) must all become
// nullable in the output. This pins that Step B does not regress Step
// A's schemas.
func TestNormalizeStrictSchema_GrepFilesActualSchema(t *testing.T) {
	in := json.RawMessage(`{
		"type": "object",
		"properties": {
			"pattern":     {"type": "string"},
			"path":        {"type": "string"},
			"include":     {"type": "array", "items": {"type": "string"}},
			"exclude":     {"type": "array", "items": {"type": "string"}},
			"max_results": {"type": "integer", "minimum": 1, "maximum": 1000}
		},
		"required": ["pattern"],
		"additionalProperties": false
	}`)
	out, err := NormalizeStrictSchema("grep_files", in)
	if err != nil {
		t.Fatalf("grep_files schema rejected: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	props := got["properties"].(map[string]any)

	// pattern is required → not nullable.
	if _, isArr := props["pattern"].(map[string]any)["type"].([]any); isArr {
		t.Errorf("pattern should not be nullable (required)")
	}

	// every other property must be nullable.
	for _, k := range []string{"path", "include", "exclude", "max_results"} {
		typ := props[k].(map[string]any)["type"]
		arr, ok := typ.([]any)
		if !ok || !containsString(arr, "null") {
			t.Errorf("property %q should be nullable, got type=%v", k, typ)
		}
	}

	// required carries every property name in sorted order.
	wantReq := []any{"exclude", "include", "max_results", "path", "pattern"}
	if !reflect.DeepEqual(got["required"], wantReq) {
		t.Errorf("required = %v, want %v", got["required"], wantReq)
	}
}

// TestNormalizeStrictSchema_EditFileActualSchema mirrors the grep_files
// canary for the edit_file (multi.go) schema, which is the more
// adversarial input: it carries the explicit operation enum from
// Wave 3 Step A. Strict mode must accept it.
func TestNormalizeStrictSchema_EditFileActualSchema(t *testing.T) {
	in := json.RawMessage(`{
		"type": "object",
		"properties": {
			"path":       {"type": "string"},
			"operation":  {"type": "string", "enum": ["replace","delete","rewrite","patch"]},
			"old_string": {"type": "string"},
			"new_string": {"type": "string"},
			"content":    {"type": "string"},
			"diff":       {"type": "string"}
		},
		"required": ["path", "operation"],
		"additionalProperties": false
	}`)
	out, err := NormalizeStrictSchema("edit_file", in)
	if err != nil {
		t.Fatalf("edit_file schema rejected: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	props := got["properties"].(map[string]any)

	// operation's enum survives the rewrite.
	op := props["operation"].(map[string]any)
	enums := op["enum"].([]any)
	if len(enums) != 4 {
		t.Errorf("operation.enum = %v, want 4 values", enums)
	}

	// required = [content, diff, new_string, old_string, operation, path].
	if reqArr, ok := got["required"].([]any); ok {
		if len(reqArr) != 6 {
			t.Errorf("required length = %d, want 6 (every property): %v", len(reqArr), reqArr)
		}
	}
}

// TestNormalizeStrictSchema_DoesNotMutateInput pins that the rewrite
// produces a fresh document rather than aliasing the caller's map. The
// registry seeds tool schemas as json.RawMessage shared across turns —
// a mutation here would silently change the canonical schema seen by
// every subsequent request, including non-strict providers.
func TestNormalizeStrictSchema_DoesNotMutateInput(t *testing.T) {
	original := `{
		"type": "object",
		"properties": {
			"x": {"type": "string"},
			"y": {"type": "integer"}
		},
		"required": ["x"]
	}`
	in := json.RawMessage(original)
	_, err := NormalizeStrictSchema("noalias", in)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	// in is a []byte alias of original — the rewrite happens on a
	// parsed copy, not on the bytes, so the bytes should match.
	if string(in) != original {
		t.Errorf("input bytes mutated; got %q", string(in))
	}
}

// containsString reports whether the supplied []any contains a string
// equal to s. Used in tests that check the type-array form for null.
func containsString(arr []any, s string) bool {
	for _, v := range arr {
		if str, ok := v.(string); ok && str == s {
			return true
		}
	}
	return false
}
