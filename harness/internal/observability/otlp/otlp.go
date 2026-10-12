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
	// always selects TLS; "http://" or no scheme selects plaintext. For
	// OTLP/HTTP a URL path is kept as a gateway prefix and the signal
	// segment is appended to it. Empty defers to the SDK's
	// OTEL_EXPORTER_OTLP_* environment variables and defaults.
	Endpoint string

	// Headers are sent on every export request. Values must already have
	// "secret://" references resolved.
	Headers map[string]string

	// HTTPClient replaces the SDK's HTTP client for OTLP/HTTP, letting a
	// caller wrap the transport. It must carry an explicit timeout.
	// Rejected for gRPC, which has no HTTP transport to wrap.
	HTTPClient *http.Client
}

// endpoint is Settings.Endpoint split into the pieces the SDK options take.
type endpoint struct {
	host     string
	basePath string
	insecure bool
}

func parseEndpoint(raw string) endpoint {
	ep := endpoint{insecure: true}
	rest := raw
	switch {
	case strings.HasPrefix(raw, "https://"):
		rest = strings.TrimPrefix(raw, "https://")
		ep.insecure = false
	case strings.HasPrefix(raw, "http://"):
		rest = strings.TrimPrefix(raw, "http://")
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		ep.host, ep.basePath = rest[:i], rest[i:]
	} else {
		ep.host = rest
	}
	return ep
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

// NewTraceExporter returns the OTLP span exporter described by s.
func NewTraceExporter(ctx context.Context, s Settings) (sdktrace.SpanExporter, error) {
	grpc, err := s.useGRPC()
	if err != nil {
		return nil, err
	}
	if grpc {
		var opts []otlptracegrpc.Option
		if s.Endpoint != "" {
			ep := parseEndpoint(s.Endpoint)
			opts = append(opts, otlptracegrpc.WithEndpoint(ep.host))
			if ep.insecure {
				opts = append(opts, otlptracegrpc.WithInsecure())
			}
		}
		if len(s.Headers) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(s.Headers))
		}
		return otlptracegrpc.New(ctx, opts...)
	}
	var opts []otlptracehttp.Option
	if s.Endpoint != "" {
		ep := parseEndpoint(s.Endpoint)
		opts = append(opts,
			otlptracehttp.WithEndpoint(ep.host),
			otlptracehttp.WithURLPath(SignalTraces.urlPath(ep.basePath)),
		)
		if ep.insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
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
	if grpc {
		var opts []otlpmetricgrpc.Option
		if s.Endpoint != "" {
			ep := parseEndpoint(s.Endpoint)
			opts = append(opts, otlpmetricgrpc.WithEndpoint(ep.host))
			if ep.insecure {
				opts = append(opts, otlpmetricgrpc.WithInsecure())
			}
		}
		if len(s.Headers) > 0 {
			opts = append(opts, otlpmetricgrpc.WithHeaders(s.Headers))
		}
		return otlpmetricgrpc.New(ctx, opts...)
	}
	var opts []otlpmetrichttp.Option
	if s.Endpoint != "" {
		ep := parseEndpoint(s.Endpoint)
		opts = append(opts,
			otlpmetrichttp.WithEndpoint(ep.host),
			otlpmetrichttp.WithURLPath(SignalMetrics.urlPath(ep.basePath)),
		)
		if ep.insecure {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
	}
	if len(s.Headers) > 0 {
		opts = append(opts, otlpmetrichttp.WithHeaders(s.Headers))
	}
	if s.HTTPClient != nil {
		opts = append(opts, otlpmetrichttp.WithHTTPClient(s.HTTPClient))
	}
	return otlpmetrichttp.New(ctx, opts...)
}

// NewLogExporter returns the OTLP log exporter described by s.
func NewLogExporter(ctx context.Context, s Settings) (sdklog.Exporter, error) {
	grpc, err := s.useGRPC()
	if err != nil {
		return nil, err
	}
	if grpc {
		var opts []otlploggrpc.Option
		if s.Endpoint != "" {
			ep := parseEndpoint(s.Endpoint)
			opts = append(opts, otlploggrpc.WithEndpoint(ep.host))
			if ep.insecure {
				opts = append(opts, otlploggrpc.WithInsecure())
			}
		}
		if len(s.Headers) > 0 {
			opts = append(opts, otlploggrpc.WithHeaders(s.Headers))
		}
		return otlploggrpc.New(ctx, opts...)
	}
	var opts []otlploghttp.Option
	if s.Endpoint != "" {
		ep := parseEndpoint(s.Endpoint)
		opts = append(opts,
			otlploghttp.WithEndpoint(ep.host),
			otlploghttp.WithURLPath(SignalLogs.urlPath(ep.basePath)),
		)
		if ep.insecure {
			opts = append(opts, otlploghttp.WithInsecure())
		}
	}
	if len(s.Headers) > 0 {
		opts = append(opts, otlploghttp.WithHeaders(s.Headers))
	}
	if s.HTTPClient != nil {
		opts = append(opts, otlploghttp.WithHTTPClient(s.HTTPClient))
	}
	return otlploghttp.New(ctx, opts...)
}
