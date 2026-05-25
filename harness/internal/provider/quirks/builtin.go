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
		// --- Schema lint / strict-mode rules (#228, Wave 3 Step B) ---
		//
		// OpenAI structured-outputs strict mode is opt-in per model:
		// the wire body emits `strict: true` on each tool and the
		// adapter rewrites the InputSchema so every property is in
		// `required` with optionals nullable. The model surface that
		// supports strict mode is documented at
		// https://platform.openai.com/docs/guides/structured-outputs
		// and grows as the API expands.
		//
		// The rules below cover the model families that explicitly
		// list strict-mode support in the OpenAI function-calling
		// guide as of LastVerified. A new model family added to the
		// supported list needs its own rule; the existing
		// `gpt-5*`/`o[1-9]*` reasoning-class rules above (which omit
		// sampling params) compose cleanly because they touch a
		// different field.
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "gpt-4o-mini*",
			Description:  "OpenAI gpt-4o-mini: enable strict-mode structured outputs",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {
				// gpt-4o-mini supports strict mode per the OpenAI
				// structured-outputs guide. The flag drives the
				// adapter to rewrite each tool's InputSchema and
				// emit `strict: true` on the wire entry.
				//
				// The glob is `gpt-4o-mini*` rather than `gpt-4o*`
				// deliberately: OpenAI's structured-outputs guide
				// lists bare gpt-4o as supporting strict mode, but
				// strict-mode behaviour on the bare model has not
				// been verified against a current snapshot. Adding
				// a wider glob risks a HTTP 400 on a deployment
				// using a gpt-4o snapshot that diverges from the
				// guide. If strict mode is confirmed against a live
				// bare-gpt-4o request, add a separate rule (or
				// widen this glob) — the negative pin in
				// TestBuiltinRulesStrictMode catches the current
				// gap so opting in is a deliberate edit.
				q.BehaviourFlags.OpenAI.StrictMode = true
			},
		},
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "gpt-4.1*",
			Description:  "OpenAI gpt-4.1 family: enable strict-mode structured outputs",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {
				// gpt-4.1, gpt-4.1-mini, and gpt-4.1-nano all support
				// strict mode per the OpenAI structured-outputs guide.
				q.BehaviourFlags.OpenAI.StrictMode = true
			},
		},
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "gpt-5*",
			Description:  "OpenAI gpt-5 family: enable strict-mode structured outputs",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {
				// The gpt-5 family supports strict mode in addition to
				// the reasoning-class sampling-param omission applied
				// by the rule above. Specificity ordering (D10) puts
				// this rule after the existing gpt-5* reasoning rule
				// (declaration order tiebreak), so both writes take
				// effect — StrictMode = true and OmitSamplingParams
				// = true on the same resolution.
				//
				// Note: the existing gpt-5-chat* carve-out runs LAST
				// because its glob is longer. That rule clears
				// OmitSamplingParams but does not touch StrictMode,
				// so gpt-5-chat-latest will still emit strict tools.
				// If a future API change rejects strict mode on the
				// chat-class fork, extend the carve-out's Apply to
				// also clear StrictMode.
				q.BehaviourFlags.OpenAI.StrictMode = true
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
		// --- Tool-choice capability rules (#230, Wave 4 A1) ---
		//
		// tool_choice is a cross-provider capability, so the resolved
		// flag lives on the top-level ProviderQuirks.ToolChoice field
		// rather than under a provider sub-struct. Each first-party
		// provider declares a base "*" rule advertising the modes its
		// API supports; adapters gate serialisation on the resolved
		// flag and emit nothing when Supported is false. The escalation
		// chunk (A2) consumes the StreamParams.ToolChoice control these
		// rules make safe to serialise.
		{
			ProviderType: "anthropic",
			ModelMatch:   "*",
			Description:  "Anthropic: native tool_choice (auto/any/tool); no native none",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {
				// Anthropic's Messages API accepts a tool_choice object
				// with type "auto", "any", or "tool". There is no native
				// "none" — a no-tools turn is expressed by omitting the
				// tools array — so None stays false and the adapter
				// handles ToolChoiceNone structurally.
				q.ToolChoice = ToolChoiceCapability{
					Supported: true,
					Auto:      true,
					Required:  true,
					None:      false,
					NamedTool: true,
				}
			},
		},
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "*",
			Description:  "OpenAI-compatible: native tool_choice (auto/required/none/function)",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {
				// OpenAI Chat Completions accepts tool_choice as a string
				// ("auto"/"required"/"none") or an object naming a
				// specific function. The full surface is supported.
				q.ToolChoice = ToolChoiceCapability{
					Supported: true,
					Auto:      true,
					Required:  true,
					None:      true,
					NamedTool: true,
				}
			},
		},
		{
			ProviderType: "gemini",
			ModelMatch:   "*",
			Description:  "Gemini: native functionCallingConfig.mode (AUTO/ANY/NONE)",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {
				// Gemini expresses tool choice via
				// toolConfig.functionCallingConfig.mode (AUTO/ANY/NONE)
				// and a specific tool via ANY + allowedFunctionNames, so
				// every mode maps onto a native shape.
				q.ToolChoice = ToolChoiceCapability{
					Supported: true,
					Auto:      true,
					Required:  true,
					None:      true,
					NamedTool: true,
				}
			},
		},
		{
			ProviderType: "gemini",
			ModelMatch:   "gemini-3*",
			Description:  "Gemini 3: reject `pattern` and `format` keywords in tool schemas",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {
				// Gemini's function-declaration Schema dialect (a
				// subset of OpenAPI 3.0) does not reliably honour
				// `pattern` and `format` for tool inputs across the
				// Gemini 3.x rollout: some surfaces silently ignore
				// the keyword, others reject the request outright.
				// The lint takes the conservative position — reject
				// at request-build time so the operator sees a clear
				// failure rather than a tool whose validation rules
				// were quietly dropped by the wire transform.
				//
				// The built-in tool schemas do not use either
				// keyword today, so this rule has no observable
				// effect on the canonical surface; it catches
				// operator-supplied or MCP-imported schemas that
				// would otherwise hit Gemini's silent-drop path.
				q.BehaviourFlags.Gemini.SchemaUnsupportedFeatures = append(
					q.BehaviourFlags.Gemini.SchemaUnsupportedFeatures,
					"pattern", "format",
				)
			},
		},
	}
}
