package provider

import (
	"encoding/json"
	"fmt"
)

// geminiSchemaLintError carries a tool name plus the field path of the
// rejected keyword. The message names the tool, keyword, and path only —
// never the schema's description or enum content — so a fail-closed log
// line cannot leak operator-supplied prose at error level.
type geminiSchemaLintError struct {
	tool    string
	path    string
	keyword string
}

func (e *geminiSchemaLintError) Error() string {
	loc := e.path
	if loc == "" {
		loc = "<root>"
	}
	if e.tool == "" {
		return fmt.Sprintf("gemini schema lint: keyword %q at %s is not supported by the resolved Gemini model", e.keyword, loc)
	}
	return fmt.Sprintf("gemini schema lint: tool %q schema at %s uses keyword %q, which is not supported by the resolved Gemini model", e.tool, loc, e.keyword)
}

// LintGeminiSchema reports the first JSON Schema keyword in `in` that
// matches any entry in `unsupported`, walking the document recursively
// through `properties` and `items`. Returns nil when the schema uses only
// keywords the resolved Gemini model accepts.
//
// Callers must only pass schemas originating from first-party tool
// registrations or the structured MCP import path — the recursion has no
// depth cap because that surface is bounded by design; see
// docs/provider-quirks.md before exposing operator-authored schemas here.
//
// The lint runs BEFORE ConvertSchema so the operator sees the policy
// rejection rather than ConvertSchema's structural rewrite reasons for the
// same shape. Matching is keyword-presence at any nesting depth — the
// value is never inspected, only the key.
func LintGeminiSchema(toolName string, in json.RawMessage, unsupported []string) error {
	if len(unsupported) == 0 || len(in) == 0 {
		return nil
	}
	unsupportedSet := make(map[string]struct{}, len(unsupported))
	for _, k := range unsupported {
		unsupportedSet[k] = struct{}{}
	}
	var node any
	if err := json.Unmarshal(in, &node); err != nil {
		// A schema that does not parse will fail in ConvertSchema with
		// the parse error; do not double-report from the linter.
		return nil
	}
	return walkLintNode(toolName, node, "", unsupportedSet)
}

// walkLintNode recurses into a schema node looking for any forbidden
// keyword. Returns the first hit's path so the operator can locate
// the offending field without scrolling through repeated rejections.
func walkLintNode(toolName string, node any, path string, unsupported map[string]struct{}) error {
	obj, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	for k := range obj {
		if _, forbidden := unsupported[k]; forbidden {
			return &geminiSchemaLintError{
				tool:    toolName,
				path:    path,
				keyword: k,
			}
		}
	}
	// Descend into `properties` (each child a schema) and `items` (one
	// schema, or an array for tuple-style validation — handle both so a
	// tuple's forbidden keyword isn't missed).
	if rawProps, has := obj["properties"]; has {
		if props, ok := rawProps.(map[string]any); ok {
			for k, v := range props {
				if err := walkLintNode(toolName, v, joinPath(path, "properties."+k), unsupported); err != nil {
					return err
				}
			}
		}
	}
	if rawItems, has := obj["items"]; has {
		switch items := rawItems.(type) {
		case map[string]any:
			if err := walkLintNode(toolName, items, joinPath(path, "items"), unsupported); err != nil {
				return err
			}
		case []any:
			for i, v := range items {
				if err := walkLintNode(toolName, v, joinPath(path, fmt.Sprintf("items[%d]", i)), unsupported); err != nil {
					return err
				}
			}
		}
	}
	// oneOf/anyOf branches are not part of the Gemini-supported
	// surface; if a quirks rule lists "oneOf" the top-level check
	// above already caught it, so we do not descend into them here.
	// The same goes for $defs / definitions — they are dropped by
	// ConvertSchema and a stricter lint rule listing them catches
	// the presence.
	return nil
}
