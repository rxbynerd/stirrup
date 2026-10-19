package sandboxidentity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	require.Equal(t, 0, ex.Requests(), "requests before any exchange")

	for i := 1; i <= 3; i++ {
		_, err := ex.Exchange(context.Background(), "aud", time.Second)
		require.NoError(t, err, "exchange %d", i)
		require.Equal(t, i, ex.Requests(), "requests after exchange %d", i)
	}

	mt.mu.Lock()
	ids := make([]string, 0, len(mt.emitted))
	for _, e := range mt.emitted {
		ids = append(ids, e.RequestID)
	}
	handlers := len(mt.handlers)
	mt.emitErr = errors.New("stream closed")
	mt.mu.Unlock()

	assert.Equal(t, []string{"sbid-1", "sbid-2", "sbid-3"}, ids, "request IDs")
	assert.Equal(t, 1, handlers, "control handlers attached by the exchanger")

	_, err := ex.Exchange(context.Background(), "aud", time.Second)
	require.Error(t, err, "expected an error when emit fails")
	require.Equal(t, 3, ex.Requests(), "a request that never left is not counted")
}
