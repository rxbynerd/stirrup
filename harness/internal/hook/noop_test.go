package hook

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNoop_ImplementsRunner is a compile-time satisfaction guard.
func TestNoop_ImplementsRunner(t *testing.T) {
	var _ Runner = NewNoop()
}

func TestNoop_RunPreAndRunPost_AreNoOps(t *testing.T) {
	n := NewNoop()

	t.Run("run pre returns no results and no error", func(t *testing.T) {
		must := require.New(t)
		results, err := n.RunPre(context.Background())
		must.NoError(err)
		must.Nil(results)
	})

	t.Run("run post returns no results and no error", func(t *testing.T) {
		must := require.New(t)
		results, err := n.RunPost(context.Background(), "success")
		must.NoError(err)
		must.Nil(results)
	})
}
