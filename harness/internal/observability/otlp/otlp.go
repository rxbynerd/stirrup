// Package otlp builds the OTLP exporters for the trace, metric, and log
// signals from one set of endpoint, TLS, header, and transport rules, so
// every signal configured from the same RunConfig dials its collector the
// same way. See docs/observability-cloud.md for the operator-facing rules.
package otlp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Signal names an OTLP telemetry signal. Its value is the per-signal
// segment of the OTLP/HTTP request path ("/v1/<signal>").
type Signal string

const (
	SignalTraces  Signal = "traces"
	SignalMetrics Signal = "metrics"
	SignalLogs    Signal = "logs"
)

// OTLP wire protocols, matching types.TraceEmitterConfig.Protocol. The
// empty string selects ProtocolGRPC.
const (
	ProtocolGRPC         = "grpc"
	ProtocolHTTPProtobuf = "http/protobuf"
)

// Settings is the transport configuration shared by the three exporter
// constructors.
type Settings struct {
	// Protocol is "", ProtocolGRPC, or ProtocolHTTPProtobuf.
	Protocol string

	// Endpoint is host:port or an http(s):// URL. An "https://" scheme
	// always selects TLS and "http://" or no scheme selects plaintext,
	// overriding any OTEL_EXPORTER_OTLP_* endpoint or insecure variable.
	// For OTLP/HTTP a URL path is kept as a gateway prefix and the signal
	// segment is appended to it. Empty defers to the SDK's
	// OTEL_EXPORTER_OTLP_* environment variables; with none of the
	// endpoint, insecure, or certificate variables set, the SDK's default
	// local collector address is dialled in plaintext.
	Endpoint string

	// Headers are sent on every export request. Values must already have
	// "secret://" references resolved.
	Headers map[string]string

	// HTTPClient replaces the SDK's HTTP client for OTLP/HTTP, letting a
	// caller wrap the transport. It must carry an explicit timeout.
	// Rejected for gRPC, which has no HTTP transport to wrap.
	HTTPClient *http.Client

	// Temporality is the metric aggregation temporality: "",
	// TemporalityCumulative, or TemporalityDelta. Empty leaves the choice
	// to OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE and then the SDK
	// default (cumulative); the SDK reads that variable only when no
	// selector is passed. Consulted by NewMetricExporter only.
	Temporality string
}

// Metric aggregation temporalities accepted on Settings.Temporality.
const (
	TemporalityCumulative = "cumulative"
	TemporalityDelta      = "delta"
)

// temporalitySelector maps Settings.Temporality to the SDK selector, or nil
// when the SDK should apply its environment and default.
func temporalitySelector(t string) (sdkmetric.TemporalitySelector, error) {
	switch t {
	case "":
		return nil, nil
	case TemporalityCumulative:
		return sdkmetric.CumulativeTemporalitySelector, nil
	case TemporalityDelta:
		return sdkmetric.DeltaTemporalitySelector, nil
	default:
		return nil, fmt.Errorf("unsupported metrics temporality %q (allowed: cumulative, delta)", t)
	}
}

// endpoint is Settings.Endpoint split into the pieces the SDK options take.
type endpoint struct {
	// url is the normalised "scheme://host[:port]" passed to the SDK's
	// WithEndpointURL, which sets the TLS mode from the scheme after the
	// OTEL_EXPORTER_OTLP_* environment has been applied.
	url      string
	basePath string

	// localPlaintext is set when no endpoint is configured anywhere, so
	// the exporter dials the SDK's default local collector without TLS.
	localPlaintext bool
}

