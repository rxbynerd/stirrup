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

// TestBuildResource_EnvVarSanitisation locks down the BLK-2 fix: hostile
// env-var values must not reach OTLP exporters. The RunConfig validation
// path screens via validateObservabilityConfig, but the env-var fallback
// used to call attribute.String() with the raw os.Getenv result. An
// attacker with K8s ConfigMap write access could inject newlines, oversized
// strings, or "=" delimiters into every emitted span and metric batch.
// Sanitisation is now applied symmetrically.
func TestBuildResource_EnvVarSanitisation(t *testing.T) {
	t.Run("newline-laced env value falls back to default", func(t *testing.T) {
		// A real attack vector: an environment variable that smuggles a
		// second key=value pair via newline delimiters. OTLP serialisers do
		// not see this as malformed because attribute.String accepts any
		// UTF-8 — the damage is done at the backend's parsing layer.
		t.Setenv(envEnvironment, "prod\nservice.name=evil")
		t.Setenv(envServiceNamespace, "")

		got := attrMap(BuildResource(ResourceOptions{}))

		if v := got["deployment.environment"]; v != DefaultEnvironment {
			t.Errorf("deployment.environment: hostile env value should be rejected, got %q want %q", v, DefaultEnvironment)
		}
	})

	t.Run("oversized env value falls back to default", func(t *testing.T) {
		// 65 chars; the pattern caps at 64. Exceeding the cap is a
		// realistic operator mistake (paste-from-uuid-tooling) as well as
		// a deliberate flood: backends like Prometheus truncate or reject
		// over-long labels, and Grafana dashboards then group rows in
		// surprising ways.
		oversized := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijkl5"
		if len(oversized) != 65 {
			t.Fatalf("test fixture broken: oversized has %d chars, want 65", len(oversized))
		}
		t.Setenv(envEnvironment, oversized)
		t.Setenv(envServiceNamespace, "")

		got := attrMap(BuildResource(ResourceOptions{}))

		if v := got["deployment.environment"]; v != DefaultEnvironment {
			t.Errorf("deployment.environment: oversized env value should be rejected, got %q (len %d) want %q", v, len(v), DefaultEnvironment)
		}
	})

	t.Run("valid env value passes through", func(t *testing.T) {
		// A name that satisfies the pattern must reach the resource
		// untouched — sanitisation is a screen, not a transform.
		t.Setenv(envEnvironment, "")
		t.Setenv(envServiceNamespace, "valid-ns")

		got := attrMap(BuildResource(ResourceOptions{}))

		if v := got["service.namespace"]; v != "valid-ns" {
			t.Errorf("service.namespace: valid env value should pass through, got %q want valid-ns", v)
		}
	})

	t.Run("equals-sign env value falls back to default", func(t *testing.T) {
		// "=" is rejected by the pattern; an attacker could otherwise
		// smuggle a key=value pair into a single attribute body.
		t.Setenv(envEnvironment, "")
		t.Setenv(envServiceNamespace, "evil=injected")

		got := attrMap(BuildResource(ResourceOptions{}))

		if v := got["service.namespace"]; v != DefaultServiceNamespace {
			t.Errorf("service.namespace: env value with '=' should be rejected, got %q want %q", v, DefaultServiceNamespace)
		}
	})
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
