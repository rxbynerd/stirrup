package observability

import (
	"testing"

	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/rxbynerd/stirrup/types/version"
)

// TestResource_HasServiceIdentity locks down the spec-required attributes:
// the OpenTelemetry semantic conventions list service.name as required for
// every signal, and recommend service.version + service.instance.id. If any
// of these go missing, downstream backends (Zipkin, Jaeger, Tempo, ...)
// fall back to "unknown_service:<binary>", which is the regression this
// test exists to prevent.
func TestResource_HasServiceIdentity(t *testing.T) {
	got := attrMap(Resource())

	if v := got["service.name"]; v != ServiceName {
		t.Errorf("service.name: got %q, want %q", v, ServiceName)
	}
	if v := got["service.version"]; v != version.Version() {
		t.Errorf("service.version: got %q, want %q", v, version.Version())
	}
	if got["service.instance.id"] == "" {
		t.Errorf("service.instance.id: got empty string, want a stable identifier")
	}
}

// TestResource_HasSDKAttributes verifies the merge with resource.Default()
// preserves the telemetry.sdk.* attributes the OTel spec mandates be set
// by the SDK itself. Without these, exporters can't tell which SDK
// produced a signal — useful when debugging.
func TestResource_HasSDKAttributes(t *testing.T) {
	got := attrMap(Resource())

	for _, key := range []string{
		"telemetry.sdk.name",
		"telemetry.sdk.language",
		"telemetry.sdk.version",
	} {
		if got[key] == "" {
			t.Errorf("%s: missing from merged resource", key)
		}
	}
}

// TestInstanceID_Stable proves the instance ID is generated once per
// process and reused for the lifetime of that process. Spec compliance:
// service.instance.id must uniquely identify a single running instance,
// not change mid-flight.
func TestInstanceID_Stable(t *testing.T) {
	first := InstanceID()
	second := InstanceID()

	if first == "" {
		t.Fatalf("InstanceID() returned empty string")
	}
	if first != second {
		t.Errorf("InstanceID not stable across calls: first=%q second=%q", first, second)
	}
}

func attrMap(res *resource.Resource) map[string]string {
	out := make(map[string]string)
	for _, kv := range res.Attributes() {
		out[string(kv.Key)] = kv.Value.AsString()
	}
	return out
}
