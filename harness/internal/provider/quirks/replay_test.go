package quirks

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
			must := require.New(t)
			got, err := ParseReplayPath(tc.path)
			must.NoError(err)
			must.Equal(tc.want, got)
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
		{"bare brackets", "[]"},
		{"key with just brackets after a dot", "a.[]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := ParseReplayPath(tc.path)
			must.Error(err)
		})
	}
}

// TestValidateReplayPath_DelegatesToParse confirms ValidateReplayPath
// is a thin wrapper around ParseReplayPath.
func TestValidateReplayPath_DelegatesToParse(t *testing.T) {
	t.Run("accepts a dotted chain", func(t *testing.T) {
		must := require.New(t)
		must.NoError(ValidateReplayPath("a.b.c"))
	})
	t.Run("rejects an empty segment", func(t *testing.T) {
		must := require.New(t)
		must.Error(ValidateReplayPath("a..b"))
	})
}

// TestCaptureReplayFields_GeminiToolCall pins the Gemini 3.x
// ReplayFields rule against a fixture-shaped document: one candidate,
// one part, a functionCall with a thoughtSignature.
func TestCaptureReplayFields_GeminiToolCall(t *testing.T) {
	const thoughtSignature = "AY89a18t+D98lADcFYKgjMgoHS7rOPAQUE=="
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
							"thoughtSignature": thoughtSignature,
						},
					},
				},
				"finishReason": "STOP",
				"index":        float64(0),
			},
		},
	}

	t.Run("does not capture a sibling of functionCall", func(t *testing.T) {
		must := require.New(t)
		// thoughtSignature is a sibling of functionCall, not a child of it.
		out := CaptureReplayFields(doc, []string{
			"candidates[].content.parts[].functionCall.thoughtSignature",
		})
		must.Empty(out)
	})

	t.Run("captures thoughtSignature on the part", func(t *testing.T) {
		must := require.New(t)
		const path = "candidates[].content.parts[].thoughtSignature"
		out := CaptureReplayFields(doc, []string{path})
		must.Contains(out, path)
		must.Len(out[path], 1)
		must.Equal(thoughtSignature, out[path][0])
	})
}

// TestCaptureReplayFields_DeepSeekReasoningContent pins the path the
// DeepSeek rules use: reasoning_content is a sibling of content on the
// assistant delta, captured by a single-segment path.
func TestCaptureReplayFields_DeepSeekReasoningContent(t *testing.T) {
	must := require.New(t)
	delta := map[string]any{
		"role":              "assistant",
		"content":           "The answer is 42.",
		"reasoning_content": "Step 1: identified the question. Step 2: computed the answer.",
	}
	out := CaptureReplayFields(delta, []string{"reasoning_content"})
	must.Contains(out, "reasoning_content")
	values := out["reasoning_content"]
	must.Len(values, 1)
	got, ok := values[0].(string)
	must.True(ok, "captured value has type %T, want string", values[0])
	must.NotEmpty(got)
}

// TestCaptureReplayFields_MissingPathIsNoOp confirms a path that does
// not resolve returns nothing rather than an error.
func TestCaptureReplayFields_MissingPathIsNoOp(t *testing.T) {
	must := require.New(t)
	doc := map[string]any{"a": map[string]any{"b": "value"}}
	out := CaptureReplayFields(doc, []string{"missing.path"})
	must.Empty(out)
}

// TestCaptureReplayFields_CapturesOnlyResolvablePaths asserts the
// walker captures from paths that resolve and silently skips those
// that don't, in a multi-path call.
func TestCaptureReplayFields_CapturesOnlyResolvablePaths(t *testing.T) {
	must := require.New(t)
	doc := map[string]any{"reasoning_content": "thinking..."}
	out := CaptureReplayFields(doc, []string{
		"reasoning_content",
		"candidates[].content.parts[].thoughtSignature",
	})
	must.Len(out, 1)
	must.Contains(out, "reasoning_content")
}

