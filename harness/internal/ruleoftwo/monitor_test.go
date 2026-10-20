package ruleoftwo

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/security"
)

// fakeLiveAWSKey is a live-shaped AKIA key (16 uppercase chars, no
// EXAMPLE suffix) so the detector's doc-example allowlist does not
// reject it.
const fakeLiveAWSKey = "AKIAQWERTYUIOPASDFGH"

func defaultCriteria() []string {
	return []string{"sensitive_data", "pii"}
}

func countPattern(patterns []string, name string) int {
	n := 0
	for _, p := range patterns {
		if p == name {
			n++
		}
	}
	return n
}

func requireEmptyDetection(t *testing.T, det Detection) {
	t.Helper()
	must := require.New(t)
	must.Empty(det.Patterns, "detection must report no patterns")
	must.Empty(det.Tier, "detection must report no tier")
	must.False(det.Transition, "detection must not report a transition")
}

func TestPatternMonitor_LatchTierFindingTripsOnce(t *testing.T) {
	must := require.New(t)
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	must.False(m.Tripped(), "fresh monitor must not be tripped")

	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{"creds: " + fakeLiveAWSKey})
	must.True(det.Transition, "first latch-tier detection must report Transition")
	must.Equal(security.TierLatch, det.Tier)
	must.Contains(det.Patterns, "secret/aws_access_key_id")
	must.True(m.Tripped(), "monitor must be tripped after a latch-tier finding")

	// One-way: there is no reset path. Subsequent observations and
	// guard trips must never report another transition and the latch
	// must stay set.
	again := m.ObserveChunks(context.Background(), "tool_result", 2, []string{fakeLiveAWSKey})
	must.False(again.Transition, "second observation must not report Transition")
	must.False(m.TripFromGuard("g1", "sensitive_data"), "TripFromGuard after latch must not report a transition")
	must.True(m.Tripped(), "latch must remain set")
}

func TestPatternMonitor_SkipsRescanAfterTrip(t *testing.T) {
	must := require.New(t)
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	m.ObserveChunks(context.Background(), "tool_result", 1, []string{fakeLiveAWSKey})
	must.True(m.Tripped(), "setup: monitor must be tripped")

	det := m.ObserveChunks(context.Background(), "tool_result", 2, []string{fakeLiveAWSKey})
	requireEmptyDetection(t, det)
}

// After a latch-tier trip, scanning is suspended for every tier, so
// warn-only content produces an empty Detection.
func TestPatternMonitor_WarnTierSuppressedAfterLatch(t *testing.T) {
	must := require.New(t)
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{fakeLiveAWSKey})
	must.True(det.Transition, "setup: latch-tier content must trip the monitor")

	det = m.ObserveChunks(context.Background(), "tool_result", 2, []string{"see secret://SOME_REF for the key"})
	requireEmptyDetection(t, det)
}

func TestPatternMonitor_WarnTierDoesNotLatch(t *testing.T) {
	must := require.New(t)
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{"see secret://SOME_REF for the key"})
	must.False(m.Tripped(), "warn-tier finding must not trip the latch")
	must.False(det.Transition, "warn-tier finding must not report Transition")
	must.Equal(security.TierWarn, det.Tier)
	must.Equal([]string{"secret/secret_ref"}, det.Patterns)
}

func TestPatternMonitor_DeduplicatesPatternNames(t *testing.T) {
	must := require.New(t)
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	// Two distinct AKIA keys in two chunks: the pattern name appears once.
	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{
		"first " + fakeLiveAWSKey,
		"second AKIAZXCVBNMASDFGHJKLQ",
	})
	must.Equal(1, countPattern(det.Patterns, "secret/aws_access_key_id"),
		"pattern name must appear exactly once (set semantics)")
}

func TestPatternMonitor_OverlappingPatternsReportedOnce(t *testing.T) {
	must := require.New(t)
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	// One sk-ant- key matches both the anthropic and openai patterns:
	// both names are reported, each exactly once.
	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{"key sk-ant-api03-aaaabbbbccccddddeeee"})
	must.Equal(1, countPattern(det.Patterns, "secret/anthropic_api_key"),
		"anthropic pattern name must appear exactly once")
	must.Equal(1, countPattern(det.Patterns, "secret/openai_api_key"),
		"openai pattern name must appear exactly once")
	must.True(det.Transition, "overlapping latch findings must still produce exactly one transition")
}

func TestPatternMonitor_StaticallySensitiveStartsTripped(t *testing.T) {
	must := require.New(t)
	m := NewPatternMonitor(false, "block-external", defaultCriteria(), true)
	must.True(m.Tripped(), "staticallySensitive monitor must start tripped")

	det := m.ObserveChunks(context.Background(), "prompt", 0, []string{fakeLiveAWSKey})
	must.False(det.Transition, "pre-tripped monitor must never report a transition")
	must.Empty(det.Patterns, "pre-tripped monitor must skip scanning")
}

func TestPatternMonitor_GuardCriteriaFiltering(t *testing.T) {
	must := require.New(t)
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	must.False(m.TripFromGuard("g1", "jailbreak"), "non-matching criterion must not trip")
	must.False(m.Tripped(), "monitor must not trip on a non-matching criterion")
	must.True(m.TripFromGuard("g1", "sensitive_data"), "matching criterion must trip and report the transition")
	must.True(m.Tripped(), "monitor must be tripped after a matching criterion")
}

