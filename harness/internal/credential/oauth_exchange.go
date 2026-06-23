package credential

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

// oauthExchangeResponse is the subset of an OAuth 2.0 token-exchange success
// response the bearer-refresh loop needs. Both JSON-bodied WIF sources —
// Anthropic (RFC 7523 jwt-bearer) and OpenAI (RFC 8693 token-exchange) —
// return this shape; provider-specific extras (e.g. OpenAI's
// issued_token_type) are ignored.
type oauthExchangeResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	Scope       string `json:"scope"`
}

// doJSONTokenExchange POSTs a pre-marshalled JSON token-exchange request and
// parses the standard OAuth success response into an oauth2.Token. It is the
// shared transport-and-parse skeleton behind the JSON-bodied WIF sources
// (anthropic_wif.go, openai_wif.go).
//
// The caller owns the provider-specific parts — the request body and the
// header set — because the grant type, field names, and version headers
// differ per provider. This helper owns the parts that are identical: the
// bounded response read (stsResponseLimit), the non-2xx error decoration,
// the empty-access_token guard, and the expires_in → Expiry calculation with
// a one-hour fallback.
//
// label prefixes every error so federation failures group together in
// operator dashboards (e.g. "Anthropic WIF", "OpenAI WIF"). correlationHeader,
// when non-empty, names a response header whose value is appended to a non-2xx
// error as request_id=<value> for console correlation; an absent header is
// simply omitted.
//
// The grant assertion (the subject token in the request body) is never
// logged: only the bounded response body, the status code, and the
// correlation header reach the error string.
func doJSONTokenExchange(
	ctx context.Context,
	client *http.Client,
	tokenURL string,
	headers map[string]string,
	body []byte,
	label string,
	correlationHeader string,
) (*oauth2.Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: build token request: %w", label, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		// TODO(issue #117 follow-up): emit a `federation_exchange_failed`
		// security event here so operators can dashboard refresh failures
		// alongside other security events. The credential package has no
		// SecurityLogger handle today — adding one requires a callback
		// wired by the factory. Deferred to keep this skeleton leaf-free.
		return nil, fmt.Errorf("%s: token request: %w", label, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, stsResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("%s: read token response: %w", label, err)
	}

	if resp.StatusCode != http.StatusOK {
		var corr string
		if correlationHeader != "" {
			// The correlation header is server-controlled. Sanitise it
			// (strip non-printable bytes, cap length) so a hostile or
			// misconfigured token endpoint cannot inject newlines into a
			// slog line or pad the error with an oversized value — the
			// response body is already bounded by truncateForError, but the
			// header is not read through it. Shares the helper with the
			// Azure source's body-borne correlation id.
			corr = sanitiseCorrelationID(resp.Header.Get(correlationHeader))
		}
		if corr == "" {
			return nil, fmt.Errorf(
				"%s: token exchange returned %d: %s",
				label, resp.StatusCode, truncateForError(respBody),
			)
		}
		return nil, fmt.Errorf(
			"%s: token exchange returned %d (request_id=%s): %s",
			label, resp.StatusCode, corr, truncateForError(respBody),
		)
	}

	var parsed oauthExchangeResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("%s: parse token response: %w", label, err)
	}
	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("%s: token exchange returned empty access_token", label)
	}

	// expires_in is in seconds. Fall back to one hour if the server omits it
	// for any reason — without a non-zero expiry the ReuseTokenSource wrapper
	// would treat the token as already expired and re-hit the exchange
	// endpoint on every adapter request.
	lifetime := time.Duration(parsed.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = time.Hour
	}
	return &oauth2.Token{
		AccessToken: parsed.AccessToken,
		TokenType:   "Bearer",
		Expiry:      time.Now().Add(lifetime),
	}, nil
}
