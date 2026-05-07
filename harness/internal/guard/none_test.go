package guard

import (
	"context"
	"testing"
)

func TestNoopAllowsEveryPhase(t *testing.T) {
	g := NewNoop()
	for _, ph := range []Phase{PhasePreTurn, PhasePreTool, PhasePostTurn} {
		t.Run(string(ph), func(t *testing.T) {
			d, err := g.Check(context.Background(), Input{Phase: ph, Content: "anything"})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d == nil {
				t.Fatal("nil decision from Noop")
			}
			if d.Verdict != VerdictAllow {
				t.Fatalf("verdict = %q, want allow", d.Verdict)
			}
			if d.GuardID != "none" {
				t.Fatalf("guard id = %q, want \"none\"", d.GuardID)
			}
		})
	}
}
