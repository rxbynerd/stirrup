package guard

import "context"

// PhaseGated wraps an inner GuardRail so that Check only delegates when
// the input's Phase is one of the explicitly listed Phases. Operators
// use this to attach an expensive cloud judge to (e.g.) only post_turn,
// while leaving pre_tool to a cheap local model.
//
// Note that an empty Phases slice means "skip every phase" — operators
// who want a guard to run on all phases should use the inner guard
// directly instead of an empty PhaseGated wrapper. This is deliberate:
// a misconfigured "all phases" gate would silently disable the guard,
// so we make the safer interpretation the default.
type PhaseGated struct {
	Phases []Phase
	Inner  GuardRail
}

// Check delegates to Inner if in.Phase appears in Phases; otherwise it
// returns a tagged allow without invoking the inner guard.
func (p PhaseGated) Check(ctx context.Context, in Input) (*Decision, error) {
	for _, ph := range p.Phases {
		if ph == in.Phase {
			return p.Inner.Check(ctx, in)
		}
	}
	return &Decision{Verdict: VerdictAllow, GuardID: "phase-gated:skipped"}, nil
}
