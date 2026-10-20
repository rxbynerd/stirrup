package otlp

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// otlpEnvVars are the SDK variables that can steer an exporter's endpoint
// or TLS mode. Each test clears them so the developer's environment cannot
// leak in.
var otlpEnvVars = []string{
	"OTEL_EXPORTER_OTLP_ENDPOINT",
	"OTEL_EXPORTER_OTLP_INSECURE",
	"OTEL_EXPORTER_OTLP_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
	"OTEL_EXPORTER_OTLP_TRACES_INSECURE",
	"OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_METRICS_INSECURE",
	"OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE",
	"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT",
	"OTEL_EXPORTER_OTLP_LOGS_INSECURE",
	"OTEL_EXPORTER_OTLP_LOGS_CERTIFICATE",
}

func clearOTLPEnv(t *testing.T) {
	t.Helper()
	for _, k := range otlpEnvVars {
		t.Setenv(k, "")
	}
}

// sendOne builds the exporter for sig and exports a single item, giving up
// when ctx is done. firstBytesOn calls it from a goroutine, so it reports
// failures with t.Errorf, which is safe off the test goroutine.
func sendOne(ctx context.Context, t *testing.T, sig Signal, s Settings) {
	shutdown := func(f func(context.Context) error) {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = f(sctx)
	}
	switch sig {
	case SignalTraces:
		exp, err := NewTraceExporter(ctx, s)
		if err != nil {
			t.Errorf("NewTraceExporter: %v", err)
			return
		}
		defer shutdown(exp.Shutdown)
		_ = exp.ExportSpans(ctx, tracetest.SpanStubs{{Name: "span"}}.Snapshots())
	case SignalMetrics:
		exp, err := NewMetricExporter(ctx, s)
		if err != nil {
			t.Errorf("NewMetricExporter: %v", err)
			return
		}
		defer shutdown(exp.Shutdown)
		_ = exp.Export(ctx, &metricdata.ResourceMetrics{
			ScopeMetrics: []metricdata.ScopeMetrics{{
				Metrics: []metricdata.Metrics{{
					Name: "otlp.test.count",
					Data: metricdata.Sum[int64]{
						Temporality: metricdata.CumulativeTemporality,
						IsMonotonic: true,
						DataPoints:  []metricdata.DataPoint[int64]{{Value: 1}},
					},
				}},
			}},
		})
	case SignalLogs:
		exp, err := NewLogExporter(ctx, s)
		if err != nil {
			t.Errorf("NewLogExporter: %v", err)
			return
		}
		defer shutdown(exp.Shutdown)
		var rec sdklog.Record
		rec.SetBody(attribute.StringValue("hello"))
		_ = exp.Export(ctx, []sdklog.Record{rec})
	}
}

// firstBytes points the sig exporter at a raw TCP listener and returns the
// first four bytes the client sends: 0x16 opens a TLS ClientHello, "PRI "
// opens a plaintext HTTP/2 (gRPC) preface, and "POST" a plaintext HTTP/1.1
// request.
func firstBytes(t *testing.T, sig Signal, protocol string, endpointFor func(addr string) string) []byte {
	t.Helper()
	must := require.New(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(err)
	return firstBytesOn(t, ln, sig, protocol, endpointFor)
}

func firstBytesOn(t *testing.T, ln net.Listener, sig Signal, protocol string, endpointFor func(addr string) string) []byte {
	t.Helper()
	defer func() { _ = ln.Close() }()

	got := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 4)
		n, _ := io.ReadFull(conn, buf)
		got <- buf[:n]
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		sendOne(ctx, t, sig, Settings{Protocol: protocol, Endpoint: endpointFor(ln.Addr().String())})
	}()

	var b []byte
	select {
	case b = <-got:
	case <-ctx.Done():
	}
	cancel()
	<-done
	return b
}

func isTLSClientHello(b []byte) bool { return len(b) > 0 && b[0] == 0x16 }

