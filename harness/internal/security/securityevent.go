package security

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// SecurityEvent represents a structured security event for monitoring/alerting.
type SecurityEvent struct {
	Timestamp string         `json:"timestamp"`
	Level     string         `json:"level"` // "info" | "warn" | "error"
	RunID     string         `json:"runId"`
	Event     string         `json:"event"`
	Data      map[string]any `json:"data,omitempty"`
}

// SecurityLogger emits structured security events as JSON lines.
type SecurityLogger struct {
	writer io.Writer
	mu     sync.Mutex
	runID  string
}

// NewSecurityLogger creates a SecurityLogger that writes to w.
func NewSecurityLogger(w io.Writer, runID string) *SecurityLogger {
	return &SecurityLogger{writer: w, runID: runID}
}

// Emit writes a security event as a JSON line.
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

// PermissionDenied emits when a permission policy refuses a tool call.
// This is distinct from a tool error: the tool was never invoked. The
// reason field is the human-readable string returned by the policy.
func (sl *SecurityLogger) PermissionDenied(toolName, reason string) {
	sl.Emit("warn", "permission_denied", map[string]any{
		"tool":   toolName,
		"reason": reason,
	})
}
