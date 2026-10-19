package sandboxidentity

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// mockTransport implements the Transport interface for testing, mirroring
// permission/askupstream_test.go's mockTransport.
type mockTransport struct {
	mu       sync.Mutex
	emitted  []types.HarnessEvent
	handlers []func(types.ControlEvent)
	emitErr  error

	// respond, when set, is invoked synchronously from Emit with the
	// emitted event's RequestID; it returns the ControlEvent to deliver
	// (or false to deliver nothing, e.g. to simulate a timeout).
	respond func(requestID string) (types.ControlEvent, bool)
}

func (m *mockTransport) Emit(event types.HarnessEvent) error {
	m.mu.Lock()
	if m.emitErr != nil {
		m.mu.Unlock()
		return m.emitErr
	}
	m.emitted = append(m.emitted, event)
	respond := m.respond
	m.mu.Unlock()

	if respond != nil {
		if ce, ok := respond(event.RequestID); ok {
			m.deliver(ce)
		}
	}
	return nil
}

func (m *mockTransport) OnControl(handler func(types.ControlEvent)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers = append(m.handlers, handler)
}

func (m *mockTransport) deliver(event types.ControlEvent) {
	m.mu.Lock()
	handlers := make([]func(types.ControlEvent), len(m.handlers))
	copy(handlers, m.handlers)
	m.mu.Unlock()
	for _, h := range handlers {
		h(event)
	}
}

func (m *mockTransport) lastEmitted() *types.HarnessEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.emitted) == 0 {
		return nil
	}
	e := m.emitted[len(m.emitted)-1]
	return &e
}

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(v int64) *int64 { return &v }

func TestExchange_ReturnsTokenAndExpiry(t *testing.T) {
	mt := &mockTransport{
		respond: func(requestID string) (types.ControlEvent, bool) {
			return types.ControlEvent{
				Type:      "sandbox_token_response",
				RequestID: requestID,
				Token:     "the-jwt",
				ExpiresAt: int64Ptr(1234),
			}, true
		},
	}

	result, err := Exchange(context.Background(), mt, "https://haybale.internal", time.Second)
	require.NoError(t, err)
	require.Equal(t, "the-jwt", result.Token)
	require.NotNil(t, result.ExpiresAt)
	require.Equal(t, int64(1234), *result.ExpiresAt)

	emitted := mt.lastEmitted()
	require.NotNil(t, emitted, "expected a sandbox_token_request to be emitted")
	require.Equal(t, "sandbox_token_request", emitted.Type)
	require.Equal(t, "https://haybale.internal", emitted.Audience)
	require.NotEmpty(t, emitted.RequestID, "RequestID should be set on the outbound request")
}

// TestExchange_TimeoutFailsClosed asserts fail-closed behaviour: no response within
// the bounded wait returns an error and a zero Result, never a partial or
// default token.
func TestExchange_TimeoutFailsClosed(t *testing.T) {
	mt := &mockTransport{} // never responds

	result, err := Exchange(context.Background(), mt, "aud", 20*time.Millisecond)
	require.Error(t, err)
	require.Empty(t, result.Token)
}

// TestExchange_ZeroTimeoutUsesDefault verifies that a timeout of zero falls
// back to DefaultTimeout rather than firing immediately. The default is too
// long to wait out, so the test cancels the context after a short delay and
// checks that the call did not return at once.
func TestExchange_ZeroTimeoutUsesDefault(t *testing.T) {
	mt := &mockTransport{} // never responds

	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	result, err := Exchange(ctx, mt, "aud", 0)
	elapsed := time.Since(start)
	require.Error(t, err)
	require.Empty(t, result.Token)
	require.GreaterOrEqual(t, elapsed, 15*time.Millisecond,
		"a zero timeout may have fired immediately instead of falling back to DefaultTimeout")
}

