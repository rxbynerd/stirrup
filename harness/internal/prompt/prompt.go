// Package prompt defines the PromptBuilder interface and implementations for
// constructing system prompts from mode templates and dynamic context.
package prompt

import "context"

// PromptContext provides the information a prompt builder needs.
type PromptContext struct {
	Mode           string
	Workspace      string
	MaxTurns       int
	DynamicContext map[string]string
}

// PromptBuilder constructs a system prompt from a PromptContext.
type PromptBuilder interface {
	Build(ctx context.Context, pc PromptContext) (string, error)
}
