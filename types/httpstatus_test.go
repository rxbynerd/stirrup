package types

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHTTPStatus_RoundTripsThroughWrapping pins that a marked status
// survives further wrapping, including MarkRetryable, and that an unmarked
// error reports none.
func TestHTTPStatus_RoundTripsThroughWrapping(t *testing.T) {
	t.Run("marked status is found through wrappers", func(t *testing.T) {
		must := require.New(t)
		base := errors.New("rejected")
		err := fmt.Errorf("stream: %w", MarkRetryable(MarkHTTPStatus(base, 400)))
		status, ok := HTTPStatus(err)
		must.True(ok)
		must.Equal(400, status)
		must.ErrorIs(err, base)
		must.Equal("stream: rejected", err.Error())
	})

	t.Run("unmarked error reports none", func(t *testing.T) {
		must := require.New(t)
		_, ok := HTTPStatus(errors.New("plain"))
		must.False(ok)
	})

	t.Run("nil stays nil", func(t *testing.T) {
		must := require.New(t)
		must.NoError(MarkHTTPStatus(nil, 500))
	})
}
