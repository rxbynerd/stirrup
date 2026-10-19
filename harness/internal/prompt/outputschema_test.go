package prompt

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const testOutputSchema = `{"type":"object","required":["title"],"properties":{"title":{"type":"string"}}}`

func outputSchemaBuilders(t *testing.T) map[string]PromptBuilder {
	t.Helper()
	tmpl, err := NewTemplatePromptBuilder("Operator preamble for {{.Mode}}.", TemplateData{Model: "claude-sonnet-4-6", Mode: "planning"})
	require.NoError(t, err)
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
			require.NoError(t, err)

			require.Contains(t, got, outputSchemaInstruction, "prompt lacks the output schema instruction")

			wantSchema := "<output_schema>\n{\n  \"type\": \"object\",\n  \"required\": [\n    \"title\"\n  ],"
			require.Contains(t, got, wantSchema, "prompt lacks the indented schema")

			schemaAt := strings.Index(got, "<output_schema>")
			untrustedAt := strings.Index(got, "<untrusted_context")
			require.Less(t, schemaAt, untrustedAt, "operator instructions must come before untrusted context")
		})
	}
}

func TestBuilders_OmitOutputSchemaSectionWithoutSchema(t *testing.T) {
	for name, b := range outputSchemaBuilders(t) {
		t.Run(name, func(t *testing.T) {
			got, err := b.Build(context.Background(), PromptContext{Mode: "planning", Model: "claude-sonnet-4-6", MaxTurns: 5})
			require.NoError(t, err)

			require.NotContains(t, got, "output_schema", "prompt without a schema mentions one")
			require.NotContains(t, got, "Final answer format", "prompt without a schema mentions one")
		})
	}
}
