package observability

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/security"
)

func TestResolveHeaders_PlaintextPassthrough(t *testing.T) {
	must := require.New(t)
	in := map[string]string{
		"X-Tenant": "team-a",
		"X-Region": "eu-west-1",
	}
	out, err := ResolveHeaders(context.Background(), security.NewEnvSecretStore(), in)
	must.NoError(err)
	must.Equal("team-a", out["X-Tenant"])
	must.Equal("eu-west-1", out["X-Region"])

	// The result must be a fresh map, not the input map.
	out["__probe__"] = "x"
	must.NotContains(in, "__probe__", "ResolveHeaders returned the input map")
}

func TestResolveHeaders_SecretEnvVar(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_GRAFANA_AUTH", "Basic dXNlcjpwYXNz")

	in := map[string]string{
		"Authorization": "secret://TEST_GRAFANA_AUTH",
	}
	out, err := ResolveHeaders(context.Background(), security.NewEnvSecretStore(), in)
	must.NoError(err)
	must.Equal("Basic dXNlcjpwYXNz", out["Authorization"])
	must.Equal("secret://TEST_GRAFANA_AUTH", in["Authorization"], "ResolveHeaders mutated its input")
}

func TestResolveHeaders_SecretFileScheme(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "token.txt")
	must.NoError(os.WriteFile(path, []byte("file-bearer-token\n"), 0o600))

	in := map[string]string{
		"Authorization": "secret://file://" + path,
	}
	out, err := ResolveHeaders(context.Background(), security.NewEnvSecretStore(), in)
	must.NoError(err)
	// EnvSecretStore.Resolve trims trailing whitespace on file values.
	must.Equal("file-bearer-token", out["Authorization"])
}

// TestResolveHeaders_MissingEnvVar pins the error path: an env var that
// is not set produces a wrapped error attributing the failure to the
// header name. This keeps the operator's first hint pointing at the
// actual mistake (typo'd header value) rather than at a generic
// "couldn't construct exporter" log line.
func TestResolveHeaders_MissingEnvVar(t *testing.T) {
	must := require.New(t)
	must.NoError(os.Unsetenv("STIRRUP_TEST_NOT_SET"))
	in := map[string]string{
		"Authorization": "secret://STIRRUP_TEST_NOT_SET",
	}
	_, err := ResolveHeaders(context.Background(), security.NewEnvSecretStore(), in)
	must.ErrorContains(err, "Authorization", "error must reference the header name")
}

// TestResolveHeaders_NilStoreWithSecret rejects a config that asks to
// resolve a secret reference without supplying a SecretStore. Falling
// through to "pass the literal secret:// string as the bearer token"
// would silently send a nonsense value to the gateway and surface as
// an opaque 401 — making this a hard error at exporter init catches
// the misconfiguration the moment it can be detected.
func TestResolveHeaders_NilStoreWithSecret(t *testing.T) {
	must := require.New(t)
	in := map[string]string{
		"Authorization": "secret://GRAFANA_CLOUD_AUTH",
	}
	_, err := ResolveHeaders(context.Background(), nil, in)
	must.ErrorContains(err, "no SecretStore")
}

// TestResolveHeaders_NilStoreWithPlaintextOnly is the symmetric case:
// when no header carries a secret reference, a nil store is fine —
// every value passes through unchanged. This keeps the helper usable
// in code paths that don't have a SecretStore in hand and don't need
// one (e.g. tests).
func TestResolveHeaders_NilStoreWithPlaintextOnly(t *testing.T) {
	must := require.New(t)
	in := map[string]string{
		"X-Tenant": "team-a",
	}
	out, err := ResolveHeaders(context.Background(), nil, in)
	must.NoError(err)
	must.Equal("team-a", out["X-Tenant"])
}

func TestResolveHeaders_EmptyInput(t *testing.T) {
	t.Run("nil input", func(t *testing.T) {
		must := require.New(t)
		out, err := ResolveHeaders(context.Background(), nil, nil)
		must.NoError(err)
		must.Nil(out)
	})

	t.Run("empty map", func(t *testing.T) {
		must := require.New(t)
		out, err := ResolveHeaders(context.Background(), nil, map[string]string{})
		must.NoError(err)
		must.Nil(out)
	})
}
