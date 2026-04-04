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

func (s *StaticSource) Resolve(ctx context.Context) (*Resolved, error) {
	token, err := s.secrets.Resolve(ctx, s.ref)
	if err != nil {
		return nil, fmt.Errorf("resolve static credential: %w", err)
	}
	return &Resolved{BearerToken: token}, nil
}
