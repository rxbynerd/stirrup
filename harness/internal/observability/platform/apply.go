package platform

import (
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability/otlp"
	"github.com/rxbynerd/stirrup/types"
)

// defaultTraceEndpoint is the trace exporter address when neither the
// config nor a profile names one.
const defaultTraceEndpoint = "localhost:4317"

// exportRequestTimeout bounds each OTLP/HTTP export request made through
// a profile-wrapped transport.
const exportRequestTimeout = 30 * time.Second

// Input is the explicit configuration Apply resolves against a Profile.
type Input struct {
	TraceEmitter types.TraceEmitterConfig

	// LogsEndpoint is observability.logsExport.endpoint.
	LogsEndpoint string

	// Headers are traceEmitter.headers with secret:// references resolved.
	Headers map[string]string

	// Getenv reads the standard OTEL_EXPORTER_OTLP_* variables; nil uses
	// os.Getenv.
	Getenv func(string) string
}

// Exporters is the resolved OTLP configuration for one run: what each
// signal's exporter dials and the run-wide resource and span decoration.
type Exporters struct {
	// Platform is the selected profile name, empty when none.
	Platform string

	Traces otlp.Settings

	// Metrics has an empty Endpoint when no metrics endpoint is
	// configured, which disables metric export.
	Metrics otlp.Settings

	// Logs has an empty Endpoint when the log exporter should take its
	// address from the OTEL_EXPORTER_OTLP_* environment.
	Logs otlp.Settings

	ResourceAttrs    []attribute.KeyValue
	WrapSpanExporter func(sdktrace.SpanExporter) sdktrace.SpanExporter
}

// Apply resolves each signal's exporter settings from in and p. For every
// setting the order is: the explicit RunConfig field, then a standard
// OTEL_EXPORTER_OTLP_* variable where the exporter already honours one,
// then the profile's default, then the SDK default. A per-signal endpoint
// inherits traceEmitter.endpoint only when the profile has no default for
// that signal. The profile's headers and transport wrapper apply only to
// signals sent to the profile's own endpoint, so credentials it adds never
// reach a collector a signal was redirected to. Apply never modifies in.
func Apply(in Input, p Profile) (Exporters, error) {
	cfg := in.TraceEmitter
	getenv := in.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}

	protocol := cfg.Protocol
	if p.RequiredProtocol != "" {
		switch protocol {
		case "":
			protocol = p.RequiredProtocol
		case p.RequiredProtocol:
		default:
			return Exporters{}, fmt.Errorf("platform %q requires protocol %s, but traceEmitter.protocol is %q",
				p.name, p.RequiredProtocol, protocol)
		}
	}
	traces := p.Signals[otlp.SignalTraces]
	if p.RequiredProtocol == otlp.ProtocolHTTPProtobuf && cfg.Endpoint == "" && traces.Endpoint == "" {
		return Exporters{}, fmt.Errorf("platform %q exports over %s and has no trace endpoint; set traceEmitter.endpoint or the platform option that names the backend",
			p.name, otlp.ProtocolHTTPProtobuf)
	}
	if protocol == "" || protocol == otlp.ProtocolGRPC {
		if p.WrapTransport != nil || profileAddsHeaders(p) {
			return Exporters{}, fmt.Errorf("platform %q signs or adds headers to export requests, which requires protocol %s",
				p.name, otlp.ProtocolHTTPProtobuf)
		}
	}
	if p.WrapTransport != nil && hasHeader(in.Headers, "Authorization") {
		return Exporters{}, fmt.Errorf("platform %q signs export requests and owns the Authorization header; remove it from traceEmitter.headers",
			p.name)
	}

	metrics := p.Signals[otlp.SignalMetrics]
	logs := p.Signals[otlp.SignalLogs]

	traceEndpoint := firstNonEmpty(cfg.Endpoint, traces.Endpoint, defaultTraceEndpoint)

	metricsEndpoint := cfg.MetricsEndpoint
	if metricsEndpoint == "" {
		metricsEndpoint = metrics.Endpoint
		if metricsEndpoint == "" {
			metricsEndpoint = cfg.Endpoint
		}
	}

	logsEndpoint := in.LogsEndpoint
	if logsEndpoint == "" {
		switch {
		case logs.Endpoint == "":
			logsEndpoint = cfg.Endpoint
		case getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT") == "" && getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "":
			logsEndpoint = logs.Endpoint
		}
	}

	settings := func(sig otlp.Signal, endpoint string, defaults SignalDefaults) otlp.Settings {
		s := otlp.Settings{
			Protocol: protocol,
			Endpoint: endpoint,
			Headers:  in.Headers,
		}
		if !defaults.owns(endpoint) {
			return s
		}
		s.Headers = signalHeaders(sig, in.Headers, defaults.Headers, getenv)
		if p.WrapTransport != nil {
			s.HTTPClient = &http.Client{
				Timeout:       exportRequestTimeout,
				Transport:     p.WrapTransport(sig, newExportTransport()),
				CheckRedirect: refuseRedirect,
			}
		}
		return s
	}

	out := Exporters{
		Platform:         p.name,
		Traces:           settings(otlp.SignalTraces, traceEndpoint, traces),
		Metrics:          settings(otlp.SignalMetrics, metricsEndpoint, metrics),
		Logs:             settings(otlp.SignalLogs, logsEndpoint, logs),
		ResourceAttrs:    p.ResourceAttrs,
		WrapSpanExporter: p.WrapSpanExporter,
	}
	out.Metrics.Temporality = cfg.MetricsTemporality
	return out, nil
}

