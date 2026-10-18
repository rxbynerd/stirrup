package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// decodeFences replaces each spotlight fence in s with the text it
// encodes, failing the test on a payload that is not base64.
func decodeFences(t *testing.T, s string) string {
	t.Helper()
	var b strings.Builder
	for {
		open := strings.Index(s, guard.SpotlightOpenTag)
		if open < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:open])
		s = s[open+len(guard.SpotlightOpenTag):]
		end := strings.Index(s, guard.SpotlightCloseTag)
		if end < 0 {
			t.Fatalf("unclosed fence in %q", s)
		}
		decoded, err := base64.StdEncoding.DecodeString(s[:end])
		if err != nil {
			t.Fatalf("fence payload is not base64: %v", err)
		}
		b.Write(decoded)
		s = s[end+len(guard.SpotlightCloseTag):]
	}
}

// fenceText fences the ranges the scrubber replaces in text, as the loop
// does for a tool result's Content.
func fenceText(text string) string {
	_, ranges, _ := security.ScrubWithSpans(text)
	return fenceRanges(text, ranges)
}

// TestFenceScrubbedSpans_InflationIsBounded pins that fencing cannot blow
// up a result: a result dense with matches is fenced whole, sparse ranges
// stay granular, and nearby ranges share one fence.
func TestFenceScrubbedSpans_InflationIsBounded(t *testing.T) {
	dense := strings.Repeat("Bearer x ", 100<<10/9)
	got := fenceText(dense)
	if ratio := float64(len(got)) / float64(len(dense)); ratio > 1.4 {
		t.Errorf("dense result fenced to %.2f times its size, want at most 1.4", ratio)
	}
	if strings.Count(got, guard.SpotlightOpenTag) != 1 || decodeFences(t, got) != dense {
		t.Errorf("dense result is not one fence of the original text")
	}

	gap := strings.Repeat("plain words ", 20)
	sparse := "a " + fakeAnthropicKey + " " + gap + " b ghp_abcdef end"
	got = fenceText(sparse)
	if strings.Count(got, guard.SpotlightOpenTag) != 2 || !strings.Contains(got, gap) || decodeFences(t, got) != sparse {
		t.Errorf("sparse result = %q, want two fences with the plain text between them", got)
	}

	near := "a " + fakeAnthropicKey + " and ghp_abcdef end"
	got = fenceText(near)
	if want := "a " + guard.ApplySpotlight(fakeAnthropicKey+" and ghp_abcdef") + " end"; got != want {
		t.Errorf("nearby ranges fenced as %q, want one fence %q", got, want)
	}
}

// TestRedactedSpansCountsMatches pins the definition of redacted_spans:
// the number of scrubber matches in the guard's input, whatever the
// fences, so two nearby matches sharing a fence count two and a secret in
// both Content and Structured counts twice.
func TestRedactedSpansCountsMatches(t *testing.T) {
	cases := []struct {
		name   string
		block  types.ContentBlock
		spans  string
		fences int
	}{
		{"two matches in one fence", types.ContentBlock{Content: "Bearer x secret://a"}, "2", 1},
		{"content and structured", types.ContentBlock{Content: "key " + fakeAnthropicKey, Structured: []byte(`{"key":"` + fakeAnthropicKey + `"}`)}, "2", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loop := buildTestLoop(nil)
			g := &verdictRecordingGuard{verdict: guard.VerdictAllow}
			loop.GuardRail = g
			block := tc.block
			block.Type, block.ToolUseID = "tool_result", "t1"
			messages := []types.Message{userText("task"), assistantToolUse("t1"), {Role: "user", Content: []types.ContentBlock{block}}}
			if outcome := loop.screenArrivedContent(context.Background(), buildTestConfig(), 1, messages); outcome != "" {
				t.Fatalf("outcome = %q", outcome)
			}
			if got := g.inputs[0].Metadata[guard.MetadataRedactedSpans]; got != tc.spans {
				t.Errorf("redacted_spans = %q, want %q", got, tc.spans)
			}
			if got := strings.Count(messages[2].Content[0].Content, guard.SpotlightOpenTag); got != tc.fences {
				t.Errorf("fences = %d, want %d", got, tc.fences)
			}
		})
	}
}

