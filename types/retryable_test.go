package types

import (
	"errors"
	"fmt"
	"testing"
)

type notRetryable struct{}

func (notRetryable) Error() string   { return "auth" }
func (notRetryable) Retryable() bool { return false }

func TestIsRetryable(t *testing.T) {
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
			if got := IsRetryable(tc.err); got != tc.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestMarkRetryable_PreservesMessageAndChain(t *testing.T) {
	base := errors.New("connection reset")
	marked := MarkRetryable(base)
	if marked.Error() != base.Error() {
		t.Errorf("message = %q, want %q", marked.Error(), base.Error())
	}
	if !errors.Is(marked, base) {
		t.Error("marked error does not unwrap to the original")
	}
	if MarkRetryable(nil) != nil {
		t.Error("MarkRetryable(nil) should return nil")
	}
}
