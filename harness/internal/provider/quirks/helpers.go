package quirks

// Shared helper functions used by builtin rule Apply closures. Each
// helper encapsulates a behaviour-flag mutation that is shared across
// more than one rule so the rules themselves stay declarative.

// applyOpenAIReasoningClass sets the behaviour-flag combination shared
// by every OpenAI reasoning-class model: TokenFieldMaxCompletionTokens
// (already the zero-value default, pinned explicitly for clarity) and
// OmitSamplingParams = true to suppress temperature, top_p,
// presence_penalty, frequency_penalty, logprobs, top_logprobs, and
// logit_bias from the request body. Reasoning models reject every one
// of these fields with HTTP 400, so the omission is mandatory rather
// than a tunable.
func applyOpenAIReasoningClass(q *ProviderQuirks) {
	q.BehaviourFlags.OpenAI.TokenField = TokenFieldMaxCompletionTokens
	q.BehaviourFlags.OpenAI.OmitSamplingParams = true
}

// removeFromOmit drops the named field from ProviderQuirks.OmitFields.
// Used by carve-out rules (e.g. gpt-5-chat*) that need to undo an
// omission applied by a broader sibling rule. A no-op if the field is
// not present so callers can use it defensively.
//
// This helper is reserved for OmitFields-driven omissions. The current
// reasoning-class carve-out (gpt-5-chat*) toggles
// OmitSamplingParams = false directly rather than touching OmitFields,
// because the batch suppression is expressed as a boolean rather than
// seven individual entries. removeFromOmit remains because future
// rules that omit a single non-standard field may need a carve-out
// for it on a sibling model glob.
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
