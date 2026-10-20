package observability

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"

	"github.com/rxbynerd/stirrup/types/version"
)

// TestBuildResource_PreservesExistingIdentity locks down the spec-required
// service.* attributes plus the SDK-managed telemetry.sdk.* attributes. If
// any are missing, downstream backends fall back to
// "unknown_service:<binary>".
func TestBuildResource_PreservesExistingIdentity(t *testing.T) {
	must := require.New(t)
	t.Setenv(envServiceName, "")
	got := attrMap(BuildResource(ResourceOptions{}))

	must.Equal(DefaultServiceName, got["service.name"])
	must.Equal(version.Version(), got["service.version"])
	must.NotEmpty(got["service.instance.id"], "service.instance.id should be a stable identifier")
	for _, key := range []string{
		"telemetry.sdk.name",
		"telemetry.sdk.language",
		"telemetry.sdk.version",
	} {
		must.NotEmpty(got[key], "%s should be present in the merged resource", key)
	}
}

// TestBuildResource_DefaultsApplied pins that an entirely-empty
// ResourceOptions still produces a usable resource: service.namespace
// falls through to "stirrup", deployment.environment.name to "local", and
// harness.run.mode is omitted entirely (no synthetic value injected).
func TestBuildResource_DefaultsApplied(t *testing.T) {
	must := require.New(t)
	// An empty value makes the environment lookup fall through to the default.
	t.Setenv(envEnvironment, "")
	t.Setenv(envServiceNamespace, "")

	got := attrMap(BuildResource(ResourceOptions{}))

	must.Equal(DefaultServiceNamespace, got["service.namespace"])
	must.Equal(DefaultEnvironment, got["deployment.environment.name"])
	must.NotContains(got, "harness.run.mode", "harness.run.mode should be omitted when RunMode is empty")
}

// TestBuildResource_ExplicitWins pins the precedence chain: explicit
// ResourceOptions values must override env vars.
func TestBuildResource_ExplicitWins(t *testing.T) {
	must := require.New(t)
	t.Setenv(envEnvironment, "env-set-environment")
	t.Setenv(envServiceNamespace, "env-set-namespace")

	got := attrMap(BuildResource(ResourceOptions{
		Environment:      "explicit-environment",
		ServiceNamespace: "explicit-namespace",
		RunMode:          "execution",
	}))

	must.Equal("explicit-environment", got["deployment.environment.name"])
	must.Equal("explicit-namespace", got["service.namespace"])
	must.Equal("execution", got["harness.run.mode"])
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
		{"default when unset", "", "", DefaultServiceName},
		{"env fallback", "", "env-agent", "env-agent"},
		{"explicit wins over env", "explicit-agent", "env-agent", "explicit-agent"},
		{"invalid env falls back to default", "", "bad name\n", DefaultServiceName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			t.Setenv(envServiceName, tc.env)
			got := attrMap(BuildResource(ResourceOptions{ServiceName: tc.explicit}))
			must.Equal(tc.want, got["service.name"])
		})
	}
}

// TestBuildResource_EnvFallback pins that env vars are consulted when
// ResourceOptions is silent.
func TestBuildResource_EnvFallback(t *testing.T) {
	must := require.New(t)
	t.Setenv(envEnvironment, "production-eu")
	t.Setenv(envServiceNamespace, "stirrup-eval")

	got := attrMap(BuildResource(ResourceOptions{}))

	must.Equal("production-eu", got["deployment.environment.name"])
	must.Equal("stirrup-eval", got["service.namespace"])
}

// TestBuildResource_RunModeOmittedWhenEmpty pins that an empty RunMode
// produces no harness.run.mode attribute, rather than an empty-string
// value that would break Grafana group-by-mode queries.
func TestBuildResource_RunModeOmittedWhenEmpty(t *testing.T) {
	must := require.New(t)
	got := attrMap(BuildResource(ResourceOptions{
		Environment:      "production",
		ServiceNamespace: "stirrup",
		// RunMode deliberately empty.
	}))
	must.NotContains(got, "harness.run.mode", "harness.run.mode should be omitted when RunMode is empty")
}

