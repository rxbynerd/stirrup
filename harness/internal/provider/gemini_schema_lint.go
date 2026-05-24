package provider

import (
	"encoding/json"
	"fmt"
)

// geminiSchemaLintError carries a tool name plus the field path of the
// rejected keyword. The exported message names the tool, the keyword,
// and the path only — never the schema's description or enum content
// — so a fail-closed log line cannot leak operator-supplied prose at
// error level (#228 §5).
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
// through `properties` and `items`. Returns nil when the schema uses
// only keywords the resolved Gemini model accepts.
//
// The lint runs BEFORE ConvertSchema so the operator sees one clear
// error (the policy rejection) rather than the structural rewrite
// reasons ConvertSchema would surface for the same shape (e.g. an
// oneOf branch list). When ConvertSchema would also reject the
// keyword on a structural basis, this linter takes precedence so the
// error message names the model-scoped policy.
//
// The match is keyword-presence at any nesting depth. A keyword listed
// in `unsupported` triggers as soon as it appears as a key in any
// schema-shaped object — the linter does not inspect the value, only
// the key. This is the conservative behaviour: if a quirks rule lists
// "pattern" the linter rejects any schema with a `pattern` key, even
// inside a nested array's items.
//
// Empty `unsupported` returns nil immediately so the lint cost on the
// default Gemini path is one slice-length check.
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
	// Recurse into known schema-walking keys. Per the existing
	// ConvertSchema convention we descend into `properties` (each
	// child a schema) and `items` (one schema, or an array of
	// schemas for tuple-style validation — we handle both forms so
	// the linter does not silently miss a tuple's `pattern`).
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
