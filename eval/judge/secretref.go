package judge

import (
	"fmt"
	"os"
	"strings"

	"github.com/rxbynerd/stirrup/types"
)

const secretRefFilePrefix = "secret://file://"

// resolveSecretRef resolves a "secret://ENV_NAME" or "secret://file:///path"
// reference, the forms types.ValidateJudgeKeyRef accepts. Errors name the
// variable or path, never the value or a malformed reference.
func resolveSecretRef(ref string) (string, error) {
	if err := types.ValidateJudgeKeyRef(ref); err != nil {
		return "", err
	}
	if path, ok := strings.CutPrefix(ref, secretRefFilePrefix); ok {
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

	name := strings.TrimPrefix(ref, "secret://")
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("environment variable %q is empty or not set", name)
	}
	return value, nil
}
