// Package platform holds the observability platform profiles selected by
// traceEmitter.platform. A profile supplies defaults for OTLP settings the
// RunConfig leaves unset; Apply merges them under the explicit config.
// Every vendor-specific name, endpoint and attribute lives in this
// package, so the trace and observability packages stay vendor-neutral.
// See docs/observability-cloud.md#platform-profiles.
package platform

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability/otlp"
)

// Profile is the set of defaults one platform contributes. Every field is
// optional; the zero value contributes nothing.
type Profile struct {
	name string

	// RequiredProtocol is the OTLP protocol the platform needs, or "" when
	// it accepts any.
	RequiredProtocol string

	// Signals holds per-signal endpoint and header defaults. A signal
	// with an Endpoint here stops inheriting traceEmitter.endpoint.
	Signals map[otlp.Signal]SignalDefaults

	// ResourceAttrs are resource attributes below OTEL_RESOURCE_ATTRIBUTES
	// and the stirrup identity attributes in precedence.
	ResourceAttrs []attribute.KeyValue

	// WrapTransport decorates the HTTP transport of each signal's
	// exporter that sends to the profile's own endpoint, for example to
	// sign requests. A profile that sets it owns the Authorization header.
	WrapTransport func(otlp.Signal, http.RoundTripper) http.RoundTripper

	// WrapSpanExporter decorates the span exporter, seeing each span's
	// final attributes at export time.
	WrapSpanExporter func(sdktrace.SpanExporter) sdktrace.SpanExporter
}

// SignalDefaults are a profile's endpoint and headers for one signal. The
// headers apply only while the signal is sent to Endpoint.
type SignalDefaults struct {
	// Endpoint is in the otlp.Settings.Endpoint form: a base URL to
	// which the exporter appends "/v1/<signal>".
	Endpoint string
	Headers  map[string]string
}

// Deps carries the external dependencies a profile builder may use. The
// zero value selects the production implementations.
type Deps struct {
	// LoadAWSConfig resolves the AWS SDK configuration through the default
	// credential chain, pinned to region when it is non-empty. Nil uses
	// config.LoadDefaultConfig.
	LoadAWSConfig func(ctx context.Context, region string) (aws.Config, error)
}

// definition describes one registered platform: the option keys it
// accepts (true marks a required key) and the builder that turns validated
// options into a Profile.
type definition struct {
	options map[string]bool
	build   func(ctx context.Context, opts map[string]string, deps Deps) (Profile, error)
}

// registry maps each traceEmitter.platform value to its definition. Its
// keys must equal types.ValidTraceEmitterPlatformValues().
var registry = map[string]definition{
	"cloudwatch": cloudwatchDefinition,
	"phoenix":    phoenixDefinition,
}

// Resolve validates opts against the named platform and builds its
// Profile. The empty name selects no platform and returns the zero
// Profile.
func Resolve(ctx context.Context, name string, opts map[string]string, deps Deps) (Profile, error) {
	if name == "" {
		if len(opts) > 0 {
			return Profile{}, fmt.Errorf("traceEmitter.platformOptions requires traceEmitter.platform")
		}
		return Profile{}, nil
	}
	def, ok := registry[name]
	if !ok {
		return Profile{}, fmt.Errorf("unknown traceEmitter.platform %q", name)
	}
	for _, key := range sortedKeys(opts) {
		if _, known := def.options[key]; !known {
			return Profile{}, fmt.Errorf("platform %q does not accept option %q (accepted: %s)",
				name, key, strings.Join(sortedKeys(def.options), ", "))
		}
	}
	for _, key := range sortedKeys(def.options) {
		if def.options[key] && opts[key] == "" {
			return Profile{}, fmt.Errorf("platform %q requires option %q", name, key)
		}
	}
	p, err := def.build(ctx, opts, deps)
	if err != nil {
		return Profile{}, fmt.Errorf("platform %q: %w", name, err)
	}
	p.name = name
	return p, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// hasHeader reports whether headers carries name, compared
// case-insensitively as HTTP header names are.
func hasHeader(headers map[string]string, name string) bool {
	return slices.ContainsFunc(sortedKeys(headers), func(k string) bool {
		return strings.EqualFold(k, name)
	})
}
