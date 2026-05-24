package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestLintGeminiSchema_EmptyUnsupportedIsNoOp pins the zero-cost path:
// every Gemini request resolves a quirks struct; the lint must do no
// work when no rule pinned any unsupported features. A non-zero cost
// here would tax every Gemini request whether or not the rule is
// active.
func TestLintGeminiSchema_EmptyUnsupportedIsNoOp(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"x":{"type":"string","pattern":"^foo"}}}`)
	if err := LintGeminiSchema("read_file", schema, nil); err != nil {
		t.Errorf("nil unsupported: got error %v, want nil", err)
	}
	if err := LintGeminiSchema("read_file", schema, []string{}); err != nil {
		t.Errorf("empty unsupported: got error %v, want nil", err)
	}
}

// TestLintGeminiSchema_RejectsTopLevelKeyword pins the basic match: a
// keyword listed in `unsupported` triggers the rejection when it
// appears at the root of the schema.
func TestLintGeminiSchema_RejectsTopLevelKeyword(t *testing.T) {
	schema := json.RawMessage(`{"type":"string","pattern":"^foo"}`)
	err := LintGeminiSchema("name_check", schema, []string{"pattern"})
	if err == nil {
		t.Fatalf("expected lint error, got nil")
	}
	var lintErr *geminiSchemaLintError
	if !errors.As(err, &lintErr) {
		t.Fatalf("expected *geminiSchemaLintError, got %T: %v", err, err)
	}
	if lintErr.tool != "name_check" {
		t.Errorf("tool = %q, want name_check", lintErr.tool)
	}
	if lintErr.keyword != "pattern" {
		t.Errorf("keyword = %q, want pattern", lintErr.keyword)
	}
	if !strings.Contains(err.Error(), "<root>") {
		t.Errorf("error %q should name <root>", err)
	}
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
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "properties.name") {
		t.Errorf("error %q should name properties.name", err)
	}
}

// TestLintGeminiSchema_RejectsInArrayItems pins the items descent. A
// schema whose array.items carries a forbidden keyword must still
// fail. Both the items-as-object and items-as-tuple forms are
// inspected.
func TestLintGeminiSchema_RejectsInArrayItems(t *testing.T) {
	schemaObject := json.RawMessage(`{
		"type": "array",
		"items": {"type": "string", "format": "uri"}
	}`)
	err := LintGeminiSchema("urls", schemaObject, []string{"format"})
	if err == nil {
		t.Fatalf("items-object: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "items") {
		t.Errorf("items-object: error %q should name items", err)
	}

	schemaTuple := json.RawMessage(`{
		"type": "array",
		"items": [{"type":"string"},{"type":"integer","format":"int32"}]
	}`)
	err = LintGeminiSchema("pair", schemaTuple, []string{"format"})
	if err == nil {
		t.Fatalf("items-tuple: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "items[1]") {
		t.Errorf("items-tuple: error %q should name items[1]", err)
	}
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
	err := LintGeminiSchema("clean", schema, []string{"pattern", "format"})
	if err != nil {
		t.Errorf("clean schema: got error %v, want nil", err)
	}
}

// TestLintGeminiSchema_DoesNotLeakDescriptionOrEnum pins the privacy
// contract from #228 §5: the lint error must NOT carry the schema's
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
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	msg := err.Error()
	for _, leak := range []string{"SECRET-DESCRIPTION-CONTENT", "SECRET-ENUM-VALUE-1", "SECRET-ENUM-VALUE-2"} {
		if strings.Contains(msg, leak) {
			t.Errorf("error %q leaks %q", msg, leak)
		}
	}
}

// TestLintGeminiSchema_UnparseableSchemaIsLintNoOp pins that an
// unparseable schema is left to ConvertSchema to reject — the linter
// must not duplicate the parse error nor mask it.
func TestLintGeminiSchema_UnparseableSchemaIsLintNoOp(t *testing.T) {
	schema := json.RawMessage(`{not valid json`)
	if err := LintGeminiSchema("broken", schema, []string{"pattern"}); err != nil {
		t.Errorf("unparseable schema: got error %v, want nil", err)
	}
}
