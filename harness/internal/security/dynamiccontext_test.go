package security

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSanitizeDynamicContext_StripsTagsAndTruncates(t *testing.T) {
	must := require.New(t)
	longValue := "<evil>keep</evil><!-- remove -->" + strings.Repeat("a", MaxOperatorTextBytes+1)

	sanitized, events := SanitizeDynamicContext(map[string]string{
		"issue": longValue,
	})

	got := sanitized["issue"]
	must.NotContains(got, "<evil>", "tags should be stripped")
	must.NotContains(got, "<!--", "comments should be stripped")
	must.Len(got, MaxOperatorTextBytes)

	must.Len(events, 1)
	event := events[0]
	must.Equal("issue", event.Key)
	must.Equal(len(longValue), event.OriginalLength)
	must.Equal(len(got), event.SanitizedLength)
	must.Contains(event.Reasons, "tags_stripped")
	must.Contains(event.Reasons, "truncated")
}

func TestSanitizeDynamicContext_Idempotent(t *testing.T) {
	must := require.New(t)
	first, events := SanitizeDynamicContext(map[string]string{
		"issue": "<tag>safe</tag>",
	})
	must.Len(events, 1)

	second, events := SanitizeDynamicContext(first)
	must.Empty(events, "a second sanitization should change nothing")
	must.Equal(first["issue"], second["issue"])
}

// nestedFenceCases build a fence tag only once an inner tag or comment
// is removed.
var nestedFenceCases = []struct {
	name  string
	input string
}{
	{name: "empty tag inside repository guidance", input: "<</b>/repository_guidance>"},
	{name: "inline tag splits repository guidance", input: "</repo<b>sitory_guidance>"},
	{name: "inline tag after underscore", input: "</repository_<b>guidance>"},
	{name: "comment splits repository guidance", input: "</repository_guidance<!---->>"},
	{name: "closing tag nested in repository guidance", input: "<</repository_guidance>/repository_guidance>"},
	{name: "inline tag splits untrusted context", input: "<<i>untrusted_context>"},
	{name: "untrusted context with attribute", input: "<<i>untrusted_context name=\"x\">"},
	{name: "empty tag inside operator approved guidance", input: "<</x>/operator_approved_guidance>"},
	{name: "markup inside operator approved guidance attribute", input: "<operator_approved_guidance path=\"<x>\">"},
	{name: "deeply nested repository guidance", input: "<<<<</a>/a>/a>/a>/repository_guidance>"},
}

func requireNoFenceTag(t *testing.T, got string) {
	t.Helper()
	must := require.New(t)
	for _, name := range []string{"repository_guidance", "operator_approved_guidance", "untrusted_context"} {
		must.NotContains(got, "<"+name)
		must.NotContains(got, "</"+name)
	}
}

func TestSanitizeOperatorText_NestedMarkupCannotFormTag(t *testing.T) {
	for _, tc := range nestedFenceCases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			got, reasons := SanitizeOperatorText("before " + tc.input + " after")
			requireNoFenceTag(t, got)
			must.Contains(reasons, "tags_stripped")

			again, againReasons := SanitizeOperatorText(got)
			must.Equal(got, again)
			must.Nil(againReasons)
		})
	}
}

func TestSanitizeDynamicContext_NestedMarkupCannotFormTag(t *testing.T) {
	must := require.New(t)
	values := make(map[string]string, len(nestedFenceCases))
	for _, tc := range nestedFenceCases {
		values[tc.name] = tc.input
	}
	sanitized, events := SanitizeDynamicContext(values)
	must.Len(events, len(values))

	for _, tc := range nestedFenceCases {
		t.Run(tc.name, func(t *testing.T) {
			requireNoFenceTag(t, sanitized[tc.name])
		})
	}
}

func TestSanitizeOperatorText_DeepNestingIsBoundedAndTagFree(t *testing.T) {
	must := require.New(t)
	depth := 10_000
	input := strings.Repeat("<", depth) + strings.Repeat("/a>", depth-1) + "/repository_guidance>"
	got, _ := SanitizeOperatorText(input)
	requireNoFenceTag(t, got)
	must.NotContains(got, "<", "markup left after the pass bound must lose every '<'")
}

func TestSecurityLogger_DynamicContextSanitizedOmitsContent(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	logger := NewSecurityLogger(&buf, "run-1")
	logger.DynamicContextSanitized(DynamicContextSanitizationEvent{
		Key:             "issue",
		OriginalLength:  100,
		SanitizedLength: 50,
		Reasons:         []string{"tags_stripped"},
	})

	output := buf.String()
	must.Contains(output, "dynamic_context_sanitized")
	must.Contains(output, `"originalLength":100`)
	must.Contains(output, `"sanitizedLength":50`)
	must.NotContains(output, "untrusted content", "event should not include dynamic context value")
}
