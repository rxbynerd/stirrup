package quirks

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestParseReplayPath_Valid covers the four legal segment shapes:
// a single key, a dotted chain, a key followed by `[]` to iterate an
// array of objects, and a chain that mixes both. The expected segments
// list is checked by reflect.DeepEqual so a future widening of the
// segment struct (e.g. an explicit index for the unsupported [N]
// form) fails this test rather than silently passing.
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

// TestParseReplayPath_Invalid pins every error arm. Each case fails
// because a typo or unsupported construct would otherwise silently
// capture nothing at runtime — better to fail at registry build time.
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
			// "dot in key (would need quoting)" — empty placeholder
			// to document the absent feature; skip the actual assertion.
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
// is a thin wrapper. The build-time validator relies on this
// equivalence — a rule that survives ValidateReplayPath must also
// parse cleanly at runtime.
func TestValidateReplayPath_DelegatesToParse(t *testing.T) {
	if err := ValidateReplayPath("a.b.c"); err != nil {
		t.Errorf("expected nil error for valid path: %v", err)
	}
	if err := ValidateReplayPath("a..b"); err == nil {
		t.Error("expected error for invalid path")
	}
}

// TestCaptureReplayFields_GeminiToolCall is the load-bearing fixture
// for the Gemini 3.x ReplayFields rule. The shape mirrors the
// gemini-3.1-pro-preview/response.sse fixture: one candidate, one
// part, a functionCall with a thoughtSignature. The path that the
// rule registers must surface the captured value here so a rule
// regression fails this test rather than only being caught at
// fixture-comparison time.
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
	// thoughtSignature lives ON the part, alongside functionCall — it
	// is NOT a child of functionCall. A rule that drills through
	// functionCall.thoughtSignature would silently capture nothing.
	// This negative assertion guards against a rule author writing
	// such a path and assuming it works without running the test.
	out := CaptureReplayFields(doc, []string{
		"candidates[].content.parts[].functionCall.thoughtSignature",
	})
	if len(out) != 0 {
		t.Errorf("path through functionCall must NOT capture sibling thoughtSignature; got %v", out)
	}

	// The correct path for Gemini 3.x's wire shape descends to the
	// part, then reads the sibling field.
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
// DeepSeek rules use. DeepSeek (and OpenAI's prior reasoning preview
// builds) carry reasoning_content as a sibling of content on the
// assistant delta — a single-segment path against the decoded delta
// object captures it directly.
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
// not resolve returns nothing rather than an error. This is the
// expected behaviour for forward-compatible rules: a path that names
// a field the response did not emit must not surface as a captured
// empty value, because that would be indistinguishable from a real
// empty-string field.
func TestCaptureReplayFields_MissingPathIsNoOp(t *testing.T) {
	doc := map[string]any{"a": map[string]any{"b": "value"}}
	out := CaptureReplayFields(doc, []string{"missing.path"})
	if len(out) != 0 {
		t.Errorf("expected empty map for missing path, got %v", out)
	}
}

// TestCaptureReplayFields_PartialResolveCapturesWhatItCan asserts the
// walker captures from paths that resolve and silently skips those
// that don't, in a multi-path call. This is the production case: a
// rule may register several alternative paths, and only one will be
// present for any given response shape.
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
// runtime safety property: a malformed path that somehow reaches the
// walker (e.g. through a non-builtin Rule injected at test time)
// returns nothing rather than panicking. The build-time validator
// (BuiltinRulesValidate) is the authoritative gate; this is defence
// in depth.
func TestCaptureReplayFields_MalformedPathSilentlyDropped(t *testing.T) {
	doc := map[string]any{"reasoning_content": "thinking..."}
	out := CaptureReplayFields(doc, []string{"a..b", "reasoning_content"})
	if len(out) != 1 {
		t.Errorf("expected only the valid path to capture, got %v", out)
	}
}

