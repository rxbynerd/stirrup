package security

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// SecurityEvent represents a structured security event for monitoring/alerting.
type SecurityEvent struct {
	Timestamp string         `json:"timestamp"`
	Level     string         `json:"level"` // "info" | "warn" | "error"
	RunID     string         `json:"runId"`
	Event     string         `json:"event"`
	Data      map[string]any `json:"data,omitempty"`
}

// EventCounter is the minimal interface SecurityLogger needs to record an
// OTel SecurityEvents counter without importing the observability package
// (which would create an import cycle: observability -> security via Scrub).
//
// The contract matches metric.Int64Counter's Add method exactly so callers
// can pass observability.Metrics.SecurityEvents directly.
type EventCounter interface {
	Add(ctx context.Context, incr int64, options ...metric.AddOption)
}

// SecurityLogger emits structured security events as JSON lines.
type SecurityLogger struct {
	writer  io.Writer
	mu      sync.Mutex
	runID   string
	counter EventCounter // optional; nil means no metric recording
}

// NewSecurityLogger creates a SecurityLogger that writes to w.
func NewSecurityLogger(w io.Writer, runID string) *SecurityLogger {
	return &SecurityLogger{writer: w, runID: runID}
}

// SetEventCounter wires an OTel counter (typically Metrics.SecurityEvents)
// that is incremented once per Emit call, tagged with the event name. Pass
// nil to disable. Safe to call concurrently with Emit: writes to sl.counter
// are guarded by sl.mu, the same mutex Emit acquires before reading it.
func (sl *SecurityLogger) SetEventCounter(c EventCounter) {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	sl.counter = c
}

// Emit writes a security event as a JSON line and, if a counter was wired
// via SetEventCounter, increments it tagged with the event name.
//
// Counter increment uses context.Background() because security events are
// fire-and-forget: callers do not pass a ctx (e.g. the executor calls
// PathTraversalBlocked deep in a non-ctx-bearing helper). The counter
// implementations we use (OTel) treat ctx primarily for cancellation, which
// is not meaningful for a single Add call.
func (sl *SecurityLogger) Emit(level, event string, data map[string]any) {
	se := SecurityEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Level:     level,
		RunID:     sl.runID,
		Event:     event,
		Data:      ScrubMap(data),
	}
	b, err := json.Marshal(se)
	if err != nil {
		return
	}
	b = append(b, '\n')

	sl.mu.Lock()
	defer sl.mu.Unlock()
	_, _ = fmt.Fprint(sl.writer, string(b))

	if sl.counter != nil {
		sl.counter.Add(context.Background(), 1,
			metric.WithAttributes(attribute.String("event", event)),
		)
	}
}

// Convenience methods for the 7 spec-defined security events.

// PathTraversalBlocked emits when a path traversal attempt is rejected.
func (sl *SecurityLogger) PathTraversalBlocked(path, workspace string) {
	sl.Emit("warn", "path_traversal_blocked", map[string]any{
		"path":      path,
		"workspace": workspace,
	})
}

// ToolInputRejected emits when tool input fails JSON Schema validation.
func (sl *SecurityLogger) ToolInputRejected(toolName string, errors []string) {
	sl.Emit("warn", "tool_input_rejected", map[string]any{
		"tool":   toolName,
		"errors": errors,
	})
}

// PrototypePollutionBlocked emits when __proto__/constructor keys are stripped.
func (sl *SecurityLogger) PrototypePollutionBlocked(toolName string, keys []string) {
	sl.Emit("warn", "prototype_pollution_blocked", map[string]any{
		"tool": toolName,
		"keys": keys,
	})
}

// ConfigValidationFailed emits when RunConfig fails security invariant checks.
func (sl *SecurityLogger) ConfigValidationFailed(errors []string) {
	sl.Emit("error", "config_validation_failed", map[string]any{
		"errors": errors,
	})
}

// SecretRedactedInOutput emits when the LogScrubber detects and redacts a secret.
func (sl *SecurityLogger) SecretRedactedInOutput(pattern, location string) {
	sl.Emit("info", "secret_redacted_in_output", map[string]any{
		"pattern":  pattern,
		"location": location,
	})
}

// FileSizeLimitExceeded emits when a file read/write is blocked by size limits.
func (sl *SecurityLogger) FileSizeLimitExceeded(path string, size, limit int64) {
	sl.Emit("warn", "file_size_limit_exceeded", map[string]any{
		"path":  path,
		"size":  size,
		"limit": limit,
	})
}

