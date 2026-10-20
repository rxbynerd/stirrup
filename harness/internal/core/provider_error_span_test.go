package core

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
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
			must := require.New(t)
			loop := buildTestLoop(nil)
			loop.Provider = prov
			exporter := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
			loop.Tracer = tp.Tracer("test")

			runTrace, err := loop.Run(context.Background(), buildTestConfig())
			must.NoError(err)
			must.Equal("error", runTrace.Outcome)

			var span *tracetest.SpanStub
			for _, s := range exporter.GetSpans() {
				if s.Name == "provider.stream" {
					span = &s
				}
			}
			must.NotNil(span, "no provider.stream span exported")
			must.Equal(codes.Error, span.Status.Code)
			must.NotContains(span.Status.Description, probe)

			var exceptionMessage string
			for _, ev := range span.Events {
				for _, attr := range ev.Attributes {
					must.NotContains(attr.Value.String(), probe,
						"span event %q attribute %s", ev.Name, attr.Key)
					if ev.Name == "exception" && attr.Key == "exception.message" {
						exceptionMessage = attr.Value.AsString()
					}
				}
			}
			must.Contains(exceptionMessage, "[REDACTED]")
		})
	}
}
