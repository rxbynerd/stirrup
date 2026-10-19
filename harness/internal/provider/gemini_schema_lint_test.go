package provider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLintGeminiSchema_EmptyUnsupportedIsNoOp pins the zero-cost path:
// every Gemini request resolves a quirks struct; the lint must do no
// work when no rule pinned any unsupported features. A non-zero cost
// here would tax every Gemini request whether or not the rule is
// active.
func TestLintGeminiSchema_EmptyUnsupportedIsNoOp(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"x":{"type":"string","pattern":"^foo"}}}`)

	t.Run("nil unsupported", func(t *testing.T) {
		require.NoError(t, LintGeminiSchema("read_file", schema, nil))
	})
	t.Run("empty unsupported", func(t *testing.T) {
		require.NoError(t, LintGeminiSchema("read_file", schema, []string{}))
	})
}

// TestLintGeminiSchema_RejectsTopLevelKeyword pins the basic match: a
// keyword listed in `unsupported` triggers the rejection when it
// appears at the root of the schema.
func TestLintGeminiSchema_RejectsTopLevelKeyword(t *testing.T) {
	schema := json.RawMessage(`{"type":"string","pattern":"^foo"}`)

	err := LintGeminiSchema("name_check", schema, []string{"pattern"})
	require.Error(t, err)

	var lintErr *geminiSchemaLintError
	require.ErrorAs(t, err, &lintErr)
	assert.Equal(t, "name_check", lintErr.tool)
	assert.Equal(t, "pattern", lintErr.keyword)
	assert.ErrorContains(t, err, "<root>")
}

// TestLintGeminiSchema_RejectsNestedKeyword pins recursive descent: a
// keyword nested under properties.x or items must still be rejected.
// The path on the error names the location so an operator can locate
// the offending field without scrolling the entire schema.
func TestLintGeminiSchema_RejectsNestedKeyword(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "pattern": "^[a-z]+$"}
		}
	}`)

	err := LintGeminiSchema("tool_x", schema, []string{"pattern"})
	require.ErrorContains(t, err, "properties.name")
}

// TestLintGeminiSchema_RejectsInArrayItems pins the items descent. A
// schema whose array.items carries a forbidden keyword must still
// fail. Both the items-as-object and items-as-tuple forms are
// inspected.
func TestLintGeminiSchema_RejectsInArrayItems(t *testing.T) {
	t.Run("items object", func(t *testing.T) {
		schema := json.RawMessage(`{
			"type": "array",
			"items": {"type": "string", "format": "uri"}
		}`)

		err := LintGeminiSchema("urls", schema, []string{"format"})
		require.ErrorContains(t, err, "items")
	})

	t.Run("items tuple", func(t *testing.T) {
		schema := json.RawMessage(`{
			"type": "array",
			"items": [{"type":"string"},{"type":"integer","format":"int32"}]
		}`)

		err := LintGeminiSchema("pair", schema, []string{"format"})
		require.ErrorContains(t, err, "items[1]")
	})
}

// TestLintGeminiSchema_PassesCleanSchema pins the no-op case: a
// schema without any of the listed unsupported keywords returns nil.
func TestLintGeminiSchema_PassesCleanSchema(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"x": {"type": "string"},
			"n": {"type": "integer", "minimum": 1}
		},
		"required": ["x"]
	}`)

	require.NoError(t, LintGeminiSchema("clean", schema, []string{"pattern", "format"}))
}

// TestLintGeminiSchema_DoesNotLeakDescriptionOrEnum pins the privacy
// contract: the lint error must NOT carry the schema's
// description or enum content into its message. Those fields may
// contain operator- or model-supplied prose; surfacing them at error
// level would leak them into trace and log sinks.
func TestLintGeminiSchema_DoesNotLeakDescriptionOrEnum(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"x": {
				"type": "string",
				"pattern": "^foo",
				"description": "SECRET-DESCRIPTION-CONTENT",
				"enum": ["SECRET-ENUM-VALUE-1", "SECRET-ENUM-VALUE-2"]
			}
		}
	}`)

	err := LintGeminiSchema("leaky_tool", schema, []string{"pattern"})
	require.Error(t, err)
	msg := err.Error()

	leaks := []struct {
		name  string
		value string
	}{
		{name: "description", value: "SECRET-DESCRIPTION-CONTENT"},
		{name: "first enum value", value: "SECRET-ENUM-VALUE-1"},
		{name: "second enum value", value: "SECRET-ENUM-VALUE-2"},
	}
	for _, leak := range leaks {
		t.Run("omits "+leak.name, func(t *testing.T) {
			require.NotContains(t, msg, leak.value)
		})
	}
}

// TestLintGeminiSchema_UnparseableSchemaIsLintNoOp pins that an
// unparseable schema is left to ConvertSchema to reject — the linter
// must not duplicate the parse error nor mask it.
func TestLintGeminiSchema_UnparseableSchemaIsLintNoOp(t *testing.T) {
	schema := json.RawMessage(`{not valid json`)

	require.NoError(t, LintGeminiSchema("broken", schema, []string{"pattern"}))
}
