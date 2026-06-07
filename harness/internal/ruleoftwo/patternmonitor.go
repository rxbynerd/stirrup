package ruleoftwo

import (
	"context"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/rxbynerd/stirrup/harness/internal/security"
)

// RedactionPlaceholder replaces each latch-tier sensitive span rewritten
// by PatternMonitor.Redact. Fixed (not per-pattern) so the placeholder
// never echoes anything derived from the matched content.
const RedactionPlaceholder = "[redacted:sensitive-data]"

// PatternMonitor is the deterministic Monitor backed by
// security.DetectSensitive. The latch is an atomic.Bool with no reset
// path; the criteria set is immutable after construction, so the only
// mutable state shared across goroutines is the latch itself.
type PatternMonitor struct {
	enforcing bool
	action    string
	criteria  map[string]struct{}
	tripped   atomic.Bool
}

var _ Monitor = (*PatternMonitor)(nil)

// NewPatternMonitor constructs the deterministic monitor. action is the
// configured onDetect (already defaulted by the factory); guardCriteria
// is the set of guard Decision.Criterion values that ratchet the latch.
// When staticallySensitive is true — the operator declared sensitivity
// in the config, so the validator already adjudicated the case — the
// latch starts tripped and no transition is ever reported.
func NewPatternMonitor(enforcing bool, action string, guardCriteria []string, staticallySensitive bool) *PatternMonitor {
	m := &PatternMonitor{
		enforcing: enforcing,
		action:    action,
		criteria:  make(map[string]struct{}, len(guardCriteria)),
	}
	for _, c := range guardCriteria {
		m.criteria[c] = struct{}{}
	}
	if staticallySensitive {
		m.tripped.Store(true)
	}
	return m
}

// ObserveChunks scans each chunk with security.DetectSensitive,
// aggregates findings as a set of pattern names (a single secret can
// produce two overlapping findings — sk-ant- keys match both the
// anthropic and openai patterns, "Bearer eyJ..." matches bearer_token
// and oidc_jwt), and latches on any latch-tier finding.
func (m *PatternMonitor) ObserveChunks(_ context.Context, _ string, _ int, chunks []string) Detection {
	// Once latched, further scans buy nothing for every action EXCEPT
	// redact: the latch is one-way, the gate (block-external /
	// ask-upstream) keys on Tripped() alone, and abort already
	// short-circuited the run — so re-scanning only burns the detector's
	// ~80ms/MB. Redact is the exception: it rewrites matched spans on
	// every just-arrived tool result for the rest of the run, so it must
	// keep scanning post-latch. Warn-tier telemetry is therefore also
	// dark post-latch for non-redact actions (the rule_of_two_triggered
	// event's scanning_suspended field records where that data ends).
	if m.tripped.Load() && !m.continuesScanning() {
		return Detection{}
	}
	var names []string
	seen := make(map[string]struct{})
	latch := false
	for _, chunk := range chunks {
		for _, f := range security.DetectSensitive(chunk) {
			if _, dup := seen[f.Name]; !dup {
				seen[f.Name] = struct{}{}
				names = append(names, f.Name)
			}
			if f.Tier == security.TierLatch {
				latch = true
			}
		}
	}
	if len(names) == 0 {
		return Detection{}
	}
	det := Detection{Patterns: names, Tier: security.TierWarn}
	if latch {
		det.Tier = security.TierLatch
		det.Transition = m.tripped.CompareAndSwap(false, true)
	}
	return det
}

// TripFromGuard latches when criterion is in the configured set.
// Returns true only for the call that wins the false→true flip.
func (m *PatternMonitor) TripFromGuard(_, criterion string) bool {
	if _, ok := m.criteria[criterion]; !ok {
		return false
	}
	return m.tripped.CompareAndSwap(false, true)
}

// Tripped reports the latch state.
func (m *PatternMonitor) Tripped() bool {
	return m.tripped.Load()
}

// Enforcing reports whether detections may change run behaviour.
func (m *PatternMonitor) Enforcing() bool {
	return m.enforcing
}

// Action returns the configured onDetect when enforcing, "warn"
// otherwise.
func (m *PatternMonitor) Action() string {
	if !m.enforcing {
		return "warn"
	}
	return m.action
}

// continuesScanning reports whether ObserveChunks keeps scanning after
// the latch trips. Only the enforcing redact action does — it rewrites
// matched spans on every subsequent tool result — so the effective
// action (Action()) being exactly "redact" is the condition.
func (m *PatternMonitor) continuesScanning() bool {
	return m.Action() == "redact"
}

// Redact replaces every latch-tier span reported by DetectSensitive
// with RedactionPlaceholder. Overlapping spans (one secret matched by
// two patterns) are merged so the output carries a single placeholder
// per contiguous sensitive region. Warn-tier spans are left intact.
func (m *PatternMonitor) Redact(content string) (string, int) {
	findings := security.DetectSensitive(content)
	spans := make([][2]int, 0, len(findings))
	for _, f := range findings {
		if f.Tier == security.TierLatch {
			spans = append(spans, [2]int{f.Start, f.End})
		}
	}
	if len(spans) == 0 {
		return content, 0
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i][0] != spans[j][0] {
			return spans[i][0] < spans[j][0]
		}
		return spans[i][1] > spans[j][1]
	})
	var b strings.Builder
	count := 0
	pos := 0
	for _, s := range spans {
		if s[0] < pos {
			// Overlaps a span already replaced: swallow the tail of the
			// secret without emitting a second placeholder.
			if s[1] > pos {
				pos = s[1]
			}
			continue
		}
		b.WriteString(content[pos:s[0]])
		b.WriteString(RedactionPlaceholder)
		pos = s[1]
		count++
	}
	b.WriteString(content[pos:])
	return b.String(), count
}
