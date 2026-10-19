package guard

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNoop_AllowsEveryPhase(t *testing.T) {
	g := NewNoop()
	for _, ph := range []Phase{PhasePreTurn, PhasePreTool, PhasePostTurn} {
		t.Run(string(ph), func(t *testing.T) {
			d, err := g.Check(context.Background(), Input{Phase: ph, Content: "anything"})
			require.NoError(t, err)
			require.NotNil(t, d)
			require.Equal(t, VerdictAllow, d.Verdict)
			require.Equal(t, "none", d.GuardID)
		})
	}
}
