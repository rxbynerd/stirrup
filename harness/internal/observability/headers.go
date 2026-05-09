package observability

import (
	"context"
	"fmt"
	"strings"

	"github.com/rxbynerd/stirrup/harness/internal/security"
)

// secretRefPrefix is the scheme that signals a value to be resolved via
// the SecretStore rather than passed through as plaintext. Kept in sync
// with security.secretPrefix (private to that package).
const secretRefPrefix = "secret://"

// ResolveHeaders walks the input map and resolves any value that begins
// with "secret://" through the supplied SecretStore. Plaintext values
// pass through unchanged. The returned map is always a fresh allocation;
// the input is never mutated.
//
// Returns a non-nil error and an empty map when:
//   - any value is a secret:// reference and store is nil
//   - the store fails to resolve a reference (missing env var, missing
//     file, etc.) — the error is wrapped so the caller can attribute the
//     failure to a specific header name
//
// A nil or empty input map produces a nil map and a nil error.
//
// This helper exists to keep the trace and metrics exporter init paths
// from each having to reimplement the same secret-resolution loop.
// Both call sites resolve once at construction time; refresh-on-rotate
// is intentionally out of scope (the OTel exporter holds the resolved
// header for the lifetime of the run, which mirrors how API keys are
// already handled by the provider adapters).
func ResolveHeaders(ctx context.Context, store security.SecretStore, headers map[string]string) (map[string]string, error) {
	if len(headers) == 0 {
		return nil, nil
	}

	resolved := make(map[string]string, len(headers))
	for name, value := range headers {
		if !strings.HasPrefix(value, secretRefPrefix) {
			resolved[name] = value
			continue
		}
		if store == nil {
			return nil, fmt.Errorf("header %q references a secret (%q) but no SecretStore is configured", name, value)
		}
		v, err := store.Resolve(ctx, value)
		if err != nil {
			return nil, fmt.Errorf("resolve header %q: %w", name, err)
		}
		resolved[name] = v
	}
	return resolved, nil
}
