package quirks

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestParseReplayPath_Valid covers the legal segment shapes: a single
// key, a dotted chain, a key followed by `[]`, and a chain mixing both.
func TestParseReplayPath_Valid(t *testing.T) {
	cases := []struct {
		name string
		path string
		want []ReplayPathSegment
	}{
		{
			name: "single key",
			path: "reasoning_content",
			want: []ReplayPathSegment{{Key: "reasoning_content"}},
		},
		{
			name: "dotted chain",
			path: "delta.reasoning_content",
			want: []ReplayPathSegment{{Key: "delta"}, {Key: "reasoning_content"}},
		},
		{
			name: "array iteration in the middle",
			path: "candidates[].content.parts[].functionCall.thoughtSignature",
			want: []ReplayPathSegment{
				{Key: "candidates", IsArray: true},
				{Key: "content"},
				{Key: "parts", IsArray: true},
				{Key: "functionCall"},
				{Key: "thoughtSignature"},
			},
		},
		{
			name: "underscore-only key",
			path: "_internal_field",
			want: []ReplayPathSegment{{Key: "_internal_field"}},
		},
		{
			name: "digits allowed after first char",
			path: "field1.subfield2",
			want: []ReplayPathSegment{{Key: "field1"}, {Key: "subfield2"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseReplayPath(tc.path)
			if err != nil {
				t.Fatalf("ParseReplayPath(%q): %v", tc.path, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseReplayPath(%q) = %+v, want %+v", tc.path, got, tc.want)
			}
		})
	}
}

// TestParseReplayPath_Invalid pins every error arm.
func TestParseReplayPath_Invalid(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"empty path", ""},
		{"empty segment in the middle", "a..b"},
		{"empty leading segment", ".a"},
		{"empty trailing segment", "a."},
		{"trailing array iteration", "candidates[]"},
		{"leading digit in key", "1field"},
		{"hyphen in key", "field-name"},
		{"space in key", "field name"},
		{"dot in key (would need quoting)", ""},
		{"bare brackets", "[]"},
		{"key with just brackets after a dot", "a.[]"},
	}
	for _, tc := range cases {
		if tc.path == "" && tc.name != "empty path" {

			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseReplayPath(tc.path)
			if err == nil {
				t.Errorf("ParseReplayPath(%q): expected error, got nil", tc.path)
			}
		})
	}
}

// TestValidateReplayPath_DelegatesToParse confirms ValidateReplayPath
// is a thin wrapper around ParseReplayPath.
func TestValidateReplayPath_DelegatesToParse(t *testing.T) {
	if err := ValidateReplayPath("a.b.c"); err != nil {
		t.Errorf("expected nil error for valid path: %v", err)
	}
	if err := ValidateReplayPath("a..b"); err == nil {
		t.Error("expected error for invalid path")
	}
}

// TestCaptureReplayFields_GeminiToolCall pins the Gemini 3.x
// ReplayFields rule against a fixture-shaped document: one candidate,
// one part, a functionCall with a thoughtSignature.
func TestCaptureReplayFields_GeminiToolCall(t *testing.T) {
	doc := map[string]any{
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"role": "model",
					"parts": []any{
						map[string]any{
							"functionCall": map[string]any{
								"name": "read_file",
								"args": map[string]any{"path": "main.go"},
							},
							"thoughtSignature": "AY89a18t+D98lADcFYKgjMgoHS7rOPAQUE==",
						},
					},
				},
				"finishReason": "STOP",
				"index":        float64(0),
			},
		},
	}
	// thoughtSignature is a sibling of functionCall, not a child of it.
	out := CaptureReplayFields(doc, []string{
		"candidates[].content.parts[].functionCall.thoughtSignature",
	})
	if len(out) != 0 {
		t.Errorf("path through functionCall must NOT capture sibling thoughtSignature; got %v", out)
	}

	out = CaptureReplayFields(doc, []string{
		"candidates[].content.parts[].thoughtSignature",
	})
	values, ok := out["candidates[].content.parts[].thoughtSignature"]
	if !ok {
		t.Fatalf("expected captured thoughtSignature; got map %v", out)
	}
	if len(values) != 1 {
		t.Fatalf("expected exactly 1 captured value, got %d: %v", len(values), values)
	}
	if got, ok := values[0].(string); !ok || got != "AY89a18t+D98lADcFYKgjMgoHS7rOPAQUE==" {
		t.Errorf("captured value = %v (%T), want the thoughtSignature string", values[0], values[0])
	}
}