// parseEndpoint rejects anything but a plain http(s) host and path. Errors
// never echo the input, which may carry credentials.
func parseEndpoint(raw string) (endpoint, error) {
	normalised := raw
	if !strings.Contains(raw, "://") {
		normalised = "http://" + raw
	}
	u, err := url.Parse(normalised)
	if err != nil {
		return endpoint{}, errors.New("OTLP endpoint is not a valid host:port or URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return endpoint{}, errors.New("OTLP endpoint scheme must be http or https")
	case u.Host == "":
		return endpoint{}, errors.New("OTLP endpoint has no host")
	case u.User != nil:
		return endpoint{}, errors.New("OTLP endpoint must not carry userinfo; send credentials as headers")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return endpoint{}, errors.New("OTLP endpoint must not carry a query or fragment")
	}
	return endpoint{
		url:      u.Scheme + "://" + u.Host,
		basePath: u.Path,
	}, nil
}

// urlPath returns the OTLP/HTTP request path for sig under basePath,
// preserving a gateway prefix such as "/otlp".
func (sig Signal) urlPath(basePath string) string {
	return strings.TrimRight(basePath, "/") + "/v1/" + string(sig)
}

// useGRPC reports whether s selects OTLP/gRPC, rejecting unknown protocols
// and an HTTP client paired with gRPC.
func (s Settings) useGRPC() (bool, error) {
	switch s.Protocol {
	case "", ProtocolGRPC:
		if s.HTTPClient != nil {
			return true, errors.New("an OTLP HTTP client requires protocol http/protobuf")
		}
		return true, nil
	case ProtocolHTTPProtobuf:
		return false, nil
	default:
		return false, fmt.Errorf("unsupported OTLP protocol %q (allowed: grpc, http/protobuf)", s.Protocol)
	}
}

// endpoint parses s.Endpoint. An endpoint with an empty url leaves the
// SDK to read its environment.
func (s Settings) endpoint(sig Signal) (endpoint, error) {
	if s.Endpoint == "" {
		return endpoint{localPlaintext: !envSelectsTransport(sig)}, nil
	}
	return parseEndpoint(s.Endpoint)
}

// envSelectsTransport reports whether an OTEL_EXPORTER_OTLP_* variable
// chooses the endpoint or TLS mode for sig, matching the SDK's rule that
// a blank value counts as unset.
func envSelectsTransport(sig Signal) bool {
	perSignal := "OTEL_EXPORTER_OTLP_" + strings.ToUpper(string(sig)) + "_"
	for _, setting := range []string{"ENDPOINT", "INSECURE", "CERTIFICATE"} {
		for _, name := range []string{"OTEL_EXPORTER_OTLP_" + setting, perSignal + setting} {
			if strings.TrimSpace(os.Getenv(name)) != "" {
				return true
			}
		}
	}
	return false
}

// NewTraceExporter returns the OTLP span exporter described by s.
func NewTraceExporter(ctx context.Context, s Settings) (sdktrace.SpanExporter, error) {
	grpc, err := s.useGRPC()
	if err != nil {
		return nil, err
	}
	ep, err := s.endpoint(SignalTraces)
	if err != nil {
		return nil, err
	}
	if grpc {
		var opts []otlptracegrpc.Option
		if ep.url != "" {
			opts = append(opts, otlptracegrpc.WithEndpointURL(ep.url))
		} else if ep.localPlaintext {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		if len(s.Headers) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(s.Headers))
		}
		return otlptracegrpc.New(ctx, opts...)
	}
	var opts []otlptracehttp.Option
	if ep.url != "" {
		opts = append(opts,
			otlptracehttp.WithEndpointURL(ep.url),
			otlptracehttp.WithURLPath(SignalTraces.urlPath(ep.basePath)),
		)
	} else if ep.localPlaintext {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	if len(s.Headers) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(s.Headers))
	}
	if s.HTTPClient != nil {
		opts = append(opts, otlptracehttp.WithHTTPClient(s.HTTPClient))
	}
	return otlptracehttp.New(ctx, opts...)
}

// NewMetricExporter returns the OTLP metric exporter described by s.
func NewMetricExporter(ctx context.Context, s Settings) (sdkmetric.Exporter, error) {
	grpc, err := s.useGRPC()
	if err != nil {
		return nil, err
	}
	ep, err := s.endpoint(SignalMetrics)
	if err != nil {
		return nil, err
	}
	selector, err := temporalitySelector(s.Temporality)
	if err != nil {
		return nil, err
	}
	if grpc {
		var opts []otlpmetricgrpc.Option
		if ep.url != "" {
			opts = append(opts, otlpmetricgrpc.WithEndpointURL(ep.url))
		} else if ep.localPlaintext {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		if len(s.Headers) > 0 {
			opts = append(opts, otlpmetricgrpc.WithHeaders(s.Headers))
		}
		if selector != nil {
			opts = append(opts, otlpmetricgrpc.WithTemporalitySelector(selector))
		}
		return otlpmetricgrpc.New(ctx, opts...)
	}
	var opts []otlpmetrichttp.Option
	if ep.url != "" {
		opts = append(opts,
			otlpmetrichttp.WithEndpointURL(ep.url),
			otlpmetrichttp.WithURLPath(SignalMetrics.urlPath(ep.basePath)),
		)
	} else if ep.localPlaintext {
		opts = append(opts, otlpmetrichttp.WithInsecure())
	}
	if len(s.Headers) > 0 {
		opts = append(opts, otlpmetrichttp.WithHeaders(s.Headers))
	}
	if s.HTTPClient != nil {
		opts = append(opts, otlpmetrichttp.WithHTTPClient(s.HTTPClient))
	}
	if selector != nil {
		opts = append(opts, otlpmetrichttp.WithTemporalitySelector(selector))
	}
	return otlpmetrichttp.New(ctx, opts...)
}

// NewLogExporter returns the OTLP log exporter described by s.
func NewLogExporter(ctx context.Context, s Settings) (sdklog.Exporter, error) {
	grpc, err := s.useGRPC()
	if err != nil {
		return nil, err
	}
	ep, err := s.endpoint(SignalLogs)
	if err != nil {
		return nil, err
	}
	if grpc {
		var opts []otlploggrpc.Option
		if ep.url != "" {
			opts = append(opts, otlploggrpc.WithEndpointURL(ep.url))
		} else if ep.localPlaintext {
			opts = append(opts, otlploggrpc.WithInsecure())
		}
		if len(s.Headers) > 0 {
			opts = append(opts, otlploggrpc.WithHeaders(s.Headers))
		}
		return otlploggrpc.New(ctx, opts...)
	}
	var opts []otlploghttp.Option
	if ep.url != "" {
		opts = append(opts,
			otlploghttp.WithEndpointURL(ep.url),
			otlploghttp.WithURLPath(SignalLogs.urlPath(ep.basePath)),
		)
	} else if ep.localPlaintext {
		opts = append(opts, otlploghttp.WithInsecure())
	}
	if len(s.Headers) > 0 {
		opts = append(opts, otlploghttp.WithHeaders(s.Headers))
	}
	if s.HTTPClient != nil {
		opts = append(opts, otlploghttp.WithHTTPClient(s.HTTPClient))
	}
	return otlploghttp.New(ctx, opts...)
}
