package guard

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// stubGuard is a configurable test double for GuardRail.
type stubGuard struct {
	d        *Decision
	err      error
	delay    time.Duration
	called   int
	onCalled func()
}

func (s *stubGuard) Check(ctx context.Context, _ Input) (*Decision, error) {
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.called++
	if s.onCalled != nil {
		s.onCalled()
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.d, nil
}

func allow(id string) *Decision     { return &Decision{Verdict: VerdictAllow, GuardID: id} }
func spotlight(id string) *Decision { return &Decision{Verdict: VerdictAllowSpot, GuardID: id} }
func deny(id, reason string) *Decision {
	return &Decision{Verdict: VerdictDeny, GuardID: id, Reason: reason}
}

func TestSequential_EmptyAllows(t *testing.T) {
	s := Sequential{ID: "seq"}
	d, err := s.Check(context.Background(), Input{Phase: PhasePreTurn})
	require.NoError(t, err)
	require.Equal(t, VerdictAllow, d.Verdict)
	require.Equal(t, "seq", d.GuardID)
}

func TestSequential_EmptyDefaultsID(t *testing.T) {
	d, err := Sequential{}.Check(context.Background(), Input{})
	require.NoError(t, err)
	require.Equal(t, "sequential", d.GuardID)
}

func TestSequential_ShortCircuitsOnDeny(t *testing.T) {
	first := &stubGuard{d: allow("a")}
	denier := &stubGuard{d: deny("b", "bad")}
	never := &stubGuard{d: allow("c")}
	s := Sequential{Guards: []GuardRail{first, denier, never}}

	d, err := s.Check(context.Background(), Input{})
	require.NoError(t, err)
	require.Equal(t, VerdictDeny, d.Verdict)
	require.Equal(t, "b", d.GuardID)
	require.Zero(t, never.called, "guards after deny must not run")
}

func TestSequential_PropagatesError(t *testing.T) {
	sentinel := errors.New("boom")
	first := &stubGuard{d: allow("a")}
	bad := &stubGuard{err: sentinel}
	never := &stubGuard{d: allow("c")}
	s := Sequential{Guards: []GuardRail{first, bad, never}}

	_, err := s.Check(context.Background(), Input{})
	require.ErrorIs(t, err, sentinel)
	require.Zero(t, never.called, "guards after an error must not run")
}

func TestSequential_ReturnsLastDecisionWhenAllAllow(t *testing.T) {
	g1 := &stubGuard{d: allow("a")}
	g2 := &stubGuard{d: spotlight("b")}
	g3 := &stubGuard{d: allow("c")}
	// Spotlight from g2 should propagate via "last decision" rules
	// only when no later guard overrides; here g3 returns allow last.
	s := Sequential{Guards: []GuardRail{g1, g2, g3}}
	d, err := s.Check(context.Background(), Input{})
	require.NoError(t, err)
	require.Equal(t, "c", d.GuardID)
}

func TestSequential_PreservesSpotlightWhenItIsLast(t *testing.T) {
	g1 := &stubGuard{d: allow("a")}
	g2 := &stubGuard{d: spotlight("b")}
	s := Sequential{Guards: []GuardRail{g1, g2}}
	d, err := s.Check(context.Background(), Input{})
	require.NoError(t, err)
	require.Equal(t, VerdictAllowSpot, d.Verdict)
}

func TestParallel_EmptyAllows(t *testing.T) {
	p := Parallel{ID: "par"}
	d, err := p.Check(context.Background(), Input{})
	require.NoError(t, err)
	require.Equal(t, VerdictAllow, d.Verdict)
	require.Equal(t, "par", d.GuardID)
}

func TestParallel_EmptyDefaultsID(t *testing.T) {
	d, err := Parallel{}.Check(context.Background(), Input{})
	require.NoError(t, err)
	require.Equal(t, "parallel", d.GuardID)
}

func TestParallel_DenyBeatsSpotlightAndAllow(t *testing.T) {
	g1 := &stubGuard{d: allow("a")}
	g2 := &stubGuard{d: spotlight("b")}
	g3 := &stubGuard{d: deny("c", "blocked")}
	p := Parallel{Guards: []GuardRail{g1, g2, g3}}

	d, err := p.Check(context.Background(), Input{})
	require.NoError(t, err)
	require.Equal(t, VerdictDeny, d.Verdict)
	require.Equal(t, "c", d.GuardID)
}

func TestParallel_SpotlightBeatsAllow(t *testing.T) {
	g1 := &stubGuard{d: allow("a")}
	g2 := &stubGuard{d: spotlight("b")}
	p := Parallel{Guards: []GuardRail{g1, g2}}
	d, err := p.Check(context.Background(), Input{})
	require.NoError(t, err)
	require.Equal(t, VerdictAllowSpot, d.Verdict)
	require.Equal(t, "b", d.GuardID)
}

func TestParallel_DenyWinsEvenWhenOtherGuardErrors(t *testing.T) {
	g1 := &stubGuard{err: errors.New("boom")}
	g2 := &stubGuard{d: deny("c", "blocked")}
	p := Parallel{Guards: []GuardRail{g1, g2}}

	d, err := p.Check(context.Background(), Input{})
	require.NoError(t, err, "a deny verdict must win over a sibling error")
	require.Equal(t, VerdictDeny, d.Verdict)
}

func TestParallel_ReturnsErrorWhenNoDenyAndAGuardErrored(t *testing.T) {
	sentinel := errors.New("boom")
	g1 := &stubGuard{err: sentinel}
	g2 := &stubGuard{d: allow("ok")}
	p := Parallel{Guards: []GuardRail{g1, g2}}

	_, err := p.Check(context.Background(), Input{})
	require.ErrorIs(t, err, sentinel)
}

func TestParallel_LatencyIsMaxOfChildren(t *testing.T) {
	short := &stubGuard{d: &Decision{Verdict: VerdictAllow, GuardID: "s", Latency: 5 * time.Millisecond}}
	long := &stubGuard{d: &Decision{Verdict: VerdictAllow, GuardID: "l", Latency: 50 * time.Millisecond}}
	mid := &stubGuard{d: &Decision{Verdict: VerdictAllow, GuardID: "m", Latency: 25 * time.Millisecond}}
	p := Parallel{Guards: []GuardRail{short, long, mid}}

	d, err := p.Check(context.Background(), Input{})
	require.NoError(t, err)
	require.Equal(t, 50*time.Millisecond, d.Latency)
}

func TestParallel_AggregatesReasonsAtStrongestRank(t *testing.T) {
	g1 := &stubGuard{d: deny("a", "first")}
	g2 := &stubGuard{d: deny("b", "second")}
	g3 := &stubGuard{d: allow("c")}
	p := Parallel{Guards: []GuardRail{g1, g2, g3}}

	d, err := p.Check(context.Background(), Input{})
	require.NoError(t, err)
	// Both denies contribute reasons; the allow does not.
	require.ElementsMatch(t, []string{"first", "second"}, strings.Split(d.Reason, ", "))
}
