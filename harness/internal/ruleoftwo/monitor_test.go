package ruleoftwo

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/security"
)

// fakeLiveAWSKey is a live-shaped AKIA key (16 uppercase chars, no
// EXAMPLE suffix) so the detector's doc-example allowlist does not
// reject it.
const fakeLiveAWSKey = "AKIAQWERTYUIOPASDFGH"

func defaultCriteria() []string {
	return []string{"sensitive_data", "pii"}
}

func TestPatternMonitor_LatchTierFindingTripsOnce(t *testing.T) {
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	if m.Tripped() {
		t.Fatal("fresh monitor must not be tripped")
	}

	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{"creds: " + fakeLiveAWSKey})
	if !det.Transition {
		t.Error("first latch-tier detection must report Transition")
	}
	if det.Tier != security.TierLatch {
		t.Errorf("Tier = %q, want %q", det.Tier, security.TierLatch)
	}
	found := false
	for _, p := range det.Patterns {
		if p == "secret/aws_access_key_id" {
			found = true
		}
	}
	if !found {
		t.Errorf("Patterns = %v, want secret/aws_access_key_id", det.Patterns)
	}
	if !m.Tripped() {
		t.Fatal("monitor must be tripped after a latch-tier finding")
	}

	// One-way: there is no reset path. Subsequent observations and
	// guard trips must never report another transition and the latch
	// must stay set.
	if again := m.ObserveChunks(context.Background(), "tool_result", 2, []string{fakeLiveAWSKey}); again.Transition {
		t.Error("second observation must not report Transition")
	}
	if m.TripFromGuard("g1", "sensitive_data") {
		t.Error("TripFromGuard after latch must not report a transition")
	}
	if !m.Tripped() {
		t.Error("latch must remain set")
	}
}

func TestPatternMonitor_SkipsRescanAfterTrip(t *testing.T) {
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	m.ObserveChunks(context.Background(), "tool_result", 1, []string{fakeLiveAWSKey})
	if !m.Tripped() {
		t.Fatal("setup: monitor must be tripped")
	}
	det := m.ObserveChunks(context.Background(), "tool_result", 2, []string{fakeLiveAWSKey})
	if len(det.Patterns) != 0 || det.Tier != "" || det.Transition {
		t.Errorf("post-trip observation must be empty, got %+v", det)
	}
}

func TestPatternMonitor_WarnTierDoesNotLatch(t *testing.T) {
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{"see secret://SOME_REF for the key"})
	if m.Tripped() {
		t.Error("warn-tier finding must not trip the latch")
	}
	if det.Transition {
		t.Error("warn-tier finding must not report Transition")
	}
	if det.Tier != security.TierWarn {
		t.Errorf("Tier = %q, want %q", det.Tier, security.TierWarn)
	}
	if len(det.Patterns) != 1 || det.Patterns[0] != "secret/secret_ref" {
		t.Errorf("Patterns = %v, want [secret/secret_ref]", det.Patterns)
	}
}

func TestPatternMonitor_DeduplicatesPatternNames(t *testing.T) {
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	// Two distinct AKIA keys in two chunks: the pattern name appears once.
	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{
		"first " + fakeLiveAWSKey,
		"second AKIAZXCVBNMASDFGHJKLQ",
	})
	count := 0
	for _, p := range det.Patterns {
		if p == "secret/aws_access_key_id" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("secret/aws_access_key_id appeared %d times, want 1 (set semantics)", count)
	}
}

func TestPatternMonitor_OverlappingFindingsAreASet(t *testing.T) {
	m := NewPatternMonitor(true, "block-external", defaultCriteria(), false)
	// One sk-ant- key matches both the anthropic and openai patterns:
	// both names are reported, each exactly once.
	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{"key sk-ant-api03-aaaabbbbccccddddeeee"})
	seen := make(map[string]int)
	for _, p := range det.Patterns {
		seen[p]++
	}
	if seen["secret/anthropic_api_key"] != 1 || seen["secret/openai_api_key"] != 1 {
		t.Errorf("Patterns = %v, want both anthropic and openai names exactly once", det.Patterns)
	}
	if det.Transition != true {
		t.Error("overlapping latch findings must still produce exactly one transition")
	}
}

func TestPatternMonitor_StaticallySensitiveStartsTripped(t *testing.T) {
	m := NewPatternMonitor(false, "block-external", defaultCriteria(), true)
	if !m.Tripped() {
		t.Fatal("staticallySensitive monitor must start tripped")
	}
	det := m.ObserveChunks(context.Background(), "prompt", 0, []string{fakeLiveAWSKey})
	if det.Transition {
		t.Error("pre-tripped monitor must never report a transition")
	}
	if len(det.Patterns) != 0 {
		t.Errorf("pre-tripped monitor must skip scanning, got patterns %v", det.Patterns)
	}
}

