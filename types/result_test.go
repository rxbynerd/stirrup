package types

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// TestCapFinalAssistantText_UnderCapPassesThrough pins that a string
// at or under the cap is returned unmodified with truncated=false.
func TestCapFinalAssistantText_UnderCapPassesThrough(t *testing.T) {
	cases := []struct {
		name    string
		s       string
		maxByte int
	}{
		{"empty string, positive cap", "", 10},
		{"empty string, zero cap", "", 0},
		{"shorter than marker", "hi", 100},
		{"exactly at cap", "hello", 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, truncated := CapFinalAssistantText(tc.s, tc.maxByte)
			require.False(t, truncated)
			require.Equal(t, tc.s, got)
		})
	}
}

// TestCapFinalAssistantText_ASCIIBoundary pins that an ASCII string
// longer than the cap is cut at the byte boundary and marked.
func TestCapFinalAssistantText_ASCIIBoundary(t *testing.T) {
	const wantPrefix = "0123"
	got, truncated := CapFinalAssistantText("0123456789", 4)
	require.True(t, truncated)
	require.True(t, strings.HasPrefix(got, wantPrefix), "capped = %q, want prefix %q", got, wantPrefix)
	require.True(t, strings.HasSuffix(got, finalAssistantTextTruncationMarker),
		"capped = %q, want suffix %q", got, finalAssistantTextTruncationMarker)
	require.Equal(t, wantPrefix+finalAssistantTextTruncationMarker, got)
}

// TestCapFinalAssistantText_MultibyteRuneStraddlesCap pins that a cap
// landing mid-rune backs off to the start of that rune instead of
// splitting it.
func TestCapFinalAssistantText_MultibyteRuneStraddlesCap(t *testing.T) {
	// "a" (1 byte) + "€" (3 bytes) + "b" (1 byte) = 5 bytes total.
	s := "a€b"
	require.Len(t, s, 5, "test fixture byte length")

	// Cap at 2 bytes lands inside the 3-byte € encoding.
	got, truncated := CapFinalAssistantText(s, 2)
	require.True(t, truncated)
	prefix := strings.TrimSuffix(got, finalAssistantTextTruncationMarker)
	require.Equal(t, "a", prefix, "the straddled rune must be dropped entirely")
	require.True(t, utf8.ValidString(got), "capped = %q is not valid UTF-8", got)
}

// TestCapFinalAssistantText_MultibyteRuneExactlyAtCap pins that a cap
// landing exactly on a rune boundary keeps the full preceding rune.
func TestCapFinalAssistantText_MultibyteRuneExactlyAtCap(t *testing.T) {
	got, truncated := CapFinalAssistantText("a€b", 4)
	require.True(t, truncated)
	prefix := strings.TrimSuffix(got, finalAssistantTextTruncationMarker)
	require.Equal(t, "a€", prefix, "a cap on a rune boundary keeps the full rune")
	require.True(t, utf8.ValidString(got), "capped = %q is not valid UTF-8", got)
}

// TestCapFinalAssistantText_CapSmallerThanSingleRune pins that a cap
// too small for even the first rune drops it entirely.
func TestCapFinalAssistantText_CapSmallerThanSingleRune(t *testing.T) {
	// A single 3-byte rune, capped at 1 byte.
	got, truncated := CapFinalAssistantText("€", 1)
	require.True(t, truncated)
	require.Equal(t, finalAssistantTextTruncationMarker, got)
	require.True(t, utf8.ValidString(got), "capped = %q is not valid UTF-8", got)
}

// TestCapFinalAssistantText_ZeroCapOnNonEmptyString pins that
// maxBytes==0 drops the entire string, leaving only the marker.
func TestCapFinalAssistantText_ZeroCapOnNonEmptyString(t *testing.T) {
	got, truncated := CapFinalAssistantText("hello", 0)
	require.True(t, truncated)
	require.Equal(t, finalAssistantTextTruncationMarker, got)
}

// TestCapFinalAssistantText_NegativeCapTreatedAsZero pins that a
// negative maxBytes behaves identically to maxBytes==0.
func TestCapFinalAssistantText_NegativeCapTreatedAsZero(t *testing.T) {
	got, truncated := CapFinalAssistantText("hello", -5)
	require.True(t, truncated)
	require.Equal(t, finalAssistantTextTruncationMarker, got)
}

// TestCapFinalAssistantText_StringShorterThanMarker pins that
// truncation still appends the full marker even when the marker is
// longer than the truncated prefix.
func TestCapFinalAssistantText_StringShorterThanMarker(t *testing.T) {
	// 11 bytes, well under len(marker).
	got, truncated := CapFinalAssistantText("hello world", 3)
	require.True(t, truncated)
	require.Equal(t, "hel"+finalAssistantTextTruncationMarker, got)
}

// TestResultSinkConfig_ResolvedMaxFinalAssistantTextBytes pins that a
// nil config or unset field falls back to the default, and a positive
// override passes through unchanged.
func TestResultSinkConfig_ResolvedMaxFinalAssistantTextBytes(t *testing.T) {
	t.Run("nil config uses default", func(t *testing.T) {
		var cfg *ResultSinkConfig
		require.Equal(t, DefaultMaxFinalAssistantTextBytes, cfg.ResolvedMaxFinalAssistantTextBytes())
	})

	t.Run("unset field uses default", func(t *testing.T) {
		cfg := &ResultSinkConfig{Type: "stdout-json"}
		require.Equal(t, DefaultMaxFinalAssistantTextBytes, cfg.ResolvedMaxFinalAssistantTextBytes())
	})

	t.Run("positive override passes through", func(t *testing.T) {
		cfg := &ResultSinkConfig{Type: "stdout-json", MaxFinalAssistantTextBytes: 4096}
		require.Equal(t, 4096, cfg.ResolvedMaxFinalAssistantTextBytes())
	})
}

// TestRunResult_StructuredOutput_SerialisesAsJSONValue pins that the
// structured output is omitted when empty and embedded as raw JSON
// otherwise.
func TestRunResult_StructuredOutput_SerialisesAsJSONValue(t *testing.T) {
	t.Run("empty structured output is omitted", func(t *testing.T) {
		out, err := json.Marshal(RunResult{Outcome: "success"})
		require.NoError(t, err)
		require.NotContains(t, string(out), "structuredOutput", "serialised: %s", out)
	})

	t.Run("structured output is embedded as a JSON value", func(t *testing.T) {
		out, err := json.Marshal(RunResult{Outcome: "success", StructuredOutput: json.RawMessage(`{"title":"Fix it"}`)})
		require.NoError(t, err)
		require.Contains(t, string(out), `"structuredOutput":{"title":"Fix it"}`, "serialised: %s", out)
	})
}

// TestExportReceipt_Delivered pins which export statuses count as
// delivered.
func TestExportReceipt_Delivered(t *testing.T) {
	cases := []struct {
		name   string
		status string
		want   bool
	}{
		{"uploaded is delivered", ExportStatusUploaded, true},
		{"empty export is delivered", ExportStatusEmpty, true},
		{"failed is not delivered", ExportStatusFailed, false},
		{"unset status is not delivered", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, ExportReceipt{Status: tc.status}.Delivered())
		})
	}
}
