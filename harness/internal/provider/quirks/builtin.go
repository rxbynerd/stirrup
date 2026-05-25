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
		// --- ReplayFields rules (design §6.5, D12) ---
		//
		// Wave 2 lands parse-side recognition only. Each rule's
		// Description ends in "(parse-side only)" so trace consumers
		// know the captured value is observable but not yet threaded
		// back into outbound history. Outbound threading is design
		// §9 risk 7, deferred to a follow-up.
		//
		// The Description suffix is enforced by
		// TestBuiltinRulesParseSideOnlySuffix in replay_test.go.
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "deepseek-reasoner*",
			Description:  "DeepSeek reasoner: preserve reasoning_content (parse-side only)",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {
				// DeepSeek's reasoner family surfaces its chain-of-
				// thought as a `reasoning_content` field on each
				// assistant delta, alongside the canonical `content`
				// field. The DeepSeek API docs describe the field as
				// part of the model's response, and the openai-
				// compatible streaming layout places it at the same
				// nesting level as `content` (a direct child of
				// `delta`). The single-segment path captures it
				// directly when the adapter walks the choice's raw
				// delta object.
				//
				// Field-path verification status: unverified against a
				// live DeepSeek-reasoner capture as of LastVerified.
				// Mark the rule stale if not re-verified within the
				// 180-day window; the staleness test will surface it.
				q.ReplayFields = append(q.ReplayFields, "reasoning_content")
			},
		},
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "deepseek-v4*",
			Description:  "DeepSeek v4: preserve reasoning_content (parse-side only)",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {
				// DeepSeek's v4 series uses the same reasoning_content
				// field on the assistant delta as the reasoner family
				// per the DeepSeek API documentation. If a future v4
				// release diverges (e.g. adopts a structured
				// `thinking` field instead) the rule needs a
				// LastVerified bump and the path adjusted; the
				// staleness test will surface the prompt.
				q.ReplayFields = append(q.ReplayFields, "reasoning_content")
			},
		},
		{
			ProviderType: "gemini",
			ModelMatch:   "gemini-3*",
			Description:  "Gemini 3: preserve thoughtSignature on functionCall parts (parse-side only)",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {
				// Gemini 3.x emits `thoughtSignature` as a sibling
				// field on every `parts[]` entry alongside the
				// `functionCall` or `text` discriminator (see
				// gemini_types.go::geminiPart). The signature is
				// part-level state, not a child of the functionCall
				// itself — a path that descends through functionCall
				// captures nothing (pinned by
				// TestCaptureReplayFields_GeminiToolCall in
				// replay_test.go).
				//
				// The path uses [] array iteration twice because the
				// response shape nests candidates (a list, even when
				// only one is requested) of content with parts (also
				// a list, one per textual/functionCall chunk).
				q.ReplayFields = append(q.ReplayFields,
					"candidates[].content.parts[].thoughtSignature",
				)
			},
		},
	}
}
