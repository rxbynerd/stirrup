package observability

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/security"
)

func TestResolveHeaders_PlaintextPassthrough(t *testing.T) {
	in := map[string]string{
		"X-Tenant": "team-a",
		"X-Region": "eu-west-1",
	}
	out, err := ResolveHeaders(context.Background(), security.NewEnvSecretStore(), in)
	if err != nil {
		t.Fatalf("ResolveHeaders: %v", err)
	}
	if got, want := out["X-Tenant"], "team-a"; got != want {
		t.Errorf("X-Tenant: got %q, want %q", got, want)
	}
	if got, want := out["X-Region"], "eu-west-1"; got != want {
		t.Errorf("X-Region: got %q, want %q", got, want)
	}
	// Input must not be mutated.
	if &out == &in {
		t.Error("ResolveHeaders returned the input map; expected a fresh allocation")
	}
}

func TestResolveHeaders_SecretEnvVar(t *testing.T) {
	t.Setenv("TEST_GRAFANA_AUTH", "Basic dXNlcjpwYXNz")

	in := map[string]string{
		"Authorization": "secret://TEST_GRAFANA_AUTH",
	}
	out, err := ResolveHeaders(context.Background(), security.NewEnvSecretStore(), in)
	if err != nil {
		t.Fatalf("ResolveHeaders: %v", err)
	}
	if got, want := out["Authorization"], "Basic dXNlcjpwYXNz"; got != want {
		t.Errorf("Authorization: got %q, want %q", got, want)
	}
	// Input untouched.
	if in["Authorization"] != "secret://TEST_GRAFANA_AUTH" {
		t.Errorf("input mutated: %q", in["Authorization"])
	}
}

func TestResolveHeaders_SecretFileScheme(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.txt")
	if err := os.WriteFile(path, []byte("file-bearer-token\n"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}

	in := map[string]string{
		"Authorization": "secret://file://" + path,
	}
	out, err := ResolveHeaders(context.Background(), security.NewEnvSecretStore(), in)
	if err != nil {
		t.Fatalf("ResolveHeaders: %v", err)
	}
	// EnvSecretStore.Resolve trims trailing whitespace on file values.
	if got, want := out["Authorization"], "file-bearer-token"; got != want {
		t.Errorf("Authorization: got %q, want %q", got, want)
	}
}

// TestResolveHeaders_MissingEnvVar pins the error path: an env var that
// is not set produces a wrapped error attributing the failure to the
// header name. This keeps the operator's first hint pointing at the
// actual mistake (typo'd header value) rather than at a generic
// "couldn't construct exporter" log line.
func TestResolveHeaders_MissingEnvVar(t *testing.T) {
	if err := os.Unsetenv("STIRRUP_TEST_NOT_SET"); err != nil {
		t.Fatalf("unset env: %v", err)
	}
	in := map[string]string{
		"Authorization": "secret://STIRRUP_TEST_NOT_SET",
	}
	out, err := ResolveHeaders(context.Background(), security.NewEnvSecretStore(), in)
	if err == nil {
		t.Fatalf("expected error for missing env var, got nil; out=%v", out)
	}
	if !strings.Contains(err.Error(), "Authorization") {
		t.Errorf("error message must reference the header name, got %q", err.Error())
	}
}

// TestResolveHeaders_NilStoreWithSecret rejects a config that asks to
// resolve a secret reference without supplying a SecretStore. Falling
// through to "pass the literal secret:// string as the bearer token"
// would silently send a nonsense value to the gateway and surface as
// an opaque 401 — making this a hard error at exporter init catches
// the misconfiguration the moment it can be detected.
func TestResolveHeaders_NilStoreWithSecret(t *testing.T) {
	in := map[string]string{
		"Authorization": "secret://GRAFANA_CLOUD_AUTH",
	}
	_, err := ResolveHeaders(context.Background(), nil, in)
	if err == nil {
		t.Fatal("expected error when store is nil and a header references a secret, got nil")
	}
	if !strings.Contains(err.Error(), "no SecretStore") {
		t.Errorf("error must call out the missing SecretStore, got %q", err.Error())
	}
}

// TestResolveHeaders_NilStoreWithPlaintextOnly is the symmetric case:
// when no header carries a secret reference, a nil store is fine —
// every value passes through unchanged. This keeps the helper usable
// in code paths that don't have a SecretStore in hand and don't need
// one (e.g. tests).
func TestResolveHeaders_NilStoreWithPlaintextOnly(t *testing.T) {
	in := map[string]string{
		"X-Tenant": "team-a",
	}
	out, err := ResolveHeaders(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("ResolveHeaders: %v", err)
	}
	if got := out["X-Tenant"]; got != "team-a" {
		t.Errorf("X-Tenant: got %q, want team-a", got)
	}
}

func TestResolveHeaders_EmptyInput(t *testing.T) {
	out, err := ResolveHeaders(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("ResolveHeaders(nil): %v", err)
	}
	if out != nil {
		t.Errorf("expected nil output for nil input, got %v", out)
	}

	out, err = ResolveHeaders(context.Background(), nil, map[string]string{})
	if err != nil {
		t.Fatalf("ResolveHeaders(empty): %v", err)
	}
	if out != nil {
		t.Errorf("expected nil output for empty input, got %v", out)
	}
}
