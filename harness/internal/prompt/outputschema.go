package prompt

import (
	"bytes"
	"context"
	"encoding/json"
)

const outputSchemaInstruction = "Final answer format: end the task with a final message that is exactly one JSON document conforming to the JSON Schema in <output_schema> below. Write nothing before or after the document; a single ```json fenced block containing only the document is also accepted. This applies only to the final message that ends your turn, not to messages that accompany tool calls. A final message that does not conform is returned to you with the validation error to correct."

// renderOutputSchemaSection is the system-prompt section that tells the
// model to answer with a document conforming to schema; empty when no
// schema is configured.
func renderOutputSchemaSection(schema json.RawMessage) string {
	if len(schema) == 0 {
		return ""
	}
	var indented bytes.Buffer
	body := string(schema)
	if err := json.Indent(&indented, schema, "", "  "); err == nil {
		body = indented.String()
	}
	return outputSchemaInstruction + "\n\n<output_schema>\n" + body + "\n</output_schema>"
}

type outputSchemaFragment struct{}

// OutputSchemaFragment returns a fragment that renders the output schema
// instruction. If OutputSchema is empty, the fragment produces an empty
// string.
func OutputSchemaFragment() PromptFragment { return &outputSchemaFragment{} }

func (f *outputSchemaFragment) Render(_ context.Context, pc PromptContext) (string, error) {
	return renderOutputSchemaSection(pc.OutputSchema), nil
}
