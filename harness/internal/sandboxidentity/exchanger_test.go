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
	check := assert.New(t)
	must := require.New(t)
	mt := &mockTransport{
		respond: func(requestID string) (types.ControlEvent, bool) {
			return types.ControlEvent{Type: "sandbox_token_response", RequestID: requestID, Token: "tok"}, true
		},
	}
	ex := NewExchanger(mt)
	must.Equal(0, ex.Requests(), "requests before any exchange")

	for i := 1; i <= 3; i++ {
		_, err := ex.Exchange(context.Background(), "aud", time.Second)
		must.NoError(err, "exchange %d", i)
		must.Equal(i, ex.Requests(), "requests after exchange %d", i)
	}

	mt.mu.Lock()
	ids := make([]string, 0, len(mt.emitted))
	for _, e := range mt.emitted {
		ids = append(ids, e.RequestID)
	}
	handlers := len(mt.handlers)
	mt.emitErr = errors.New("stream closed")
	mt.mu.Unlock()

	check.Equal([]string{"sbid-1", "sbid-2", "sbid-3"}, ids, "request IDs")
	check.Equal(1, handlers, "control handlers attached by the exchanger")

	_, err := ex.Exchange(context.Background(), "aud", time.Second)
	must.Error(err, "expected an error when emit fails")
	must.Equal(3, ex.Requests(), "a request that never left is not counted")
}
