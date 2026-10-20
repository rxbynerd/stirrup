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
			must := require.New(t)
			got, truncated := CapFinalAssistantText(tc.s, tc.maxByte)
			must.False(truncated)
			must.Equal(tc.s, got)
		})
	}
}

// TestCapFinalAssistantText_ASCIIBoundary pins that an ASCII string
// longer than the cap is cut at the byte boundary and marked.
func TestCapFinalAssistantText_ASCIIBoundary(t *testing.T) {
	must := require.New(t)
	const wantPrefix = "0123"
	got, truncated := CapFinalAssistantText("0123456789", 4)
	must.True(truncated)
	must.True(strings.HasPrefix(got, wantPrefix), "capped = %q, want prefix %q", got, wantPrefix)
	must.True(strings.HasSuffix(got, finalAssistantTextTruncationMarker),
		"capped = %q, want suffix %q", got, finalAssistantTextTruncationMarker)
	must.Equal(wantPrefix+finalAssistantTextTruncationMarker, got)
}

// TestCapFinalAssistantText_MultibyteRuneStraddlesCap pins that a cap
// landing mid-rune backs off to the start of that rune instead of
// splitting it.
func TestCapFinalAssistantText_MultibyteRuneStraddlesCap(t *testing.T) {
	must := require.New(t)
	// "a" (1 byte) + "€" (3 bytes) + "b" (1 byte) = 5 bytes total.
	s := "a€b"
	must.Len(s, 5, "test fixture byte length")

	// Cap at 2 bytes lands inside the 3-byte € encoding.
	got, truncated := CapFinalAssistantText(s, 2)
	must.True(truncated)
	prefix := strings.TrimSuffix(got, finalAssistantTextTruncationMarker)
	must.Equal("a", prefix, "the straddled rune must be dropped entirely")
	must.True(utf8.ValidString(got), "capped = %q is not valid UTF-8", got)
}

// TestCapFinalAssistantText_MultibyteRuneExactlyAtCap pins that a cap
// landing exactly on a rune boundary keeps the full preceding rune.
func TestCapFinalAssistantText_MultibyteRuneExactlyAtCap(t *testing.T) {
	must := require.New(t)
	got, truncated := CapFinalAssistantText("a€b", 4)
	must.True(truncated)
	prefix := strings.TrimSuffix(got, finalAssistantTextTruncationMarker)
	must.Equal("a€", prefix, "a cap on a rune boundary keeps the full rune")
	must.True(utf8.ValidString(got), "capped = %q is not valid UTF-8", got)
}

// TestCapFinalAssistantText_CapSmallerThanSingleRune pins that a cap
// too small for even the first rune drops it entirely.
func TestCapFinalAssistantText_CapSmallerThanSingleRune(t *testing.T) {
	must := require.New(t)
	// A single 3-byte rune, capped at 1 byte.
	got, truncated := CapFinalAssistantText("€", 1)
	must.True(truncated)
	must.Equal(finalAssistantTextTruncationMarker, got)
	must.True(utf8.ValidString(got), "capped = %q is not valid UTF-8", got)
}

// TestCapFinalAssistantText_ZeroCapOnNonEmptyString pins that
// maxBytes==0 drops the entire string, leaving only the marker.
func TestCapFinalAssistantText_ZeroCapOnNonEmptyString(t *testing.T) {
	must := require.New(t)
	got, truncated := CapFinalAssistantText("hello", 0)
	must.True(truncated)
	must.Equal(finalAssistantTextTruncationMarker, got)
}

// TestCapFinalAssistantText_NegativeCapTreatedAsZero pins that a
// negative maxBytes behaves identically to maxBytes==0.
func TestCapFinalAssistantText_NegativeCapTreatedAsZero(t *testing.T) {
	must := require.New(t)
	got, truncated := CapFinalAssistantText("hello", -5)
	must.True(truncated)
	must.Equal(finalAssistantTextTruncationMarker, got)
}

// TestCapFinalAssistantText_StringShorterThanMarker pins that
// truncation still appends the full marker even when the marker is
// longer than the truncated prefix.
func TestCapFinalAssistantText_StringShorterThanMarker(t *testing.T) {
	must := require.New(t)
	// 11 bytes, well under len(marker).
	got, truncated := CapFinalAssistantText("hello world", 3)
	must.True(truncated)
	must.Equal("hel"+finalAssistantTextTruncationMarker, got)
}

// TestResultSinkConfig_ResolvedMaxFinalAssistantTextBytes pins that a
// nil config or unset field falls back to the default, and a positive
// override passes through unchanged.
func TestResultSinkConfig_ResolvedMaxFinalAssistantTextBytes(t *testing.T) {
	t.Run("nil config uses default", func(t *testing.T) {
		must := require.New(t)
		var cfg *ResultSinkConfig
		must.Equal(DefaultMaxFinalAssistantTextBytes, cfg.ResolvedMaxFinalAssistantTextBytes())
	})

	t.Run("unset field uses default", func(t *testing.T) {
		must := require.New(t)
		cfg := &ResultSinkConfig{Type: "stdout-json"}
		must.Equal(DefaultMaxFinalAssistantTextBytes, cfg.ResolvedMaxFinalAssistantTextBytes())
	})

	t.Run("positive override passes through", func(t *testing.T) {
		must := require.New(t)
		cfg := &ResultSinkConfig{Type: "stdout-json", MaxFinalAssistantTextBytes: 4096}
		must.Equal(4096, cfg.ResolvedMaxFinalAssistantTextBytes())
	})
}

// TestRunResult_StructuredOutput_SerialisesAsJSONValue pins that the
// structured output is omitted when empty and embedded as raw JSON
// otherwise.
func TestRunResult_StructuredOutput_SerialisesAsJSONValue(t *testing.T) {
	t.Run("empty structured output is omitted", func(t *testing.T) {
		must := require.New(t)
		out, err := json.Marshal(RunResult{Outcome: "success"})
		must.NoError(err)
		must.NotContains(string(out), "structuredOutput", "serialised: %s", out)
	})

	t.Run("structured output is embedded as a JSON value", func(t *testing.T) {
		must := require.New(t)
		out, err := json.Marshal(RunResult{Outcome: "success", StructuredOutput: json.RawMessage(`{"title":"Fix it"}`)})
		must.NoError(err)
		must.Contains(string(out), `"structuredOutput":{"title":"Fix it"}`, "serialised: %s", out)
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
			must := require.New(t)
			must.Equal(tc.want, ExportReceipt{Status: tc.status}.Delivered())
		})
	}
}
