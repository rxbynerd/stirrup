package core

// This file defines the seam between the agentic loop and the missed-tool
// recovery policy: the loop never decides whether a no-tool answer is
// suspicious or how to recover — that lives behind EscalationPolicy,
// injected by the factory, preserving the loop-as-pure-interfaces invariant.

// EscalationKind enumerates the recovery action the policy selected for a
// suspected missed-tool turn. The zero value (EscalationNone) means "do
// not escalate" so a nil-safe / disabled policy is a no-op by construction.
type EscalationKind int

const (
	// EscalationNone means the turn is not a missed-tool failure; the loop accepts the answer unchanged.
	EscalationNone EscalationKind = iota

	// EscalationNative retries with provider-native required tool choice; chosen only when the provider advertises Supported && Required.
	EscalationNative

	// EscalationPrompt retries with an injected stronger-prompt message; the fallback when native tool-choice forcing isn't available.
	EscalationPrompt
)

// String returns the wire/label form of the escalation kind, used on the
// trace span attribute so operators can audit which recovery ran.
func (k EscalationKind) String() string {
	switch k {
	case EscalationNative:
		return "native"
	case EscalationPrompt:
		return "prompt"
	default:
		return "none"
	}
}

// EscalationInput is the value-typed view of a completed turn the loop
// hands the policy. It carries only data the loop already has in scope, so
// the policy needs no access to loop internals: the run mode, the resolved
// provider/model (for capability lookup), the model's stop reason, whether
// any tool was available this turn, whether the model emitted any tool call
// this turn, the turn index, and how many escalations have already fired in
// this inner-loop run.
type EscalationInput struct {
	// Mode is the run mode; the policy picks the mode-specific requirement from it.
	Mode string

	// Provider and Model resolve the native tool-choice capability; empty strings fall back to the prompt path.
	Provider string
	Model    string

	// StopReason is the turn's provider stop reason; "tool_use" is never a missed-tool failure.
	StopReason string

	// ToolsAvailable reports whether any tool was attached to the turn; the trigger never fires when false.
	ToolsAvailable bool

	// PriorToolCalls gates escalation to the first assistant turn (== 0); a model that has already used tools is trusted.
	PriorToolCalls int

	// Turn is the zero-based turn index within the inner-loop run.
	Turn int

	// EscalationsSoFar is compared against the policy's cap so a retry cannot loop unboundedly.
	EscalationsSoFar int
}

// EscalationDecision is the policy's verdict for one turn. The zero value
// (Kind == EscalationNone) is the safe default for a disabled or nil policy.
type EscalationDecision struct {
	// Kind selects the recovery action; EscalationNone means no retry.
	Kind EscalationKind

	// PromptMessage is appended before the retry when Kind == EscalationPrompt; empty for the native path.
	PromptMessage string

	// Reason is a short, non-model-derived explanation attached to the trace span; it is NOT a metric label.
	Reason string
}

// EscalationPolicy decides whether a completed no-tool turn is a missed-tool
// failure and, if so, how to recover; injected by the factory so the loop
// stays free of the detection heuristic. Decide must be safe to call on
// every turn — the EscalationNone zero value means "do nothing", and a nil
// EscalationPolicy on the loop is treated as disabled.
type EscalationPolicy interface {
	// Decide must never return Native/Prompt when ToolsAvailable is false, StopReason is "tool_use", PriorToolCalls > 0, or EscalationsSoFar reached the cap.
	Decide(in EscalationInput) EscalationDecision
}

// escalationDecision resolves the loop's EscalationPolicy, treating a nil
// policy as disabled (EscalationNone zero value).
func (l *AgenticLoop) escalationDecision(in EscalationInput) EscalationDecision {
	if l.Escalation == nil {
		return EscalationDecision{Kind: EscalationNone}
	}
	return l.Escalation.Decide(in)
}
