package sandboxidentity

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestExchange_Success(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Exchange() error: %v", err)
	}
	if result.Token != "the-jwt" {
		t.Errorf("Token = %q, want %q", result.Token, "the-jwt")
	}
	if result.ExpiresAt == nil || *result.ExpiresAt != 1234 {
		t.Errorf("ExpiresAt = %v, want 1234", result.ExpiresAt)
	}

	emitted := mt.lastEmitted()
	if emitted == nil {
		t.Fatal("expected a sandbox_token_request to be emitted")
	}
	if emitted.Type != "sandbox_token_request" {
		t.Errorf("Type = %q, want sandbox_token_request", emitted.Type)
	}
	if emitted.Audience != "https://haybale.internal" {
		t.Errorf("Audience = %q, want https://haybale.internal", emitted.Audience)
	}
	if emitted.RequestID == "" {
		t.Error("RequestID should be set on the outbound request")
	}
}

// TestExchange_Timeout asserts fail-closed behaviour: no response within
// the bounded wait returns an error and a zero Result, never a partial or
// default token.
func TestExchange_Timeout(t *testing.T) {
	mt := &mockTransport{} // never responds

	result, err := Exchange(context.Background(), mt, "aud", 20*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if result.Token != "" {
		t.Errorf("expected empty token on timeout, got %q", result.Token)
	}
}

// TestExchange_ContextCancelled asserts cancellation also fails closed.
func TestExchange_ContextCancelled(t *testing.T) {
	mt := &mockTransport{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := Exchange(ctx, mt, "aud", time.Second)
	if err == nil {
		t.Fatal("expected an error from a cancelled context, got nil")
	}
	if result.Token != "" {
		t.Errorf("expected empty token, got %q", result.Token)
	}
}

// TestExchange_Decline asserts a control-plane decline (IsError) aborts
// with an error incorporating Reason, never proceeding with a token.
func TestExchange_Decline(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected an error from a declined exchange")
	}
	if !strings.Contains(err.Error(), "no issuer configured") {
		t.Errorf("error %q should incorporate the decline reason", err.Error())
	}
	if result.Token != "" {
		t.Errorf("expected empty token on decline, got %q", result.Token)
	}
}

// TestExchange_OverCap asserts a token exceeding MaxTokenBytes is treated
// as an error and never truncated-and-used.
func TestExchange_OverCap(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected an error for an oversized token")
	}
	if result.Token != "" {
		t.Error("an oversized token must not be returned, truncated or otherwise")
	}
	if strings.Contains(err.Error(), oversized) {
		t.Error("error message must not contain the raw oversized token content")
	}
}

// TestExchange_EmptyToken asserts a success-shaped response (IsError not
// set) carrying no token is still treated as a failure rather than an
// empty-string credential silently flowing into env composition.
func TestExchange_EmptyToken(t *testing.T) {
	mt := &mockTransport{
		respond: func(requestID string) (types.ControlEvent, bool) {
			return types.ControlEvent{
				Type:      "sandbox_token_response",
				RequestID: requestID,
			}, true
		},
	}

	_, err := Exchange(context.Background(), mt, "aud", time.Second)
	if err == nil {
		t.Fatal("expected an error for an empty token")
	}
}

// TestExchange_NoTokenInLogs is the CF-1 carry-forward test (issue #516):
// no slog record emitted during a token exchange (success, decline,
// timeout, or over-cap) may contain the literal token value, and returned
// errors must not surface it either. Exchange must destructure
// ControlEvent to named fields rather than %v/%+v-formatting or
// json.Marshal-ing the whole struct.
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
	if err != nil {
		t.Fatalf("Exchange() error: %v", err)
	}
	if result.Token != secretToken {
		t.Fatalf("sanity check: expected the returned token to equal the secret fixture")
	}

	if strings.Contains(buf.String(), secretToken) {
		t.Errorf("token leaked into slog output: %s", buf.String())
	}

	// Also exercise the decline and over-cap paths, which construct error
	// strings from response fields — confirm neither leaks a token value
	// through fmt.Errorf.
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
	if declineErr == nil || strings.Contains(declineErr.Error(), secretToken) {
		t.Errorf("decline error must not contain the token: %v", declineErr)
	}

	if strings.Contains(buf.String(), secretToken) {
		t.Errorf("token leaked into slog output after decline path: %s", buf.String())
	}
}