// TestCaptureReplayFields_DeepSeekReasoningContent pins the path the
// DeepSeek rules use: reasoning_content is a sibling of content on the
// assistant delta, captured by a single-segment path.
func TestCaptureReplayFields_DeepSeekReasoningContent(t *testing.T) {
	delta := map[string]any{
		"role":              "assistant",
		"content":           "The answer is 42.",
		"reasoning_content": "Step 1: identified the question. Step 2: computed the answer.",
	}
	out := CaptureReplayFields(delta, []string{"reasoning_content"})
	values, ok := out["reasoning_content"]
	if !ok {
		t.Fatalf("expected reasoning_content captured; got %v", out)
	}
	if len(values) != 1 {
		t.Fatalf("expected exactly 1 value, got %d", len(values))
	}
	if got, ok := values[0].(string); !ok || got == "" {
		t.Errorf("captured value = %v, want non-empty string", values[0])
	}
}

// TestCaptureReplayFields_MissingPathIsNoOp confirms a path that does
// not resolve returns nothing rather than an error.
func TestCaptureReplayFields_MissingPathIsNoOp(t *testing.T) {
	doc := map[string]any{"a": map[string]any{"b": "value"}}
	out := CaptureReplayFields(doc, []string{"missing.path"})
	if len(out) != 0 {
		t.Errorf("expected empty map for missing path, got %v", out)
	}
}

// TestCaptureReplayFields_PartialResolveCapturesWhatItCan asserts the
// walker captures from paths that resolve and silently skips those
// that don't, in a multi-path call.
func TestCaptureReplayFields_PartialResolveCapturesWhatItCan(t *testing.T) {
	doc := map[string]any{"reasoning_content": "thinking..."}
	out := CaptureReplayFields(doc, []string{
		"reasoning_content",
		"candidates[].content.parts[].thoughtSignature",
	})
	if len(out) != 1 {
		t.Fatalf("expected exactly one path captured, got %d: %v", len(out), out)
	}
	if _, ok := out["reasoning_content"]; !ok {
		t.Errorf("expected reasoning_content captured; got %v", out)
	}
}

// TestCaptureReplayFields_MalformedPathSilentlyDropped pins the
// runtime safety property: a malformed path that reaches the walker
// returns nothing rather than panicking.
func TestCaptureReplayFields_MalformedPathSilentlyDropped(t *testing.T) {
	doc := map[string]any{"reasoning_content": "thinking..."}
	out := CaptureReplayFields(doc, []string{"a..b", "reasoning_content"})
	if len(out) != 1 {
		t.Errorf("expected only the valid path to capture, got %v", out)
	}
}

// TestCaptureReplayFields_EmptyDocOrPaths handles the two no-op
// inputs; both must return nil.
func TestCaptureReplayFields_EmptyDocOrPaths(t *testing.T) {
	if got := CaptureReplayFields(nil, []string{"a"}); got != nil {
		t.Errorf("nil doc: got %v, want nil", got)
	}
	if got := CaptureReplayFields(map[string]any{"a": "b"}, nil); got != nil {
		t.Errorf("empty paths: got %v, want nil", got)
	}
}

// TestCaptureFromJSON_HappyPath pins the JSON-bytes convenience wrapper.
func TestCaptureFromJSON_HappyPath(t *testing.T) {
	raw := json.RawMessage(`{"role":"assistant","reasoning_content":"thinking"}`)
	out := CaptureFromJSON(raw, []string{"reasoning_content"})
	if len(out) != 1 {
		t.Fatalf("expected one captured path, got %v", out)
	}
	values := out["reasoning_content"]
	if len(values) != 1 || values[0] != "thinking" {
		t.Errorf("captured = %v, want [thinking]", values)
	}
}

// TestCaptureFromJSON_MalformedJSONIsNoOp confirms a decode failure
// is silent; the SSE parser is the authoritative reporter for chunk
// decode failures.
func TestCaptureFromJSON_MalformedJSONIsNoOp(t *testing.T) {
	if got := CaptureFromJSON(json.RawMessage(`{not json`), []string{"a"}); got != nil {
		t.Errorf("expected nil for malformed JSON, got %v", got)
	}
}

