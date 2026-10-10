package transport

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/rxbynerd/stirrup/types"
)

// maxEventFieldBytes bounds tool_result content and tool_call input on the
// gRPC stream. It sits well under grpc-go's 4 MiB default receive limit,
// which a control plane enforces by aborting the whole stream.
const maxEventFieldBytes = 256 << 10

const eventTruncationFmt = "\n[truncated by harness for the control stream: %d of %d bytes shown]"

// boundEventFields caps the event fields that carry tool payloads. The
// model's view is bounded separately by tools.maxResultBytes; this cap
// protects the control stream when that budget is raised or a tool input
// is large.
func boundEventFields(event *types.HarnessEvent) {
	switch event.Type {
	case "tool_result":
		event.Content = capEventText(event.Content)
	case "tool_call":
		event.Input = capEventInput(event.Input)
	}
}

// capEventText keeps the head of s on a UTF-8 boundary, marker included,
// within maxEventFieldBytes.
func capEventText(s string) string {
	if len(s) <= maxEventFieldBytes {
		return s
	}
	longest := fmt.Sprintf(eventTruncationFmt, len(s), len(s))
	head := truncateUTF8(s, maxEventFieldBytes-len(longest))
	return head + fmt.Sprintf(eventTruncationFmt, len(head), len(s))
}

// capEventInput replaces an oversized JSON input with a JSON string holding
// its head and a marker, so the field stays valid JSON for the control
// plane. JSON escaping can expand the head up to six-fold; a head that
// still does not fit is dropped, leaving the marker alone.
func capEventInput(input json.RawMessage) json.RawMessage {
	if len(input) <= maxEventFieldBytes {
		return input
	}
	head := truncateUTF8(string(input), maxEventFieldBytes/2)
	encoded, err := json.Marshal(head + fmt.Sprintf(eventTruncationFmt, len(head), len(input)))
	if err != nil || len(encoded) > maxEventFieldBytes {
		encoded, _ = json.Marshal(fmt.Sprintf(eventTruncationFmt, 0, len(input)))
	}
	return encoded
}

func truncateUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
