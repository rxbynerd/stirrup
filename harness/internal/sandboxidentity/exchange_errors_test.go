package sandboxidentity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func respondWith(ev types.ControlEvent) func(string) (types.ControlEvent, bool) {
	return func(requestID string) (types.ControlEvent, bool) {
		ev.Type = "sandbox_token_response"
		ev.RequestID = requestID
		return ev, true
	}
}

// TestExchange_ErrorWrapsSentinelAndTransientFlag pins the sentinel each
// outcome wraps and which of them count as transient, since the refresher's
// retry decision rests on that classification.
func TestExchange_ErrorWrapsSentinelAndTransientFlag(t *testing.T) {
	cases := []struct {
		name      string
		mt        *mockTransport
		want      error
		transient bool
	}{
		{
			name:      "decline",
			mt:        &mockTransport{respond: respondWith(types.ControlEvent{IsError: boolPtr(true), Reason: "issuer revoked"})},
			want:      ErrDeclined,
			transient: false,
		},
		{
			name:      "timeout",
			mt:        &mockTransport{},
			want:      ErrTimeout,
			transient: true,
		},
		{
			name:      "emit failure",
			mt:        &mockTransport{emitErr: errors.New("stream closed")},
			want:      ErrTransport,
			transient: true,
		},
		{
			name:      "empty token",
			mt:        &mockTransport{respond: respondWith(types.ControlEvent{})},
			want:      ErrEmptyToken,
			transient: false,
		},
		{
			name:      "oversized token",
			mt:        &mockTransport{respond: respondWith(types.ControlEvent{Token: strings.Repeat("a", MaxTokenBytes+1)})},
			want:      ErrTokenTooLarge,
			transient: false,
		},
		{
			name:      "malformed token",
			mt:        &mockTransport{respond: respondWith(types.ControlEvent{Token: "abc\nusername=attacker"})},
			want:      ErrMalformedToken,
			transient: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Exchange(context.Background(), tc.mt, "aud", 20*time.Millisecond)
			require.ErrorIs(t, err, tc.want)
			require.Empty(t, result.Token, "a failed exchange must not return a token")
			require.Equal(t, tc.transient, IsTransient(err))
		})
	}
}

// TestExchange_RejectsTokensThatCouldInjectHelperDirectives asserts a
// token is confined to printable ASCII: the credential helper echoes it
// into git's line-oriented protocol, so a newline or carriage return would
// let a control plane append directives of its own. The rejected value
// must not surface in the error either.
func TestExchange_RejectsTokensThatCouldInjectHelperDirectives(t *testing.T) {
	for name, token := range map[string]string{
		"newline":         "abc\nusername=attacker",
		"carriage return": "abc\rquit=1",
		"nul":             "abc\x00def",
		"tab":             "abc\tdef",
		"delete":          "abc\x7fdef",
		"non-ascii":       "ünïcode.token",
	} {
		t.Run(name, func(t *testing.T) {
			mt := &mockTransport{respond: respondWith(types.ControlEvent{Token: token})}
			result, err := Exchange(context.Background(), mt, "aud", time.Second)
			require.ErrorIs(t, err, ErrMalformedToken)
			require.Empty(t, result.Token, "a malformed token must not be returned")
			require.NotContains(t, err.Error(), "abc", "error must not carry the rejected token")
		})
	}

	t.Run("accepts base64url jwt", func(t *testing.T) {
		mt := &mockTransport{respond: respondWith(types.ControlEvent{Token: "eyJhbGciOiJFUzI1NiJ9.payload-with_all~allowed+chars/=.sig"})}
		_, err := Exchange(context.Background(), mt, "aud", time.Second)
		require.NoError(t, err, "a base64url JWT with dots must be accepted")
	})
}

// TestExchange_SanitisesDeclineReason asserts a control-plane reason is
// stripped of control characters and capped before it reaches an error
// that is later interpolated into a warning event and a log line.
func TestExchange_SanitisesDeclineReason(t *testing.T) {
	hostile := "issuer revoked\nusername=attacker\r\x00" + strings.Repeat("x", 500)
	mt := &mockTransport{respond: respondWith(types.ControlEvent{IsError: boolPtr(true), Reason: hostile})}

	_, err := Exchange(context.Background(), mt, "aud", time.Second)
	require.ErrorIs(t, err, ErrDeclined)

	msg := err.Error()
	require.False(t, strings.ContainsAny(msg, "\n\r\x00"), "error %q carries control characters from the reason", msg)
	require.Contains(t, msg, "issuer revokedusername=attacker")
	require.LessOrEqual(t, strings.Count(msg, "x"), maxReasonRunes, "reason must be capped at %d runes", maxReasonRunes)
	require.True(t, strings.HasSuffix(msg, "…"), "a capped reason should end with an ellipsis, got %q", msg)
}
