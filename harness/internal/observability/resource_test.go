package observability

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/rxbynerd/stirrup/types/version"
)

// TestBuildResource_PreservesExistingIdentity locks down the spec-required
// service.* attributes plus the SDK-managed telemetry.sdk.* attributes. If
// any go missing, downstream backends fall back to
// "unknown_service:<binary>".
func TestBuildResource_PreservesExistingIdentity(t *testing.T) {
	t.Setenv(envServiceName, "")
	got := attrMap(BuildResource(ResourceOptions{}))

	if v := got["service.name"]; v != DefaultServiceName {
		t.Errorf("service.name: got %q, want %q", v, DefaultServiceName)
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
// falls through to "stirrup", deployment.environment.name to "local", and
// harness.run.mode is omitted entirely (no synthetic value injected).
func TestBuildResource_DefaultsApplied(t *testing.T) {
	// t.Setenv("", ...) clears the var for the pure-default path.
	t.Setenv(envEnvironment, "")
	t.Setenv(envServiceNamespace, "")

	got := attrMap(BuildResource(ResourceOptions{}))

	if v := got["service.namespace"]; v != DefaultServiceNamespace {
		t.Errorf("service.namespace: got %q, want %q", v, DefaultServiceNamespace)
	}
	if v := got["deployment.environment.name"]; v != DefaultEnvironment {
		t.Errorf("deployment.environment.name: got %q, want %q", v, DefaultEnvironment)
	}
	if _, present := got["harness.run.mode"]; present {
		t.Errorf("harness.run.mode: should be omitted when RunMode is empty, got %q", got["harness.run.mode"])
	}
}

// TestBuildResource_ExplicitWins pins the precedence chain: explicit
// ResourceOptions values must override env vars.
func TestBuildResource_ExplicitWins(t *testing.T) {
	t.Setenv(envEnvironment, "env-set-environment")
	t.Setenv(envServiceNamespace, "env-set-namespace")

	got := attrMap(BuildResource(ResourceOptions{
		Environment:      "explicit-environment",
		ServiceNamespace: "explicit-namespace",
		RunMode:          "execution",
	}))

	if v := got["deployment.environment.name"]; v != "explicit-environment" {
		t.Errorf("deployment.environment.name: got %q, want explicit-environment", v)
	}
	if v := got["service.namespace"]; v != "explicit-namespace" {
		t.Errorf("service.namespace: got %q, want explicit-namespace", v)
	}
	if v := got["harness.run.mode"]; v != "execution" {
		t.Errorf("harness.run.mode: got %q, want execution", v)
	}
}

// TestBuildResource_ServiceNamePrecedence pins explicit > OTEL_SERVICE_NAME
// > default for service.name, and that an invalid env value falls back to
// the default rather than reaching the resource.
func TestBuildResource_ServiceNamePrecedence(t *testing.T) {
	cases := []struct {
		name     string
		explicit string
		env      string
		want     string
	}{
		{"default", "", "", DefaultServiceName},
		{"env fallback", "", "env-agent", "env-agent"},
		{"explicit wins", "explicit-agent", "env-agent", "explicit-agent"},
		{"invalid env sanitised", "", "bad name\n", DefaultServiceName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envServiceName, tc.env)
			got := attrMap(BuildResource(ResourceOptions{ServiceName: tc.explicit}))
			if v := got["service.name"]; v != tc.want {
				t.Errorf("service.name: got %q, want %q", v, tc.want)
			}
		})
	}
}

// TestBuildResource_EnvFallback pins that env vars are consulted when
// ResourceOptions is silent.
func TestBuildResource_EnvFallback(t *testing.T) {
	t.Setenv(envEnvironment, "production-eu")
	t.Setenv(envServiceNamespace, "stirrup-eval")

	got := attrMap(BuildResource(ResourceOptions{}))

	if v := got["deployment.environment.name"]; v != "production-eu" {
		t.Errorf("deployment.environment.name: got %q, want production-eu", v)
	}
	if v := got["service.namespace"]; v != "stirrup-eval" {
		t.Errorf("service.namespace: got %q, want stirrup-eval", v)
	}
}

// TestBuildResource_RunModeOmittedWhenEmpty pins that an empty RunMode
// produces no harness.run.mode attribute, rather than an empty-string
// value that would break Grafana group-by-mode queries.
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

