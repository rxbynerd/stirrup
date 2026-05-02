package transport

import (
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func TestNullTransport_ImplementsInterface(t *testing.T) {
	var _ Transport = (*NullTransport)(nil)
}

func TestNullTransport_EmitReturnsNil(t *testing.T) {
	tp := NewNullTransport()
	if err := tp.Emit(types.HarnessEvent{Type: "text_delta", Text: "hello"}); err != nil {
		t.Errorf("Emit() returned error: %v", err)
	}
}

func TestNullTransport_CloseReturnsNil(t *testing.T) {
	tp := NewNullTransport()
	if err := tp.Close(); err != nil {
		t.Errorf("Close() returned error: %v", err)
	}
}

func TestNullTransport_OnControlDoesNotPanic(t *testing.T) {
	tp := NewNullTransport()
	// Should not panic even with a non-nil handler.
	tp.OnControl(func(_ types.ControlEvent) {})
}

// onControlOnly is a stub HasOnControl that does NOT implement
// NullCapable. It is the analogue of the production stdio/grpc
// transports for the purpose of IsNull checks.
type onControlOnly struct{}

func (onControlOnly) OnControl(_ func(types.ControlEvent)) {}

// embedsNullTransport wraps NullTransport. The async tool dispatch path
// promotes IsNullTransport through embedding: a sub-agent's
// captureTransport embeds NullTransport, and IsNull must still report it
// as null-equivalent so async dispatch fails fast rather than blocking
// on a transport that never delivers.
type embedsNullTransport struct {
	*NullTransport
}

func TestIsNull_NilReturnsFalse(t *testing.T) {
	// IsNull(nil) is false by contract. Callers that pass a nil
	// transport should fail their own nil-check separately; IsNull is
	// strictly "is this a null-equivalent transport", not "is this a
	// usable transport".
	if IsNull(nil) {
		t.Fatal("IsNull(nil) returned true; expected false")
	}
}

func TestIsNull_NullTransport(t *testing.T) {
	if !IsNull(NewNullTransport()) {
		t.Fatal("IsNull(NewNullTransport()) returned false; expected true")
	}
}

func TestIsNull_NonNullTransport(t *testing.T) {
	// A transport that exposes OnControl but does NOT implement
	// NullCapable must be reported as non-null. This guards against a
	// regression where IsNull's type assertion is loosened to a
	// structural check that would mistakenly flag every Transport.
	if IsNull(onControlOnly{}) {
		t.Fatal("IsNull(onControlOnly{}) returned true; expected false")
	}
}

func TestIsNull_EmbeddedNullTransportPromoted(t *testing.T) {
	// Wrappers that embed *NullTransport inherit IsNullTransport via
	// method promotion. This is load-bearing for the sub-agent path:
	// the sub-agent's captureTransport embeds NullTransport, and async
	// tool dispatch on a sub-agent must see the embedder as null-
	// equivalent so it fails fast rather than blocking on the per-call
	// timeout.
	wrapped := &embedsNullTransport{NullTransport: NewNullTransport()}
	if !IsNull(wrapped) {
		t.Fatal("IsNull(embedsNullTransport) returned false; expected true (method promotion)")
	}
}
