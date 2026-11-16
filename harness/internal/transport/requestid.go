package transport

import (
	"fmt"
	"sync/atomic"
)

// requestSeq numbers every correlated request the process issues, across
// all prefixes, so one response can never match two pending requests on
// a transport that several subsystems attach handlers to.
var requestSeq atomic.Uint64

// NewRequestID returns "<prefix>-<n>", where n is drawn from a single
// process-wide increasing sequence. The ID is unique for the life of the
// process; the prefix only names the issuing subsystem for readers.
func NewRequestID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, requestSeq.Add(1))
}
