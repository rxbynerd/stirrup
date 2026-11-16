package sandboxidentity

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// TestExchanger_CountsRequestsAndSequencesIDs asserts one Exchanger counts
// the requests that reached the transport, issues increasing sbid- request
// IDs across calls through one control handler, and never
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

	must.Len(ids, 3, "request IDs")
	prev := uint64(0)
	for _, id := range ids {
		n, ok := strings.CutPrefix(id, "sbid-")
		must.True(ok, "request ID %q lacks the sbid- prefix", id)
		seq, err := strconv.ParseUint(n, 10, 64)
		must.NoError(err, "request ID %q", id)
		check.Greater(seq, prev, "request IDs increase in issue order")
		prev = seq
	}
	check.Equal(1, handlers, "control handlers attached by the exchanger")

	_, err := ex.Exchange(context.Background(), "aud", time.Second)
	must.Error(err, "expected an error when emit fails")
	must.Equal(3, ex.Requests(), "a request that never left is not counted")
}
