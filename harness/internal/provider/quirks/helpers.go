package quirks

// Shared helper functions used by builtin rule Apply closures. Step 1
// reserves the file with package-private stubs so Step 2 can land the
// rule implementations alongside the helpers in a single change.
//
// Helpers stay unexported in Step 1 (rather than being exported and
// empty) so the public surface of the quirks package does not advertise
// behaviour that doesn't exist yet. Step 2 will rename to
// ApplyOpenAIReasoningClass / RemoveFromOmit when the implementations
// land and external rule files (e.g. compat/zai) need to call them.

// applyOpenAIReasoningClass sets the behaviour-flag combination shared
// by every OpenAI reasoning-class model: TokenFieldMaxCompletionTokens
// (already the zero-value default, but pinned explicitly) and
// OmitSamplingParams to suppress temperature, top_p, presence_penalty,
// frequency_penalty, logprobs, top_logprobs, and logit_bias.
//
// TODO(Step 2): implement.
func applyOpenAIReasoningClass(_ *ProviderQuirks) {}

// removeFromOmit drops the named field from ProviderQuirks.OmitFields,
// used by carve-out rules (e.g. gpt-5-chat*) that need to undo an
// omission applied by a broader sibling rule.
//
// TODO(Step 2): implement.
func removeFromOmit(_ *ProviderQuirks, _ string) {}
