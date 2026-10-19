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
	require.Empty(t, det.Patterns, "detection must report no patterns")
	require.Empty(t, det.Tier, "detection must report no tier")
	require.False(t, det.Transition, "detection must not report a transition")
}

func TestPatternMonitor_LatchTierFindingTripsOnce(t *testing.T) {
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	require.False(t, m.Tripped(), "fresh monitor must not be tripped")

	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{"creds: " + fakeLiveAWSKey})
	require.True(t, det.Transition, "first latch-tier detection must report Transition")
	require.Equal(t, security.TierLatch, det.Tier)
	require.Contains(t, det.Patterns, "secret/aws_access_key_id")
	require.True(t, m.Tripped(), "monitor must be tripped after a latch-tier finding")

	// One-way: there is no reset path. Subsequent observations and
	// guard trips must never report another transition and the latch
	// must stay set.
	again := m.ObserveChunks(context.Background(), "tool_result", 2, []string{fakeLiveAWSKey})
	require.False(t, again.Transition, "second observation must not report Transition")
	require.False(t, m.TripFromGuard("g1", "sensitive_data"), "TripFromGuard after latch must not report a transition")
	require.True(t, m.Tripped(), "latch must remain set")
}

func TestPatternMonitor_SkipsRescanAfterTrip(t *testing.T) {
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	m.ObserveChunks(context.Background(), "tool_result", 1, []string{fakeLiveAWSKey})
	require.True(t, m.Tripped(), "setup: monitor must be tripped")

	det := m.ObserveChunks(context.Background(), "tool_result", 2, []string{fakeLiveAWSKey})
	requireEmptyDetection(t, det)
}

// After a latch-tier trip, scanning is suspended for every tier, so
// warn-only content produces an empty Detection.
func TestPatternMonitor_WarnTierSuppressedAfterLatch(t *testing.T) {
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{fakeLiveAWSKey})
	require.True(t, det.Transition, "setup: latch-tier content must trip the monitor")

	det = m.ObserveChunks(context.Background(), "tool_result", 2, []string{"see secret://SOME_REF for the key"})
	requireEmptyDetection(t, det)
}

func TestPatternMonitor_WarnTierDoesNotLatch(t *testing.T) {
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{"see secret://SOME_REF for the key"})
	require.False(t, m.Tripped(), "warn-tier finding must not trip the latch")
	require.False(t, det.Transition, "warn-tier finding must not report Transition")
	require.Equal(t, security.TierWarn, det.Tier)
	require.Equal(t, []string{"secret/secret_ref"}, det.Patterns)
}

func TestPatternMonitor_DeduplicatesPatternNames(t *testing.T) {
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	// Two distinct AKIA keys in two chunks: the pattern name appears once.
	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{
		"first " + fakeLiveAWSKey,
		"second AKIAZXCVBNMASDFGHJKLQ",
	})
	require.Equal(t, 1, countPattern(det.Patterns, "secret/aws_access_key_id"),
		"pattern name must appear exactly once (set semantics)")
}

func TestPatternMonitor_OverlappingPatternsReportedOnce(t *testing.T) {
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	// One sk-ant- key matches both the anthropic and openai patterns:
	// both names are reported, each exactly once.
	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{"key sk-ant-api03-aaaabbbbccccddddeeee"})
	require.Equal(t, 1, countPattern(det.Patterns, "secret/anthropic_api_key"),
		"anthropic pattern name must appear exactly once")
	require.Equal(t, 1, countPattern(det.Patterns, "secret/openai_api_key"),
		"openai pattern name must appear exactly once")
	require.True(t, det.Transition, "overlapping latch findings must still produce exactly one transition")
}

func TestPatternMonitor_StaticallySensitiveStartsTripped(t *testing.T) {
	m := NewPatternMonitor(false, "block-external", defaultCriteria(), true)
	require.True(t, m.Tripped(), "staticallySensitive monitor must start tripped")

	det := m.ObserveChunks(context.Background(), "prompt", 0, []string{fakeLiveAWSKey})
	require.False(t, det.Transition, "pre-tripped monitor must never report a transition")
	require.Empty(t, det.Patterns, "pre-tripped monitor must skip scanning")
}

func TestPatternMonitor_GuardCriteriaFiltering(t *testing.T) {
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	require.False(t, m.TripFromGuard("g1", "jailbreak"), "non-matching criterion must not trip")
	require.False(t, m.Tripped(), "monitor must not trip on a non-matching criterion")
	require.True(t, m.TripFromGuard("g1", "sensitive_data"), "matching criterion must trip and report the transition")
	require.True(t, m.Tripped(), "monitor must be tripped after a matching criterion")
}

func TestPatternMonitor_TransitionReportedExactlyOnceConcurrently(t *testing.T) {
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
	require.EqualValues(t, 1, transitions.Load(), "exactly one concurrent latch attempt must report the transition")
	require.True(t, m.Tripped(), "monitor must be tripped after concurrent latch attempts")
}

