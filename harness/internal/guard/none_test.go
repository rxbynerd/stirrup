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
			must := require.New(t)
			d, err := g.Check(context.Background(), Input{Phase: ph, Content: "anything"})
			must.NoError(err)
			must.NotNil(d)
			must.Equal(VerdictAllow, d.Verdict)
			must.Equal("none", d.GuardID)
		})
	}
}