// TestExchange_CancelledContextFailsClosed asserts cancellation also fails closed.
func TestExchange_CancelledContextFailsClosed(t *testing.T) {
	mt := &mockTransport{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := Exchange(ctx, mt, "aud", time.Second)
	require.Error(t, err)
	require.Empty(t, result.Token)
}

// TestExchange_DeclineReturnsReason asserts a control-plane decline (IsError) aborts
// with an error incorporating Reason, never proceeding with a token.
func TestExchange_DeclineReturnsReason(t *testing.T) {
	mt := &mockTransport{
		respond: func(requestID string) (types.ControlEvent, bool) {
			return types.ControlEvent{
				Type:      "sandbox_token_response",
				RequestID: requestID,
				IsError:   boolPtr(true),
				Reason:    "no issuer configured",
			}, true
		},
	}

	result, err := Exchange(context.Background(), mt, "aud", time.Second)
	require.ErrorContains(t, err, "no issuer configured")
	require.Empty(t, result.Token)
}

// TestExchange_OversizedTokenRejected asserts a token exceeding MaxTokenBytes is treated
// as an error and never truncated-and-used.
func TestExchange_OversizedTokenRejected(t *testing.T) {
	oversized := strings.Repeat("a", MaxTokenBytes+1)
	mt := &mockTransport{
		respond: func(requestID string) (types.ControlEvent, bool) {
			return types.ControlEvent{
				Type:      "sandbox_token_response",
				RequestID: requestID,
				Token:     oversized,
			}, true
		},
	}

	result, err := Exchange(context.Background(), mt, "aud", time.Second)
	require.Error(t, err)
	require.Empty(t, result.Token)
	require.NotContains(t, err.Error(), oversized, "error message must not contain the raw token content")
}

// TestExchange_EmptyTokenIsError asserts a success-shaped response (IsError not
// set) carrying no token is still treated as a failure rather than an
// empty-string credential silently flowing into env composition.
func TestExchange_EmptyTokenIsError(t *testing.T) {
	mt := &mockTransport{
		respond: func(requestID string) (types.ControlEvent, bool) {
			return types.ControlEvent{
				Type:      "sandbox_token_response",
				RequestID: requestID,
			}, true
		},
	}

	_, err := Exchange(context.Background(), mt, "aud", time.Second)
	require.Error(t, err)
}

// TestExchange_NoTokenInLogs asserts that neither slog output nor errors from a
// successful or declined exchange contain the token. Exchange must read named
// ControlEvent fields, never format the whole struct.
func TestExchange_NoTokenInLogs(t *testing.T) {
	const secretToken = "eyJhbGciOiJFUzI1NiJ9.super-secret-payload-value.signature-goes-here"

	var buf strings.Builder
	handler := slog.NewTextHandler(&buf, nil)
	prevDefault := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(prevDefault)

	mt := &mockTransport{
		respond: func(requestID string) (types.ControlEvent, bool) {
			return types.ControlEvent{
				Type:      "sandbox_token_response",
				RequestID: requestID,
				Token:     secretToken,
				ExpiresAt: int64Ptr(time.Now().Add(time.Hour).Unix()),
			}, true
		},
	}

	result, err := Exchange(context.Background(), mt, "aud", time.Second)
	require.NoError(t, err)
	require.Equal(t, secretToken, result.Token, "returned token should match the fixture")
	require.NotContains(t, buf.String(), secretToken, "token leaked into slog output")

	// The decline path builds its error from response fields, so the error
	// must not embed the token either.
	mtDecline := &mockTransport{
		respond: func(requestID string) (types.ControlEvent, bool) {
			return types.ControlEvent{
				Type:      "sandbox_token_response",
				RequestID: requestID,
				IsError:   boolPtr(true),
				Reason:    "issuer down",
			}, true
		},
	}
	_, declineErr := Exchange(context.Background(), mtDecline, "aud", time.Second)
	require.Error(t, declineErr)
	require.NotContains(t, declineErr.Error(), secretToken, "decline error must not contain the token")
	require.NotContains(t, buf.String(), secretToken, "token leaked into slog output after decline path")
}
