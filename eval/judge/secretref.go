package judge

import (
	"fmt"
	"os"
	"strings"
)

const (
	secretRefPrefix     = "secret://"
	secretRefFilePrefix = "secret://file://"
)

// resolveSecretRef resolves a "secret://ENV_NAME" or "secret://file:///path"
// reference, the same syntax the harness accepts. Errors name the variable or
// path, never the value.
func resolveSecretRef(ref string) (string, error) {
	if strings.HasPrefix(ref, secretRefFilePrefix) {
		path := strings.TrimPrefix(ref, secretRefFilePrefix)
		if path == "" {
			return "", fmt.Errorf("empty file path in secret reference %q", ref)
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

	if !strings.HasPrefix(ref, secretRefPrefix) {
		return "", fmt.Errorf("unknown secret reference scheme: %q", ref)
	}
	name := strings.TrimPrefix(ref, secretRefPrefix)
	if name == "" {
		return "", fmt.Errorf("empty environment variable name in secret reference %q", ref)
	}
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("environment variable %q is empty or not set", name)
	}
	return value, nil
}
