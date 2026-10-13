package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	oteltrace "go.opentelemetry.io/otel/trace"
)

// ctxWithSpan returns a context carrying a valid (remote) span context with
// the given trace/span IDs. A remote SpanContext is the simplest way to get
// IsValid() == true without standing up a TracerProvider.
func ctxWithSpan(t *testing.T, traceHex, spanHex string) context.Context {
	t.Helper()
	traceID, err := oteltrace.TraceIDFromHex(traceHex)
	if err != nil {
		t.Fatalf("TraceIDFromHex(%q): %v", traceHex, err)
	}
	spanID, err := oteltrace.SpanIDFromHex(spanHex)
	if err != nil {
		t.Fatalf("SpanIDFromHex(%q): %v", spanHex, err)
	}
	sc := oteltrace.NewSpanContext(oteltrace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: oteltrace.FlagsSampled,
		Remote:     true,
	})
	return oteltrace.ContextWithSpanContext(context.Background(), sc)
}

// TestSpanContextHandler_InjectsIDsWhenSpanActive asserts that a record
// emitted via InfoContext inside an active span carries lowercase trace_id
// and span_id attributes matching the span context.
func TestSpanContextHandler_InjectsIDsWhenSpanActive(t *testing.T) {
	const (
		traceHex = "0102030405060708090a0b0c0d0e0f10"
		spanHex  = "0102030405060708"
	)
	var buf bytes.Buffer
	logger := NewLogger("run-123", slog.LevelInfo, &buf)

	logger.InfoContext(ctxWithSpan(t, traceHex, spanHex), "inside span")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("unmarshal log record: %v\n%s", err, buf.String())
	}
	if got, _ := record["trace_id"].(string); got != traceHex {
		t.Errorf("trace_id = %q, want %q", got, traceHex)
	}
	if got, _ := record["span_id"].(string); got != spanHex {
		t.Errorf("span_id = %q, want %q", got, spanHex)
	}
}

// TestSpanContextHandler_OmitsIDsWhenNoSpan asserts that a record emitted
// without an active span (context.Background or a non-Context call) does not
// carry trace_id / span_id, so untraced lines stay clean.
func TestSpanContextHandler_OmitsIDsWhenNoSpan(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("run-123", slog.LevelInfo, &buf)

	logger.InfoContext(context.Background(), "no span here")

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("unmarshal log record: %v\n%s", err, buf.String())
	}
	if _, ok := record["trace_id"]; ok {
		t.Errorf("trace_id present without an active span: %v", record)
	}
	if _, ok := record["span_id"]; ok {
		t.Errorf("span_id present without an active span: %v", record)
	}
}

// TestSpanContextHandler_NonContextCallOmitsIDs asserts that the plain
// (non-Context) Info path, which slog drives with context.Background(),
// also produces no correlation fields.
func TestSpanContextHandler_NonContextCallOmitsIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("run-123", slog.LevelInfo, &buf)

	logger.Info("plain info")

	output := buf.String()
	if strings.Contains(output, "trace_id") || strings.Contains(output, "span_id") {
		t.Errorf("correlation fields present on a non-context call: %s", output)
	}
}

// TestSpanContextHandler_StillScrubs asserts the span handler sits outside
// the scrubber so a secret in an attribute is still redacted when a span is
// active (the scrubber runs on every value, including alongside the injected
// correlation IDs).
func TestSpanContextHandler_StillScrubs(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger("run-123", slog.LevelInfo, &buf)

	ctx := ctxWithSpan(t, "0102030405060708090a0b0c0d0e0f10", "0102030405060708")
	logger.InfoContext(ctx, "auth", slog.String("token", anthropicKeyFixture))

	output := buf.String()
	if strings.Contains(output, anthropicKeyFixture) {
		t.Errorf("secret leaked despite active span: %s", output)
	}
	if !strings.Contains(output, "trace_id") {
		t.Errorf("expected trace_id alongside the scrubbed value: %s", output)
	}
}

// TestScopedLogger_FallsBackToScopeSpan asserts that a record without a
// span of its own carries the scope's span, a record with one keeps
// it, and an invalid scope span stops the fallback.
func TestScopedLogger_FallsBackToScopeSpan(t *testing.T) {
	const (
		scopeTrace = "0102030405060708090a0b0c0d0e0f10"
		scopeSpan  = "0102030405060708"
		ownTrace   = "a1a2a3a4a5a6a7a8a9aaabacadaeafb0"
		ownSpan    = "b1b2b3b4b5b6b7b8"
	)
	var buf bytes.Buffer
	scope := NewRunScope("run-1")
	scope.SetSpanContext(oteltrace.SpanContextFromContext(ctxWithSpan(t, scopeTrace, scopeSpan)))
	logger := NewScopedLoggerWithExport(scope, slog.LevelInfo, &buf, nil, nil)

	logger.Info("no context")
	logger.InfoContext(ctxWithSpan(t, ownTrace, ownSpan), "own span")
	scope.SetSpanContext(oteltrace.SpanContext{})
	logger.Info("cleared")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d records, want 3:\n%s", len(lines), buf.String())
	}
	want := [][2]string{{scopeTrace, scopeSpan}, {ownTrace, ownSpan}, {"", ""}}
	for i, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("unmarshal record %d: %v", i, err)
		}
		traceID, _ := record["trace_id"].(string)
		spanID, _ := record["span_id"].(string)
		if traceID != want[i][0] || spanID != want[i][1] {
			t.Errorf("record %q: trace_id=%q span_id=%q, want %q %q", record["msg"], traceID, spanID, want[i][0], want[i][1])
		}
	}
}
