package guard

import (
	"context"
	"errors"
	"testing"
	"time"
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

func TestSequentialEmptyAllows(t *testing.T) {
	s := Sequential{ID: "seq"}
	d, err := s.Check(context.Background(), Input{Phase: PhasePreTurn})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Verdict != VerdictAllow {
		t.Fatalf("verdict = %q, want allow", d.Verdict)
	}
	if d.GuardID != "seq" {
		t.Fatalf("guard id = %q, want \"seq\"", d.GuardID)
	}
}

func TestSequentialEmptyDefaultsID(t *testing.T) {
	d, err := Sequential{}.Check(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	if d.GuardID != "sequential" {
		t.Fatalf("default id = %q, want \"sequential\"", d.GuardID)
	}
}

func TestSequentialShortCircuitsOnDeny(t *testing.T) {
	first := &stubGuard{d: allow("a")}
	denier := &stubGuard{d: deny("b", "bad")}
	never := &stubGuard{d: allow("c")}
	s := Sequential{Guards: []GuardRail{first, denier, never}}

	d, err := s.Check(context.Background(), Input{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d.Verdict != VerdictDeny {
		t.Fatalf("verdict = %q, want deny", d.Verdict)
	}
	if d.GuardID != "b" {
		t.Fatalf("guard id = %q, want \"b\"", d.GuardID)
	}
	if never.called != 0 {
		t.Fatalf("guards after deny were invoked %d times, expected 0", never.called)
	}
}

func TestSequentialPropagatesError(t *testing.T) {
	sentinel := errors.New("boom")
	first := &stubGuard{d: allow("a")}
	bad := &stubGuard{err: sentinel}
	never := &stubGuard{d: allow("c")}
	s := Sequential{Guards: []GuardRail{first, bad, never}}

	_, err := s.Check(context.Background(), Input{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want sentinel", err)
	}
	if never.called != 0 {
		t.Fatalf("guards after error were invoked %d times, expected 0", never.called)
	}
}

func TestSequentialReturnsLastDecisionWhenAllAllow(t *testing.T) {
	g1 := &stubGuard{d: allow("a")}
	g2 := &stubGuard{d: spotlight("b")}
	g3 := &stubGuard{d: allow("c")}
	// Spotlight from g2 should propagate via "last decision" rules
	// only when no later guard overrides; here g3 returns allow last.
	s := Sequential{Guards: []GuardRail{g1, g2, g3}}
	d, err := s.Check(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	if d.GuardID != "c" {
		t.Fatalf("expected last guard's decision, got id %q", d.GuardID)
	}
}

func TestSequentialPreservesSpotlightWhenItIsLast(t *testing.T) {
	g1 := &stubGuard{d: allow("a")}
	g2 := &stubGuard{d: spotlight("b")}
	s := Sequential{Guards: []GuardRail{g1, g2}}
	d, err := s.Check(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Verdict != VerdictAllowSpot {
		t.Fatalf("verdict = %q, want spotlight", d.Verdict)
	}
}

func TestParallelEmptyAllows(t *testing.T) {
	p := Parallel{ID: "par"}
	d, err := p.Check(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Verdict != VerdictAllow {
		t.Fatalf("verdict = %q, want allow", d.Verdict)
	}
	if d.GuardID != "par" {
		t.Fatalf("guard id = %q, want \"par\"", d.GuardID)
	}
}

func TestParallelEmptyDefaultsID(t *testing.T) {
	d, err := Parallel{}.Check(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	if d.GuardID != "parallel" {
		t.Fatalf("default id = %q, want \"parallel\"", d.GuardID)
	}
}

func TestParallelDenyBeatsSpotlightAndAllow(t *testing.T) {
	g1 := &stubGuard{d: allow("a")}
	g2 := &stubGuard{d: spotlight("b")}
	g3 := &stubGuard{d: deny("c", "blocked")}
	p := Parallel{Guards: []GuardRail{g1, g2, g3}}

	d, err := p.Check(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Verdict != VerdictDeny {
		t.Fatalf("verdict = %q, want deny", d.Verdict)
	}
	if d.GuardID != "c" {
		t.Fatalf("guard id = %q, want \"c\"", d.GuardID)
	}
}

func TestParallelSpotlightBeatsAllow(t *testing.T) {
	g1 := &stubGuard{d: allow("a")}
	g2 := &stubGuard{d: spotlight("b")}
	p := Parallel{Guards: []GuardRail{g1, g2}}
	d, err := p.Check(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Verdict != VerdictAllowSpot {
		t.Fatalf("verdict = %q, want spotlight", d.Verdict)
	}
	if d.GuardID != "b" {
		t.Fatalf("guard id = %q, want \"b\"", d.GuardID)
	}
}

func TestParallelDenyWinsEvenWhenOtherGuardErrors(t *testing.T) {
	g1 := &stubGuard{err: errors.New("boom")}
	g2 := &stubGuard{d: deny("c", "blocked")}
	p := Parallel{Guards: []GuardRail{g1, g2}}

	d, err := p.Check(context.Background(), Input{})
	if err != nil {
		t.Fatalf("expected no error when deny wins; got %v", err)
	}
	if d.Verdict != VerdictDeny {
		t.Fatalf("verdict = %q, want deny", d.Verdict)
	}
}

func TestParallelReturnsErrorWhenNoDenyAndAGuardErrored(t *testing.T) {
	sentinel := errors.New("boom")
	g1 := &stubGuard{err: sentinel}
	g2 := &stubGuard{d: allow("ok")}
	p := Parallel{Guards: []GuardRail{g1, g2}}

	_, err := p.Check(context.Background(), Input{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want sentinel", err)
	}
}

func TestParallelLatencyIsMaxOfChildren(t *testing.T) {
	short := &stubGuard{d: &Decision{Verdict: VerdictAllow, GuardID: "s", Latency: 5 * time.Millisecond}}
	long := &stubGuard{d: &Decision{Verdict: VerdictAllow, GuardID: "l", Latency: 50 * time.Millisecond}}
	mid := &stubGuard{d: &Decision{Verdict: VerdictAllow, GuardID: "m", Latency: 25 * time.Millisecond}}
	p := Parallel{Guards: []GuardRail{short, long, mid}}

	d, err := p.Check(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Latency != 50*time.Millisecond {
		t.Fatalf("latency = %v, want max (50ms)", d.Latency)
	}
}

func TestParallelAggregatesReasonsAtStrongestRank(t *testing.T) {
	g1 := &stubGuard{d: deny("a", "first")}
	g2 := &stubGuard{d: deny("b", "second")}
	g3 := &stubGuard{d: allow("c")}
	p := Parallel{Guards: []GuardRail{g1, g2, g3}}

	d, err := p.Check(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	// Both denies contribute reasons; the allow does not.
	if d.Reason != "first, second" && d.Reason != "second, first" {
		t.Fatalf("reason = %q, want concatenation of deny reasons", d.Reason)
	}
}