// OutputTruncated emits when command output exceeds the cap.
func (sl *SecurityLogger) OutputTruncated(command string, originalSize, limit int) {
	sl.Emit("info", "output_truncated", map[string]any{
		"command":      command,
		"originalSize": originalSize,
		"limit":        limit,
	})
}

// ToolCallGuardTriggered emits when the prompt-injection tripwire rejects a
// tool call before dispatch.
func (sl *SecurityLogger) ToolCallGuardTriggered(toolName string, findings []ToolGuardFinding) {
	serialized := make([]any, 0, len(findings))
	for _, finding := range findings {
		serialized = append(serialized, map[string]any{
			"rule":   finding.Rule,
			"field":  finding.Field,
			"reason": finding.Reason,
		})
	}
	sl.Emit("warn", "tool_call_guard_triggered", map[string]any{
		"tool":     toolName,
		"findings": serialized,
	})
}

// DynamicContextSanitized emits when dynamic context is modified before prompt
// construction. The event intentionally includes metadata only.
func (sl *SecurityLogger) DynamicContextSanitized(event DynamicContextSanitizationEvent) {
	sl.Emit("warn", "dynamic_context_sanitized", map[string]any{
		"key":             event.Key,
		"originalLength":  event.OriginalLength,
		"sanitizedLength": event.SanitizedLength,
		"reasons":         event.Reasons,
	})
}

// PermissionDenied emits when a permission policy refuses a tool call.
// This is distinct from a tool error: the tool was never invoked. The
// reason field is the human-readable string returned by the policy.
func (sl *SecurityLogger) PermissionDenied(toolName, reason string) {
	sl.Emit("warn", "permission_denied", map[string]any{
		"tool":   toolName,
		"reason": reason,
	})
}

// GuardAllowed emits when a guard passed content/calls without
// modification. Note: this fires once per guard.Check call across all
// three phases (pre_turn, pre_tool, post_turn) so it can be high volume
// — in production runs, expect one event per turn at minimum. Operators
// who need quieter logs can filter on event=guard_allowed downstream.
// Logged at info level rather than debug because Emit only supports
// info/warn/error today; promoting Emit to debug is intentionally
// deferred until an operator asks.
func (sl *SecurityLogger) GuardAllowed(phase, guardID string) {
	sl.Emit("info", "guard_allowed", map[string]any{
		"phase":   phase,
		"guardId": guardID,
	})
}

// GuardSpotlighted emits when a guard returned VerdictAllowSpot — the
// content is forwarded but rewrapped via ApplySpotlight so the model
// treats it as untrusted.
func (sl *SecurityLogger) GuardSpotlighted(phase, guardID, reason string) {
	sl.Emit("warn", "guard_spotlighted", map[string]any{
		"phase":   phase,
		"guardId": guardID,
		"reason":  reason,
	})
}

// GuardDenied emits when a guard returned VerdictDeny. The criterion
// names which configured rule fired (when the adapter exposes one); the
// reason carries the adapter's human-readable explanation. Content
// itself is intentionally NOT logged: at info/warn levels a redaction
// regress would silently leak prompt-injection payloads or sensitive
// tool inputs into the security event stream. Operators who want
// content correlation can derive a hash at the call site and pass it
// through the reason field; that capability is reserved for a future
// helper if it is asked for.
func (sl *SecurityLogger) GuardDenied(phase, guardID, criterion, reason string) {
	sl.Emit("warn", "guard_denied", map[string]any{
		"phase":     phase,
		"guardId":   guardID,
		"criterion": criterion,
		"reason":    reason,
	})
}

// GuardSkipped emits when a guard short-circuited without contacting
// the upstream classifier. The canonical case is the granite-guardian
// MinChunkChars optimisation, where pre-turn chunks below the threshold
// skip the classifier outright. Distinguishing skip from allow keeps
// dashboards honest: a sudden surge of skips means tiny tool outputs
// are bypassing the guard, which an operator may want to know about.
func (sl *SecurityLogger) GuardSkipped(phase, guardID string) {
	sl.Emit("info", "guard_skipped", map[string]any{
		"phase":   phase,
		"guardId": guardID,
	})
}

// GuardError emits when a guard.Check returned a Go error. Whether the
// loop converts this into a deny or an allow is a fail-open policy
// decision; the event records the underlying error string regardless so
// operators can spot recurring transport / parse failures.
func (sl *SecurityLogger) GuardError(phase, guardID, errorMessage string) {
	sl.Emit("error", "guard_error", map[string]any{
		"phase":   phase,
		"guardId": guardID,
		"error":   errorMessage,
	})
}
