package quirks

// applyOpenAIReasoningClass sets the behaviour-flag combination shared by
// every OpenAI reasoning-class model: TokenFieldMaxCompletionTokens and
// OmitSamplingParams = true. Reasoning models reject the suppressed fields
// with HTTP 400, so the omission is mandatory rather than a tunable.
func applyOpenAIReasoningClass(q *ProviderQuirks) {
	q.BehaviourFlags.OpenAI.TokenField = TokenFieldMaxCompletionTokens
	q.BehaviourFlags.OpenAI.OmitSamplingParams = true
}

// applyAnthropicNoSamplingParamsClass sets OmitSamplingParams = true for
// the Anthropic model families that reject a non-default temperature with
// an HTTP 400 rather than ignoring it.
func applyAnthropicNoSamplingParamsClass(q *ProviderQuirks) {
	q.BehaviourFlags.Anthropic.OmitSamplingParams = true
}

// removeFromOmit drops the named field from ProviderQuirks.OmitFields.
// Used by carve-out rules that need to undo an omission applied by a
// broader sibling rule. A no-op if the field is not present.
func removeFromOmit(q *ProviderQuirks, name string) {
	if len(q.OmitFields) == 0 {
		return
	}
	out := q.OmitFields[:0]
	for _, f := range q.OmitFields {
		if f != name {
			out = append(out, f)
		}
	}
	q.OmitFields = out
}

// applyAnthropicAdaptiveClass covers the Claude generations that both
// reject a non-default temperature and accept every output_config.effort
// level from low to max.
func applyAnthropicAdaptiveClass(q *ProviderQuirks) {
	applyAnthropicNoSamplingParamsClass(q)
	q.BehaviourFlags.Anthropic.EffortLevels = []string{"low", "medium", "high", "xhigh", "max"}
}

// applyAnthropicAutoToolChoiceOnly narrows the base Anthropic tool_choice
// capability to auto for models that reject forced tool choice.
func applyAnthropicAutoToolChoiceOnly(q *ProviderQuirks) {
	q.ToolChoice = ToolChoiceCapability{Supported: true, Auto: true}
}

// applyDeepSeekThinkingClass covers DeepSeek's first-party thinking models:
// reasoning_content must be replayed on every assistant turn once tools are
// present, sampling params are ignored in thinking mode, and
// reasoning_effort accepts every provider-neutral level (DeepSeek folds
// them onto low/high/max server-side).
func applyDeepSeekThinkingClass(q *ProviderQuirks) {
	q.ReplayFields = append(q.ReplayFields, "reasoning_content")
	q.BehaviourFlags.OpenAI.OmitSamplingParams = true
	q.BehaviourFlags.OpenAI.TokenField = TokenFieldMaxTokens
	q.BehaviourFlags.OpenAI.ReasoningEffortLevels = []string{"minimal", "low", "medium", "high", "xhigh", "max"}
}
