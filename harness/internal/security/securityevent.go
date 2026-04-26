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
