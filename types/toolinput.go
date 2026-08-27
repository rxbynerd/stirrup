package types

import (
	"encoding/json"
	"strings"
)

// NormalizeToolInput returns a tool_use ContentBlock.Input value guaranteed
// to serialise as a JSON object. Absent, blank, and literal-null input all
// map to {}.
//
// A zero-argument tool call carries no arguments on the wire, which decodes
// to a nil map and re-encodes as JSON null. Providers reject null there —
// the field is specified as an object — so both the schema gate and the
// outbound replay of a stored assistant turn must see {} instead. Apply this
// at every boundary where tool input is serialised.
func NormalizeToolInput(raw json.RawMessage) json.RawMessage {
	switch strings.TrimSpace(string(raw)) {
	case "", "null":
		return json.RawMessage("{}")
	default:
		return raw
	}
}