// The scheme of an explicit endpoint decides TLS for every signal and
// protocol, whatever OTEL_EXPORTER_OTLP_* variables the platform injects.
func TestExporters_SchemeDecidesTLSOverEnvironment(t *testing.T) {
	signals := []Signal{SignalTraces, SignalMetrics, SignalLogs}
	protocols := []string{ProtocolGRPC, ProtocolHTTPProtobuf}
	envCases := []struct {
		name string
		env  func(sig Signal) map[string]string
	}{
		{"clean env", func(Signal) map[string]string { return nil }},
		{"generic insecure", func(Signal) map[string]string {
			return map[string]string{"OTEL_EXPORTER_OTLP_INSECURE": "true"}
		}},
		{"generic http endpoint", func(Signal) map[string]string {
			return map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:9"}
		}},
		{"signal insecure", func(sig Signal) map[string]string {
			return map[string]string{"OTEL_EXPORTER_OTLP_" + strings.ToUpper(string(sig)) + "_INSECURE": "true"}
		}},
		{"signal http endpoint", func(sig Signal) map[string]string {
			return map[string]string{"OTEL_EXPORTER_OTLP_" + strings.ToUpper(string(sig)) + "_ENDPOINT": "http://localhost:9/v1/" + string(sig)}
		}},
	}

	for _, sig := range signals {
		for _, protocol := range protocols {
			for _, ec := range envCases {
				t.Run(string(sig)+"/"+protocol+"/https with "+ec.name, func(t *testing.T) {
					must := require.New(t)
					clearOTLPEnv(t)
					for k, v := range ec.env(sig) {
						t.Setenv(k, v)
					}
					b := firstBytes(t, sig, protocol, func(addr string) string { return "https://" + addr + "/otlp" })
					must.True(isTLSClientHello(b), "first bytes = %q, want a TLS ClientHello", b)
				})
			}

			want := "PRI "
			if protocol == ProtocolHTTPProtobuf {
				want = "POST"
			}
			plaintextCases := []struct {
				name        string
				genericEnv  string
				endpointFor func(addr string) string
			}{
				{"http scheme", "", func(addr string) string { return "http://" + addr }},
				{"scheme-less", "", func(addr string) string { return addr }},
				{"http endpoint beats https env", "https://localhost:9", func(addr string) string { return "http://" + addr }},
			}
			for _, pc := range plaintextCases {
				t.Run(string(sig)+"/"+protocol+"/"+pc.name, func(t *testing.T) {
					must := require.New(t)
					clearOTLPEnv(t)
					if pc.genericEnv != "" {
						t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", pc.genericEnv)
					}
					must.Equal(want, string(firstBytes(t, sig, protocol, pc.endpointFor)))
				})
			}
		}
	}
}

func TestSettingsEndpoint_EmptyIsLocalPlaintextUnlessEnvChooses(t *testing.T) {
	for _, sig := range []Signal{SignalTraces, SignalMetrics, SignalLogs} {
		upper := strings.ToUpper(string(sig))
		cases := []struct {
			name string
			env  string
			want bool
		}{
			{"no env", "", true},
			{"generic endpoint", "OTEL_EXPORTER_OTLP_ENDPOINT", false},
			{"generic insecure", "OTEL_EXPORTER_OTLP_INSECURE", false},
			{"generic certificate", "OTEL_EXPORTER_OTLP_CERTIFICATE", false},
			{"signal endpoint", "OTEL_EXPORTER_OTLP_" + upper + "_ENDPOINT", false},
			{"signal insecure", "OTEL_EXPORTER_OTLP_" + upper + "_INSECURE", false},
			{"signal certificate", "OTEL_EXPORTER_OTLP_" + upper + "_CERTIFICATE", false},
		}
		for _, tc := range cases {
			t.Run(string(sig)+"/"+tc.name, func(t *testing.T) {
				must := require.New(t)
				clearOTLPEnv(t)
				if tc.env != "" {
					t.Setenv(tc.env, "x")
				}
				ep, err := Settings{}.endpoint(sig)
				must.NoError(err)
				must.Empty(ep.url, "endpoint = %+v", ep)
				must.Equal(tc.want, ep.localPlaintext, "endpoint = %+v", ep)
			})
		}
	}
}

// With no endpoint configured anywhere, every exporter reaches the SDK's
// default local collector port without TLS.
func TestExporters_EmptyEndpointDialsLocalCollectorInPlaintext(t *testing.T) {
	cases := []struct {
		protocol string
		port     string
		want     string
	}{
		{ProtocolGRPC, "4317", "PRI "},
		{ProtocolHTTPProtobuf, "4318", "POST"},
	}
	for _, sig := range []Signal{SignalTraces, SignalMetrics, SignalLogs} {
		for _, tc := range cases {
			t.Run(string(sig)+"/"+tc.protocol, func(t *testing.T) {
				must := require.New(t)
				clearOTLPEnv(t)
				ln, err := net.Listen("tcp", "127.0.0.1:"+tc.port)
				if err != nil {
					t.Skipf("default collector port %s is in use: %v", tc.port, err)
				}
				b := firstBytesOn(t, ln, sig, tc.protocol, func(string) string { return "" })
				must.Equal(tc.want, string(b))
			})
		}
	}
}
