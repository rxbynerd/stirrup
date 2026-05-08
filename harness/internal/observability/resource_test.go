package observability

import (
	"testing"

	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/rxbynerd/stirrup/types/version"
)

// TestBuildResource_PreservesExistingIdentity locks down the spec-required
// service.* attributes plus the SDK-managed telemetry.sdk.* attributes. The
// OpenTelemetry semantic conventions list service.name as required for
// every signal, and recommend service.version + service.instance.id. If
// any of these go missing, downstream backends (Zipkin, Jaeger, Tempo, ...)
// fall back to "unknown_service:<binary>", which is the regression this
// test exists to prevent. The telemetry.sdk.* keys come from the OTel SDK's
// own resource.Default() — without them, exporters can't tell which SDK
// produced a signal.
func TestBuildResource_PreservesExistingIdentity(t *testing.T) {
	got := attrMap(BuildResource(ResourceOptions{}))

	if v := got["service.name"]; v != ServiceName {
		t.Errorf("service.name: got %q, want %q", v, ServiceName)
	}
	if v := got["service.version"]; v != version.Version() {
		t.Errorf("service.version: got %q, want %q", v, version.Version())
	}
	if got["service.instance.id"] == "" {
		t.Errorf("service.instance.id: got empty string, want a stable identifier")
	}
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

// TestBuildResource_DefaultsApplied pins that an entirely-empty
// ResourceOptions still produces a usable resource: service.namespace
// falls through to "stirrup", deployment.environment to "local", and
// harness.run.mode is omitted entirely (no synthetic value injected). This
// is the path a fresh "stirrup harness --prompt ..." invocation takes when
// the operator has not pinned any environment, so it must not regress.
func TestBuildResource_DefaultsApplied(t *testing.T) {
	// Clear env vars so we measure the pure-default path. t.Setenv with
	// the empty string still records a value; Unsetenv via t.Setenv-on-
	// empty has been the convention since Go 1.17.
	t.Setenv(envEnvironment, "")
	t.Setenv(envServiceNamespace, "")

	got := attrMap(BuildResource(ResourceOptions{}))

	if v := got["service.namespace"]; v != DefaultServiceNamespace {
		t.Errorf("service.namespace: got %q, want %q", v, DefaultServiceNamespace)
	}
	if v := got["deployment.environment"]; v != DefaultEnvironment {
		t.Errorf("deployment.environment: got %q, want %q", v, DefaultEnvironment)
	}
	if _, present := got["harness.run.mode"]; present {
		t.Errorf("harness.run.mode: should be omitted when RunMode is empty, got %q", got["harness.run.mode"])
	}
}

// TestBuildResource_ExplicitWins pins the precedence chain: explicit
// ResourceOptions values must override env vars. Without this guarantee an
// operator's --deployment-environment flag could be silently swallowed by a
// stale env var inherited from the parent shell, which is the exact
// foot-gun this precedence chain exists to prevent.
func TestBuildResource_ExplicitWins(t *testing.T) {
	t.Setenv(envEnvironment, "env-set-environment")
	t.Setenv(envServiceNamespace, "env-set-namespace")

	got := attrMap(BuildResource(ResourceOptions{
		Environment:      "explicit-environment",
		ServiceNamespace: "explicit-namespace",
		RunMode:          "execution",
	}))

	if v := got["deployment.environment"]; v != "explicit-environment" {
		t.Errorf("deployment.environment: got %q, want explicit-environment", v)
	}
	if v := got["service.namespace"]; v != "explicit-namespace" {
		t.Errorf("service.namespace: got %q, want explicit-namespace", v)
	}
	if v := got["harness.run.mode"]; v != "execution" {
		t.Errorf("harness.run.mode: got %q, want execution", v)
	}
}

// TestBuildResource_EnvFallback pins that env vars are consulted when
// ResourceOptions is silent. This is the path most production deployments
// take: the K8s job sets OTEL_DEPLOYMENT_ENVIRONMENT in the pod spec, and
// the harness picks it up without anyone having to thread it through the
// RunConfig.
func TestBuildResource_EnvFallback(t *testing.T) {
	t.Setenv(envEnvironment, "production-eu")
	t.Setenv(envServiceNamespace, "stirrup-eval")

	got := attrMap(BuildResource(ResourceOptions{}))

	if v := got["deployment.environment"]; v != "production-eu" {
		t.Errorf("deployment.environment: got %q, want production-eu", v)
	}
	if v := got["service.namespace"]; v != "stirrup-eval" {
		t.Errorf("service.namespace: got %q, want stirrup-eval", v)
	}
}

// TestBuildResource_RunModeOmittedWhenEmpty pins that an empty RunMode
// produces no harness.run.mode attribute. Read-only modes that the eval
// runner constructs without a Mode value (or future replay paths that load
// recorded traces lacking the field) must not silently inject an empty-
// string mode label that would break Grafana group-by-mode queries.
func TestBuildResource_RunModeOmittedWhenEmpty(t *testing.T) {
	got := attrMap(BuildResource(ResourceOptions{
		Environment:      "production",
		ServiceNamespace: "stirrup",
		// RunMode deliberately empty.
	}))
	if _, present := got["harness.run.mode"]; present {
		t.Errorf("harness.run.mode: should be omitted when RunMode is empty, got %q", got["harness.run.mode"])
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
