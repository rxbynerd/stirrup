package security

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestEnvSecretStore_ResolveEnvVar(t *testing.T) {
	store := NewEnvSecretStore()
	ctx := context.Background()

	const key = "STIRRUP_TEST_SECRET"
	t.Setenv(key, "my-secret-value")

	val, err := store.Resolve(ctx, "secret://"+key)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "my-secret-value" {
		t.Errorf("got %q, want %q", val, "my-secret-value")
	}
}

func TestEnvSecretStore_ResolveEnvVarEmpty(t *testing.T) {
	store := NewEnvSecretStore()
	ctx := context.Background()

	// Use a variable name that is extremely unlikely to be set.
	_, err := store.Resolve(ctx, "secret://STIRRUP_DEFINITELY_NOT_SET_12345")
	if err == nil {
		t.Fatal("expected error for unset env var, got nil")
	}
}

func TestEnvSecretStore_ResolveFile(t *testing.T) {
	store := NewEnvSecretStore()
	ctx := context.Background()

	dir := t.TempDir()
	secretFile := filepath.Join(dir, "api_key")
	if err := os.WriteFile(secretFile, []byte("  file-secret-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	val, err := store.Resolve(ctx, "secret://file://"+secretFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "file-secret-value" {
		t.Errorf("got %q, want %q", val, "file-secret-value")
	}
}

func TestEnvSecretStore_ResolveFileNotFound(t *testing.T) {
	store := NewEnvSecretStore()
	ctx := context.Background()

	_, err := store.Resolve(ctx, "secret://file:///nonexistent/path/secret")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestEnvSecretStore_ResolveFileEmpty(t *testing.T) {
	store := NewEnvSecretStore()
	ctx := context.Background()

	dir := t.TempDir()
	secretFile := filepath.Join(dir, "empty")
	if err := os.WriteFile(secretFile, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := store.Resolve(ctx, "secret://file://"+secretFile)
	if err == nil {
		t.Fatal("expected error for empty file, got nil")
	}
}

func TestEnvSecretStore_ResolveUnknownScheme(t *testing.T) {
	store := NewEnvSecretStore()
	ctx := context.Background()

	_, err := store.Resolve(ctx, "vault://my-secret")
	if err == nil {
		t.Fatal("expected error for unknown scheme, got nil")
	}
}

func TestEnvSecretStore_ResolveEmptyEnvName(t *testing.T) {
	store := NewEnvSecretStore()
	ctx := context.Background()

	_, err := store.Resolve(ctx, "secret://")
	if err == nil {
		t.Fatal("expected error for empty env var name, got nil")
	}
}
