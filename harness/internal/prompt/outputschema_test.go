package prompt

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const testOutputSchema = `{"type":"object","required":["title"],"properties":{"title":{"type":"string"}}}`

func outputSchemaBuilders(t *testing.T) map[string]PromptBuilder {
	t.Helper()
	tmpl, err := NewTemplatePromptBuilder("Operator preamble for {{.Mode}}.", TemplateData{Model: "claude-sonnet-4-6", Mode: "planning"})
	if err != nil {
		t.Fatalf("NewTemplatePromptBuilder: %v", err)
	}
	return map[string]PromptBuilder{
		"default":  NewDefaultPromptBuilder(),
		"composed": NewComposedPromptBuilder(WithFragments(DefaultComposedFragments()...)),
		"override": NewOverridePromptBuilder("Fixed preamble."),
		"template": tmpl,
	}
}

func TestBuilders_RenderOutputSchemaSection(t *testing.T) {
	for name, b := range outputSchemaBuilders(t) {
		t.Run(name, func(t *testing.T) {
			got, err := b.Build(context.Background(), PromptContext{
				Mode:           "planning",
				Model:          "claude-sonnet-4-6",
				MaxTurns:       5,
				OutputSchema:   json.RawMessage(testOutputSchema),
				DynamicContext: map[string]string{"issue": "body"},
			})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if !strings.Contains(got, outputSchemaInstruction) {
				t.Errorf("prompt lacks the output schema instruction:\n%s", got)
			}
			wantSchema := "<output_schema>\n{\n  \"type\": \"object\",\n  \"required\": [\n    \"title\"\n  ],"
			if !strings.Contains(got, wantSchema) {
				t.Errorf("prompt lacks the indented schema:\n%s", got)
			}
			schemaAt := strings.Index(got, "<output_schema>")
			if untrustedAt := strings.Index(got, "<untrusted_context"); untrustedAt < schemaAt {
				t.Errorf("schema section at %d follows untrusted context at %d; operator instructions come first", schemaAt, untrustedAt)
			}
		})
	}
}

func TestBuilders_OmitOutputSchemaSectionWithoutSchema(t *testing.T) {
	for name, b := range outputSchemaBuilders(t) {
		t.Run(name, func(t *testing.T) {
			got, err := b.Build(context.Background(), PromptContext{Mode: "planning", Model: "claude-sonnet-4-6", MaxTurns: 5})
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if strings.Contains(got, "output_schema") || strings.Contains(got, "Final answer format") {
				t.Errorf("prompt without a schema mentions one:\n%s", got)
			}
		})
	}
}