// TestBuiltinRulesValidate_ReplayFieldsPathsAreSyntacticallyValid walks
// every builtin rule, materialises its effect on a fresh ProviderQuirks,
// and asserts every entry in the resulting ReplayFields slice parses
// cleanly.
func TestBuiltinRulesValidate_ReplayFieldsPathsAreSyntacticallyValid(t *testing.T) {
	for i, rule := range BuiltinRules() {
		if rule.Apply == nil {
			continue
		}
		q := ProviderQuirks{
			FieldRenames:   map[string]string{},
			OmitFields:     []string{},
			ValueOverrides: map[string]Value{},
			EnumCoercions:  map[string]map[string]string{},
			ReplayFields:   []string{},
			BehaviourFlags: ProviderBehaviourFlags{OpenAI: OpenAIBehaviourFlags{ExtraBodyFields: map[string]any{}}},
		}
		rule.Apply(&q)
		for _, path := range q.ReplayFields {
			if err := ValidateReplayPath(path); err != nil {
				t.Errorf("BuiltinRules()[%d] (%q): ReplayFields path %q is invalid: %v", i, rule.Description, path, err)
			}
		}
	}
}

// canonicalOpenAIMessageFieldNames mirrors openai.go's unexported
// canonicalOpenAIMessageFields (the registry must not import the
// provider package). Add a new canonical message key here too.
var canonicalOpenAIMessageFieldNames = map[string]bool{
	"role":         true,
	"content":      true,
	"tool_calls":   true,
	"tool_call_id": true,
	"name":         true,
}

// TestBuiltinRulesReplayFieldsSuffix pins the observability convention
// documented in docs/provider-quirks.md §3.1: a rule that registers a
// ReplayFields path must end its Description with "(threaded)" or
// "(parse-side only)", and openai-compatible threaded paths must be
// single-segment, non-array, and not a canonical wire-message key.
func TestBuiltinRulesReplayFieldsSuffix(t *testing.T) {
	const threadedSuffix = "(threaded)"
	const parseSideSuffix = "(parse-side only)"
	for i, rule := range BuiltinRules() {
		if rule.Apply == nil {
			continue
		}
		q := ProviderQuirks{
			FieldRenames:   map[string]string{},
			OmitFields:     []string{},
			ValueOverrides: map[string]Value{},
			EnumCoercions:  map[string]map[string]string{},
			ReplayFields:   []string{},
			BehaviourFlags: ProviderBehaviourFlags{OpenAI: OpenAIBehaviourFlags{ExtraBodyFields: map[string]any{}}},
		}
		rule.Apply(&q)
		if len(q.ReplayFields) == 0 {
			continue
		}
		switch rule.ProviderType {
		case "openai-compatible":
			if !strings.HasSuffix(rule.Description, threadedSuffix) {
				t.Errorf("BuiltinRules()[%d] (%q): openai-compatible ReplayFields rule description must end with %q (the adapter threads its captures outbound)", i, rule.Description, threadedSuffix)
			}
			for _, path := range q.ReplayFields {
				segments, err := ParseReplayPath(path)
				if err != nil {

					continue
				}
				if len(segments) != 1 || segments[0].IsArray {
					t.Errorf("BuiltinRules()[%d] (%q): ReplayFields path %q is not threadable (must be a single non-array segment for outbound emission)", i, rule.Description, path)
				}
				if canonicalOpenAIMessageFieldNames[path] {
					t.Errorf("BuiltinRules()[%d] (%q): ReplayFields path %q collides with a canonical openai message key", i, rule.Description, path)
				}
			}
		default:
			if !strings.HasSuffix(rule.Description, parseSideSuffix) {
				t.Errorf("BuiltinRules()[%d] (%q): ReplayFields rule description must end with %q (no outbound threading exists for provider %q)", i, rule.Description, parseSideSuffix, rule.ProviderType)
			}
			if strings.HasSuffix(rule.Description, threadedSuffix) {
				t.Errorf("BuiltinRules()[%d] (%q): provider %q has no outbound threading; the description must not claim %q", i, rule.Description, rule.ProviderType, threadedSuffix)
			}
		}
	}
}

// TestSortedReplayPathKeys pins that consumers must sort.Strings the
// captured key set themselves — the walker does not sort (Go maps are
// unordered).
func TestSortedReplayPathKeys(t *testing.T) {
	doc := map[string]any{
		"reasoning_content": "thinking",
		"other_field":       "value",
	}
	out := CaptureReplayFields(doc, []string{"reasoning_content", "other_field"})
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) != 2 || keys[0] != "other_field" || keys[1] != "reasoning_content" {
		t.Errorf("sorted keys = %v, want [other_field reasoning_content]", keys)
	}
}
