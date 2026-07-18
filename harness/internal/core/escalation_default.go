package core

import (
	"fmt"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
)

// capabilityResolver resolves a (provider, model) pair to its tool-choice
// capability. An interface so tests can inject a fake without a real
// *quirks.Registry.
type capabilityResolver interface {
	Resolve(providerType, model string) quirks.ProviderQuirks
}

// modeRequirement describes the first-turn tool expectation for a run mode.
// A mode absent from the table, or with a zero-value requirement, needs no
// tool and never escalates.
type modeRequirement struct {
	// description is injected into the prompt-fallback message and the
	// trace span. Empty means the mode requires no tool.
	description string
}

// modeRequirements is the mode-aware policy table; a mode not listed here
// requires no tool and is never escalated.
var modeRequirements = map[string]modeRequirement{
	"planning":  {description: "inspect the relevant files with a read or search tool before answering"},
	"review":    {description: "inspect the code under review with a read or search tool before answering"},
	"research":  {description: "gather the needed context with a read, search, or fetch tool before answering"},
	"execution": {description: "read the relevant files with a read or search tool before making changes or answering"},
	"toil":      {description: "inspect the relevant files with a read or search tool before answering"},
}

// defaultEscalationPolicy is the production EscalationPolicy. It is off
// unless maxRetries > 0. It fires only on the first assistant turn of an
// inner-loop run when tools were available, none was called, the mode has
// a tool requirement, and the retry cap has not been reached; native vs
// prompt fallback is chosen from the resolved provider tool-choice
// capability.
type defaultEscalationPolicy struct {
	maxRetries int
	caps       capabilityResolver
}

var _ EscalationPolicy = (*defaultEscalationPolicy)(nil)

// newDefaultEscalationPolicy builds the production policy. maxRetries <= 0
// never escalates. A nil caps degrades every decision to the prompt
// fallback rather than panicking.
func newDefaultEscalationPolicy(maxRetries int, caps capabilityResolver) *defaultEscalationPolicy {
	return &defaultEscalationPolicy{maxRetries: maxRetries, caps: caps}
}

// Decide implements EscalationPolicy.
func (p *defaultEscalationPolicy) Decide(in EscalationInput) EscalationDecision {
	none := EscalationDecision{Kind: EscalationNone}

	if p.maxRetries <= 0 {
		return none
	}
	if in.EscalationsSoFar >= p.maxRetries {
		return none
	}
	if in.StopReason == "tool_use" {
		return none
	}
	if !in.ToolsAvailable {
		return none
	}
	// Gated on PriorToolCalls rather than Turn: a forced retry advances the
	// turn counter (which would make the cap above unreachable for
	// maxRetries > 1), and Turn is reset by context compaction.
	if in.PriorToolCalls > 0 {
		return none
	}
	req, ok := modeRequirements[in.Mode]
	if !ok || req.description == "" {
		return none
	}

	// The turn is a likely missed-tool failure. Choose native required
	// tool choice when the provider supports it, else the prompt fallback.
	if p.supportsNativeRequired(in.Provider, in.Model) {
		return EscalationDecision{
			Kind:   EscalationNative,
			Reason: fmt.Sprintf("mode %q expects a tool call before answering and none was made; retrying with provider-native required tool choice", in.Mode),
		}
	}
	return EscalationDecision{
		Kind:          EscalationPrompt,
		PromptMessage: fmt.Sprintf("You answered without calling any tool, but this task requires it: %s. Call an appropriate tool now rather than answering from assumption.", req.description),
		Reason:        fmt.Sprintf("mode %q expects a tool call before answering and none was made; provider lacks native required tool choice, retrying with a stronger prompt", in.Mode),
	}
}

// supportsNativeRequired reports whether the resolved (provider, model)
// pair can force at least one tool call natively.
func (p *defaultEscalationPolicy) supportsNativeRequired(providerType, model string) bool {
	if p.caps == nil || providerType == "" {
		return false
	}
	tc := p.caps.Resolve(providerType, model).ToolChoice
	return tc.Supported && tc.Required
}
