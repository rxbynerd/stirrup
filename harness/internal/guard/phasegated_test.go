package guard

import (
	"context"
	"testing"
)

func TestPhaseGatedDelegatesOnSelectedPhase(t *testing.T) {
	inner := &stubGuard{d: deny("inner", "blocked")}
	g := PhaseGated{Phases: []Phase{PhasePreTool}, Inner: inner}

	d, err := g.Check(context.Background(), Input{Phase: PhasePreTool})
	if err != nil {
		t.Fatal(err)
	}
	if inner.called != 1 {
		t.Fatalf("inner called %d times, want 1", inner.called)
	}
	// Decision should be propagated verbatim.
	if d.Verdict != VerdictDeny || d.GuardID != "inner" || d.Reason != "blocked" {
		t.Fatalf("decision = %+v, want verbatim deny from inner", d)
	}
}

func TestPhaseGatedSkipsUnselectedPhase(t *testing.T) {
	inner := &stubGuard{d: deny("inner", "should-not-fire")}
	g := PhaseGated{Phases: []Phase{PhasePreTool}, Inner: inner}

	d, err := g.Check(context.Background(), Input{Phase: PhasePostTurn})
	if err != nil {
		t.Fatal(err)
	}
	if inner.called != 0 {
		t.Fatalf("inner called %d times, want 0", inner.called)
	}
	if d.Verdict != VerdictAllow {
		t.Fatalf("verdict = %q, want allow", d.Verdict)
	}
	if d.GuardID != "phase-gated:skipped" {
		t.Fatalf("guard id = %q, want \"phase-gated:skipped\"", d.GuardID)
	}
}

func TestPhaseGatedEmptyPhasesSkipsEverything(t *testing.T) {
	inner := &stubGuard{d: deny("inner", "should-not-fire")}
	g := PhaseGated{Inner: inner}

	for _, ph := range []Phase{PhasePreTurn, PhasePreTool, PhasePostTurn} {
		t.Run(string(ph), func(t *testing.T) {
			d, err := g.Check(context.Background(), Input{Phase: ph})
			if err != nil {
				t.Fatal(err)
			}
			if d.Verdict != VerdictAllow {
				t.Fatalf("verdict = %q, want allow (empty Phases skips all)", d.Verdict)
			}
		})
	}
	if inner.called != 0 {
		t.Fatalf("inner invoked %d times despite empty Phases, want 0", inner.called)
	}
}
