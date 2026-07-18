package quirks

// BuiltinRules returns the first-party rule set baked into the harness.
// Rules compose under specificity-then-declaration-order (longer
// ModelMatch globs run last); see docs/provider-quirks.md for the
// full rule table and the composition examples.
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

				q.BehaviourFlags.OpenAI.OmitSamplingParams = false
			},
		},
		{
			ProviderType: "gemini",
			ModelMatch:   "*",
			Description:  "Gemini: off streamFunctionCallArguments (post-#191 default)",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {

				q.BehaviourFlags.Gemini.StreamFunctionCallArgsShape = StreamArgsOff
			},
		},
		// ReplayFields rules: Description must end "(threaded)" or
		// "(parse-side only)" per docs/provider-quirks.md §3.1;
		// enforced by TestBuiltinRulesReplayFieldsSuffix.
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "deepseek-reasoner*",
			Description:  "DeepSeek reasoner: replay reasoning_content, omit sampling params, legacy max_tokens (threaded)",
			LastVerified: Date("2026-06-07"),
			Apply: func(q *ProviderQuirks) {
				// SUNSET: deepseek-reasoner retires 2026-07-24; see
				// docs/provider-quirks.md §9 risk 9.
				q.ReplayFields = append(q.ReplayFields, "reasoning_content")

				q.BehaviourFlags.OpenAI.OmitSamplingParams = true

				q.BehaviourFlags.OpenAI.TokenField = TokenFieldMaxTokens
			},
		},
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "deepseek-v4*",
			Description:  "DeepSeek v4: replay reasoning_content, omit sampling params, legacy max_tokens (threaded)",
			LastVerified: Date("2026-06-07"),
			Apply: func(q *ProviderQuirks) {

				q.ReplayFields = append(q.ReplayFields, "reasoning_content")

				q.BehaviourFlags.OpenAI.OmitSamplingParams = true

				q.BehaviourFlags.OpenAI.TokenField = TokenFieldMaxTokens

			},
		},
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "deepseek/deepseek-v4*",
			Description:  "DeepSeek v4 via gateway prefix: replay reasoning_content, omit sampling params, legacy max_tokens (threaded)",
			LastVerified: Date("2026-06-07"),
			Apply: func(q *ProviderQuirks) {

				q.ReplayFields = append(q.ReplayFields, "reasoning_content")
				q.BehaviourFlags.OpenAI.OmitSamplingParams = true
				q.BehaviourFlags.OpenAI.TokenField = TokenFieldMaxTokens
			},
		},

		{
			ProviderType: "openai-compatible",
			ModelMatch:   "gpt-4o-mini*",
			Description:  "OpenAI gpt-4o-mini: enable strict-mode structured outputs",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {
				// Narrower than gpt-4o* deliberately; see
				// docs/provider-quirks.md.
				q.BehaviourFlags.OpenAI.StrictMode = true
			},
		},
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "gpt-4.1*",
			Description:  "OpenAI gpt-4.1 family: enable strict-mode structured outputs",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {

				q.BehaviourFlags.OpenAI.StrictMode = true
			},
		},
		{
			ProviderType: "openai-compatible",
			ModelMatch:   "gpt-5*",
			Description:  "OpenAI gpt-5 family: enable strict-mode structured outputs",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {

				q.BehaviourFlags.OpenAI.StrictMode = true
			},
		},
		{
			ProviderType: "gemini",
			ModelMatch:   "gemini-3*",
			Description:  "Gemini 3: preserve thoughtSignature on functionCall parts (parse-side only)",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {

				q.ReplayFields = append(q.ReplayFields,
					"candidates[].content.parts[].thoughtSignature",
				)
			},
		},

		{
			ProviderType: "anthropic",
			ModelMatch:   "*",
			Description:  "Anthropic: native tool_choice (auto/any/tool); no native none",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {

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
			ProviderType: "anthropic",
			ModelMatch:   "*",
			Description:  "Anthropic: tool_result content accepts a content-block array (text + structured JSON block)",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {

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
				// Conservative reject-at-build-time lint; see
				// docs/provider-quirks.md.
				q.BehaviourFlags.Gemini.SchemaUnsupportedFeatures = append(
					q.BehaviourFlags.Gemini.SchemaUnsupportedFeatures,
					"pattern", "format",
				)
			},
		},

		{
			ProviderType: "anthropic",
			ModelMatch:   "*",
			Description:  "Anthropic: disable_parallel_tool_use on tool_choice; accepts schema examples",
			LastVerified: Date("2026-05-24"),
			Apply: func(q *ProviderQuirks) {

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

				q.ParallelToolCalls = ParallelToolCallsCapability{Supported: true, Disable: true}
				q.ToolExamples = ToolExamplesCapability{Supported: true}

				q.BehaviourFlags.OpenAIResponses.TokenField = TokenFieldMaxOutputTokens
				q.BehaviourFlags.OpenAIResponses.StoreMode = StoreFalse
				q.BehaviourFlags.OpenAIResponses.InputItemShape = TypedInputItems
			},
		},

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
