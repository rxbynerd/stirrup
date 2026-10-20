package security

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEnvSecretStore_ResolveEnvVar(t *testing.T) {
	must := require.New(t)
	store := NewEnvSecretStore()
	ctx := context.Background()

	const key = "STIRRUP_TEST_SECRET"
	t.Setenv(key, "my-secret-value")

	val, err := store.Resolve(ctx, "secret://"+key)
	must.NoError(err)
	must.Equal("my-secret-value", val)
}

func TestEnvSecretStore_ResolveUnsetEnvVarErrors(t *testing.T) {
	must := require.New(t)
	store := NewEnvSecretStore()
	ctx := context.Background()

	// The name is chosen so that it is practically never set in a developer or CI environment.
	_, err := store.Resolve(ctx, "secret://STIRRUP_DEFINITELY_NOT_SET_12345")
	must.Error(err)
}

func TestEnvSecretStore_ResolveFile(t *testing.T) {
	must := require.New(t)
	store := NewEnvSecretStore()
	ctx := context.Background()

	dir := t.TempDir()
	secretFile := filepath.Join(dir, "api_key")
	must.NoError(os.WriteFile(secretFile, []byte("  file-secret-value\n"), 0o600))

	val, err := store.Resolve(ctx, "secret://file://"+secretFile)
	must.NoError(err)
	must.Equal("file-secret-value", val)
}

func TestEnvSecretStore_ResolveMissingFileErrors(t *testing.T) {
	must := require.New(t)
	store := NewEnvSecretStore()
	ctx := context.Background()

	_, err := store.Resolve(ctx, "secret://file:///nonexistent/path/secret")
	must.Error(err)
}

func TestEnvSecretStore_ResolveBlankFileErrors(t *testing.T) {
	must := require.New(t)
	store := NewEnvSecretStore()
	ctx := context.Background()

	dir := t.TempDir()
	secretFile := filepath.Join(dir, "empty")
	must.NoError(os.WriteFile(secretFile, []byte("  \n"), 0o600))

	_, err := store.Resolve(ctx, "secret://file://"+secretFile)
	must.Error(err)
}

func TestEnvSecretStore_ResolveUnknownSchemeErrors(t *testing.T) {
	must := require.New(t)
	store := NewEnvSecretStore()
	ctx := context.Background()

	_, err := store.Resolve(ctx, "vault://my-secret")
	must.Error(err)
}

func TestEnvSecretStore_ResolveEmptyEnvNameErrors(t *testing.T) {
	must := require.New(t)
	store := NewEnvSecretStore()
	ctx := context.Background()

	_, err := store.Resolve(ctx, "secret://")
	must.Error(err)
}
