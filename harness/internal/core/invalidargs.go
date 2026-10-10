package core

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

const (
	// invalidArgsQuoteRunes bounds how much of the model's own malformed
	// arguments are quoted back to it; the parse error already locates
	// the fault.
	invalidArgsQuoteRunes = 200

	// invalidArgsTraceBytes bounds the scrubbed raw arguments kept in the
	// trace record's ErrorReason.
	invalidArgsTraceBytes = 4 * 1024

	// invalidArgsScrubWindow is how much of the raw arguments is scrubbed
	// before either cut above is taken. It is wider than both cuts so a
	// secret straddling a cut is still recognised whole.
	invalidArgsScrubWindow = 64 * 1024
)

// invalidArgsMessage is the model-facing tool result for a call whose
// arguments were not a JSON object.
func invalidArgsMessage(name string, inv *types.InvalidToolInput) string {
	quoted, cut := truncateRunes(scrubbedInvalidArgs(inv.Raw), invalidArgsQuoteRunes)
	label := "Arguments received"
	if cut {
		label = fmt.Sprintf("First %d characters of the arguments received", invalidArgsQuoteRunes)
	}
	return fmt.Sprintf("Your arguments for `%s` were not valid JSON: `%s`. Re-issue the call with valid JSON.\n%s: %s",
		name, security.Scrub(inv.Error), label, quoted)
}

// invalidArgsTraceReason is the ErrorReason recorded for a call whose
// arguments were not a JSON object. It is the only place the raw
// arguments reach the trace, and only in scrubbed, bounded form.
func invalidArgsTraceReason(name string, inv *types.InvalidToolInput) string {
	raw, cut := truncateBytes(scrubbedInvalidArgs(inv.Raw), invalidArgsTraceBytes)
	if cut {
		raw += asyncResultTruncationSuffix
	}
	return fmt.Sprintf("arguments for %s were not valid JSON: %s; raw arguments (%d bytes): %s",
		name, security.Scrub(inv.Error), len(inv.Raw), raw)
}

// invalidArgsWarning is the transport warning emitted when a streamed tool
// call arrives with arguments that are not a JSON object.
func invalidArgsWarning(id, name string, inv *types.InvalidToolInput) string {
	return fmt.Sprintf("tool call %s (%s): arguments were not valid JSON and the call is returned to the model as an error: %s",
		id, name, security.Scrub(inv.Error))
}

// stallInput is the input the stall detector compares for call. A marked
// call's Input is always the empty object, so its raw arguments are used
// instead; raw text that failed to decode can never equal a valid call's
// input bytes.
func stallInput(call types.ToolCall) json.RawMessage {
	if call.InvalidInput != nil {
		return json.RawMessage(call.InvalidInput.Raw)
	}
	return call.Input
}

// toolInputSize is the byte size of the arguments the model sent for call.
func toolInputSize(call types.ToolCall) int {
	if call.InvalidInput != nil {
		return len(call.InvalidInput.Raw)
	}
	return len(call.Input)
}

func scrubbedInvalidArgs(raw string) string {
	window, _ := truncateBytes(raw, invalidArgsScrubWindow)
	return security.Scrub(window)
}

// truncateRunes returns at most n runes of s and whether s was cut.
func truncateRunes(s string, n int) (string, bool) {
	count := 0
	for i := range s {
		if count == n {
			return s[:i], true
		}
		count++
	}
	return s, false
}

// truncateBytes returns at most n bytes of s, backing off to a rune
// boundary, and whether s was cut.
func truncateBytes(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	end := n
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end], true
}
