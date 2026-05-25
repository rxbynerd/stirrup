// Package zai provides the Z.ai GLM compatibility rule for the
// openai-compatible adapter. Z.ai exposes an OpenAI-compatible Chat
// Completions endpoint but with two divergences from the canonical
// upstream:
//
//   - It requires the legacy "max_tokens" key, not
//     "max_completion_tokens" (which OpenAI's reasoning models now
//     mandate).
//   - It accepts an extension flag "tool_stream: true" that enables
//     streaming of tool-call results.
//
// Z.ai is a compatibility-only target (design D3): there is no
// peer "zai" ProviderType. Operators using Z.ai configure
// provider.type = "openai-compatible" and set
// provider.compatProfile = "zai-glm" in their RunConfig; the
// factory injects CompatRule() into the adapter's registry.
//
// The rule is intentionally not part of BuiltinRules() — its
// activation is operator-gated through the CompatProfile field.
package zai

import (
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
)

// CompatRule returns the Z.ai-specific quirks rule for injection
// into an openai-compatible adapter's registry. The rule targets
// every GLM model under "glm-*" (the only family Z.ai serves today);
// operators using a custom alias must register a tailored rule
// themselves.
//
// Design risk 5 notes that the precise inbound-side semantics of
// tool_stream:true are not fully documented; the conservative
// assumption is that it is a send-side request flag with no impact
// on the parse-side SSE event shape. If a real Z.ai endpoint is
// later observed to emit a different event shape when tool_stream
// is true, an additional parse-side flag will be needed on
// OpenAIBehaviourFlags.
func CompatRule() quirks.Rule {
	return quirks.Rule{
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
	}
}
