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
