package provider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodeSchema unmarshals a normalised schema into a generic map so tests
// can inspect individual keys.
func decodeSchema(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got), "normalised schema is not valid JSON")
	return got
}

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
	require.NoError(t, err)
	got := decodeSchema(t, out)

	// required is the sorted full property name list.
	assert.Equal(t, []any{"limit", "path", "start_line"}, got["required"])
	assert.Equal(t, false, got["additionalProperties"])

	props := got["properties"].(map[string]any)

	// path was required, so it is not nullable-wrapped.
	pathSchema := props["path"].(map[string]any)
	assert.Equal(t, "string", pathSchema["type"])

	// start_line was optional, so it is nullable-wrapped.
	startType, _ := props["start_line"].(map[string]any)["type"].([]any)
	assert.Contains(t, startType, "integer")
	assert.Contains(t, startType, "null")

	// limit was optional, so it is nullable-wrapped; minimum and maximum survive.
	limitSchema := props["limit"].(map[string]any)
	limitType, _ := limitSchema["type"].([]any)
	assert.Contains(t, limitType, "integer")
	assert.Contains(t, limitType, "null")
	assert.Contains(t, limitSchema, "minimum")
	assert.Contains(t, limitSchema, "maximum")
}

// TestNormalizeStrictSchema_RewritesNestedObjects pins the recursive
// rewrite: nested objects also get additionalProperties=false, and their
// properties expand into a sorted full-required list.
func TestNormalizeStrictSchema_RewritesNestedObjects(t *testing.T) {
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
	require.NoError(t, err)
	got := decodeSchema(t, out)

	outer := got["properties"].(map[string]any)["outer"].(map[string]any)
	assert.Equal(t, false, outer["additionalProperties"])
	assert.Equal(t, []any{"inner_opt", "inner_req"}, outer["required"])

	innerOptType, _ := outer["properties"].(map[string]any)["inner_opt"].(map[string]any)["type"].([]any)
	assert.Contains(t, innerOptType, "null", "nested optional property should be nullable")
}

// TestNormalizeStrictSchema_RewritesArrayItemObjects pins that array items
// schemas are normalised too: an array's items of type object expand just
// like a top-level object.
func TestNormalizeStrictSchema_RewritesArrayItemObjects(t *testing.T) {
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
	require.NoError(t, err)
	got := decodeSchema(t, out)

	items := got["properties"].(map[string]any)["events"].(map[string]any)["items"].(map[string]any)
	assert.Equal(t, false, items["additionalProperties"])
	assert.Equal(t, []any{"count", "name"}, items["required"])
}

// TestNormalizeStrictSchema_PreservesEnumAndDescription pins the
// no-field-deletion contract: enum and description on each property
// survive the rewrite verbatim. An enum such as edit_file's operation
// property carries the strict-mode validation semantic the model relies on.
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
	require.NoError(t, err)
	got := decodeSchema(t, out)

	op := got["properties"].(map[string]any)["op"].(map[string]any)
	assert.Equal(t, "Pick one.", op["description"])

	enums, _ := op["enum"].([]any)
	require.Len(t, enums, 3)
	assert.Equal(t, "a", enums[0])
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
			require.Error(t, err)
			var schemaErr *strictSchemaError
			require.ErrorAs(t, err, &schemaErr)
			assert.Equal(t, "test_tool", schemaErr.tool)
			require.ErrorContains(t, err, tc.wantKeyword)
			require.ErrorContains(t, err, tc.wantPath)
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
	require.Error(t, err)
	require.ErrorContains(t, err, "no type")
	assert.ErrorContains(t, err, "properties.x")
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
	require.ErrorContains(t, err, "tuple")
}

// TestNormalizeStrictSchema_EmptyInputBecomesEmptyObject pins the
// empty-input shortcut: an empty string or "{}" yields a well-formed empty
// object schema so a no-arg tool serialises cleanly under strict mode.
func TestNormalizeStrictSchema_EmptyInputBecomesEmptyObject(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{name: "empty string", in: ""},
		{name: "empty object", in: "{}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := NormalizeStrictSchema("noarg_tool", json.RawMessage(tc.in))
			require.NoError(t, err)
			got := decodeSchema(t, out)

			assert.Equal(t, "object", got["type"])
			assert.Equal(t, false, got["additionalProperties"])
			assert.Contains(t, got, "required")
		})
	}
}