// TestBuildResource_EnvVarSanitisation pins that hostile env-var values
// (an attacker with K8s ConfigMap write access could inject newlines,
// oversized strings, or "=" delimiters) do not reach OTLP exporters.
func TestBuildResource_EnvVarSanitisation(t *testing.T) {
	t.Run("newline-laced env value falls back to default", func(t *testing.T) {

		t.Setenv(envEnvironment, "prod\nservice.name=evil")
		t.Setenv(envServiceNamespace, "")

		got := attrMap(BuildResource(ResourceOptions{}))

		if v := got["deployment.environment.name"]; v != DefaultEnvironment {
			t.Errorf("deployment.environment.name: hostile env value should be rejected, got %q want %q", v, DefaultEnvironment)
		}
	})

	t.Run("oversized env value falls back to default", func(t *testing.T) {
		// 65 chars; the pattern caps at 64.
		oversized := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijkl5"
		if len(oversized) != 65 {
			t.Fatalf("test fixture broken: oversized has %d chars, want 65", len(oversized))
		}
		t.Setenv(envEnvironment, oversized)
		t.Setenv(envServiceNamespace, "")

		got := attrMap(BuildResource(ResourceOptions{}))

		if v := got["deployment.environment.name"]; v != DefaultEnvironment {
			t.Errorf("deployment.environment.name: oversized env value should be rejected, got %q (len %d) want %q", v, len(v), DefaultEnvironment)
		}
	})

	t.Run("valid env value passes through", func(t *testing.T) {

		t.Setenv(envEnvironment, "")
		t.Setenv(envServiceNamespace, "valid-ns")

		got := attrMap(BuildResource(ResourceOptions{}))

		if v := got["service.namespace"]; v != "valid-ns" {
			t.Errorf("service.namespace: valid env value should pass through, got %q want valid-ns", v)
		}
	})

	t.Run("equals-sign env value falls back to default", func(t *testing.T) {

		t.Setenv(envEnvironment, "")
		t.Setenv(envServiceNamespace, "evil=injected")

		got := attrMap(BuildResource(ResourceOptions{}))

		if v := got["service.namespace"]; v != DefaultServiceNamespace {
			t.Errorf("service.namespace: env value with '=' should be rejected, got %q want %q", v, DefaultServiceNamespace)
		}
	})
}

// A rejected env value is reported by variable name, without the value,
// so an operator sees why the default label was used.
func TestBuildResource_RejectedEnvValueWarns(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Setenv(envServiceName, "my org/agent")
	t.Setenv(envEnvironment, "")
	t.Setenv(envServiceNamespace, "valid-ns")

	got := attrMap(BuildResource(ResourceOptions{}))
	if v := got["service.name"]; v != DefaultServiceName {
		t.Errorf("service.name = %q, want %q", v, DefaultServiceName)
	}
	out := buf.String()
	if !strings.Contains(out, "variable="+envServiceName) {
		t.Errorf("warning does not name %s: %q", envServiceName, out)
	}
	if strings.Contains(out, "my org/agent") {
		t.Errorf("warning echoes the rejected value: %q", out)
	}
	if strings.Contains(out, envServiceNamespace) {
		t.Errorf("valid %s must not warn: %q", envServiceNamespace, out)
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

// TestBuildResource_ExtraHasLowestPrecedence pins that Extra attributes
// appear on the resource but lose to the SDK default resource (which
// carries OTEL_RESOURCE_ATTRIBUTES) and to the stirrup identity overlay.
func TestBuildResource_ExtraHasLowestPrecedence(t *testing.T) {
	t.Setenv(envServiceNamespace, "")
	got := attrMap(BuildResource(ResourceOptions{Extra: []attribute.KeyValue{
		attribute.String("platform.extra", "kept"),
		attribute.String("telemetry.sdk.language", "overridden-by-default"),
		attribute.String("service.namespace", "overridden-by-overlay"),
	}}))

	if v := got["platform.extra"]; v != "kept" {
		t.Errorf("platform.extra: got %q, want %q", v, "kept")
	}
	if v := got["telemetry.sdk.language"]; v != "go" {
		t.Errorf("telemetry.sdk.language: got %q, want the SDK default %q", v, "go")
	}
	if v := got["service.namespace"]; v != DefaultServiceNamespace {
		t.Errorf("service.namespace: got %q, want the stirrup overlay %q", v, DefaultServiceNamespace)
	}
}

func attrMap(res *resource.Resource) map[string]string {
	out := make(map[string]string)
	for _, kv := range res.Attributes() {
		out[string(kv.Key)] = kv.Value.AsString()
	}
	return out
}
