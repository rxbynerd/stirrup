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
