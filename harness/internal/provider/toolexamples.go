package provider

import (
	"encoding/json"

	"github.com/rxbynerd/stirrup/types"
)

// mergeSchemaExamples returns schema with the tool's worked examples injected
// under the JSON-Schema `examples` keyword (issue #222). It is the shared
// fold step the OpenAI Chat, OpenAI Responses, and Anthropic adapters use when
// the resolved ToolExamples capability advertises support: `examples` is a
// standard JSON-Schema 2020-12 keyword those providers pass through to the
// model context.
//
// The rewrite is purely additive and defensive:
//   - no examples → the schema is returned untouched;
//   - a schema that is not a JSON object (or is invalid) → returned untouched,
//     so a malformed operator/MCP schema cannot turn a tool definition into a
//     hard error here (the description still carries the example);
//   - a schema that already declares `examples` → returned untouched, so an
//     operator-authored explicit examples array wins over the derived one.
//
// Callers must NOT apply this to a schema bound for OpenAI strict mode: the
// structured-outputs subset rejects the `examples` keyword, so the OpenAI
// adapters fold examples only on non-strict tools.
func mergeSchemaExamples(schema json.RawMessage, examples []json.RawMessage) (json.RawMessage, error) {
	if len(examples) == 0 {
		return schema, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(schema, &obj); err != nil {
		return schema, nil
	}
	if _, exists := obj["examples"]; exists {
		return schema, nil
	}
	arr, err := json.Marshal(examples)
	if err != nil {
		return schema, err
	}
	obj["examples"] = arr
	return json.Marshal(obj)
}

// toolInputExamples returns the worked examples carried on a tool definition's
// Presentation (issue #222), or nil when the tool carries none. It centralises
// the nil-pointer guard the adapters would otherwise repeat.
func toolInputExamples(t types.ToolDefinition) []json.RawMessage {
	if t.Presentation == nil {
		return nil
	}
	return t.Presentation.InputExamples
}
