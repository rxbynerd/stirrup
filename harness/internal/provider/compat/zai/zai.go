// Package zai provides the Z.ai GLM compatibility rules for the
// openai-compatible adapter. Z.ai exposes an OpenAI-compatible Chat
// Completions endpoint but with several divergences from the canonical
// upstream:
//
//   - It requires the legacy "max_tokens" key, not
//     "max_completion_tokens" (which OpenAI's reasoning models now
//     mandate). This applies to every GLM model.
//   - It accepts an extension flag "tool_stream: true" that enables
//     streaming of tool-call results.
//   - The GLM-4.5-and-above "thinking" family (glm-4.5, glm-4.6,
//     glm-4.7, and the forthcoming glm-5 line) emits a
//     chain-of-thought as `delta.reasoning_content` alongside the
//     assistant content, and accepts a top-level "thinking" request
//     object ({"type":"enabled"}). For multi-turn tool calling the
//     captured reasoning_content must be replayed onto subsequent
//     requests to keep the reasoning coherent — wired here via
//     ReplayFields, which the openai-compatible adapter threads back
//     outbound with no adapter-side code.
//
// The thinking divergences are scoped to GLM-4.5 and above: the
// older hyphenated line (glm-4-plus, glm-4-flash, glm-4-air) has no
// thinking mode and must not receive the thinking quirks. The glob
// boundary is the dot after "4." — see CompatRules for the exact
// matching rationale.
//
// Z.ai is a compatibility-only target (design D3): there is no
// peer "zai" ProviderType. Operators using Z.ai configure
// provider.type = "openai-compatible" and set
// provider.compatProfile = "zai-glm" in their RunConfig; the
// factory injects CompatRules() into the adapter's registry.
//
// The rules are intentionally not part of BuiltinRules() — their
// activation is operator-gated through the CompatProfile field.
package zai

import (
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
)

