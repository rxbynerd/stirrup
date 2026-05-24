package core

// Tool-choice escalation (#230). This file defines the seam between the
// agentic loop and the missed-tool recovery policy. The loop never decides
// *whether* a no-tool answer is suspicious or *how* to recover — that
// judgement lives behind the EscalationPolicy interface, injected by the
// factory. Keeping the decision out of the loop preserves the
// loop-as-pure-interfaces invariant (CLAUDE.md): the loop reads no
// environment, touches no filesystem, and imports no concrete component
// here. It hands the policy a value-typed EscalationInput and acts on the
// returned EscalationDecision.

// EscalationKind enumerates the recovery action the policy selected for a
// suspected missed-tool turn. The zero value (EscalationNone) means "do
// not escalate" so a nil-safe / disabled policy is a no-op by construction.
type EscalationKind int

const (
	// EscalationNone — the turn is not a missed-tool failure (or the
	// feature is off / the cap is exhausted). The loop accepts the
	// model's answer unchanged.
	EscalationNone EscalationKind = iota

	// EscalationNative — retry the turn with provider-native required
	// tool choice (StreamParams.ToolChoice = ToolChoiceRequired). Chosen
	// only when the resolved provider capability advertises
	// Supported && Required.
	EscalationNative

	// EscalationPrompt — retry the turn after injecting a stronger-prompt
	// user message stating the missing requirement. The fallback when the
	// provider cannot force tool use natively.
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
	// Mode is the run mode ("planning"/"review"/"research"/"execution"/
	// "toil"). The policy uses it to pick the mode-specific requirement
	// and to stay silent for modes where no tool is required.
	Mode string

	// Provider and Model identify the resolved (provider, model) pair so
	// the policy can resolve the native tool-choice capability and decide
	// native-vs-prompt fallback. Empty strings resolve to no native
	// capability, so the policy falls back to the prompt path.
	Provider string
	Model    string

	// StopReason is the turn's provider stop reason. A missed-tool answer
	// is a final/text stop reason (e.g. "end_turn"); a "tool_use" turn is
	// never a missed-tool failure.
	StopReason string

	// ToolsAvailable reports whether the loop attached any tool
	// definitions to the turn. The trigger never fires when false — a run
	// with no tools cannot be expected to call one.
	ToolsAvailable bool

	// PriorToolCalls is the number of tool calls dispatched so far in this
	// inner-loop run. The conservative trigger fires only on the *first*
	// assistant turn (PriorToolCalls == 0): a model that has already used
	// tools and then answers is making a legitimate judgement.
	PriorToolCalls int

	// Turn is the zero-based turn index within the inner-loop run.
	Turn int

	// EscalationsSoFar is the number of escalations already performed in
	// this inner-loop run. The policy compares it against its cap so a
	// retry that itself ends in a no-tool answer cannot loop unboundedly.
	EscalationsSoFar int
}

// EscalationDecision is the policy's verdict for one turn.
//
// The zero value (Kind == EscalationNone) is the safe default: a disabled
// or nil policy returns it and the loop accepts the model's answer
// unchanged.
type EscalationDecision struct {
	// Kind selects the recovery action. EscalationNone means no retry.
	Kind EscalationKind

	// PromptMessage is the user-message text the loop appends before the
	// retry when Kind == EscalationPrompt. Empty for the native path
	// (the provider field forces the call, so no extra prompt is added).
	PromptMessage string

	// Reason is a short, bounded, non-model-derived explanation of why the
	// escalation fired (e.g. the mode requirement that was unmet). It is
	// attached to the trace span so operators can audit the trigger
	// without the raw model output. It is NOT a metric label.
	Reason string
}

// EscalationPolicy decides whether a completed no-tool turn is a likely
// missed-tool failure and, if so, how to recover. It is injected into the
// AgenticLoop by the factory so the loop stays free of the detection
// heuristic, the mode-requirement table, and the provider-capability
// lookup.
//
// Implementations MUST be safe to call on every turn: the loop calls
// Decide unconditionally and relies on the EscalationNone zero value to
// mean "do nothing". A nil EscalationPolicy on the loop is treated as a
// disabled policy (see (*AgenticLoop).escalationDecision).
type EscalationPolicy interface {
	// Decide returns the recovery action for the given turn. It must never
	// return EscalationNative/EscalationPrompt when in.ToolsAvailable is
	// false, when in.StopReason is "tool_use", when in.PriorToolCalls > 0,
	// or when in.EscalationsSoFar has reached the cap.
	Decide(in EscalationInput) EscalationDecision
}

// escalationDecision resolves the loop's EscalationPolicy, treating a nil
// policy as disabled (returns the EscalationNone zero value). Centralising
// the nil check keeps the call site in runInnerLoop free of a guard and
// guarantees the loop is a no-op whenever the factory left the policy
// unset (the OFF-by-default path).
func (l *AgenticLoop) escalationDecision(in EscalationInput) EscalationDecision {
	if l.Escalation == nil {
		return EscalationDecision{Kind: EscalationNone}
	}
	return l.Escalation.Decide(in)
}
