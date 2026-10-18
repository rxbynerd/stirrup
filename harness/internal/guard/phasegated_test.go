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

func TestChecksPhase(t *testing.T) {
	judge := &stubGuard{d: &Decision{Verdict: VerdictAllow, GuardID: "stub"}}
	cases := []struct {
		name string
		g    GuardRail
		want bool
	}{
		{"nil", nil, false},
		{"noop", NewNoop(), false},
		{"adapter", judge, true},
		{"gated to the phase", &PhaseGated{Phases: []Phase{PhasePreTurn}, Inner: judge}, true},
		{"gated to another phase", &PhaseGated{Phases: []Phase{PhasePostTurn}, Inner: judge}, false},
		{"gated noop", &PhaseGated{Phases: []Phase{PhasePreTurn}, Inner: NewNoop()}, false},
		{"composite with a checking member", &Sequential{Guards: []GuardRail{NewNoop(), &PhaseGated{Phases: []Phase{PhasePreTurn}, Inner: judge}}}, true},
		{"composite without one", Parallel{Guards: []GuardRail{NewNoop(), &PhaseGated{Phases: []Phase{PhasePreTool}, Inner: judge}}}, false},
		{"typed nil phase gate", (*PhaseGated)(nil), false},
		{"typed nil sequential", (*Sequential)(nil), false},
		{"typed nil parallel", (*Parallel)(nil), false},
		{"typed nil noop", (*Noop)(nil), false},
		{"composite holding a typed nil", &Sequential{Guards: []GuardRail{(*PhaseGated)(nil), judge}}, true},
	}
	for _, tc := range cases {
		if got := ChecksPhase(tc.g, PhasePreTurn); got != tc.want {
			t.Errorf("%s: ChecksPhase = %v, want %v", tc.name, got, tc.want)
		}
	}
}
