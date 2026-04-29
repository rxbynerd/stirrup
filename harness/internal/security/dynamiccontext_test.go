package security

import (
	"bytes"
	"strings"
	"testing"
)

func TestSanitizeDynamicContext_StripsTagsAndTruncates(t *testing.T) {
	longValue := "<evil>keep</evil><!-- remove -->" + strings.Repeat("a", maxDynamicContextValueLength+1)

	sanitized, events := SanitizeDynamicContext(map[string]string{
		"issue": longValue,
	})

	got := sanitized["issue"]
	if strings.Contains(got, "<evil>") || strings.Contains(got, "<!--") {
		t.Fatalf("expected tags/comments to be stripped, got %q", got[:40])
	}
	if len(got) != maxDynamicContextValueLength {
		t.Fatalf("expected length %d, got %d", maxDynamicContextValueLength, len(got))
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Key != "issue" {
		t.Fatalf("expected key issue, got %q", events[0].Key)
	}
	if events[0].OriginalLength != len(longValue) || events[0].SanitizedLength != len(got) {
		t.Fatalf("unexpected event lengths: %#v", events[0])
	}
	if !containsReason(events[0].Reasons, "tags_stripped") || !containsReason(events[0].Reasons, "truncated") {
		t.Fatalf("missing reasons in %#v", events[0].Reasons)
	}
}

func TestSanitizeDynamicContext_Idempotent(t *testing.T) {
	first, events := SanitizeDynamicContext(map[string]string{
		"issue": "<tag>safe</tag>",
	})
	if len(events) != 1 {
		t.Fatalf("expected first sanitization event")
	}

	second, events := SanitizeDynamicContext(first)
	if len(events) != 0 {
		t.Fatalf("expected second sanitization to be a no-op, got %#v", events)
	}
	if second["issue"] != first["issue"] {
		t.Fatalf("expected idempotent output, got %q want %q", second["issue"], first["issue"])
	}
}

func TestSecurityLogger_DynamicContextSanitizedOmitsContent(t *testing.T) {
	var buf bytes.Buffer
	logger := NewSecurityLogger(&buf, "run-1")
	logger.DynamicContextSanitized(DynamicContextSanitizationEvent{
		Key:             "issue",
		OriginalLength:  100,
		SanitizedLength: 50,
		Reasons:         []string{"tags_stripped"},
	})

	output := buf.String()
	if !strings.Contains(output, "dynamic_context_sanitized") {
		t.Fatalf("expected event name in %q", output)
	}
	if !strings.Contains(output, `"originalLength":100`) || !strings.Contains(output, `"sanitizedLength":50`) {
		t.Fatalf("expected event metadata in %q", output)
	}
	if strings.Contains(output, "untrusted content") {
		t.Fatalf("event should not include dynamic context value: %q", output)
	}
}

func containsReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}
