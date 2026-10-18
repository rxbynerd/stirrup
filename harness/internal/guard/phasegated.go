package guard

import (
	"context"
	"slices"
)

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

// ChecksPhase reports whether g evaluates content of phase p, so a caller
// can tell a configured guard from the Noop default: false for nil, Noop,
// a PhaseGated that does not list p, and a composite with no member that
// checks p; true for any other guard.
func ChecksPhase(g GuardRail, p Phase) bool {
	switch v := g.(type) {
	case nil, Noop, *Noop:
		return false
	case PhaseGated:
		return slices.Contains(v.Phases, p) && ChecksPhase(v.Inner, p)
	case *PhaseGated:
		return slices.Contains(v.Phases, p) && ChecksPhase(v.Inner, p)
	case Sequential:
		return anyChecksPhase(v.Guards, p)
	case *Sequential:
		return anyChecksPhase(v.Guards, p)
	case Parallel:
		return anyChecksPhase(v.Guards, p)
	case *Parallel:
		return anyChecksPhase(v.Guards, p)
	default:
		return true
	}
}

func anyChecksPhase(guards []GuardRail, p Phase) bool {
	for _, g := range guards {
		if ChecksPhase(g, p) {
			return true
		}
	}
	return false
}
