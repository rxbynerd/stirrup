package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	oteltrace "go.opentelemetry.io/otel/trace"
)

// ctxWithSpan returns a context carrying a valid (remote) span context with
// the given trace/span IDs. A remote SpanContext is the simplest way to get
// IsValid() == true without standing up a TracerProvider.
func ctxWithSpan(t *testing.T, traceHex, spanHex string) context.Context {
	t.Helper()
	must := require.New(t)
	traceID, err := oteltrace.TraceIDFromHex(traceHex)
	must.NoError(err, "TraceIDFromHex(%q)", traceHex)
	spanID, err := oteltrace.SpanIDFromHex(spanHex)
	must.NoError(err, "SpanIDFromHex(%q)", spanHex)
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
	must := require.New(t)
	check := assert.New(t)
	const (
		traceHex = "0102030405060708090a0b0c0d0e0f10"
		spanHex  = "0102030405060708"
	)
	var buf bytes.Buffer
	logger := NewLogger("run-123", slog.LevelInfo, &buf)

	logger.InfoContext(ctxWithSpan(t, traceHex, spanHex), "inside span")

	var record map[string]any
	must.NoError(json.Unmarshal(buf.Bytes(), &record), "log output:\n%s", buf.String())
	traceID, _ := record["trace_id"].(string)
	spanID, _ := record["span_id"].(string)
	check.Equal(traceHex, traceID, "trace_id")
	check.Equal(spanHex, spanID, "span_id")
}

// TestSpanContextHandler_OmitsIDsWhenNoSpan asserts that a record emitted
// without an active span (context.Background or a non-Context call) does not
// carry trace_id / span_id, so untraced lines stay clean.
func TestSpanContextHandler_OmitsIDsWhenNoSpan(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	var buf bytes.Buffer
	logger := NewLogger("run-123", slog.LevelInfo, &buf)

	logger.InfoContext(context.Background(), "no span here")

	var record map[string]any
	must.NoError(json.Unmarshal(buf.Bytes(), &record), "log output:\n%s", buf.String())
	check.NotContains(record, "trace_id", "record: %v", record)
	check.NotContains(record, "span_id", "record: %v", record)
}

// TestSpanContextHandler_NonContextCallOmitsIDs asserts that the plain
// (non-Context) Info path, which slog drives with context.Background(),
// also produces no correlation fields.
func TestSpanContextHandler_NonContextCallOmitsIDs(t *testing.T) {
	check := assert.New(t)
	var buf bytes.Buffer
	logger := NewLogger("run-123", slog.LevelInfo, &buf)

	logger.Info("plain info")

	output := buf.String()
	check.NotContains(output, "trace_id", "correlation fields present on a non-context call: %s", output)
	check.NotContains(output, "span_id", "correlation fields present on a non-context call: %s", output)
}

// TestSpanContextHandler_ScrubsSecretsWithActiveSpan asserts the span handler
// sits outside the scrubber so a secret in an attribute is still redacted when
// a span is active (the scrubber runs on every value, including alongside the
// injected correlation IDs).
func TestSpanContextHandler_ScrubsSecretsWithActiveSpan(t *testing.T) {
	check := assert.New(t)
	var buf bytes.Buffer
	logger := NewLogger("run-123", slog.LevelInfo, &buf)

	ctx := ctxWithSpan(t, "0102030405060708090a0b0c0d0e0f10", "0102030405060708")
	logger.InfoContext(ctx, "auth", slog.String("token", anthropicKeyFixture))

	output := buf.String()
	check.NotContains(output, anthropicKeyFixture, "secret leaked despite active span: %s", output)
	check.Contains(output, "trace_id", "expected trace_id alongside the scrubbed value")
}

// TestScopedLogger_FallsBackToScopeSpan asserts that a record without a
// span of its own carries the scope's span, a record with one keeps
// it, and an invalid scope span stops the fallback.
func TestScopedLogger_FallsBackToScopeSpan(t *testing.T) {
	must := require.New(t)
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
	must.Len(lines, 3, "log output:\n%s", buf.String())

	cases := []struct {
		name    string
		traceID string
		spanID  string
	}{
		{name: "record without own span uses scope span", traceID: scopeTrace, spanID: scopeSpan},
		{name: "record with own span keeps it", traceID: ownTrace, spanID: ownSpan},
		{name: "record after scope span cleared has no IDs"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			var record map[string]any
			must.NoError(json.Unmarshal([]byte(lines[i]), &record), "record %d", i)
			traceID, _ := record["trace_id"].(string)
			spanID, _ := record["span_id"].(string)
			must.Equal(tc.traceID, traceID, "trace_id")
			must.Equal(tc.spanID, spanID, "span_id")
		})
	}
}
