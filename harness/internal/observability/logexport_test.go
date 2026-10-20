package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/observability/otlp"
)

// captureStore holds the records captured by a captureHandler tree. It is
// shared (by pointer) across WithAttrs / WithGroup derivations so the
// handler the logger ends up using writes into the same slice the test
// inspects — slog calls WithAttrs for the .With("runId", ...) default, which
// would otherwise strand records in a derived handler.
type captureStore struct {
	mu      sync.Mutex
	records []slog.Record
}

// captureHandler is a leaf slog.Handler that records every record it
// receives into a shared store. It stands in for the OTLP-bridge leaf so
// tests can assert what reaches the export path without standing up a
// collector.
type captureHandler struct {
	store *captureStore
	attrs []slog.Attr
}

func newCaptureHandler() *captureHandler {
	return &captureHandler{store: &captureStore{}}
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := r.Clone()
	rec.AddAttrs(h.attrs...)
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	h.store.records = append(h.store.records, rec)
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &captureHandler{
		store: h.store,
		attrs: append(append([]slog.Attr{}, h.attrs...), attrs...),
	}
}

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// snapshot returns a copy of the captured records.
func (h *captureHandler) snapshot() []slog.Record {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	out := make([]slog.Record, len(h.store.records))
	copy(out, h.store.records)
	return out
}

// attrValue returns the string value of the named attribute on a record, or
// "" with ok=false when absent.
func attrValue(r slog.Record, key string) (string, bool) {
	var out string
	var found bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out = a.Value.String()
			found = true
			return false
		}
		return true
	})
	return out, found
}

// TestNewLoggerWithExport_NilHandlerIsStderrOnly asserts that passing a nil
// export handler (the disabled default) produces JSON on the writer and does
// not fan out anywhere else — the stderr-only behaviour is unchanged.
func TestNewLoggerWithExport_NilHandlerIsStderrOnly(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	logger := NewLoggerWithExport("run-1", slog.LevelInfo, &buf, nil, nil)

	logger.Info("hello", "k", "v")

	var record map[string]any
	err := json.Unmarshal(buf.Bytes(), &record)
	must.NoError(err, "stderr output is not valid JSON:\n%s", buf.String())
	must.Equal("hello", record["msg"])
}

// TestNewLoggerWithExport_FansOutToBothSinks asserts that with an export
// handler wired, the same record reaches the stderr JSON sink and the export
// sink.
func TestNewLoggerWithExport_FansOutToBothSinks(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var buf bytes.Buffer
	sink := newCaptureHandler()
	logger := NewLoggerWithExport("run-1", slog.LevelInfo, &buf, nil, sink)

	logger.Info("fanned out", "k", "v")

	check.Contains(buf.String(), "fanned out", "message missing from stderr sink")

	records := sink.snapshot()
	must.Len(records, 1, "export sink records")
	must.Equal("fanned out", records[0].Message)
}

// TestNewLoggerWithExport_ScrubsOnExportPath asserts that a secret is
// redacted on the export path too — the scrubber sits above the fan-out, so
// the captured record carries the [REDACTED] placeholder rather than the raw
// key.
func TestNewLoggerWithExport_ScrubsOnExportPath(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	var buf bytes.Buffer
	sink := newCaptureHandler()
	logger := NewLoggerWithExport("run-1", slog.LevelInfo, &buf, nil, sink)

	logger.Info("auth", slog.String("token", anthropicKeyFixture))

	check.NotContains(buf.String(), anthropicKeyFixture, "secret leaked into stderr sink")

	records := sink.snapshot()
	must.Len(records, 1, "export sink records")
	token, ok := attrValue(records[0], "token")
	must.True(ok, "token attribute missing on export record")
	check.NotContains(token, "sk-ant-api03", "secret leaked into export sink")
	check.Contains(token, "[REDACTED]", "expected [REDACTED] on export path")
}

// TestNewLoggerWithExport_CorrelatesOnExportPath asserts the span handler
// also enriches records on the export path: a record emitted via InfoContext
// inside a span carries trace_id / span_id on the captured export record.
func TestNewLoggerWithExport_CorrelatesOnExportPath(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	const (
		traceHex = "0102030405060708090a0b0c0d0e0f10"
		spanHex  = "0102030405060708"
	)
	var buf bytes.Buffer
	sink := newCaptureHandler()
	logger := NewLoggerWithExport("run-1", slog.LevelInfo, &buf, nil, sink)

	logger.InfoContext(ctxWithSpan(t, traceHex, spanHex), "inside span")

	records := sink.snapshot()
	must.Len(records, 1, "export sink records")
	traceID, _ := attrValue(records[0], "trace_id")
	check.Equal(traceHex, traceID, "export record trace_id")
	spanID, _ := attrValue(records[0], "span_id")
	check.Equal(spanHex, spanID, "export record span_id")
}

// TestNewLogExporter_ConstructsWithoutDialing asserts the OTLP/gRPC log
// exporter constructs and returns a usable handler without contacting the
// endpoint (the gRPC exporter dials lazily on first export), and that Close
// on the returned exporter is safe.
func TestNewLogExporter_ConstructsWithoutDialing(t *testing.T) {
	must := require.New(t)
	exp, handler, err := NewLogExporter(context.Background(), otlp.Settings{Endpoint: "localhost:4317"}, ResourceOptions{})
	must.NoError(err)
	must.NotNil(handler, "NewLogExporter returned a nil handler")
	must.NotNil(exp, "NewLogExporter returned a nil exporter")
	must.NoError(exp.Close())
}

// TestLogExporter_NilReceiverSafe asserts Flush and Close tolerate a nil
// receiver so the factory can defer them unconditionally.
func TestLogExporter_NilReceiverSafe(t *testing.T) {
	var e *LogExporter

	t.Run("flush on nil receiver", func(t *testing.T) {
		must := require.New(t)
		must.NoError(e.Flush(context.Background()))
	})

	t.Run("close on nil receiver", func(t *testing.T) {
		must := require.New(t)
		must.NoError(e.Close())
	})
}
