package sandboxidentity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

func respondWith(ev types.ControlEvent) func(string) (types.ControlEvent, bool) {
	return func(requestID string) (types.ControlEvent, bool) {
		ev.Type = "sandbox_token_response"
		ev.RequestID = requestID
		return ev, true
	}
}

// TestExchange_ErrorSentinels pins the sentinel each outcome wraps and
// which of them count as transient, since the refresher's retry decision
// rests on that classification.
func TestExchange_ErrorSentinels(t *testing.T) {
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
			if !errors.Is(err, tc.want) {
				t.Fatalf("Exchange() error = %v, want %v", err, tc.want)
			}
			if result.Token != "" {
				t.Errorf("Exchange() returned a token alongside %v", tc.want)
			}
			if IsTransient(err) != tc.transient {
				t.Errorf("IsTransient(%v) = %v, want %v", err, IsTransient(err), tc.transient)
			}
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
			if !errors.Is(err, ErrMalformedToken) {
				t.Fatalf("Exchange() error = %v, want ErrMalformedToken", err)
			}
			if result.Token != "" {
				t.Error("a malformed token must not be returned")
			}
			if strings.Contains(err.Error(), "abc") {
				t.Errorf("error %q must not carry the rejected token", err)
			}
		})
	}

	mt := &mockTransport{respond: respondWith(types.ControlEvent{Token: "eyJhbGciOiJFUzI1NiJ9.payload-with_all~allowed+chars/=.sig"})}
	if _, err := Exchange(context.Background(), mt, "aud", time.Second); err != nil {
		t.Errorf("a base64url JWT with dots must be accepted, got %v", err)
	}
}

// TestExchange_SanitisesDeclineReason asserts a control-plane reason is
// stripped of control characters and capped before it reaches an error
// that is later interpolated into a warning event and a log line.
func TestExchange_SanitisesDeclineReason(t *testing.T) {
	hostile := "issuer revoked\nusername=attacker\r\x00" + strings.Repeat("x", 500)
	mt := &mockTransport{respond: respondWith(types.ControlEvent{IsError: boolPtr(true), Reason: hostile})}

	_, err := Exchange(context.Background(), mt, "aud", time.Second)
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("Exchange() error = %v, want ErrDeclined", err)
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\n\r\x00") {
		t.Errorf("error %q carries control characters from the reason", msg)
	}
	if !strings.Contains(msg, "issuer revokedusername=attacker") {
		t.Errorf("error %q should keep the printable part of the reason", msg)
	}
	if strings.Count(msg, "x") > maxReasonRunes {
		t.Errorf("error %q was not capped at %d runes", msg, maxReasonRunes)
	}
	if !strings.HasSuffix(msg, "…") {
		t.Errorf("a capped reason should end with an ellipsis, got %q", msg)
	}
}