// TestBuildResource_EnvVarSanitisation pins that hostile env-var values
// (an attacker with K8s ConfigMap write access could inject newlines,
// oversized strings, or "=" delimiters) do not reach OTLP exporters.
func TestBuildResource_EnvVarSanitisation(t *testing.T) {
	t.Run("newline-laced env value falls back to default", func(t *testing.T) {
		must := require.New(t)
		t.Setenv(envEnvironment, "prod\nservice.name=evil")
		t.Setenv(envServiceNamespace, "")

		got := attrMap(BuildResource(ResourceOptions{}))

		must.Equal(DefaultEnvironment, got["deployment.environment.name"], "hostile env value should be rejected")
	})

	t.Run("oversized env value falls back to default", func(t *testing.T) {
		must := require.New(t)
		// 65 chars; the pattern caps at 64.
		oversized := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijkl5"
		must.Len(oversized, 65, "test fixture broken")
		t.Setenv(envEnvironment, oversized)
		t.Setenv(envServiceNamespace, "")

		got := attrMap(BuildResource(ResourceOptions{}))

		must.Equal(DefaultEnvironment, got["deployment.environment.name"], "oversized env value should be rejected")
	})

	t.Run("valid env value passes through", func(t *testing.T) {
		must := require.New(t)
		t.Setenv(envEnvironment, "")
		t.Setenv(envServiceNamespace, "valid-ns")

		got := attrMap(BuildResource(ResourceOptions{}))

		must.Equal("valid-ns", got["service.namespace"], "valid env value should pass through")
	})

	t.Run("equals-sign env value falls back to default", func(t *testing.T) {
		must := require.New(t)
		t.Setenv(envEnvironment, "")
		t.Setenv(envServiceNamespace, "evil=injected")

		got := attrMap(BuildResource(ResourceOptions{}))

		must.Equal(DefaultServiceNamespace, got["service.namespace"], "env value with '=' should be rejected")
	})
}

// A rejected env value is reported by variable name, without the value,
// so an operator sees why the default label was used.
func TestBuildResource_RejectedEnvValueWarns(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Setenv(envServiceName, "my org/agent")
	t.Setenv(envEnvironment, "")
	t.Setenv(envServiceNamespace, "valid-ns")

	got := attrMap(BuildResource(ResourceOptions{}))
	must.Equal(DefaultServiceName, got["service.name"])

	out := buf.String()
	must.Contains(out, "variable="+envServiceName, "warning should name the rejected variable")
	must.NotContains(out, "my org/agent", "warning should not echo the rejected value")
	must.NotContains(out, envServiceNamespace, "valid env value should not produce a warning")
}

// TestInstanceID_Stable proves the instance ID is generated once per
// process and reused for the lifetime of that process. Spec compliance:
// service.instance.id must uniquely identify a single running instance,
// not change mid-flight.
func TestInstanceID_Stable(t *testing.T) {
	must := require.New(t)
	first := InstanceID()
	second := InstanceID()

	must.NotEmpty(first, "InstanceID() returned empty string")
	must.Equal(first, second, "InstanceID should be stable across calls")
}

// TestBuildResource_ExtraHasLowestPrecedence pins that Extra attributes
// appear on the resource but lose to the SDK default resource (which
// carries OTEL_RESOURCE_ATTRIBUTES) and to the stirrup identity overlay.
func TestBuildResource_ExtraHasLowestPrecedence(t *testing.T) {
	must := require.New(t)
	t.Setenv(envServiceNamespace, "")
	got := attrMap(BuildResource(ResourceOptions{Extra: []attribute.KeyValue{
		attribute.String("platform.extra", "kept"),
		attribute.String("telemetry.sdk.language", "overridden-by-default"),
		attribute.String("service.namespace", "overridden-by-overlay"),
	}}))

	must.Equal("kept", got["platform.extra"], "Extra attribute should be kept")
	must.Equal("go", got["telemetry.sdk.language"], "SDK default should win over Extra")
	must.Equal(DefaultServiceNamespace, got["service.namespace"], "stirrup overlay should win over Extra")
}

func attrMap(res *resource.Resource) map[string]string {
	out := make(map[string]string)
	for _, kv := range res.Attributes() {
		out[string(kv.Key)] = kv.Value.AsString()
	}
	return out
}
