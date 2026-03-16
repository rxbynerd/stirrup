// Package security provides secret resolution, log scrubbing, and input
// validation primitives for the harness.
package security

import (
	"context"
	"fmt"
	"os"
	"strings"
)

const (
	secretPrefix     = "secret://"
	fileSchemePrefix  = "secret://file://"
	redactedPlaceholder = "[REDACTED]"
)

// SecretStore resolves secret references to their plaintext values.
type SecretStore interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

// EnvSecretStore resolves secret references from environment variables and
// local files. It supports two reference formats:
//   - "secret://ENV_VAR_NAME" — resolves to os.Getenv("ENV_VAR_NAME")
//   - "secret://file:///path/to/file" — reads the file and trims whitespace
type EnvSecretStore struct{}

// NewEnvSecretStore returns a new EnvSecretStore.
func NewEnvSecretStore() *EnvSecretStore {
	return &EnvSecretStore{}
}

// Resolve parses a secret reference and returns the resolved plaintext value.
func (s *EnvSecretStore) Resolve(_ context.Context, ref string) (string, error) {
	if !strings.HasPrefix(ref, secretPrefix) {
		return "", fmt.Errorf("unknown secret reference scheme: %q", ref)
	}

	if strings.HasPrefix(ref, fileSchemePrefix) {
		return s.resolveFile(ref)
	}

	return s.resolveEnv(ref)
}

func (s *EnvSecretStore) resolveEnv(ref string) (string, error) {
	envVar := strings.TrimPrefix(ref, secretPrefix)
	if envVar == "" {
		return "", fmt.Errorf("empty environment variable name in secret reference: %q", ref)
	}

	value := os.Getenv(envVar)
	if value == "" {
		return "", fmt.Errorf("environment variable %q is empty or not set", envVar)
	}

	return value, nil
}

func (s *EnvSecretStore) resolveFile(ref string) (string, error) {
	path := strings.TrimPrefix(ref, fileSchemePrefix)
	if path == "" {
		return "", fmt.Errorf("empty file path in secret reference: %q", ref)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading secret file %q: %w", path, err)
	}

	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", fmt.Errorf("secret file %q is empty", path)
	}

	return value, nil
}