// TestCaptureReplayFields_EmptyDocOrPaths handles the two no-op
// inputs. Both must return nil so callers can branch cleanly on
// `len(captured) == 0` without inspecting the map shape.
func TestCaptureReplayFields_EmptyDocOrPaths(t *testing.T) {
	if got := CaptureReplayFields(nil, []string{"a"}); got != nil {
		t.Errorf("nil doc: got %v, want nil", got)
	}
	if got := CaptureReplayFields(map[string]any{"a": "b"}, nil); got != nil {
		t.Errorf("empty paths: got %v, want nil", got)
	}
}

// TestCaptureFromJSON_HappyPath pins the JSON-bytes convenience
// wrapper. The openai adapter holds a json.RawMessage for the delta
// object inside an SSE chunk; the wrapper decodes it once and delegates
// to CaptureReplayFields.
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
// is silent. The SSE parser is the authoritative reporter for chunk
// decode failures (it already returns an error event); this wrapper
// must not double-report.
func TestCaptureFromJSON_MalformedJSONIsNoOp(t *testing.T) {
	if got := CaptureFromJSON(json.RawMessage(`{not json`), []string{"a"}); got != nil {
		t.Errorf("expected nil for malformed JSON, got %v", got)
	}
}

// TestBuiltinRulesValidate_ReplayFieldsPathsAreSyntacticallyValid is
// the gate that catches a typo in a rule's ReplayFields entry at
// registry-build time. Walks every builtin rule, materialises its
// effect on a fresh ProviderQuirks, and asserts every entry in the
// resulting ReplayFields slice parses cleanly.
//
// This extends the existing TestBuiltinRulesValidate; the path-parse
// check sits in this file so the path parser and its validator are
// tested together, against the same shared catalogue.
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

// canonicalOpenAIMessageFieldNames mirrors the canonical Chat
// Completions message-key surface enumerated by
// harness/internal/provider/openai.go's canonicalOpenAIMessageFields.
// The duplication is intentional, following the existing
// canonicalOpenAIFieldNames precedent in quirks_test.go: the adapter's
// set is unexported and the registry must not import the provider
// package, so the test cross-checks rules against a local copy. When
// the adapter learns a new canonical message key, add it here too.
var canonicalOpenAIMessageFieldNames = map[string]bool{
	"role":         true,
	"content":      true,
	"tool_calls":   true,
	"tool_call_id": true,
	"name":         true,
}

// TestBuiltinRulesReplayFieldsSuffix pins design §9 risk 7's
// observability convention now that outbound threading exists: a rule
// that registers a ReplayFields path MUST end its Description with
// exactly one of two markers so trace consumers know whether the
// captured value is round-tripped.
//
//   - openai-compatible rules must end "(threaded)" — the adapter
//     threads their captures back onto subsequent requests — and every
//     path they declare must be threadable: single-segment, no []
//     iteration, and not a canonical wire-message key. A first-party
//     rule that declares a non-threadable path fails here at build
//     time (the adapter would silently skip it at runtime, which is
//     the wrong failure mode for first-party rules — loud at build,
//     safe at runtime).
//   - every other provider's rules must end "(parse-side only)" —
//     their adapters have no outbound threading path (Gemini's real
//     round-trip is the typed block-level ThoughtSignature), so
//     claiming "(threaded)" would be a lie in the trace.
//
// The check applies only to rules that write to ReplayFields, so
// wire-shape rules are unaffected.
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
		switch {
		case rule.ProviderType == "openai-compatible":
			if !strings.HasSuffix(rule.Description, threadedSuffix) {
				t.Errorf("BuiltinRules()[%d] (%q): openai-compatible ReplayFields rule description must end with %q (the adapter threads its captures outbound)", i, rule.Description, threadedSuffix)
			}
			for _, path := range q.ReplayFields {
				segments, err := ParseReplayPath(path)
				if err != nil {
					// TestBuiltinRulesValidate_ReplayFieldsPathsAreSyntacticallyValid
					// reports the parse failure with a better message.
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

// TestSortedReplayPathKeys is a small utility test for the trace-attribute
// rendering layer: when an operator looks at a captured replay-fields
// map in a trace, the keys should be presented in sorted order for
// deterministic comparison. The walker does not sort (Go maps are
// unordered), so consumers are expected to call sort.Strings on the
// key set themselves; this test pins that contract via a real
// invocation.
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
