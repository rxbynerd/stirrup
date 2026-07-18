package types

import (
	"strings"
	"testing"
	"unicode/utf8"
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
			if truncated {
				t.Errorf("truncated = true, want false for %q under cap %d", tc.s, tc.maxByte)
			}
			if got != tc.s {
				t.Errorf("capped = %q, want %q unmodified", got, tc.s)
			}
		})
	}
}

// TestCapFinalAssistantText_ASCIIBoundary pins that an ASCII string
// longer than the cap is cut at the byte boundary and marked.
func TestCapFinalAssistantText_ASCIIBoundary(t *testing.T) {
	s := "0123456789"
	got, truncated := CapFinalAssistantText(s, 4)
	if !truncated {
		t.Fatal("truncated = false, want true")
	}
	wantPrefix := "0123"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("capped = %q, want prefix %q", got, wantPrefix)
	}
	if !strings.HasSuffix(got, finalAssistantTextTruncationMarker) {
		t.Errorf("capped = %q, want suffix %q", got, finalAssistantTextTruncationMarker)
	}
	if got != wantPrefix+finalAssistantTextTruncationMarker {
		t.Errorf("capped = %q, want %q", got, wantPrefix+finalAssistantTextTruncationMarker)
	}
}

// TestCapFinalAssistantText_MultibyteRuneStraddlesCap pins that a cap
// landing mid-rune backs off to the start of that rune instead of
// splitting it.
func TestCapFinalAssistantText_MultibyteRuneStraddlesCap(t *testing.T) {
	// "a" (1 byte) + "€" (3 bytes) + "b" (1 byte) = 5 bytes total.
	s := "a€b"
	if len(s) != 5 {
		t.Fatalf("test fixture len(s) = %d, want 5", len(s))
	}
	// Cap at 2 bytes lands inside the 3-byte € encoding.
	got, truncated := CapFinalAssistantText(s, 2)
	if !truncated {
		t.Fatal("truncated = false, want true")
	}
	prefix := strings.TrimSuffix(got, finalAssistantTextTruncationMarker)
	if prefix != "a" {
		t.Errorf("capped prefix = %q, want %q (the straddled rune must be dropped entirely)", prefix, "a")
	}
	if !utf8.ValidString(got) {
		t.Errorf("capped = %q is not valid UTF-8", got)
	}
}

// TestCapFinalAssistantText_MultibyteRuneExactlyAtCap pins that a cap
// landing exactly on a rune boundary keeps the full preceding rune.
func TestCapFinalAssistantText_MultibyteRuneExactlyAtCap(t *testing.T) {
	s := "a€b"
	got, truncated := CapFinalAssistantText(s, 4)
	if !truncated {
		t.Fatal("truncated = false, want true")
	}
	prefix := strings.TrimSuffix(got, finalAssistantTextTruncationMarker)
	if prefix != "a€" {
		t.Errorf("capped prefix = %q, want %q (cap exactly at rune boundary keeps the full rune)", prefix, "a€")
	}
	if !utf8.ValidString(got) {
		t.Errorf("capped = %q is not valid UTF-8", got)
	}
}

// TestCapFinalAssistantText_CapSmallerThanSingleRune pins that a cap
// too small for even the first rune drops it entirely.
func TestCapFinalAssistantText_CapSmallerThanSingleRune(t *testing.T) {
	s := "€" // a single 3-byte rune
	got, truncated := CapFinalAssistantText(s, 1)
	if !truncated {
		t.Fatal("truncated = false, want true")
	}
	if got != finalAssistantTextTruncationMarker {
		t.Errorf("capped = %q, want just the marker %q", got, finalAssistantTextTruncationMarker)
	}
	if !utf8.ValidString(got) {
		t.Errorf("capped = %q is not valid UTF-8", got)
	}
}

// TestCapFinalAssistantText_ZeroCapOnNonEmptyString pins that
// maxBytes==0 drops the entire string, leaving only the marker.
func TestCapFinalAssistantText_ZeroCapOnNonEmptyString(t *testing.T) {
	got, truncated := CapFinalAssistantText("hello", 0)
	if !truncated {
		t.Fatal("truncated = false, want true")
	}
	if got != finalAssistantTextTruncationMarker {
		t.Errorf("capped = %q, want just the marker %q", got, finalAssistantTextTruncationMarker)
	}
}

// TestCapFinalAssistantText_NegativeCapTreatedAsZero pins that a
// negative maxBytes behaves identically to maxBytes==0.
func TestCapFinalAssistantText_NegativeCapTreatedAsZero(t *testing.T) {
	got, truncated := CapFinalAssistantText("hello", -5)
	if !truncated {
		t.Fatal("truncated = false, want true")
	}
	if got != finalAssistantTextTruncationMarker {
		t.Errorf("capped = %q, want just the marker %q", got, finalAssistantTextTruncationMarker)
	}
}

// TestCapFinalAssistantText_StringShorterThanMarker pins that
// truncation still appends the full marker even when the marker is
// longer than the truncated prefix.
func TestCapFinalAssistantText_StringShorterThanMarker(t *testing.T) {
	s := "hello world" // 11 bytes, well under len(marker)
	got, truncated := CapFinalAssistantText(s, 3)
	if !truncated {
		t.Fatal("truncated = false, want true")
	}
	want := "hel" + finalAssistantTextTruncationMarker
	if got != want {
		t.Errorf("capped = %q, want %q", got, want)
	}
}

// TestResolvedMaxFinalAssistantTextBytes pins that a nil config or
// unset field falls back to the default, and a positive override
// passes through unchanged.
func TestResolvedMaxFinalAssistantTextBytes(t *testing.T) {
	var nilCfg *ResultSinkConfig
	if got := nilCfg.ResolvedMaxFinalAssistantTextBytes(); got != DefaultMaxFinalAssistantTextBytes {
		t.Errorf("nil config: ResolvedMaxFinalAssistantTextBytes() = %d, want default %d", got, DefaultMaxFinalAssistantTextBytes)
	}
	unset := &ResultSinkConfig{Type: "stdout-json"}
	if got := unset.ResolvedMaxFinalAssistantTextBytes(); got != DefaultMaxFinalAssistantTextBytes {
		t.Errorf("unset field: ResolvedMaxFinalAssistantTextBytes() = %d, want default %d", got, DefaultMaxFinalAssistantTextBytes)
	}
	override := &ResultSinkConfig{Type: "stdout-json", MaxFinalAssistantTextBytes: 4096}
	if got := override.ResolvedMaxFinalAssistantTextBytes(); got != 4096 {
		t.Errorf("override: ResolvedMaxFinalAssistantTextBytes() = %d, want 4096", got)
	}
}