// TestNormalizeStrictSchema_GrepFilesSchemaMakesOptionalsNullable is the
// integration canary: the real grep_files schema must round-trip through
// the strict-mode normaliser without error, and its optional fields (path,
// include, exclude, max_results) must all become nullable in the output.
func TestNormalizeStrictSchema_GrepFilesSchemaMakesOptionalsNullable(t *testing.T) {
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
	require.NoError(t, err)
	got := decodeSchema(t, out)
	props := got["properties"].(map[string]any)

	// pattern is required, so it is not nullable.
	_, patternNullable := props["pattern"].(map[string]any)["type"].([]any)
	assert.False(t, patternNullable, "pattern is required and must not be nullable")

	for _, k := range []string{"path", "include", "exclude", "max_results"} {
		t.Run(k+" is nullable", func(t *testing.T) {
			typ := props[k].(map[string]any)["type"]
			require.Contains(t, typ, "null")
		})
	}

	// required carries every property name in sorted order.
	assert.Equal(t, []any{"exclude", "include", "max_results", "path", "pattern"}, got["required"])
}

// TestNormalizeStrictSchema_EditFileSchemaKeepsEnumAndRequiresAll covers
// the edit_file schema, which carries an explicit operation enum and a
// required list naming every property. Strict mode must accept it.
func TestNormalizeStrictSchema_EditFileSchemaKeepsEnumAndRequiresAll(t *testing.T) {
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
	require.NoError(t, err)
	got := decodeSchema(t, out)
	props := got["properties"].(map[string]any)

	op := props["operation"].(map[string]any)
	assert.Len(t, op["enum"], 4)

	// required lists every property: content, diff, new_string, old_string, operation, path.
	if reqArr, ok := got["required"].([]any); ok {
		assert.Len(t, reqArr, 6, "required should name every property")
	}
}

// TestNormalizeStrictSchema_DoesNotMutateInput pins that the rewrite
// produces a fresh document rather than aliasing the caller's bytes. The
// registry seeds tool schemas as json.RawMessage shared across turns, so a
// mutation here would change the canonical schema seen by every later
// request, including non-strict providers.
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
	require.NoError(t, err)
	require.Equal(t, original, string(in))
}

// TestNormalizeStrictSchema_IsIdempotent pins that normalising a schema
// twice yields byte-identical output. It guards against a second "null"
// being added to an already-nullable type, and against reordering a
// required list that is already sorted. Cached output must also survive a
// second pass through the normaliser unchanged.
func TestNormalizeStrictSchema_IsIdempotent(t *testing.T) {
	in := json.RawMessage(`{
		"type": "object",
		"properties": {
			"req":      {"type": "string"},
			"opt_int":  {"type": "integer"},
			"opt_arr":  {"type": "array", "items": {"type": "string"}},
			"nested":   {
				"type": "object",
				"properties": {
					"inner_req": {"type": "string"},
					"inner_opt": {"type": "integer"}
				},
				"required": ["inner_req"]
			}
		},
		"required": ["req"]
	}`)
	first, err := NormalizeStrictSchema("idempotent_tool", in)
	require.NoError(t, err)
	second, err := NormalizeStrictSchema("idempotent_tool", first)
	require.NoError(t, err)
	require.Equal(t, string(first), string(second))
}

// TestNormalizeStrictSchema_ObjectWithoutPropertiesIsEmpty pins the output
// for an object node that has no properties key. The result must still
// carry the strict-mode shape (additionalProperties=false, an empty
// properties object and an empty required array) so the wire body is
// well-formed for a no-arg tool that declares type object explicitly.
func TestNormalizeStrictSchema_ObjectWithoutPropertiesIsEmpty(t *testing.T) {
	out, err := NormalizeStrictSchema("noargs_tool", json.RawMessage(`{"type":"object"}`))
	require.NoError(t, err)
	got := decodeSchema(t, out)

	assert.Equal(t, "object", got["type"])
	assert.Equal(t, false, got["additionalProperties"])
	assert.Equal(t, map[string]any{}, got["properties"])
	assert.Equal(t, []any{}, got["required"])
}

// TestNormalizeStrictSchema_ArrayWithoutItemsPassesThrough pins that an
// array node with no items key round-trips without error and is left
// untouched. Built-in schemas always declare items, but a permissive
// imported schema might not, and the normaliser must not reject it.
func TestNormalizeStrictSchema_ArrayWithoutItemsPassesThrough(t *testing.T) {
	in := json.RawMessage(`{
		"type": "object",
		"properties": {
			"tags": {"type": "array"}
		},
		"required": ["tags"]
	}`)
	out, err := NormalizeStrictSchema("array_noitems_tool", in)
	require.NoError(t, err)
	got := decodeSchema(t, out)

	tags := got["properties"].(map[string]any)["tags"].(map[string]any)
	assert.Equal(t, "array", tags["type"])
	assert.NotContains(t, tags, "items")
}
