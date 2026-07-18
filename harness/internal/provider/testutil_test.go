package provider

import (
	"context"
	"errors"

	"golang.org/x/oauth2"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
)

// staticBearer returns a credential.BearerTokenFunc that yields s on every
// call with no IO.
func staticBearer(s string) credential.BearerTokenFunc {
	return func(_ context.Context) (string, error) {
		return s, nil
	}
}

// erroringBearer returns a credential.BearerTokenFunc that always fails,
// for asserting adapters surface the resolve error before the HTTP request.
func erroringBearer(msg string) credential.BearerTokenFunc {
	return func(_ context.Context) (string, error) {
		return "", errors.New(msg)
	}
}

// bearerFromTokenSource adapts an oauth2.TokenSource to BearerTokenFunc
// for the gemini tests, which construct adapters around an oauth2 stub.
func bearerFromTokenSource(ts oauth2.TokenSource) credential.BearerTokenFunc {
	return func(_ context.Context) (string, error) {
		tok, err := ts.Token()
		if err != nil {
			return "", err
		}
		return tok.AccessToken, nil
	}
}
