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
