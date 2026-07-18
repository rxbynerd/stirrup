// Package zai provides Z.ai GLM compatibility rules for the
// openai-compatible adapter, injected when provider.compatProfile =
// "zai-glm" (docs/provider-quirks.md#6-zai-compat-placement). Z.ai
// has no peer ProviderType; rule details, provenance, and glob
// boundaries are documented there, not repeated here.
package zai

import (
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
)

// CompatRules returns the Z.ai-specific quirks rules for injection
// into an openai-compatible adapter's registry. Rules compose:
// quirks.Registry.Resolve runs every matching rule's Apply in
// specificity-then-declaration order, so the base "glm-*" rule
// supplies the shared wire shape and the more specific thinking-family
// rules add to it. See docs/provider-quirks.md#6-zai-compat-placement
// for the full rule table and provenance.
func CompatRules() []quirks.Rule {
	return []quirks.Rule{

		{
			ProviderType: "openai-compatible",
			ModelMatch:   "glm-*",
			Description:  "Z.ai GLM: legacy max_tokens field and tool_stream extension",
			LastVerified: quirks.Date("2026-05-24"),
			Apply: func(q *quirks.ProviderQuirks) {
				q.BehaviourFlags.OpenAI.TokenField = quirks.TokenFieldMaxTokens
				// ExtraBodyFields is pre-initialised by Resolve to an
				// empty non-nil map, so direct assignment is safe.
				q.BehaviourFlags.OpenAI.ExtraBodyFields["tool_stream"] = true
			},
		},
		// The [5-9] char class bounds this at glm-4.9; glm-4.10 would
		// need a revisit (see TestZAICompatRules_GLM410IsAKnownGap).
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "glm-4.[5-9]*",
			Description:  "Z.ai GLM-4.5+ thinking: replay reasoning_content, enable thinking (threaded)",
			LastVerified: quirks.Date("2026-06-08"),
			Apply:        applyThinkingFamily,
		},

		{
			ProviderType: "openai-compatible",
			ModelMatch:   "glm-5*",
			Description:  "Z.ai GLM-5 thinking: replay reasoning_content, enable thinking (threaded)",
			LastVerified: quirks.Date("2026-06-08"),
			Apply:        applyThinkingFamily,
		},

		{
			ProviderType: "openai-compatible",
			ModelMatch:   "z-ai/glm-*",
			Description:  "Z.ai GLM via gateway prefix: legacy max_tokens, replay reasoning_content (threaded)",
			LastVerified: quirks.Date("2026-06-08"),
			Apply: func(q *quirks.ProviderQuirks) {
				q.BehaviourFlags.OpenAI.TokenField = quirks.TokenFieldMaxTokens
				q.ReplayFields = append(q.ReplayFields, "reasoning_content")
			},
		},
	}
}

// applyThinkingFamily is the shared Apply for the GLM-4.5+ and GLM-5
// thinking-family rules; both lines expose an identical thinking
// surface.
func applyThinkingFamily(q *quirks.ProviderQuirks) {

	q.ReplayFields = append(q.ReplayFields, "reasoning_content")

	q.BehaviourFlags.OpenAI.ExtraBodyFields["thinking"] = map[string]any{"type": "enabled"}
}
