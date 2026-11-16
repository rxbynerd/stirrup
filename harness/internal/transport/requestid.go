package transport

import (
	"fmt"
	"sync/atomic"
)

// requestSeq numbers every correlated request the process issues. One
// sequence shared by every prefix keeps request IDs unique on a
// transport however many subsystems attach their own response handler
// to it: each handler fans in every control event of its type, so two
// pending requests sharing an ID would both resolve on one response.
var requestSeq atomic.Uint64

// NewRequestID returns "<prefix>-<n>", where n is drawn from a single
// process-wide increasing sequence. The ID is unique for the life of the
// process; the prefix only names the issuing subsystem for readers.
func NewRequestID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, requestSeq.Add(1))
}
