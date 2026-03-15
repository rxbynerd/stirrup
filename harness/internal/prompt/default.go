package prompt

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// modeTemplates maps each run mode to its base system prompt.
var modeTemplates = map[string]string{
	"execution": "You are a coding agent. Make changes, run tests, iterate until done.",
	"planning":  "Analyze the codebase and produce a step-by-step implementation plan. You have read-only access.",
	"review":    "Review the following changes. Identify bugs, style issues, missed edge cases, and opportunities.",
	"research":  "Research the following topic. Explore the codebase, read documentation, synthesize findings.",
	"toil":      "Check for the specified trigger. Prepare a briefing for the engineer.",
}

// DefaultPromptBuilder constructs system prompts from hardcoded mode templates
// and wraps dynamic context in untrusted_context delimiters.
type DefaultPromptBuilder struct{}

// NewDefaultPromptBuilder creates a new DefaultPromptBuilder.
func NewDefaultPromptBuilder() *DefaultPromptBuilder {
	return &DefaultPromptBuilder{}
}

// Build constructs the system prompt for the given context.
func (b *DefaultPromptBuilder) Build(_ context.Context, pc PromptContext) (string, error) {
	tmpl, ok := modeTemplates[pc.Mode]
	if !ok {
		return "", fmt.Errorf("unknown mode: %q", pc.Mode)
	}

	var sb strings.Builder
	sb.WriteString(tmpl)

	if len(pc.DynamicContext) > 0 {
		sb.WriteString("\n\n")
		// Sort keys for deterministic output.
		keys := make([]string, 0, len(pc.DynamicContext))
		for k := range pc.DynamicContext {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, k := range keys {
			sb.WriteString(fmt.Sprintf("<untrusted_context name=%q>%s</untrusted_context>\n", k, pc.DynamicContext[k]))
		}
	}

	return sb.String(), nil
}
