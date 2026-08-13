package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// TestGeminiSchemaLint_FailsClosedOnUnsupportedFeature pins that
// BuildGenerateContentRequest errors before marshalling when a tool's
// schema uses a JSON Schema keyword the resolved quirks list as
// unsupported; the error names the tool and offending field path.
func TestGeminiSchemaLint_FailsClosedOnUnsupportedFeature(t *testing.T) {

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

// TestGeminiSchemaLint_PassesCleanSchema pins that a schema using only
// Gemini-supported keywords clears the lint and proceeds to ConvertSchema.
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

	if !strings.Contains(string(body), "search") {
		t.Errorf("body should reference the tool: %s", body)
	}
}

// TestGeminiSchemaLint_IsNoOpForGemini25 pins that gemini-2.5, having no
// SchemaUnsupportedFeatures rule, accepts a schema with `pattern`.
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
