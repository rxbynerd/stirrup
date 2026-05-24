package core

import (
	"fmt"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
)

// capabilityResolver is the minimal view of the quirks registry the
// default escalation policy needs: resolve a (provider, model) pair to its
// tool-choice capability. Declared as an interface so tests can inject a
// fake capability without a real registry, and so the policy depends on a
// behaviour rather than the concrete *quirks.Registry.
type capabilityResolver interface {
	Resolve(providerType, model string) quirks.ProviderQuirks
}

// modeRequirement describes the first-turn tool expectation for a run mode.
// A mode absent from the table (or one whose requirement is the zero value)
// is treated as "no tool required" and never escalates — this is what keeps
// modes where a no-tool answer is legitimate safe from spurious retries.
type modeRequirement struct {
	// description is the human-readable requirement injected into the
	// prompt-fallback message and surfaced on the trace span. It names
	// what the model was expected to do (e.g. "inspect the workspace with
	// a read or search tool"). Empty means the mode requires no tool.
	description string
}

// modeRequirements is the conservative, mode-aware policy table (#230).
//
//   - planning / review: expect at least one read/search tool call before
//     a final answer — both modes reason about existing code.
//   - research: a fetch is an acceptable first tool, so the requirement is
//     phrased around gathering context rather than reading the workspace.
//   - execution: the agent edits code, so it must read before it answers.
//   - toil: a read-only mode for routine maintenance; like planning/review
//     it should inspect before concluding.
//
// A mode not present here requires no tool and is never escalated. The
// table is intentionally small and explicit; widening it is a deliberate
// policy change, not an accident of defaulting.
var modeRequirements = map[string]modeRequirement{
	"planning":  {description: "inspect the relevant files with a read or search tool before answering"},
	"review":    {description: "inspect the code under review with a read or search tool before answering"},
	"research":  {description: "gather the needed context with a read, search, or fetch tool before answering"},
	"execution": {description: "read the relevant files with a read or search tool before making changes or answering"},
	"toil":      {description: "inspect the relevant files with a read or search tool before answering"},
}

// defaultEscalationPolicy is the production EscalationPolicy. It is OFF
// unless maxRetries > 0 (the factory passes 0 when the operator did not
// opt in via RunConfig.ToolChoiceEscalation), so a bare run never escalates.
//
// The detection heuristic is deliberately conservative: it fires only on
// the first assistant turn of an inner-loop run, only when tools were
// available and none was called, only for modes with a tool requirement,
// and never beyond the configured cap. Native vs prompt fallback is chosen
// from the resolved provider tool-choice capability.
type defaultEscalationPolicy struct {
	maxRetries int
	caps       capabilityResolver
}

var _ EscalationPolicy = (*defaultEscalationPolicy)(nil)

// newDefaultEscalationPolicy builds the production policy. maxRetries <= 0
// yields a policy that never escalates (the disabled / OFF-by-default
// case); the factory only passes a positive cap when the operator opted
// in. caps resolves provider tool-choice capability; a nil caps degrades
// every decision to the prompt fallback (no native capability is ever
// detected) rather than panicking.
func newDefaultEscalationPolicy(maxRetries int, caps capabilityResolver) *defaultEscalationPolicy {
	return &defaultEscalationPolicy{maxRetries: maxRetries, caps: caps}
}

// Decide implements EscalationPolicy. See the conservative-trigger
// description on defaultEscalationPolicy. The guard ordering mirrors the
// interface contract so each early return documents one reason a no-tool
// answer is NOT a missed-tool failure.
func (p *defaultEscalationPolicy) Decide(in EscalationInput) EscalationDecision {
	none := EscalationDecision{Kind: EscalationNone}

	// Feature off / cap not configured.
	if p.maxRetries <= 0 {
		return none
	}
	// Never exceed the cap — bounds the recovery so a model that keeps
	// refusing to call a tool cannot drive an unbounded retry loop.
	if in.EscalationsSoFar >= p.maxRetries {
		return none
	}
	// A tool_use turn is the model doing exactly what we wanted; only a
	// final/text answer is a candidate.
	if in.StopReason == "tool_use" {
		return none
	}
	// No tools available → the model cannot be expected to call one. This
	// also covers purely conceptual questions on a tool-less run.
	if !in.ToolsAvailable {
		return none
	}
	// Only the first assistant turn. A model that already used tools and
	// then answers is making a legitimate judgement, not skipping context.
	if in.PriorToolCalls > 0 || in.Turn > 0 {
		return none
	}
	// Mode-aware requirement. Modes absent from the table require no tool,
	// so a no-tool answer there is legitimate and must not be escalated.
	req, ok := modeRequirements[in.Mode]
	if !ok || req.description == "" {
		return none
	}

	// The turn is a likely missed-tool failure. Choose native required
	// tool choice when the provider supports it, else the prompt fallback.
	if p.supportsNativeRequired(in.Provider, in.Model) {
		return EscalationDecision{
			Kind:   EscalationNative,
			Reason: fmt.Sprintf("mode %q expects a tool call on the first turn; retrying with provider-native required tool choice", in.Mode),
		}
	}
	return EscalationDecision{
		Kind:          EscalationPrompt,
		PromptMessage: fmt.Sprintf("You answered without calling any tool, but this task requires it: %s. Call an appropriate tool now rather than answering from assumption.", req.description),
		Reason:        fmt.Sprintf("mode %q expects a tool call on the first turn; provider lacks native required tool choice, retrying with a stronger prompt", in.Mode),
	}
}

// supportsNativeRequired reports whether the resolved (provider, model)
// pair can force at least one tool call natively. A nil resolver or an
// empty provider means "no native capability", so the policy falls back to
// the prompt path.
func (p *defaultEscalationPolicy) supportsNativeRequired(providerType, model string) bool {
	if p.caps == nil || providerType == "" {
		return false
	}
	tc := p.caps.Resolve(providerType, model).ToolChoice
	return tc.Supported && tc.Required
}
