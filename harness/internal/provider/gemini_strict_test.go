package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// TestGeminiSchemaLint_FailsClosedOnUnsupportedFeature pins the
// Gemini lint contract: when the resolved quirks list a JSON Schema
// keyword as unsupported and a tool's schema uses it,
// BuildGenerateContentRequest returns an error BEFORE marshalling
// the request body. The error names the tool and offending field
// path so an operator can locate the issue without grepping the
// schema.
func TestGeminiSchemaLint_FailsClosedOnUnsupportedFeature(t *testing.T) {
	// Build the quirks struct as if the gemini-3 rule fired.
	q := quirks.DefaultRegistry().Resolve("gemini", "gemini-3-pro")
	if len(q.BehaviourFlags.Gemini.SchemaUnsupportedFeatures) == 0 {
		t.Fatalf("gemini-3 rule should pin SchemaUnsupportedFeatures, got empty")
	}

	params := types.StreamParams{
		Model:    "gemini-3-pro",
		Messages: []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools: []types.ToolDefinition{
			{
				Name:        "name_check",
				Description: "uses pattern which gemini-3 rejects",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","pattern":"^[a-z]+$"}},"required":["name"]}`),
			},
		},
	}
	_, _, err := BuildGenerateContentRequest(params, nil, q)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "name_check") {
		t.Errorf("error %q does not name the tool", err)
	}
	if !strings.Contains(err.Error(), "pattern") {
		t.Errorf("error %q does not name the offending keyword", err)
	}
	if !strings.Contains(err.Error(), "properties.name") {
		t.Errorf("error %q does not name the offending field path", err)
	}
}

// TestGeminiSchemaLint_PassesCleanSchema pins that a tool whose schema
// uses only Gemini-supported keywords clears the lint and proceeds to
// ConvertSchema as before.
func TestGeminiSchemaLint_PassesCleanSchema(t *testing.T) {
	q := quirks.DefaultRegistry().Resolve("gemini", "gemini-3-pro")
	params := types.StreamParams{
		Model:    "gemini-3-pro",
		Messages: []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools: []types.ToolDefinition{
			{
				Name:        "search",
				Description: "clean schema",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
			},
		},
	}
	body, _, err := BuildGenerateContentRequest(params, nil, q)
	if err != nil {
		t.Fatalf("clean schema rejected: %v", err)
	}
	// Sanity check: the body should carry the tool name.
	if !strings.Contains(string(body), "search") {
		t.Errorf("body should reference the tool: %s", body)
	}
}

// TestGeminiSchemaLint_IsNoOpForGemini25 pins the negative case: the
// gemini-2.5 surface has no SchemaUnsupportedFeatures rule today, so a
// schema with `pattern` proceeds to ConvertSchema. ConvertSchema
// passes unknown keywords through, so the request body still
// serialises.
func TestGeminiSchemaLint_IsNoOpForGemini25(t *testing.T) {
	q := quirks.DefaultRegistry().Resolve("gemini", "gemini-2.5-pro")
	if len(q.BehaviourFlags.Gemini.SchemaUnsupportedFeatures) != 0 {
		t.Fatalf("gemini-2.5 should not pin SchemaUnsupportedFeatures: %v", q.BehaviourFlags.Gemini.SchemaUnsupportedFeatures)
	}
	params := types.StreamParams{
		Model:    "gemini-2.5-pro",
		Messages: []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}}},
		Tools: []types.ToolDefinition{
			{
				Name:        "with_pattern",
				Description: "schema uses pattern",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","pattern":"^[a-z]+$"}},"required":["name"]}`),
			},
		},
	}
	_, _, err := BuildGenerateContentRequest(params, nil, q)
	if err != nil {
		t.Errorf("gemini-2.5 should accept schema with pattern (no lint rule), got error %v", err)
	}
}
