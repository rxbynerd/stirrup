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

// anthropicOAuthURL is the public Anthropic OAuth token-exchange
// endpoint. Held as a const and copied into each source's tokenURL
// field so tests can swap in an httptest.Server without mutating
// shared state. Production code never modifies the field.
const anthropicOAuthURL = "https://api.anthropic.com/v1/oauth/token"

// anthropicVersionHeader pins the Anthropic API version for the
// token-exchange request. Anthropic's docs require the header on every
// request to /v1/oauth/token; the value here matches what the official
// SDKs send.
const anthropicVersionHeader = "2023-06-01"

// anthropicJWTBearerGrantType is the OAuth 2.0 grant type for JWT
// bearer assertion (RFC 7523). Anthropic uses the same identifier the
// IETF spec defines.
const anthropicJWTBearerGrantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"

// AnthropicWIFSource exchanges an OIDC identity token (from any
// TokenSource — file/env/aws-irsa/azure-imds/github-actions-oidc/
// gke-metadata) for a short-lived Anthropic access token via Workload
// Identity Federation. The result is a Bearer token suitable for
// Authorization on /v1/messages.
//
// Flow on each refresh:
//  1. Fetch the OIDC JWT from tokenSource.Token(ctx). Re-read every
//     time — projected k8s tokens and GitHub Actions OIDC tokens
//     rotate ahead of their nominal exp, and Anthropic's docs require
//     the JWT to be unexpired when the assertion is sent.
//  2. POST JSON to https://api.anthropic.com/v1/oauth/token with the
//     federation identifiers (rule, org, service account, optional
//     workspace) and grant_type=jwt-bearer.
//  3. Parse the OAuth response and return an oauth2.Token; the wrapping
//     ReuseTokenSource caches it until expiry-soon.
//
// Concurrency is provided entirely by oauth2.ReuseTokenSource's built-
// in single-flight; AnthropicWIFSource itself holds no mutable state
// after construction (tokenURL is set once at NewAnthropicWIFSource
// time and is only mutated by tests before use).
//
// No new SDK dependencies — Anthropic's OAuth endpoint is plain HTTPS
// JSON; we hand-roll the request to keep the dependency surface small
// (consistent with the rest of the credential package).
type AnthropicWIFSource struct {
	tokenSource      TokenSource
	federationRuleID string
	organizationID   string
	serviceAccountID string
	workspaceID      string

	httpClient *http.Client
	tokenURL   string // overridable for testing
}

// NewAnthropicWIFSource constructs an Anthropic WIF source. ts supplies
// the OIDC proof; the four identifier fields parameterise the
// token-exchange request. workspaceID may be empty (rule bound to a
// single workspace), the literal "default" (Anthropic-side magic
// string), or a structured "wrkspc_..." identifier — all three forms
// are accepted by the Anthropic OAuth endpoint.
func NewAnthropicWIFSource(ts TokenSource, federationRuleID, organizationID, serviceAccountID, workspaceID string) *AnthropicWIFSource {
	return &AnthropicWIFSource{
		tokenSource:      ts,
		federationRuleID: federationRuleID,
		organizationID:   organizationID,
		serviceAccountID: serviceAccountID,
		workspaceID:      workspaceID,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
			},
		},
		tokenURL: anthropicOAuthURL,
	}
}

// Resolve validates the configured fields and returns a Resolved whose
// BearerToken closure performs the token exchange lazily on first
// invocation. The closure is wrapped in oauth2.ReuseTokenSource so
// subsequent calls return the cached access token until the documented
// expiry, with single-flight refresh under contention.
//
// The "Anthropic WIF:" error prefix is a deliberate log-greppability
// convention shared with the GCP WIF source so federation errors
// group together in operator dashboards.
//
//nolint:staticcheck // ST1005: capitalized prefix is intentional, see above.
func (a *AnthropicWIFSource) Resolve(_ context.Context) (*Resolved, error) {
	if a.tokenSource == nil {
		return nil, fmt.Errorf("Anthropic WIF: token source is required")
	}
	if a.federationRuleID == "" {
		return nil, fmt.Errorf("Anthropic WIF: federation_rule_id is required")
	}
	if a.organizationID == "" {
		return nil, fmt.Errorf("Anthropic WIF: organization_id is required")
	}
	if a.serviceAccountID == "" {
		return nil, fmt.Errorf("Anthropic WIF: service_account_id is required")
	}

	inner := &anthropicWIFTokenSource{src: a}
	cached := oauth2.ReuseTokenSource(nil, inner)
	return &Resolved{BearerToken: bearerFromTokenSource(cached)}, nil
}

