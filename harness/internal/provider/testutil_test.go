package provider

import (
	"context"

	"golang.org/x/oauth2"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
)

// staticBearer returns a credential.BearerTokenFunc that yields s on every
// call with no IO. Used across provider tests in place of the old
// string-typed apiKey constructor argument so callers can keep the
// existing `NewAnthropicAdapter("test-key")` ergonomics behind the
// closure shape: `NewAnthropicAdapter(staticBearer("test-key"))`.
func staticBearer(s string) credential.BearerTokenFunc {
	return func(_ context.Context) (string, error) {
		return s, nil
	}
}

// bearerFromTokenSource adapts an oauth2.TokenSource to BearerTokenFunc
// for the gemini tests. It is the test-side mirror of the production
// credential.bearerFromTokenSource helper, kept package-local because
// only the gemini tests still construct adapters around an oauth2
// stub (stubTokenSource) directly.
func bearerFromTokenSource(ts oauth2.TokenSource) credential.BearerTokenFunc {
	return func(_ context.Context) (string, error) {
		tok, err := ts.Token()
		if err != nil {
			return "", err
		}
		return tok.AccessToken, nil
	}
}
