package sandboxidentity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// TestExchanger_CountsRequestsAndSequencesIDs asserts one Exchanger counts
// the requests that reached the transport, draws request IDs from a single
// monotonic sequence across calls through one control handler, and never
// counts an emit that failed.
func TestExchanger_CountsRequestsAndSequencesIDs(t *testing.T) {
	mt := &mockTransport{
		respond: func(requestID string) (types.ControlEvent, bool) {
			return types.ControlEvent{Type: "sandbox_token_response", RequestID: requestID, Token: "tok"}, true
		},
	}
	ex := NewExchanger(mt)
	if got := ex.Requests(); got != 0 {
		t.Fatalf("Requests() before any exchange = %d, want 0", got)
	}

	for i := 1; i <= 3; i++ {
		if _, err := ex.Exchange(context.Background(), "aud", time.Second); err != nil {
			t.Fatalf("Exchange() %d error: %v", i, err)
		}
		if got := ex.Requests(); got != i {
			t.Errorf("Requests() after %d exchanges = %d", i, got)
		}
	}

	mt.mu.Lock()
	ids := make([]string, 0, len(mt.emitted))
	for _, e := range mt.emitted {
		ids = append(ids, e.RequestID)
	}
	handlers := len(mt.handlers)
	mt.emitErr = errors.New("stream closed")
	mt.mu.Unlock()

	want := []string{"sbid-1", "sbid-2", "sbid-3"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("request IDs = %v, want %v", ids, want)
	}
	if handlers != 1 {
		t.Errorf("Exchanger attached %d control handlers, want 1", handlers)
	}

	if _, err := ex.Exchange(context.Background(), "aud", time.Second); err == nil {
		t.Fatal("expected an error when emit fails")
	}
	if got := ex.Requests(); got != 3 {
		t.Errorf("Requests() after a failed emit = %d, want 3 (a request that never left is not counted)", got)
	}
}
