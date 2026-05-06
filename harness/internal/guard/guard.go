// Package guard defines the GuardRail interface and supporting types for
// the harness's content-safety component.
//
// A GuardRail evaluates content (a user prompt, a tool input, a model
// response) at well-defined phases of the agentic loop and returns a
// Decision: allow the content through, allow it but spotlight (wrap) it,
// or deny it outright.
//
// This package is intentionally leaf-level: it imports nothing from
// elsewhere in the harness so it can be wired in by the factory in a
// later integration step without creating import cycles. Concrete
// adapters (Granite Guardian, cloud judges) and loop integration land
// in subsequent chunks of issue #43.
package guard

import (
	"context"
	"encoding/json"
	"time"
)

// Phase identifies where in the loop a guard is being asked to evaluate
// content. Each phase corresponds to a different content source and a
// different blast radius for a denial.
type Phase string

const (
	// PhasePreTurn evaluates content before the model is invoked, i.e.
	// the user prompt or freshly-injected dynamic context.
	PhasePreTurn Phase = "pre_turn"
	// PhasePreTool evaluates a tool's input arguments before dispatch.
	PhasePreTool Phase = "pre_tool"
	// PhasePostTurn evaluates the model's response after a turn completes.
	PhasePostTurn Phase = "post_turn"
)

// Verdict is the outcome of a guard's evaluation. The relative ordering
// Deny > Spotlight > Allow defines aggregation precedence used by the
// composite guards in this package.
type Verdict string

const (
	// VerdictAllow signals the content is safe to pass through unchanged.
	VerdictAllow Verdict = "allow"
	// VerdictAllowSpot signals the content should be passed through but
	// wrapped via ApplySpotlight so the model treats it as untrusted.
	VerdictAllowSpot Verdict = "spotlight"
	// VerdictDeny signals the content must not be passed through and the
	// loop should refuse the request or short-circuit.
	VerdictDeny Verdict = "deny"
)

// Decision is the structured result of a guard's Check call. It is
// returned even on allow so callers can record latency and provenance
// in traces. The zero value is not meaningful; callers should always
// set Verdict and GuardID.
type Decision struct {
	Verdict   Verdict
	Score     float64
	Criterion string
	Reason    string
	Latency   time.Duration
	GuardID   string
}

// Input is the payload a guard evaluates. Phase and Content are always
// populated; the remaining fields carry context that some adapters
// (e.g. cloud judges that prompt with the tool name) can consult.
type Input struct {
	Phase     Phase
	Content   string
	Source    string
	ToolName  string
	ToolInput json.RawMessage
	Mode      string
	RunID     string
	Metadata  map[string]string
}

// GuardRail evaluates Input and returns a Decision. Implementations
// must be safe for concurrent use because the parallel composite fans
// out across multiple goroutines.
type GuardRail interface {
	Check(ctx context.Context, in Input) (*Decision, error)
}

// ValidPhase reports whether p is one of the three Phase constants
// defined in this package. Used by config validation in later chunks.
func ValidPhase(p Phase) bool {
	switch p {
	case PhasePreTurn, PhasePreTool, PhasePostTurn:
		return true
	default:
		return false
	}
}

// IsValidVerdict reports whether v is one of the three Verdict
// constants defined in this package.
func IsValidVerdict(v Verdict) bool {
	switch v {
	case VerdictAllow, VerdictAllowSpot, VerdictDeny:
		return true
	default:
		return false
	}
}