func TestPatternMonitor_ActionReflectsEnforcement(t *testing.T) {
	t.Run("enforcing monitor reports the configured action", func(t *testing.T) {
		m := NewPatternMonitor(true, "abort", defaultCriteria(), false)
		require.True(t, m.Enforcing())
		require.Equal(t, "abort", m.Action())
	})

	t.Run("observe-only monitor forces the warn action", func(t *testing.T) {
		m := NewPatternMonitor(false, "abort", defaultCriteria(), false)
		require.False(t, m.Enforcing())
		require.Equal(t, "warn", m.Action(), "observe-only action must be forced to warn")
	})
}

func TestPatternMonitor_RedactReplacesLatchSpans(t *testing.T) {
	m := NewPatternMonitor(true, "redact", defaultCriteria(), false)
	in := "before " + fakeLiveAWSKey + " after"
	out, n := m.Redact(in)
	require.Equal(t, 1, n, "redaction count")
	require.NotContains(t, out, fakeLiveAWSKey, "redacted output must not contain the key")
	require.Contains(t, out, RedactionPlaceholder, "redacted output must contain the placeholder")
	require.True(t, strings.HasPrefix(out, "before "), "text before the key must be preserved: %q", out)
	require.True(t, strings.HasSuffix(out, " after"), "text after the key must be preserved: %q", out)
}

func TestPatternMonitor_RedactMergesOverlappingSpans(t *testing.T) {
	m := NewPatternMonitor(true, "redact", defaultCriteria(), false)
	// sk-ant- matches both the anthropic and openai patterns over
	// overlapping spans; the output must carry a single placeholder.
	in := "key sk-ant-api03-aaaabbbbccccddddeeee end"
	out, n := m.Redact(in)
	require.Equal(t, 1, n, "overlapping spans must merge into one redaction")
	require.Equal(t, 1, strings.Count(out, RedactionPlaceholder), "output must carry a single placeholder: %q", out)
	require.NotContains(t, out, "sk-ant-", "redacted output must not contain key material")
}

func TestPatternMonitor_RedactLeavesWarnTierIntact(t *testing.T) {
	m := NewPatternMonitor(true, "redact", defaultCriteria(), false)
	in := "see secret://SOME_REF for details"
	out, n := m.Redact(in)
	require.Equal(t, 0, n, "warn-tier content must not be redacted")
	require.Equal(t, in, out, "warn-tier content must pass through unchanged")
}

// An enforcing redact monitor is the one action that keeps scanning
// after the latch, so later tool results can have their spans rewritten.
// Every other action suspends scanning; see
// TestPatternMonitor_NonRedactActionSuspendsScanning.
func TestPatternMonitor_RedactKeepsScanningAfterLatch(t *testing.T) {
	m := NewPatternMonitor(true, "redact", defaultCriteria(), false)
	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{fakeLiveAWSKey})
	require.True(t, det.Transition, "setup: first latch-tier content must transition")

	det = m.ObserveChunks(context.Background(), "tool_result", 2, []string{"another " + fakeLiveAWSKey})
	require.False(t, det.Transition, "redact must not report a second transition")
	require.NotEmpty(t, det.Patterns, "redact must keep scanning after the latch")
	require.Equal(t, security.TierLatch, det.Tier)
}

func TestPatternMonitor_NonRedactActionSuspendsScanning(t *testing.T) {
	for _, action := range []string{"block-external", "ask-upstream", "abort"} {
		t.Run(action, func(t *testing.T) {
			m := NewPatternMonitor(true, action, defaultCriteria(), false)
			m.ObserveChunks(context.Background(), "tool_result", 1, []string{fakeLiveAWSKey})
			det := m.ObserveChunks(context.Background(), "tool_result", 2, []string{fakeLiveAWSKey})
			require.Empty(t, det.Patterns, "action %q must suspend scanning after the latch", action)
		})
	}
}

func TestNoop_NeverTripsNeverEnforces(t *testing.T) {
	m := NewNoop()
	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{fakeLiveAWSKey})
	require.Empty(t, det.Patterns, "Noop must not detect patterns")
	require.False(t, det.Transition, "Noop must not report a transition")
	require.False(t, m.TripFromGuard("g1", "sensitive_data"), "Noop must not trip from a guard")
	require.False(t, m.Tripped(), "Noop must never be tripped")
	require.False(t, m.Enforcing(), "Noop must never enforce")
	require.Equal(t, "warn", m.Action())

	out, n := m.Redact("anything " + fakeLiveAWSKey)
	require.Equal(t, "anything "+fakeLiveAWSKey, out, "Noop Redact must pass content through unchanged")
	require.Equal(t, 0, n, "Noop Redact must not count redactions")
}
