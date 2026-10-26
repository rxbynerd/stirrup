package credential

import (
	"context"
	"fmt"

	"github.com/rxbynerd/stirrup/harness/internal/security"
)

// StaticSource resolves a bearer token from the existing SecretStore.
// This is the default credential source for API-key-based providers
// (Anthropic, OpenAI-compatible) and maintains backward compatibility
// with the secret:// reference scheme.
type StaticSource struct {
	secrets security.SecretStore
	ref     string
}

// Resolve fetches the secret once via the SecretStore, then returns a
// Resolved whose BearerToken closure yields the captured value on every
// call without further IO. The closure is safe for concurrent use because
// it reads only the captured immutable string.
//
// An empty reference means no credential: the closure yields an empty
// token and the SecretStore is not consulted. The OpenAI adapters send an
// empty token as no auth; the Anthropic adapter would send an empty
// x-api-key, so the factory admits an empty reference only for an
// OpenAI-wire provider at a custom baseUrl.
func (s *StaticSource) Resolve(ctx context.Context) (*Resolved, error) {
	if s.ref == "" {
		return &Resolved{
			BearerToken: func(_ context.Context) (string, error) {
				return "", nil
			},
		}, nil
	}
	token, err := s.secrets.Resolve(ctx, s.ref)
	if err != nil {
		return nil, fmt.Errorf("resolve static credential: %w", err)
	}
	cached := token
	return &Resolved{
		BearerToken: func(_ context.Context) (string, error) {
			return cached, nil
		},
	}, nil
}
