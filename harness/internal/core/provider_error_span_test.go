package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/rxbynerd/stirrup/harness/internal/provider"
)

// TestLoop_ProviderErrorSpanIsScrubbed pins that a provider error carrying
// a credential reaches the provider.stream span's exception event and
// status only in scrubbed form, on both the request and the stream path.
func TestLoop_ProviderErrorSpanIsScrubbed(t *testing.T) {
	const probe = "AKIAIOSFODNN7EXAMPLE"
	probeErr := errors.New("upstream rejected key " + probe)

	cases := map[string]provider.ProviderAdapter{
		"request error": &errorProvider{err: probeErr},
		"stream error":  &streamErrorProvider{err: probeErr},
	}
	for name, prov := range cases {
		t.Run(name, func(t *testing.T) {
			loop := buildTestLoop(nil)
			loop.Provider = prov
			exporter := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
			loop.Tracer = tp.Tracer("test")

			runTrace, err := loop.Run(context.Background(), buildTestConfig())
			if err != nil {
				t.Fatalf("Run() error: %v", err)
			}
			if runTrace.Outcome != "error" {
				t.Errorf("Outcome = %q, want error", runTrace.Outcome)
			}

			var span *tracetest.SpanStub
			for _, s := range exporter.GetSpans() {
				if s.Name == "provider.stream" {
					span = &s
				}
			}
			if span == nil {
				t.Fatal("no provider.stream span exported")
			}
			if span.Status.Code != codes.Error || strings.Contains(span.Status.Description, probe) {
				t.Errorf("span status = %+v, want an unleaked error status", span.Status)
			}

			var exceptionMessage string
			for _, ev := range span.Events {
				for _, attr := range ev.Attributes {
					if strings.Contains(attr.Value.Emit(), probe) {
						t.Errorf("span event %q attribute %s leaks the probe: %q", ev.Name, attr.Key, attr.Value.Emit())
					}
					if ev.Name == "exception" && attr.Key == "exception.message" {
						exceptionMessage = attr.Value.AsString()
					}
				}
			}
			if !strings.Contains(exceptionMessage, "[REDACTED]") {
				t.Errorf("exception.message = %q, want the scrubbed error", exceptionMessage)
			}
		})
	}
}