// owns reports whether a signal resolved to endpoint goes to the
// profile's own endpoint, which is always the case for a profile that
// sets none.
func (d SignalDefaults) owns(endpoint string) bool {
	return d.Endpoint == "" || strings.TrimRight(endpoint, "/") == strings.TrimRight(d.Endpoint, "/")
}

// refuseRedirect stops a profile-wrapped client from following a
// redirect, which the wrapper would otherwise sign for the new host.
func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func profileAddsHeaders(p Profile) bool {
	for _, d := range p.Signals {
		if len(d.Headers) > 0 {
			return true
		}
	}
	return false
}

// signalHeaders merges a profile's header defaults for one signal under
// the explicit headers. The exporter reads OTEL_EXPORTER_OTLP_*HEADERS
// only when it is given no headers, so when the profile supplies some and
// the config does not, the environment's headers are merged in here.
func signalHeaders(sig otlp.Signal, explicit, defaults map[string]string, getenv func(string) string) map[string]string {
	if len(defaults) == 0 {
		return explicit
	}
	merged := maps.Clone(defaults)
	if len(explicit) == 0 {
		for k, v := range envHeaders(sig, getenv) {
			setHeader(merged, k, v)
		}
	}
	for k, v := range explicit {
		setHeader(merged, k, v)
	}
	return merged
}

// setHeader sets k to v, replacing any key that differs only in case.
func setHeader(headers map[string]string, k, v string) {
	for existing := range headers {
		if strings.EqualFold(existing, k) {
			delete(headers, existing)
		}
	}
	headers[k] = v
}

// envHeaders parses the signal's OTEL_EXPORTER_OTLP_<SIGNAL>_HEADERS, or
// OTEL_EXPORTER_OTLP_HEADERS when that is unset, as the OTel SDK does:
// comma-separated key=value pairs with URL-encoded values. Malformed
// entries are skipped.
func envHeaders(sig otlp.Signal, getenv func(string) string) map[string]string {
	raw := getenv("OTEL_EXPORTER_OTLP_" + strings.ToUpper(string(sig)) + "_HEADERS")
	if raw == "" {
		raw = getenv("OTEL_EXPORTER_OTLP_HEADERS")
	}
	if raw == "" {
		return nil
	}
	headers := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" || strings.ContainsAny(k, " \t\r\n:") {
			continue
		}
		decoded, err := url.PathUnescape(v)
		if err != nil || strings.ContainsAny(decoded, "\r\n") {
			continue
		}
		headers[k] = strings.TrimSpace(decoded)
	}
	return headers
}

// newExportTransport returns the base transport a profile wraps, matching
// the OTel SDK's default OTLP/HTTP transport.
func newExportTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
