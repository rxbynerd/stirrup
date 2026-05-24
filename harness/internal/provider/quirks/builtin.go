package quirks

// BuiltinRules returns the first-party rule set baked into the harness.
//
// Rules are added in the order they take effect under
// specificity-then-declaration-order (design D10): longer ModelMatch
// globs run last and win on overlapping fields. The current set:
//
//   - openai-compatible / "o[1-9]*"       — reasoning class. Matches
//     both bare aliases ("o1", "o3", "o4") and dash-suffixed variants
//     ("o1-mini", "o3-mini", "o4-mini"). Omits sampling params via
//     applyOpenAIReasoningClass. The "[1-9]" class requires the
//     leading digit be 1-9; two-digit series like "o10-mini" also
//     match because the trailing "*" consumes the second digit, so
//     forward-compat with a future o10+ alias is automatic.
//   - openai-compatible / "gpt-5*"        — reasoning class: same
//     behaviour as the o-series.
//   - openai-compatible / "gpt-5-chat*"   — carve-out: gpt-5-chat-latest
//     is a chat-class fork of the gpt-5 family and accepts the standard
//     sampling parameters. This rule undoes the gpt-5* omission for
//     models matching the longer glob.
//
// Composition example:
//   - "gpt-5-nano"        — matches "gpt-5*" only; OmitSamplingParams = true.
//   - "gpt-5-chat-latest" — matches "gpt-5*" then "gpt-5-chat*"; the
//     longer carve-out runs last and sets OmitSamplingParams = false.
//   - "gpt-4o"            — matches neither; zero-value behaviour.
//
// Operators who want a non-default rule (e.g. Z.ai compat) inject it
// via NewRegistry — see harness/internal/provider/compat/zai for the
// pattern.
//
// Gemini base rule: the "gemini / *" entry pins StreamArgsOff as the
// resolved value for every Gemini request. The flag is the zero value
// of GeminiStreamArgsShape, so the rule is a no-op functionally; it
// exists so every Gemini request explicitly resolves the StreamArgs
// decision through the registry, and so a future model-scoped rule
// (e.g. a Gemini 3.x V3Deltas pilot) only needs to add a more-
// specific entry without re-touching gemini_request.go. Aligns with
// design §7 Step 3.
func BuiltinRules() []Rule {
	return []Rule{
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "o[1-9]*",
			Description:  "OpenAI reasoning-class (o-series, single-digit prefix): omit sampling params",
			LastVerified: Date("2026-05-24"),
			Apply:        applyOpenAIReasoningClass,
		},
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "gpt-5*",
			Description:  "OpenAI gpt-5 family: omit sampling params (reasoning-class)",
			LastVerified: Date("2026-05-24"),
			Apply:        applyOpenAIReasoningClass,
		},
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "gpt-5-chat*",
			Description:  "OpenAI gpt-5-chat carve-out: chat-class accepts sampling params",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {
				// gpt-5-chat-latest is a chat-class fork of the gpt-5
				// family and accepts temperature / top_p / penalties.
				// The broader gpt-5* rule above set
				// OmitSamplingParams = true; clearing it here is the
				// carve-out. Specificity ordering (D10) guarantees this
				// rule runs after gpt-5*.
				q.BehaviourFlags.OpenAI.OmitSamplingParams = false
			},
		},
		{
			ProviderType: "gemini",
			ModelMatch:   "*",
			Description:  "Gemini: off streamFunctionCallArguments (post-#191 default)",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {
				// StreamArgsOff is the zero value, so this write is a
				// no-op functionally. The rule exists to document the
				// design intent: every Gemini request resolves the
				// StreamArgs decision via the registry, and a future
				// model-scoped rule (e.g. "gemini-3*" pinning
				// StreamArgsV3Deltas after live verification) can
				// override this baseline without touching the adapter.
				q.BehaviourFlags.Gemini.StreamFunctionCallArgsShape = StreamArgsOff
			},
		},
	}
}
