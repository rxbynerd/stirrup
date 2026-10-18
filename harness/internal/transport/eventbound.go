package transport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/rxbynerd/stirrup/types"
)

// maxEventFieldBytes bounds tool_result content, tool_call input,
// user_message text and thinking_delta text on the gRPC stream. It sits
// well under grpc-go's 4 MiB default receive limit, which a control plane
// enforces by aborting the whole stream.
const maxEventFieldBytes = 256 << 10

const eventTruncationFmt = "\n[truncated by harness for the control stream: %d of %d bytes shown]"

// boundEventFields caps the event fields that carry tool payloads,
// appended user text and reasoning. The model's view is bounded
// separately by tools.maxResultBytes; this cap protects the control
// stream when that budget is raised, a tool input is large, a
// user_message carries a long prompt, or a provider delivers a whole
// reasoning block as one thinking_delta. text_delta stays uncapped.
func boundEventFields(event *types.HarnessEvent) {
	switch event.Type {
	case "tool_result":
		event.Content = capEventText(event.Content)
	case "user_message", "thinking_delta":
		event.Text = capEventText(event.Text)
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
// plane. Escaping a JSON document doubles its quotes and backslashes and
// can expand control characters six-fold, so a smaller head is tried when
// the first does not fit, and the marker alone is the last resort.
func capEventInput(input json.RawMessage) json.RawMessage {
	if len(input) <= maxEventFieldBytes {
		return input
	}
	var encoded []byte
	for _, n := range []int{maxEventFieldBytes / 2, maxEventFieldBytes / 4, 0} {
		head := truncateUTF8(string(input), n)
		encoded = encodeJSONString(head + fmt.Sprintf(eventTruncationFmt, len(head), len(input)))
		if len(encoded) <= maxEventFieldBytes {
			break
		}
	}
	return encoded
}

// encodeJSONString encodes s as a JSON string without HTML escaping, which
// would otherwise expand every '<', '>' and '&' six-fold.
func encodeJSONString(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string value always encodes
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
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
