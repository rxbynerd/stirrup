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
	must := require.New(t)
	s := Sequential{ID: "seq"}
	d, err := s.Check(context.Background(), Input{Phase: PhasePreTurn})
	must.NoError(err)
	must.Equal(VerdictAllow, d.Verdict)
	must.Equal("seq", d.GuardID)
}

func TestSequential_EmptyDefaultsID(t *testing.T) {
	must := require.New(t)
	d, err := Sequential{}.Check(context.Background(), Input{})
	must.NoError(err)
	must.Equal("sequential", d.GuardID)
}

func TestSequential_ShortCircuitsOnDeny(t *testing.T) {
	must := require.New(t)
	first := &stubGuard{d: allow("a")}
	denier := &stubGuard{d: deny("b", "bad")}
	never := &stubGuard{d: allow("c")}
	s := Sequential{Guards: []GuardRail{first, denier, never}}

	d, err := s.Check(context.Background(), Input{})
	must.NoError(err)
	must.Equal(VerdictDeny, d.Verdict)
	must.Equal("b", d.GuardID)
	must.Zero(never.called, "guards after deny must not run")
}

func TestSequential_PropagatesError(t *testing.T) {
	must := require.New(t)
	sentinel := errors.New("boom")
	first := &stubGuard{d: allow("a")}
	bad := &stubGuard{err: sentinel}
	never := &stubGuard{d: allow("c")}
	s := Sequential{Guards: []GuardRail{first, bad, never}}

	_, err := s.Check(context.Background(), Input{})
	must.ErrorIs(err, sentinel)
	must.Zero(never.called, "guards after an error must not run")
}

func TestSequential_ReturnsLastDecisionWhenAllAllow(t *testing.T) {
	must := require.New(t)
	g1 := &stubGuard{d: allow("a")}
	g2 := &stubGuard{d: spotlight("b")}
	g3 := &stubGuard{d: allow("c")}
	// Spotlight from g2 should propagate via "last decision" rules
	// only when no later guard overrides; here g3 returns allow last.
	s := Sequential{Guards: []GuardRail{g1, g2, g3}}
	d, err := s.Check(context.Background(), Input{})
	must.NoError(err)
	must.Equal("c", d.GuardID)
}

func TestSequential_PreservesSpotlightWhenItIsLast(t *testing.T) {
	must := require.New(t)
	g1 := &stubGuard{d: allow("a")}
	g2 := &stubGuard{d: spotlight("b")}
	s := Sequential{Guards: []GuardRail{g1, g2}}
	d, err := s.Check(context.Background(), Input{})
	must.NoError(err)
	must.Equal(VerdictAllowSpot, d.Verdict)
}

func TestParallel_EmptyAllows(t *testing.T) {
	must := require.New(t)
	p := Parallel{ID: "par"}
	d, err := p.Check(context.Background(), Input{})
	must.NoError(err)
	must.Equal(VerdictAllow, d.Verdict)
	must.Equal("par", d.GuardID)
}

func TestParallel_EmptyDefaultsID(t *testing.T) {
	must := require.New(t)
	d, err := Parallel{}.Check(context.Background(), Input{})
	must.NoError(err)
	must.Equal("parallel", d.GuardID)
}

func TestParallel_DenyBeatsSpotlightAndAllow(t *testing.T) {
	must := require.New(t)
	g1 := &stubGuard{d: allow("a")}
	g2 := &stubGuard{d: spotlight("b")}
	g3 := &stubGuard{d: deny("c", "blocked")}
	p := Parallel{Guards: []GuardRail{g1, g2, g3}}

	d, err := p.Check(context.Background(), Input{})
	must.NoError(err)
	must.Equal(VerdictDeny, d.Verdict)
	must.Equal("c", d.GuardID)
}

func TestParallel_SpotlightBeatsAllow(t *testing.T) {
	must := require.New(t)
	g1 := &stubGuard{d: allow("a")}
	g2 := &stubGuard{d: spotlight("b")}
	p := Parallel{Guards: []GuardRail{g1, g2}}
	d, err := p.Check(context.Background(), Input{})
	must.NoError(err)
	must.Equal(VerdictAllowSpot, d.Verdict)
	must.Equal("b", d.GuardID)
}

func TestParallel_DenyWinsEvenWhenOtherGuardErrors(t *testing.T) {
	must := require.New(t)
	g1 := &stubGuard{err: errors.New("boom")}
	g2 := &stubGuard{d: deny("c", "blocked")}
	p := Parallel{Guards: []GuardRail{g1, g2}}

	d, err := p.Check(context.Background(), Input{})
	must.NoError(err, "a deny verdict must win over a sibling error")
	must.Equal(VerdictDeny, d.Verdict)
}

func TestParallel_ReturnsErrorWhenNoDenyAndAGuardErrored(t *testing.T) {
	must := require.New(t)
	sentinel := errors.New("boom")
	g1 := &stubGuard{err: sentinel}
	g2 := &stubGuard{d: allow("ok")}
	p := Parallel{Guards: []GuardRail{g1, g2}}

	_, err := p.Check(context.Background(), Input{})
	must.ErrorIs(err, sentinel)
}

func TestParallel_LatencyIsMaxOfChildren(t *testing.T) {
	must := require.New(t)
	short := &stubGuard{d: &Decision{Verdict: VerdictAllow, GuardID: "s", Latency: 5 * time.Millisecond}}
	long := &stubGuard{d: &Decision{Verdict: VerdictAllow, GuardID: "l", Latency: 50 * time.Millisecond}}
	mid := &stubGuard{d: &Decision{Verdict: VerdictAllow, GuardID: "m", Latency: 25 * time.Millisecond}}
	p := Parallel{Guards: []GuardRail{short, long, mid}}

	d, err := p.Check(context.Background(), Input{})
	must.NoError(err)
	must.Equal(50*time.Millisecond, d.Latency)
}

func TestParallel_AggregatesReasonsAtStrongestRank(t *testing.T) {
	must := require.New(t)
	g1 := &stubGuard{d: deny("a", "first")}
	g2 := &stubGuard{d: deny("b", "second")}
	g3 := &stubGuard{d: allow("c")}
	p := Parallel{Guards: []GuardRail{g1, g2, g3}}

	d, err := p.Check(context.Background(), Input{})
	must.NoError(err)
	// Both denies contribute reasons; the allow does not.
	must.ElementsMatch([]string{"first", "second"}, strings.Split(d.Reason, ", "))
}
