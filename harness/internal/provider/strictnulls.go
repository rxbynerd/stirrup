package provider

import (
	"encoding/json"

	"github.com/rxbynerd/stirrup/types"
)

// strictNullables records, per tool sent with strict:true, the top-level
// properties that were optional and not nullable in the tool's original
// schema. Strict-mode normalisation makes each of them required and
// nullable, so the model sends an explicit null to omit one. The harness
// validates the call against the original schema, which has no null for
// those properties, so the adapter drops the synthetic nulls first.
type strictNullables map[string]map[string]struct{}

// optionalNonNullable returns the top-level properties of a tool's original
// schema that are optional and do not admit null, or nil when there are
// none. A schema that does not parse yields nil: strict normalisation
// would already have rejected it.
func optionalNonNullable(raw json.RawMessage) map[string]struct{} {
	var schema struct {
		Properties map[string]struct {
			Type json.RawMessage `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil
	}
	required := make(map[string]struct{}, len(schema.Required))
	for _, name := range schema.Required {
		required[name] = struct{}{}
	}
	var out map[string]struct{}
	for name, prop := range schema.Properties {
		if _, ok := required[name]; ok || typeAdmitsNull(prop.Type) {
			continue
		}
		if out == nil {
			out = map[string]struct{}{}
		}
		out[name] = struct{}{}
	}
	return out
}

// with records a strict tool's nullable property set, which may be shared
// with the strict schema cache and is never modified.
func (n strictNullables) with(tool string, nullable map[string]struct{}) strictNullables {
	if len(nullable) == 0 {
		return n
	}
	if n == nil {
		n = strictNullables{}
	}
	n[tool] = nullable
	return n
}

// typeAdmitsNull reports whether a JSON Schema "type" value is "null" or
// an array containing "null".
func typeAdmitsNull(raw json.RawMessage) bool {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single == "null"
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return false
	}
	for _, s := range list {
		if s == "null" {
			return true
		}
	}
	return false
}

// strip deletes, from a tool_call event's input, each synthetic null the
// strict-mode normaliser introduced for that tool. Other events, explicit
// nulls on required properties, and nulls the original schema permits are
// left untouched.
func (n strictNullables) strip(ev types.StreamEvent) types.StreamEvent {
	if ev.Type != "tool_call" {
		return ev
	}
	n.dropFrom(ev.Name, ev.Input)
	return ev
}

// dropFrom deletes from args each synthetic null strict mode introduced
// for the named tool.
func (n strictNullables) dropFrom(tool string, args map[string]any) {
	for name := range n[tool] {
		if v, ok := args[name]; ok && v == nil {
			delete(args, name)
		}
	}
}
