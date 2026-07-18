// Package observability provides OTel resource construction shared by the
// trace emitter and the metrics provider. Both signals must be tagged with
// the same resource so backends can correlate traces and metrics for one
// running Stirrup process.
package observability

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"regexp"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"

	"github.com/rxbynerd/stirrup/types/version"
)

// ServiceName is the value emitted as service.name on every span and metric.
// It is the canonical identifier the harness presents to OTel-aware backends
// (Zipkin, Jaeger, Tempo, Honeycomb, Datadog, etc.).
const ServiceName = "stirrup"

// DefaultServiceNamespace is the value emitted as service.namespace when no
// operator-supplied or env-var value is available. It is intentionally the
// same as ServiceName so a fresh "stirrup harness --prompt ..." run still
// groups under a sensible service-namespace label rather than leaving the
// attribute unset (which makes Grafana's group-by quietly drop rows).
const DefaultServiceNamespace = "stirrup"

// DefaultEnvironment is the value emitted as deployment.environment when no
// operator-supplied or env-var value is available. "local" matches the
// convention used by the OTel demo and Grafana sample dashboards, so the
// out-of-the-box experience hits the right tile rather than a fallback bucket.
const DefaultEnvironment = "local"

// envEnvironment / envServiceNamespace are the env-var fallbacks consulted
// when ResourceOptions does not pin a value. We adopt the OTEL_ naming prefix
// for discoverability alongside the SDK's own OTEL_RESOURCE_ATTRIBUTES.
// These are Stirrup-specific env vars — they are not part of the OTel SDK
// specification.
const (
	envEnvironment      = "OTEL_DEPLOYMENT_ENVIRONMENT"
	envServiceNamespace = "OTEL_SERVICE_NAMESPACE"
)

// observabilityLabelPattern mirrors the canonical types.observabilityLabelPattern
// used by ValidateRunConfig. It is duplicated here (rather than imported) to
// keep the observability package free of any dependency on the types package's
// validation surface, which would create an import cycle if the types package
// ever needs to read a constructed Resource. If the canonical pattern in
// types/runconfig.go changes, update this one too.
var observabilityLabelPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

var (
	instanceIDOnce sync.Once
	instanceID     string
)

// InstanceID returns a stable random identifier for the current process,
// generated once and reused for the process's lifetime. Used as
// service.instance.id so concurrent harness processes can be distinguished
// in OTel-aware backends. Falls back to "unknown" only if
// crypto/rand.Read fails.
func InstanceID() string {
	instanceIDOnce.Do(func() {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			instanceID = "unknown"
			return
		}
		instanceID = hex.EncodeToString(b[:])
	})
	return instanceID
}

// ResourceOptions carries the run-scoped attributes that ride on the OTel
// Resource. Only low-cardinality labels belong here — putting RunID,
// provider, or model on the resource would explode metric series cardinality
// on backends like Mimir, so those continue to be emitted at the span /
// instrument level.
//
// Empty values trigger fallbacks: Environment / ServiceNamespace fall through
// to the OTEL_DEPLOYMENT_ENVIRONMENT / OTEL_SERVICE_NAMESPACE env vars and
// then to the documented defaults; RunMode is omitted entirely from the
// resource when empty so read-only callers (eval baselines, traces replayed
// without a config) do not pollute the resource set with a synthetic mode.
type ResourceOptions struct {
	Environment      string
	ServiceNamespace string
	// RunMode is derived from RunConfig.Mode — not from ObservabilityConfig.
	// Do not add it to ObservabilityConfig or the proto message: it is a
	// derived attribute, not an operator-configurable label.
	RunMode string
}

// BuildResource builds the OTel Resource that identifies this Stirrup
// process, merging resource.Default() with Stirrup-specific service
// identity (service.name/version/instance.id and the run-scoped opts);
// the Stirrup overlay wins on key conflicts. See docs/architecture.md for
// the merge-failure fallback and the deployment.environment key choice.
func BuildResource(opts ResourceOptions) *resource.Resource {
	// Env-var fallbacks are sanitised against the same character set the
	// RunConfig validator enforces, so a hostile OTEL_DEPLOYMENT_ENVIRONMENT
	// (embedded newline, oversized, containing `=`) can't reach OTLP output.
	env := sanitiseLabel(firstNonEmpty(opts.Environment, os.Getenv(envEnvironment)), DefaultEnvironment)
	ns := sanitiseLabel(firstNonEmpty(opts.ServiceNamespace, os.Getenv(envServiceNamespace)), DefaultServiceNamespace)

	attrs := []attribute.KeyValue{
		semconv.ServiceName(ServiceName),
		semconv.ServiceVersion(version.Version()),
		semconv.ServiceInstanceID(InstanceID()),
		semconv.ServiceNamespace(ns),
		// Legacy stable key; see docs/architecture.md.
		attribute.String("deployment.environment", env),
	}
	if opts.RunMode != "" {
		// harness.run.mode has no semconv equivalent. Omitted entirely
		// when empty so callers that don't know the mode (eval replays,
		// ad-hoc tools) don't pollute the resource set with a synthetic
		// value.
		attrs = append(attrs, attribute.String("harness.run.mode", opts.RunMode))
	}

	base := resource.Default()
	overlay := resource.NewWithAttributes(base.SchemaURL(), attrs...)
	merged, err := resource.Merge(base, overlay)
	if err != nil {
		return resource.NewSchemaless(attrs...)
	}
	return merged
}

// firstNonEmpty returns the first non-empty argument, or "" if all are empty.
// Used to express the precedence chain (explicit -> env -> default) for
// resource attributes in a single line.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// sanitiseLabel returns v if it matches observabilityLabelPattern, otherwise
// fallback. Empty inputs also resolve to fallback, so callers can use it as
// a unified "first valid label, else default" reducer.
func sanitiseLabel(v, fallback string) string {
	if v == "" {
		return fallback
	}
	if observabilityLabelPattern.MatchString(v) {
		return v
	}
	return fallback
}
