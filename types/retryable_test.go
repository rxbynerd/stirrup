package types

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

type notRetryable struct{}

func (notRetryable) Error() string   { return "auth" }
func (notRetryable) Retryable() bool { return false }

func TestIsRetryable_ReportsOnlyMarkedErrors(t *testing.T) {
	base := errors.New("overloaded")
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "plain error", err: base, want: false},
		{name: "marked", err: MarkRetryable(base), want: true},
		{name: "marked then wrapped", err: fmt.Errorf("stream: %w", MarkRetryable(base)), want: true},
		{name: "explicitly not retryable", err: notRetryable{}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, IsRetryable(tc.err), "IsRetryable(%v)", tc.err)
		})
	}
}

func TestMarkRetryable_PreservesMessageAndChain(t *testing.T) {
	base := errors.New("connection reset")
	marked := MarkRetryable(base)

	t.Run("keeps message", func(t *testing.T) {
		require.Equal(t, base.Error(), marked.Error())
	})
	t.Run("unwraps to original", func(t *testing.T) {
		require.ErrorIs(t, marked, base)
	})
	t.Run("nil returns nil", func(t *testing.T) {
		require.NoError(t, MarkRetryable(nil))
	})
}