// anthropicWIFTokenSource implements oauth2.TokenSource. Token() runs
// the full OAuth exchange flow and returns a token whose Expiry the
// ReuseTokenSource wrapper inspects to decide when to call us again.
type anthropicWIFTokenSource struct {
	src *AnthropicWIFSource
}

// anthropicOAuthRequest is the JSON body shape documented at
// https://platform.claude.com/docs/en/manage-claude/wif-reference.
// workspace_id is omitted from the request when empty (omitempty)
// because Anthropic's endpoint requires the field absent — not
// present-as-empty-string — when the federation rule is bound to a
// single workspace.
type anthropicOAuthRequest struct {
	GrantType        string `json:"grant_type"`
	Assertion        string `json:"assertion"`
	FederationRuleID string `json:"federation_rule_id"`
	OrganizationID   string `json:"organization_id"`
	ServiceAccountID string `json:"service_account_id"`
	WorkspaceID      string `json:"workspace_id,omitempty"`
}

// anthropicOAuthResponse mirrors the documented success response. We
// parse only the fields needed to drive the bearer-token refresh loop;
// trailing fields (`scope`) are read but unused beyond surfacing.
type anthropicOAuthResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	Scope       string `json:"scope"`
}

// Token performs the OAuth token-exchange. Implements oauth2.TokenSource.
//
// The "Anthropic WIF:" error prefix is a deliberate convention shared
// with google_federation.go so federation failures group together in
// log/dashboard searches.
//
//nolint:staticcheck // ST1005: capitalized prefix is intentional, see above.
func (t *anthropicWIFTokenSource) Token() (*oauth2.Token, error) {
	// Internal context — the oauth2.TokenSource contract has no caller
	// ctx. A cancelled adapter request must not poison subsequent
	// refreshes (mirrors the rationale in google_federation.go's
	// federationTokenSource.Token). 30 seconds covers the single
	// round-trip with comfortable headroom.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	jwt, err := t.src.tokenSource.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("Anthropic WIF: fetch OIDC token: %w", err)
	}
	if len(jwt) == 0 {
		return nil, fmt.Errorf("Anthropic WIF: token source returned empty token")
	}

	body, err := json.Marshal(&anthropicOAuthRequest{
		GrantType:        anthropicJWTBearerGrantType,
		Assertion:        string(jwt),
		FederationRuleID: t.src.federationRuleID,
		OrganizationID:   t.src.organizationID,
		ServiceAccountID: t.src.serviceAccountID,
		WorkspaceID:      t.src.workspaceID,
	})
	if err != nil {
		return nil, fmt.Errorf("Anthropic WIF: marshal token request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.src.tokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("Anthropic WIF: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", anthropicVersionHeader)

	resp, err := t.src.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Anthropic WIF: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, stsResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("Anthropic WIF: read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Anthropic returns a request_id header on every response; surface
		// it in the error so operators can correlate with the Console
		// authentication-history page. The body is bounded by
		// truncateForError so a hostile or misconfigured endpoint cannot
		// flood logs through this path.
		reqID := resp.Header.Get("request-id")
		if reqID == "" {
			return nil, fmt.Errorf(
				"Anthropic WIF: token exchange returned %d: %s",
				resp.StatusCode,
				truncateForError(respBody),
			)
		}
		return nil, fmt.Errorf(
			"Anthropic WIF: token exchange returned %d (request_id=%s): %s",
			resp.StatusCode,
			reqID,
			truncateForError(respBody),
		)
	}

	var parsed anthropicOAuthResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("Anthropic WIF: parse token response: %w", err)
	}
	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("Anthropic WIF: token exchange returned empty access_token")
	}

	// Anthropic's docs document expires_in in seconds (typically 3600,
	// configurable per federation rule from 60–86400). Fall back to a
	// 1-hour assumption if the server omits it for any reason — without
	// a non-zero expiry the ReuseTokenSource wrapper would treat the
	// token as already expired and re-hit the exchange endpoint on
	// every adapter request.
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
