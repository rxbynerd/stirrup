package guard

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPhaseGated_DelegatesOnSelectedPhase(t *testing.T) {
	inner := &stubGuard{d: deny("inner", "blocked")}
	g := PhaseGated{Phases: []Phase{PhasePreTool}, Inner: inner}

	d, err := g.Check(context.Background(), Input{Phase: PhasePreTool})
	require.NoError(t, err)
	require.Equal(t, 1, inner.called, "inner should be called once")

	// The inner decision is propagated verbatim.
	require.Equal(t, VerdictDeny, d.Verdict)
	require.Equal(t, "inner", d.GuardID)
	require.Equal(t, "blocked", d.Reason)
}

func TestPhaseGated_SkipsUnselectedPhase(t *testing.T) {
	inner := &stubGuard{d: deny("inner", "should-not-fire")}
	g := PhaseGated{Phases: []Phase{PhasePreTool}, Inner: inner}

	d, err := g.Check(context.Background(), Input{Phase: PhasePostTurn})
	require.NoError(t, err)
	require.Equal(t, 0, inner.called, "inner should not be called")
	require.Equal(t, VerdictAllow, d.Verdict)
	require.Equal(t, "phase-gated:skipped", d.GuardID)
}

func TestPhaseGated_EmptyPhasesSkipsEverything(t *testing.T) {
	inner := &stubGuard{d: deny("inner", "should-not-fire")}
	g := PhaseGated{Inner: inner}

	for _, ph := range []Phase{PhasePreTurn, PhasePreTool, PhasePostTurn} {
		t.Run(string(ph), func(t *testing.T) {
			d, err := g.Check(context.Background(), Input{Phase: ph})
			require.NoError(t, err)
			require.Equal(t, VerdictAllow, d.Verdict, "empty Phases skips all")
		})
	}
	require.Equal(t, 0, inner.called, "inner should not be invoked despite empty Phases")
}

func TestChecksPhase_ReportsWhetherGuardChecksPhase(t *testing.T) {
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
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ChecksPhase(tc.g, PhasePreTurn))
		})
	}
}