// CompatRules returns the Z.ai-specific quirks rules for injection
// into an openai-compatible adapter's registry. Quirks compose:
// quirks.Registry.Resolve runs every matching rule's Apply in
// specificity-then-declaration order, all mutating the same
// ProviderQuirks. The base "glm-*" rule therefore supplies the
// shared wire shape (legacy token field, tool_stream) and the more
// specific thinking-family rules ADD to it without re-setting those
// fields.
//
// LastVerified dates are doc-derived (Z.ai docs + model card +
// OpenRouter listing); live first-party capture is still pending for
// the thinking-family rules (token-key acceptance, thinking wire
// placement, reasoning_content replay strictness, parallel tool
// calls) — same provenance discipline as the DeepSeek v4 rules.
//
// Design risk 5 notes that the precise inbound-side semantics of
// tool_stream:true are not fully documented; the conservative
// assumption is that it is a send-side request flag with no impact
// on the parse-side SSE event shape. If a real Z.ai endpoint is
// later observed to emit a different event shape when tool_stream
// is true, an additional parse-side flag will be needed on
// OpenAIBehaviourFlags.
func CompatRules() []quirks.Rule {
	return []quirks.Rule{
		// Rule A — base: every GLM model, including the legacy
		// hyphenated line (glm-4-plus, glm-4-flash, glm-4-air).
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
		// Rule B — GLM-4.5/4.6/4.7 thinking family. The glob uses a
		// dot ("4.") so it matches glm-4.5, glm-4.6, glm-4.7, and
		// glm-4.5-air but NOT the hyphenated legacy line
		// (glm-4-plus, glm-4-flash, glm-4-air), which has no thinking
		// mode. Thinking is "GLM-4.5 and above" only — applying the
		// thinking quirks to the legacy line would be wrong. The
		// [5-9] char class bounds the family at glm-4.9 (precedent:
		// the o[1-9]* builtin rule); glm-4.10+ would need a revisit.
		//
		// Provenance: doc-derived from the Z.ai thinking-mode and
		// core-parameters guides plus the GLM-4.7 model card (sources
		// in tmp/glm-4.7-quirks-plan.md §1). Not yet verified against a
		// live first-party capture — same discipline as the DeepSeek v4
		// builtin rules.
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "glm-4.[5-9]*",
			Description:  "Z.ai GLM-4.5+ thinking: replay reasoning_content, enable thinking (threaded)",
			LastVerified: quirks.Date("2026-06-08"),
			Apply:        applyThinkingFamily,
		},
		// Rule C — GLM-5/5.1 thinking family. Shares applyThinkingFamily
		// with Rule B. glm-5* covers glm-5, glm-5.1, and future minor
		// versions on the GLM-5 line, all of which inherit the thinking
		// surface.
		//
		// Breadth caveat: glm-5* is deliberately broader than the
		// verified id set — it forward-matches an as-yet-unreleased
		// line. If Z.ai later ships a non-thinking GLM-5 variant (a
		// hypothetical glm-5-flash), it would need a longer-glob
		// carve-out that clears these flags, exactly as gpt-5-chat*
		// carves out of gpt-5* in the builtin rules. Provenance: same
		// doc-derived, pending-live-capture status as Rule B.
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "glm-5*",
			Description:  "Z.ai GLM-5 thinking: replay reasoning_content, enable thinking (threaded)",
			LastVerified: quirks.Date("2026-06-08"),
			Apply:        applyThinkingFamily,
		},
		// Rule D — OpenRouter (and OpenRouter-style gateway) ids,
		// which serve GLM under a vendor prefix (z-ai/glm-4.7). The
		// bare "glm-*" glob cannot match these: path.Match's `*` does
		// not cross `/`, so this sibling rule names the prefix
		// literally — the same construction as the deepseek/deepseek-v4*
		// gateway rule.
		//
		// Only the portable quirks are mirrored: the legacy max_tokens
		// field and reasoning_content replay. tool_stream and the
		// thinking object are NOT set here — those are first-party
		// vendor extras whose behaviour through gateways is unverified
		// (a gateway may reject or silently drop them). When live
		// gateway capture confirms them, widen this rule.
		//
		// Provenance: the OpenRouter GLM-4.7 listing
		// (https://openrouter.ai/z-ai/glm-4.7) plus the first-party
		// docs above; not yet verified against a live gateway capture.
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
		// Explicit non-goals across all rules above (no Apply sets
		// these, by design):
		//   - OmitSamplingParams: GLM accepts and recommends
		//     temperature=1.0, top_p=0.95 (verified) — unlike DeepSeek
		//     v4 / the OpenAI reasoning class, sampling params are NOT
		//     suppressed.
		//   - StrictMode / ToolChoice / ParallelToolCalls overrides:
		//     left at their base-rule values; no GLM-specific override.
		//   - clear_thinking: not pinned (see Rule B comment).
	}
}

// applyThinkingFamily is the shared Apply for the GLM-4.5+ thinking
// rules (Rules B and C). Both lines expose the identical thinking
// surface, so the body lives in one place — a field added here reaches
// both rules — mirroring the applyOpenAIReasoningClass precedent in
// quirks/helpers.go.
func applyThinkingFamily(q *quirks.ProviderQuirks) {
	// reasoning_content arrives as a sibling of `content` on each
	// assistant delta (a direct child of `delta`), so the
	// single-segment path captures it when the adapter walks the
	// decoded delta object. The Z.ai thinking-mode docs require it be
	// replayed across multi-turn tool calls to keep the reasoning
	// coherent; the openai-compatible adapter threads it back outbound.
	// Replaying on every turn is safe — the field is optional, not
	// forbidden, on non-thinking turns.
	q.ReplayFields = append(q.ReplayFields, "reasoning_content")
	// The top-level "thinking" object turns thinking mode
	// deterministically on for agentic tool use (matches the documented
	// default). On the openai-compatible endpoint it is a top-level body
	// key (OpenAI SDK extra_body → top-level). clear_thinking is
	// deliberately NOT pinned: its server-side semantics are unverified
	// by live capture and Preserved-Thinking behaviour is already
	// carried client-side by replaying reasoning_content.
	q.BehaviourFlags.OpenAI.ExtraBodyFields["thinking"] = map[string]any{"type": "enabled"}
}
