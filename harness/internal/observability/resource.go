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

// InstanceID returns a stable random identifier for the current process.
// It is generated once at first call and reused for the rest of the
// process's lifetime. Used as service.instance.id so concurrent harness
// processes can be distinguished in OTel-aware backends.
//
// 16 bytes (128 bits) of randomness is enough that collisions across all
// running Stirrup processes are vanishingly unlikely. The fallback string
// "unknown" is only ever returned if crypto/rand.Read fails, which on Unix
// would imply /dev/urandom is unreadable — an environment in which
// stirrup cannot meaningfully run anyway.
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

// BuildResource builds the OTel Resource that identifies this Stirrup process.
//
// It merges two sources, in order of increasing specificity (later wins):
//  1. resource.Default() — provides telemetry.sdk.{name,language,version},
//     host.name, and any operator-supplied OTEL_RESOURCE_ATTRIBUTES.
//     resource.Default() also seeds service.name as
//     "unknown_service:<binary>"; our overlay below replaces it.
//  2. Stirrup-specific service identity — service.name, service.version,
//     service.instance.id, and the run-scoped opts (deployment.environment,
//     service.namespace, harness.run.mode).
//
// The Default resource's SchemaURL is reused for the overlay so resource.Merge
// always succeeds. If Merge nonetheless fails (e.g. a future SDK changes the
// merge contract), we fall back to a schemaless resource carrying just the
// Stirrup attributes; that still fixes "unknown_service:stirrup", which is
// the user-visible problem we're solving.
//
// Note: the deployment-environment attribute key is "deployment.environment"
// (the legacy stable form), not the newer "deployment.environment.name" that
// semconv v1.40.0 exposes via DeploymentEnvironmentName. The legacy key is
// what every existing Grafana dashboard, Tempo derived metric, and OTel
// collector processor (e.g. resourcedetectionprocessor) looks for; switching
// to the .name suffix would silently break operator dashboards. When the
// upstream conventions stabilise on the .name form across the ecosystem we
// will re-evaluate.
func BuildResource(opts ResourceOptions) *resource.Resource {
	// Env-var fallbacks are sanitised against the same character set the
	// RunConfig validator enforces. Without this, a hostile pod-spec
	// OTEL_DEPLOYMENT_ENVIRONMENT (e.g. embedded newline, > 64 chars,
	// containing `=`) would propagate verbatim to every OTLP span and
	// metric batch — the RunConfig path validates via
	// validateObservabilityConfig but the env-var path bypassed it.
	env := sanitiseLabel(firstNonEmpty(opts.Environment, os.Getenv(envEnvironment)), DefaultEnvironment)
	ns := sanitiseLabel(firstNonEmpty(opts.ServiceNamespace, os.Getenv(envServiceNamespace)), DefaultServiceNamespace)

	attrs := []attribute.KeyValue{
		semconv.ServiceName(ServiceName),
		semconv.ServiceVersion(version.Version()),
		semconv.ServiceInstanceID(InstanceID()),
		semconv.ServiceNamespace(ns),
		// See the doc-comment above for why we emit "deployment.environment"
		// instead of using semconv.DeploymentEnvironmentName.
		attribute.String("deployment.environment", env),
	}
	if opts.RunMode != "" {
		// harness.run.mode has no semconv equivalent; we use a stirrup-
		// scoped key. Omitted entirely when empty so callers that don't
		// know the mode (eval replays, ad-hoc tools) don't pollute the
		// resource set with a synthetic value.
		attrs = append(attrs, attribute.String("harness.run.mode", opts.RunMode))
	}

	// resource.Default() picks up OTEL_RESOURCE_ATTRIBUTES and other SDK
	// defaults. Our overlay is the second argument and wins on key
	// conflicts, so our service.namespace and deployment.environment take
	// precedence over any OTEL_RESOURCE_ATTRIBUTES value for those keys.
	// Operators who need to set these attributes should use
	// --deployment-environment / --service-namespace, RunConfig.Observability,
	// or OTEL_DEPLOYMENT_ENVIRONMENT / OTEL_SERVICE_NAMESPACE — not
	// OTEL_RESOURCE_ATTRIBUTES.
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
// fallback. Empty inputs also resolve to fallback so callers can use it as
// a unified "first valid label, else default" reducer. Without this, a
// hostile env-var value (containing newlines, over 64 bytes, or characters
// outside the validated charset) would propagate verbatim to every OTLP
// span and metric batch — the RunConfig path validates via
// types.validateObservabilityConfig but the env-var path used to bypass it.
func sanitiseLabel(v, fallback string) string {
	if v == "" {
		return fallback
	}
	if observabilityLabelPattern.MatchString(v) {
		return v
	}
	return fallback
}
