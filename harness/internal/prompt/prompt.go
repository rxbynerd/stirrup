// Package prompt defines the PromptBuilder interface and implementations for
// constructing system prompts from mode templates and dynamic context.
package prompt

import (
	"context"
	"encoding/json"
)

// PromptContext provides the information a prompt builder needs.
type PromptContext struct {
	Mode      string
	Workspace string
	MaxTurns  int
	// Model is the resolved prompt model (RunConfig.EffectivePromptModel):
	// the model identity the system prompt templates render against. It may
	// differ from the wire model when promptBuilder.promptModel is set for
	// a prompt/model comparison run.
	Model          string
	DynamicContext map[string]string
	// OutputSchema is RunConfig.OutputSchema: the JSON Schema the final
	// answer must satisfy. Empty when the run returns free text.
	OutputSchema json.RawMessage
}

// PromptBuilder constructs a system prompt from a PromptContext.
type PromptBuilder interface {
	Build(ctx context.Context, pc PromptContext) (string, error)
}
