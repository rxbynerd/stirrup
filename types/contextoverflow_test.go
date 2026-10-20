package types

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMarkContextOverflow_KeepsMessageAndDetection(t *testing.T) {
	must := require.New(t)
	base := errors.New("prompt is too long")
	marked := MarkContextOverflow(base)

	must.True(IsContextOverflow(marked), "marked error must be detected as an overflow")
	must.Equal(base.Error(), marked.Error())
	must.ErrorIs(marked, base)
	must.True(IsContextOverflow(fmt.Errorf("execute request: %w", marked)), "mark must be found under a wrapping error")
	must.False(IsContextOverflow(base), "unmarked error must not be detected as an overflow")
	must.False(IsContextOverflow(nil))
	must.NoError(MarkContextOverflow(nil))
}