func TestPatternMonitor_TransitionReportedExactlyOnceConcurrently(t *testing.T) {
	must := require.New(t)
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	const n = 32
	var transitions atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{"key " + fakeLiveAWSKey})
			if det.Transition {
				transitions.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if m.TripFromGuard("g1", "sensitive_data") {
				transitions.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	must.EqualValues(1, transitions.Load(), "exactly one concurrent latch attempt must report the transition")
	must.True(m.Tripped(), "monitor must be tripped after concurrent latch attempts")
}

func TestPatternMonitor_ActionReflectsEnforcement(t *testing.T) {
	t.Run("enforcing monitor reports the configured action", func(t *testing.T) {
		must := require.New(t)
		m := NewPatternMonitor(true, "abort", defaultCriteria(), false)
		must.True(m.Enforcing())
		must.Equal("abort", m.Action())
	})

	t.Run("observe-only monitor forces the warn action", func(t *testing.T) {
		must := require.New(t)
		m := NewPatternMonitor(false, "abort", defaultCriteria(), false)
		must.False(m.Enforcing())
		must.Equal("warn", m.Action(), "observe-only action must be forced to warn")
	})
}

func TestPatternMonitor_RedactReplacesLatchSpans(t *testing.T) {
	must := require.New(t)
	m := NewPatternMonitor(true, "redact", defaultCriteria(), false)
	in := "before " + fakeLiveAWSKey + " after"
	out, n := m.Redact(in)
	must.Equal(1, n, "redaction count")
	must.NotContains(out, fakeLiveAWSKey, "redacted output must not contain the key")
	must.Contains(out, RedactionPlaceholder, "redacted output must contain the placeholder")
	must.True(strings.HasPrefix(out, "before "), "text before the key must be preserved: %q", out)
	must.True(strings.HasSuffix(out, " after"), "text after the key must be preserved: %q", out)
}

func TestPatternMonitor_RedactMergesOverlappingSpans(t *testing.T) {
	must := require.New(t)
	m := NewPatternMonitor(true, "redact", defaultCriteria(), false)
	// sk-ant- matches both the anthropic and openai patterns over
	// overlapping spans; the output must carry a single placeholder.
	in := "key sk-ant-api03-aaaabbbbccccddddeeee end"
	out, n := m.Redact(in)
	must.Equal(1, n, "overlapping spans must merge into one redaction")
	must.Equal(1, strings.Count(out, RedactionPlaceholder), "output must carry a single placeholder: %q", out)
	must.NotContains(out, "sk-ant-", "redacted output must not contain key material")
}

func TestPatternMonitor_RedactLeavesWarnTierIntact(t *testing.T) {
	must := require.New(t)
	m := NewPatternMonitor(true, "redact", defaultCriteria(), false)
	in := "see secret://SOME_REF for details"
	out, n := m.Redact(in)
	must.Equal(0, n, "warn-tier content must not be redacted")
	must.Equal(in, out, "warn-tier content must pass through unchanged")
}

// An enforcing redact monitor is the one action that keeps scanning
// after the latch, so later tool results can have their spans rewritten.
// Every other action suspends scanning; see
// TestPatternMonitor_NonRedactActionSuspendsScanning.
func TestPatternMonitor_RedactKeepsScanningAfterLatch(t *testing.T) {
	must := require.New(t)
	m := NewPatternMonitor(true, "redact", defaultCriteria(), false)
	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{fakeLiveAWSKey})
	must.True(det.Transition, "setup: first latch-tier content must transition")

	det = m.ObserveChunks(context.Background(), "tool_result", 2, []string{"another " + fakeLiveAWSKey})
	must.False(det.Transition, "redact must not report a second transition")
	must.NotEmpty(det.Patterns, "redact must keep scanning after the latch")
	must.Equal(security.TierLatch, det.Tier)
}

func TestPatternMonitor_NonRedactActionSuspendsScanning(t *testing.T) {
	for _, action := range []string{"block-external", "ask-upstream", "abort"} {
		t.Run(action, func(t *testing.T) {
			must := require.New(t)
			m := NewPatternMonitor(true, action, defaultCriteria(), false)
			m.ObserveChunks(context.Background(), "tool_result", 1, []string{fakeLiveAWSKey})
			det := m.ObserveChunks(context.Background(), "tool_result", 2, []string{fakeLiveAWSKey})
			must.Empty(det.Patterns, "action %q must suspend scanning after the latch", action)
		})
	}
}

func TestNoop_NeverTripsNeverEnforces(t *testing.T) {
	must := require.New(t)
	m := NewNoop()
	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{fakeLiveAWSKey})
	must.Empty(det.Patterns, "Noop must not detect patterns")
	must.False(det.Transition, "Noop must not report a transition")
	must.False(m.TripFromGuard("g1", "sensitive_data"), "Noop must not trip from a guard")
	must.False(m.Tripped(), "Noop must never be tripped")
	must.False(m.Enforcing(), "Noop must never enforce")
	must.Equal("warn", m.Action())

	out, n := m.Redact("anything " + fakeLiveAWSKey)
	must.Equal("anything "+fakeLiveAWSKey, out, "Noop Redact must pass content through unchanged")
	must.Equal(0, n, "Noop Redact must not count redactions")
}
