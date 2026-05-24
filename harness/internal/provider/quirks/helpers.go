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
//
// The stubs carry //nolint:unused because Step 1 ships an empty
// BuiltinRules() so nothing calls them yet; Step 2's first rule
// addition is the first caller. Removing the stubs would leave Step 2
// to invent the names at the same time as the rule logic; preserving
// the names here pins the helper API surface up-front.

// applyOpenAIReasoningClass sets the behaviour-flag combination shared
// by every OpenAI reasoning-class model: TokenFieldMaxCompletionTokens
// (already the zero-value default, but pinned explicitly) and
// OmitSamplingParams to suppress temperature, top_p, presence_penalty,
// frequency_penalty, logprobs, top_logprobs, and logit_bias.
//
// TODO(Step 2): implement.
//
//nolint:unused // Step 2 caller is queued; see file-level comment.
func applyOpenAIReasoningClass(_ *ProviderQuirks) {}

// removeFromOmit drops the named field from ProviderQuirks.OmitFields,
// used by carve-out rules (e.g. gpt-5-chat*) that need to undo an
// omission applied by a broader sibling rule.
//
// TODO(Step 2): implement.
//
//nolint:unused // Step 2 caller is queued; see file-level comment.
func removeFromOmit(_ *ProviderQuirks, _ string) {}
