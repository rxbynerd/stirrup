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
		results, err := n.RunPre(context.Background())
		require.NoError(t, err)
		require.Nil(t, results)
	})

	t.Run("run post returns no results and no error", func(t *testing.T) {
		results, err := n.RunPost(context.Background(), "success")
		require.NoError(t, err)
		require.Nil(t, results)
	})
}
