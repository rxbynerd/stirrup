// Package observability provides OTel resource construction shared by the
// trace emitter and the metrics provider. Both signals must be tagged with
// the same resource so backends can correlate traces and metrics for one
// running Stirrup process.
package observability

import (
	"crypto/rand"
	"encoding/hex"
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

// Resource builds the OTel Resource that identifies this Stirrup process.
//
// It merges three sources, in order of increasing specificity (later wins):
//  1. resource.Default() — provides telemetry.sdk.{name,language,version},
//     host.name, and any operator-supplied OTEL_RESOURCE_ATTRIBUTES.
//     resource.Default() also seeds service.name as
//     "unknown_service:<binary>"; our overlay below replaces it.
//  2. Stirrup-specific service identity — service.name, service.version,
//     service.instance.id.
//
// The Default resource's SchemaURL is reused for the overlay so resource.Merge
// always succeeds. If Merge nonetheless fails (e.g. a future SDK changes the
// merge contract), we fall back to a schemaless resource carrying just the
// Stirrup attributes; that still fixes "unknown_service:stirrup", which is
// the user-visible problem we're solving.
func Resource() *resource.Resource {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(ServiceName),
		semconv.ServiceVersion(version.Version()),
		semconv.ServiceInstanceID(InstanceID()),
	}

	base := resource.Default()
	overlay := resource.NewWithAttributes(base.SchemaURL(), attrs...)
	merged, err := resource.Merge(base, overlay)
	if err != nil {
		return resource.NewSchemaless(attrs...)
	}
	return merged
}
