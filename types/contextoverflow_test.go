package types

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMarkContextOverflow_KeepsMessageAndDetection(t *testing.T) {
	base := errors.New("prompt is too long")
	marked := MarkContextOverflow(base)

	require.True(t, IsContextOverflow(marked), "marked error must be detected as an overflow")
	require.Equal(t, base.Error(), marked.Error())
	require.ErrorIs(t, marked, base)
	require.True(t, IsContextOverflow(fmt.Errorf("execute request: %w", marked)), "mark must be found under a wrapping error")
	require.False(t, IsContextOverflow(base), "unmarked error must not be detected as an overflow")
	require.False(t, IsContextOverflow(nil))
	require.NoError(t, MarkContextOverflow(nil))
}
