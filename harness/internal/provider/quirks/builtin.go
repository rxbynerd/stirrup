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
		// Each rule's Description ends in exactly one of two markers so
		// trace consumers know what happens to the captured value:
		//
		//   - "(threaded)"        — the value is captured parse-side AND
		//     echoed back onto subsequent requests (the §9 risk 7
		//     outbound threading, implemented for openai-compatible).
		//     Threaded paths must be single-segment and must not collide
		//     with a canonical wire-message key.
		//   - "(parse-side only)" — the value surfaces in length-only
		//     observability (slog/OTel) but is not round-tripped via
		//     this surface. Gemini's real round-trip is the typed
		//     block-level ThoughtSignature, so its ReplayFields rule
		//     stays parse-side.
		//
		// The suffix convention and the threadable-path constraint are
		// enforced by TestBuiltinRulesReplayFieldsSuffix in
		// replay_test.go.
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "deepseek-reasoner*",
			Description:  "DeepSeek reasoner: replay reasoning_content, omit sampling params, legacy max_tokens (threaded)",
			LastVerified: Date("2026-06-07"),
			Apply: func(q *ProviderQuirks) {
				// SUNSET: deepseek-reasoner is now a first-party
				// compatibility alias for deepseek-v4-flash thinking
				// mode, fully retired after 2026-07-24 15:59 UTC
				// (https://api-docs.deepseek.com/updates). Remove this
				// rule after that date. Note the legacy reasoning-model
				// guide and the v4 thinking-mode guide give conflicting
				// input semantics for reasoning_content during the
				// alias window; the explicit deepseek-v4* ids below are
				// the supported target.
				//
				// reasoning_content arrives as a sibling of `content`
				// on each assistant delta (a direct child of `delta`),
				// so the single-segment path captures it when the
				// adapter walks the choice's raw delta object. The
				// shape is documented in both the legacy reasoning-model
				// guide and the v4 thinking-mode guide; not yet verified
				// against a live first-party capture.
				q.ReplayFields = append(q.ReplayFields, "reasoning_content")
				// The reasoner lineage ignores temperature / top_p /
				// penalties ("will not trigger an error but will also
				// have no effect") while logprobs / top_logprobs DO
				// trigger an API error — see
				// https://api-docs.deepseek.com/guides/reasoning_model.
				// OmitSamplingParams covers both sets: hygiene for the
				// inert params, 400-protection for the log* ones.
				q.BehaviourFlags.OpenAI.OmitSamplingParams = true
				// The reasoner guide documents max_tokens (32K default,
				// 64K max) and never mentions max_completion_tokens.
				// Doc-derived, pending live-capture verification of
				// whether the modern key is also accepted.
				q.BehaviourFlags.OpenAI.TokenField = TokenFieldMaxTokens
			},
		},
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "deepseek-v4*",
			Description:  "DeepSeek v4: replay reasoning_content, omit sampling params, legacy max_tokens (threaded)",
			LastVerified: Date("2026-06-07"),
			Apply: func(q *ProviderQuirks) {
				// DeepSeek v4 (deepseek-v4-flash / deepseek-v4-pro) is
				// dual-mode with thinking DEFAULT-ON, emitting
				// chain-of-thought as `delta.reasoning_content`. The
				// thinking-mode guide
				// (https://api-docs.deepseek.com/guides/thinking_mode)
				// is explicit that "for turns that do perform tool
				// calls, the reasoning_content must be fully passed
				// back to the API in all subsequent requests" and that
				// the API returns a 400 error otherwise — so this rule
				// is load-bearing for multi-turn tool-calling, not just
				// observability. Replaying on all turns is safe: the
				// field is optional, not forbidden, on non-tool-call
				// turns. Doc-verified 2026-06-07 with broad community
				// 400-report corroboration; not yet verified against a
				// live first-party capture.
				q.ReplayFields = append(q.ReplayFields, "reasoning_content")
				// Thinking mode IGNORES temperature / top_p / penalties
				// ("will not trigger an error but will also have no
				// effect") — omitting them is hygiene, not
				// 400-protection — while logprobs / top_logprobs
				// hard-error on the reasoner lineage. See
				// https://api-docs.deepseek.com/guides/thinking_mode.
				q.BehaviourFlags.OpenAI.OmitSamplingParams = true
				// DeepSeek docs use the legacy max_tokens key throughout
				// and never mention max_completion_tokens. Doc-derived,
				// pending live-capture verification of whether
				// max_completion_tokens is also accepted.
				q.BehaviourFlags.OpenAI.TokenField = TokenFieldMaxTokens
				// Deliberately NOT set: StrictMode (per-tool strict is
				// Beta-only on a separate /beta base URL with a known
				// malformed-JSON bug — deepseek-ai/DeepSeek-V3#1069) and
				// any ToolChoice/ParallelToolCalls/ToolExamples override
				// (the openai-compatible base rules already advertise
				// the documented v4 surface: full tool_choice, 128
				// tools). parallel_tool_calls is undocumented for
				// DeepSeek; live-capture TODO alongside required/named
				// tool-choice honouring in thinking mode.
			},
		},
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "deepseek/deepseek-v4*",
			Description:  "DeepSeek v4 via gateway prefix: replay reasoning_content, omit sampling params, legacy max_tokens (threaded)",
			LastVerified: Date("2026-06-07"),
			Apply: func(q *ProviderQuirks) {
				// OpenRouter (and OpenRouter-style gateways) serve the
				// same models under prefixed ids
				// (deepseek/deepseek-v4-flash, deepseek/deepseek-v4-pro)
				// that the bare deepseek-v4* glob cannot match:
				// path.Match's `*` does not cross `/`, so this sibling
				// rule names the prefix literally. The slash also means
				// the longer glob sorts after deepseek-v4* under
				// specificity ordering (D10) — moot today because the
				// two globs are disjoint, but pinned by test so a future
				// overlap composes predictably.
				//
				// Community reports through OpenRouter show the same
				// 400-on-missing-replay behaviour as the first-party
				// endpoint, so the full v4 quirk set is mirrored.
				// Unverified gateway divergences, needing live capture
				// before any rule change: some gateways rename the
				// field to `reasoning` (no path for it here until
				// observed), and gateway thinking-mode defaults may
				// diverge from first-party default-on.
				//
				// Note: the openai-compatible base "*" rules (tool
				// choice, parallel tool calls, schema examples) do NOT
				// match slash-prefixed ids for the same path.Match
				// reason. The "*/*" sibling rules alongside them now
				// restore those capabilities for one level of vendor
				// prefix, so gateway DeepSeek ids advertise the same
				// tool surface as the bare ids — this rule only adds the
				// DeepSeek-specific wire behaviour (replay, sampling,
				// token key) on top.
				q.ReplayFields = append(q.ReplayFields, "reasoning_content")
				q.BehaviourFlags.OpenAI.OmitSamplingParams = true
				q.BehaviourFlags.OpenAI.TokenField = TokenFieldMaxTokens
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
			ProviderType: "openai-compatible",
			ModelMatch:   "*/*",
			Description:  "OpenAI-compatible vendor-prefixed ids: native tool_choice (auto/required/none/function)",
			LastVerified: Date("2026-06-30"),
			Apply: func(q *ProviderQuirks) {
				// path.Match's `*` does not cross `/`, so the bare "*" rule
				// above misses the vendor/model ids that LM Studio,
				// OpenRouter, and similar gateways serve (qwen/qwen3.6-27b,
				// deepseek/deepseek-v4-flash, mlx-community/...). This
				// sibling restores the identical tool_choice surface for one
				// level of prefix so a locally-hosted model is not silently
				// denied native tool choice. The two globs are disjoint (a
				// bare id never contains a slash; a prefixed id never matches
				// "*"), so there is no ordering interaction. Deeper nesting
				// (a/b/c) is not observed in the wild and intentionally
				// unmatched.
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
		// --- Structured tool-result capability rules (#231, Wave 4 B2) ---
		//
		// "tool results can carry structure" is a cross-provider concept, so
		// the resolved capability lives on the top-level
		// ProviderQuirks.StructuredToolResults field rather than a provider
		// sub-struct. Each first-party provider declares a base "*" rule
		// advertising the wire shape its API actually accepts; the adapters
		// gate serialisation on the resolved capability and send only the
		// text Content when Supported is false. A provider with no rule here
		// (e.g. bedrock) therefore stays text-only by construction.
		{
			ProviderType: "anthropic",
			ModelMatch:   "*",
			Description:  "Anthropic: tool_result content accepts a content-block array (text + structured JSON block)",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {
				// Anthropic's Messages API accepts tool_result `content`
				// as either a plain string or an array of content blocks
				// (https://docs.anthropic.com/en/api/messages — tool_result
				// content is `string | Array<text|image>`). There is no
				// native JSON content type, so the structured envelope is
				// carried as an additional `text` block alongside the
				// canonical text block. A model (or downstream tool) that
				// only reads the first block still receives the text
				// fallback verbatim.
				q.StructuredToolResults = StructuredToolResultCapability{
					Supported:         true,
					ContentBlockArray: true,
				}
			},
		},
		{
			ProviderType: "gemini",
			ModelMatch:   "*",
			Description:  "Gemini: functionResponse.response is a free-form JSON object (carries structured envelope)",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {
				// Vertex AI's functionResponse.response field is a free-form
				// JSON object (https://cloud.google.com/vertex-ai/generative-ai/docs/multimodal/function-calling
				// — FunctionResponse.response is a Struct). The structured
				// envelope maps directly into that object; the canonical
				// text rides alongside under a reserved "content" key so a
				// model that ignores the structured fields still sees the
				// text fallback. OpenAI is deliberately absent from this rule
				// set: a Chat Completions `tool` message and a Responses
				// function_call_output are both plain strings on the wire, so
				// the OpenAI adapters stay text-only with no capability.
				q.StructuredToolResults = StructuredToolResultCapability{
					Supported:      true,
					ObjectResponse: true,
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
		// --- Parallel-tool-call + input-example capability rules (#222) ---
		//
		// Both are cross-provider capabilities, so the resolved flags live on
		// the top-level ProviderQuirks.ParallelToolCalls / .ToolExamples
		// fields rather than a provider sub-struct. Each first-party provider
		// that has a native parallel control and/or accepts the JSON-Schema
		// `examples` keyword declares a base "*" rule; the adapters gate
		// serialisation on the resolved capability and emit nothing when it is
		// unsupported.
		//
		// Gemini and Bedrock are deliberately absent: Gemini has no native
		// parallel control and its Schema dialect rejects `examples` (the
		// example still reaches the model via the #227 description text), and
		// the Bedrock Converse API has neither control. Both therefore stay at
		// the zero-value (unsupported) capability by construction — pinned by
		// the negative assertions in TestParallelToolCallsCapabilityRules and
		// TestToolExamplesCapabilityRules.
		{
			ProviderType: "anthropic",
			ModelMatch:   "*",
			Description:  "Anthropic: disable_parallel_tool_use on tool_choice; accepts schema examples",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {
				// Anthropic expresses "no parallel" via
				// tool_choice.disable_parallel_tool_use; there is no top-level
				// field, so the adapter synthesises a tool_choice object when a
				// disable is requested. The Messages API passes through
				// arbitrary JSON-Schema keywords in input_schema, so `examples`
				// reaches the model.
				q.ParallelToolCalls = ParallelToolCallsCapability{Supported: true, Disable: true}
				q.ToolExamples = ToolExamplesCapability{Supported: true}
			},
		},
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "*",
			Description:  "OpenAI-compatible: top-level parallel_tool_calls; accepts schema examples",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {
				// OpenAI Chat Completions accepts a top-level
				// `parallel_tool_calls` bool (either direction) and passes
				// through the JSON-Schema `examples` keyword in a function's
				// parameters object.
				q.ParallelToolCalls = ParallelToolCallsCapability{Supported: true, Disable: true}
				q.ToolExamples = ToolExamplesCapability{Supported: true}
			},
		},
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "*/*",
			Description:  "OpenAI-compatible vendor-prefixed ids: top-level parallel_tool_calls; accepts schema examples",
			LastVerified: Date("2026-06-30"),
			Apply: func(q *ProviderQuirks) {
				// Sibling of the bare "*" rule for vendor/model ids served by
				// LM Studio / OpenRouter / similar gateways; see the
				// tool_choice "*/*" rule for why path.Match needs both globs.
				q.ParallelToolCalls = ParallelToolCallsCapability{Supported: true, Disable: true}
				q.ToolExamples = ToolExamplesCapability{Supported: true}
			},
		},
		{
			ProviderType: "openai-responses",
			ModelMatch:   "*",
			Description:  "OpenAI Responses: typed input items, max_output_tokens, store:false; top-level parallel_tool_calls; accepts schema examples",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {
				// The Responses API shares the Chat Completions
				// `parallel_tool_calls` bool and schema passthrough.
				q.ParallelToolCalls = ParallelToolCallsCapability{Supported: true, Disable: true}
				q.ToolExamples = ToolExamplesCapability{Supported: true}
				// Pin the Responses-specific wire divergences so the
				// resolved quirks struct is the single source of truth for
				// the adapter's send path (the Codec invariant that already
				// holds for Chat/Anthropic/Gemini). Each value is the zero
				// value of its enum, so the write is a no-op functionally and
				// the emitted bytes are byte-identical to the pre-quirks
				// hard-coded shape — the pin documents the decision and gives
				// a future model-scoped rule somewhere to override.
				q.BehaviourFlags.OpenAIResponses.TokenField = TokenFieldMaxOutputTokens
				q.BehaviourFlags.OpenAIResponses.StoreMode = StoreFalse
				q.BehaviourFlags.OpenAIResponses.InputItemShape = TypedInputItems
			},
		},
		// --- Anthropic sampling-param omission rules ---
		//
		// Claude Opus 4.7 removed support for non-default temperature /
		// top_p / top_k (a 400 error, not a silent ignore); Claude Opus 4.8,
		// Claude Sonnet 5, and Claude Fable 5 / Mythos 5 inherit the same
		// constraint. The harness's loop unconditionally resolves a
		// non-nil default temperature for every provider call
		// (core.defaultTemperature) when RunConfig.Temperature is unset,
		// so without this rule every request to one of these models 400s
		// on its first turn. Mirrors the openai-compatible reasoning-class
		// rules above: one entry per affected model family, all applying
		// the same omission.
		//
		// Deliberately NOT matched: claude-opus-4-6*, claude-sonnet-4-6*,
		// claude-haiku-4-5* (still accept a non-default temperature), and
		// claude-mythos-preview (predecessor to Mythos 5; its sampling-
		// param behaviour is not confirmed against a live capture — add a
		// rule once verified rather than guessing).
		{
			ProviderType: "anthropic",
			ModelMatch:   "claude-opus-4-7*",
			Description:  "Anthropic Claude Opus 4.7: omit sampling params (400 on non-default temperature/top_p/top_k)",
			LastVerified: Date("2026-07-01"),
			Apply:        applyAnthropicNoSamplingParamsClass,
		},
		{
			ProviderType: "anthropic",
			ModelMatch:   "claude-opus-4-8*",
			Description:  "Anthropic Claude Opus 4.8: omit sampling params (400 on non-default temperature/top_p/top_k)",
			LastVerified: Date("2026-07-01"),
			Apply:        applyAnthropicNoSamplingParamsClass,
		},
		{
			ProviderType: "anthropic",
			ModelMatch:   "claude-sonnet-5*",
			Description:  "Anthropic Claude Sonnet 5: omit sampling params (400 on non-default temperature/top_p/top_k)",
			LastVerified: Date("2026-07-01"),
			Apply:        applyAnthropicNoSamplingParamsClass,
		},
		{
			ProviderType: "anthropic",
			ModelMatch:   "claude-fable-5*",
			Description:  "Anthropic Claude Fable 5: omit sampling params (400 on non-default temperature/top_p/top_k)",
			LastVerified: Date("2026-07-01"),
			Apply:        applyAnthropicNoSamplingParamsClass,
		},
		{
			ProviderType: "anthropic",
			ModelMatch:   "claude-mythos-5*",
			Description:  "Anthropic Claude Mythos 5: omit sampling params (same API surface as Fable 5; 400 on non-default temperature/top_p/top_k)",
			LastVerified: Date("2026-07-01"),
			Apply:        applyAnthropicNoSamplingParamsClass,
		},
	}
}