// TestCaptureReplayFields_MalformedPathSilentlyDropped pins the
// runtime safety property: a malformed path that reaches the walker
// returns nothing rather than panicking.
func TestCaptureReplayFields_MalformedPathSilentlyDropped(t *testing.T) {
	must := require.New(t)
	doc := map[string]any{"reasoning_content": "thinking..."}
	out := CaptureReplayFields(doc, []string{"a..b", "reasoning_content"})
	must.Len(out, 1, "expected only the valid path to capture")
}

// TestCaptureReplayFields_EmptyDocOrPaths handles the two no-op
// inputs; both must return nil.
func TestCaptureReplayFields_EmptyDocOrPaths(t *testing.T) {
	t.Run("nil doc", func(t *testing.T) {
		must := require.New(t)
		must.Nil(CaptureReplayFields(nil, []string{"a"}))
	})
	t.Run("empty paths", func(t *testing.T) {
		must := require.New(t)
		must.Nil(CaptureReplayFields(map[string]any{"a": "b"}, nil))
	})
}

// TestCaptureFromJSON_ReturnsCapturedValues pins the JSON-bytes convenience wrapper.
func TestCaptureFromJSON_ReturnsCapturedValues(t *testing.T) {
	must := require.New(t)
	raw := json.RawMessage(`{"role":"assistant","reasoning_content":"thinking"}`)
	out := CaptureFromJSON(raw, []string{"reasoning_content"})
	must.Equal(map[string][]any{"reasoning_content": {"thinking"}}, out)
}

// TestCaptureFromJSON_MalformedJSONReturnsNil confirms a decode failure
// is silent; the SSE parser is the authoritative reporter for chunk
// decode failures.
func TestCaptureFromJSON_MalformedJSONReturnsNil(t *testing.T) {
	must := require.New(t)
	must.Nil(CaptureFromJSON(json.RawMessage(`{not json`), []string{"a"}))
}

// TestBuiltinRulesValidate_ReplayFieldsPathsAreSyntacticallyValid walks
// every builtin rule, materialises its effect on a fresh ProviderQuirks,
// and asserts every entry in the resulting ReplayFields slice parses
// cleanly.
func TestBuiltinRulesValidate_ReplayFieldsPathsAreSyntacticallyValid(t *testing.T) {
	for _, rule := range BuiltinRules() {
		if rule.Apply == nil {
			continue
		}
		t.Run(rule.Description, func(t *testing.T) {
			must := require.New(t)
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
				must.NoError(ValidateReplayPath(path), "ReplayFields path %q is invalid", path)
			}
		})
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
	for _, rule := range BuiltinRules() {
		if rule.Apply == nil {
			continue
		}
		t.Run(rule.Description, func(t *testing.T) {
			must := require.New(t)
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
				return
			}
			switch rule.ProviderType {
			case "openai-compatible":
				must.True(strings.HasSuffix(rule.Description, threadedSuffix),
					"openai-compatible ReplayFields rule description must end with %q (the adapter threads its captures outbound)", threadedSuffix)
				for _, path := range q.ReplayFields {
					segments, err := ParseReplayPath(path)
					if err != nil {
						continue
					}
					must.Len(segments, 1,
						"ReplayFields path %q is not threadable (must be a single segment for outbound emission)", path)
					must.False(segments[0].IsArray,
						"ReplayFields path %q is not threadable (must be a non-array segment for outbound emission)", path)
					must.False(canonicalOpenAIMessageFieldNames[path],
						"ReplayFields path %q collides with a canonical openai message key", path)
				}
			default:
				must.True(strings.HasSuffix(rule.Description, parseSideSuffix),
					"ReplayFields rule description must end with %q (no outbound threading exists for provider %q)", parseSideSuffix, rule.ProviderType)
				must.False(strings.HasSuffix(rule.Description, threadedSuffix),
					"provider %q has no outbound threading; the description must not claim %q", rule.ProviderType, threadedSuffix)
			}
		})
	}
}

// TestCaptureReplayFields_CallerMustSortKeys pins that consumers must sort.Strings the
// captured key set themselves — the walker does not sort (Go maps are
// unordered).
func TestCaptureReplayFields_CallerMustSortKeys(t *testing.T) {
	must := require.New(t)
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
	must.Equal([]string{"other_field", "reasoning_content"}, keys)
}