// TestScreenArrivedContent_StructuredOnlyMatch pins a match found only in
// the structured payload: the payload is dropped, nothing is fenced so no
// fence is recorded, and an empty Content is replaced by the scrubbed
// payload the guard read, so the model never gets an empty result.
func TestScreenArrivedContent_StructuredOnlyMatch(t *testing.T) {
	structured := []byte(`{"token":"` + fakeAnthropicKey + `"}`)
	cases := []struct {
		name, content, want string
	}{
		{"plain content", "the token is in the payload", "the token is in the payload"},
		{"empty content", "", `{"token":"[REDACTED]"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var events bytes.Buffer
			loop := buildTestLoop(nil)
			loop.Security = security.NewSecurityLogger(&events, "run")
			g := &verdictRecordingGuard{verdict: guard.VerdictAllow}
			loop.GuardRail = g
			messages := []types.Message{userText("task"), assistantToolUse("t1"), {Role: "user", Content: []types.ContentBlock{{
				Type: "tool_result", ToolUseID: "t1", Content: tc.content, Structured: structured, Kind: "test",
			}}}}
			if outcome := loop.screenArrivedContent(context.Background(), buildTestConfig(), 1, messages); outcome != "" {
				t.Fatalf("outcome = %q", outcome)
			}
			b := messages[2].Content[0]
			if b.Content != tc.want || b.Structured != nil || b.Kind != "" {
				t.Errorf("tool result = content %q structured %s kind %q, want content %q and no payload", b.Content, b.Structured, b.Kind, tc.want)
			}
			if strings.Contains(events.String(), harnessSpotlightGuardID) {
				t.Errorf("a fence was recorded though nothing was fenced: %s", events.String())
			}
			if got := g.inputs[0].Metadata[guard.MetadataRedactedSpans]; got != "1" {
				t.Errorf("redacted_spans = %q, want 1", got)
			}
		})
	}
}

// TestScreenArrivedContent_NoPreTurnGuardSkipsScrub pins that the guard's
// scrubbed copy is not built when no guard checks pre_turn: history is
// untouched and the scrub's cost is not paid.
func TestScreenArrivedContent_NoPreTurnGuardSkipsScrub(t *testing.T) {
	output := strings.Repeat("Bearer x ", 100<<10/9)
	postTurnOnly := guard.PhaseGated{Phases: []guard.Phase{guard.PhasePostTurn}, Inner: &verdictRecordingGuard{verdict: guard.VerdictAllow}}
	allocs := func(g guard.GuardRail) (float64, string) {
		loop := buildTestLoop(nil)
		loop.GuardRail = g
		var got string
		n := testing.AllocsPerRun(3, func() {
			messages := arrivedToolResult(output)
			loop.screenArrivedContent(context.Background(), buildTestConfig(), 1, messages)
			got = messages[2].Content[0].Content
		})
		return n, got
	}
	skipped, history := allocs(postTurnOnly)
	if history != output {
		t.Error("tool result changed in history with no pre_turn guard")
	}
	checked, _ := allocs(&verdictRecordingGuard{verdict: guard.VerdictAllow})
	if skipped*10 > checked {
		t.Errorf("screening without a pre_turn guard made %.0f allocations, not an order of magnitude below the %.0f with one", skipped, checked)
	}

	for name, g := range map[string]guard.GuardRail{"post_turn only": postTurnOnly, "no guard": nil} {
		loop := buildTestLoop(nil)
		loop.GuardRail = g
		chunks := []string{"prompt", output}
		var copied []string
		n := testing.AllocsPerRun(3, func() { copied, _ = loop.preTurnGuardCopy(chunks) })
		if n != 0 || copied[1] != output {
			t.Errorf("%s: turn-0 and user_response guard copy made %.0f allocations and changed the text", name, n)
		}
	}
}