func TestPatternMonitor_GuardCriteriaFiltering(t *testing.T) {
	m := NewPatternMonitor(true, "block-external", []string{"sensitive_data", "pii"}, false)
	if m.TripFromGuard("g1", "jailbreak") {
		t.Error("non-matching criterion must not trip")
	}
	if m.Tripped() {
		t.Fatal("monitor tripped on a non-matching criterion")
	}
	if !m.TripFromGuard("g1", "sensitive_data") {
		t.Error("matching criterion must trip and report the transition")
	}
	if !m.Tripped() {
		t.Fatal("monitor must be tripped after a matching criterion")
	}
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
	if got := transitions.Load(); got != 1 {
		t.Fatalf("transitions = %d, want exactly 1", got)
	}
	if !m.Tripped() {
		t.Fatal("monitor must be tripped after concurrent latch attempts")
	}
}

func TestPatternMonitor_ActionAndEnforcing(t *testing.T) {
	enforcing := NewPatternMonitor(true, "abort", defaultCriteria(), false)
	if !enforcing.Enforcing() {
		t.Error("Enforcing() = false, want true")
	}
	if got := enforcing.Action(); got != "abort" {
		t.Errorf("enforcing Action() = %q, want %q", got, "abort")
	}

	observing := NewPatternMonitor(false, "abort", defaultCriteria(), false)
	if observing.Enforcing() {
		t.Error("Enforcing() = true, want false")
	}
	if got := observing.Action(); got != "warn" {
		t.Errorf("observe-only Action() = %q, want %q (forced when not enforcing)", got, "warn")
	}
}

func TestPatternMonitor_RedactReplacesLatchSpans(t *testing.T) {
	m := NewPatternMonitor(true, "redact", defaultCriteria(), false)
	in := "before " + fakeLiveAWSKey + " after"
	out, n := m.Redact(in)
	if n != 1 {
		t.Errorf("redaction count = %d, want 1", n)
	}
	if strings.Contains(out, fakeLiveAWSKey) {
		t.Errorf("redacted output still contains the key: %q", out)
	}
	if !strings.Contains(out, RedactionPlaceholder) {
		t.Errorf("redacted output missing placeholder: %q", out)
	}
	if !strings.HasPrefix(out, "before ") || !strings.HasSuffix(out, " after") {
		t.Errorf("surrounding text not preserved: %q", out)
	}
}

func TestPatternMonitor_RedactMergesOverlappingSpans(t *testing.T) {
	m := NewPatternMonitor(true, "redact", defaultCriteria(), false)
	// sk-ant- matches both the anthropic and openai patterns over
	// overlapping spans; the output must carry a single placeholder.
	in := "key sk-ant-api03-aaaabbbbccccddddeeee end"
	out, n := m.Redact(in)
	if n != 1 {
		t.Errorf("redaction count = %d, want 1 (overlapping spans merge)", n)
	}
	if got := strings.Count(out, RedactionPlaceholder); got != 1 {
		t.Errorf("placeholder count = %d, want 1: %q", got, out)
	}
	if strings.Contains(out, "sk-ant-") {
		t.Errorf("redacted output still contains key material: %q", out)
	}
}

func TestPatternMonitor_RedactLeavesWarnTierIntact(t *testing.T) {
	m := NewPatternMonitor(true, "redact", defaultCriteria(), false)
	in := "see secret://SOME_REF for details"
	out, n := m.Redact(in)
	if n != 0 {
		t.Errorf("redaction count = %d, want 0 for warn-tier content", n)
	}
	if out != in {
		t.Errorf("warn-tier content must pass through unchanged, got %q", out)
	}
}

func TestNoop_NeverTripsNeverEnforces(t *testing.T) {
	m := NewNoop()
	det := m.ObserveChunks(context.Background(), "tool_result", 1, []string{fakeLiveAWSKey})
	if len(det.Patterns) != 0 || det.Transition {
		t.Errorf("Noop must not detect, got %+v", det)
	}
	if m.TripFromGuard("g1", "sensitive_data") {
		t.Error("Noop must not trip from a guard")
	}
	if m.Tripped() {
		t.Error("Noop must never be tripped")
	}
	if m.Enforcing() {
		t.Error("Noop must never enforce")
	}
	if got := m.Action(); got != "warn" {
		t.Errorf("Noop Action() = %q, want %q", got, "warn")
	}
	if out, n := m.Redact("anything " + fakeLiveAWSKey); out != "anything "+fakeLiveAWSKey || n != 0 {
		t.Errorf("Noop Redact must pass through unchanged, got (%q, %d)", out, n)
	}
}
