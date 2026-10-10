package provider

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rxbynerd/stirrup/types"
)

// toolCallEvent builds the "tool_call" event for a call whose arguments
// arrived as JSON text. Empty text is a zero-argument call. Text that does
// not decode to a JSON object yields a marked event rather than a stream
// error, so the loop can return the parse error to the model as a tool
// result instead of ending the run.
func toolCallEvent(id, name, raw string) types.StreamEvent {
	ev := types.StreamEvent{Type: "tool_call", ID: id, Name: name}
	if raw == "" {
		return ev
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		ev.InvalidInput = &types.InvalidToolInput{
			Raw:   raw,
			Error: describeToolArgsError(err),
		}
		return ev
	}
	ev.Input = input
	return ev
}

// describeToolArgsError adds the byte offset to a syntax error, which the
// decoder's message omits and the model needs to locate its mistake.
func describeToolArgsError(err error) string {
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return fmt.Sprintf("%s (at byte offset %d)", syntaxErr.Error(), syntaxErr.Offset)
	}
	return err.Error()
}
