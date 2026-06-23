package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

// openAIOAuthURL is the OpenAI Workload Identity Federation token-exchange
// endpoint. The host is auth.openai.com (the auth plane), NOT api.openai.com.
// Held as a const and copied into each source's tokenURL field so tests can
// swap in an httptest.Server without mutating shared state. Production code
// never modifies the field.
const openAIOAuthURL = "https://auth.openai.com/oauth/token"

// openAITokenExchangeGrantType is the RFC 8693 OAuth 2.0 Token Exchange
// grant. OpenAI WIF swaps a subject token for an access token; this is the
// structural divergence from Anthropic WIF, which uses the RFC 7523
// jwt-bearer grant.
const openAITokenExchangeGrantType = "urn:ietf:params:oauth:grant-type:token-exchange"

// openAIDefaultSubjectTokenType is the default subject_token_type for the
// exchange. Every OpenAI-documented identity provider (GKE/EKS/AKS projected
// tokens, GCE/Azure IMDS, GitHub Actions OIDC, SPIFFE JWT-SVID) presents a
// JWT subject token, so jwt is the right default; operators may override it
// (e.g. an id_token) through config.
const openAIDefaultSubjectTokenType = "urn:ietf:params:oauth:token-type:jwt"

// OpenAIWIFSource exchanges an OIDC identity token (from any TokenSource —
// file/env/aws-irsa/azure-imds/github-actions-oidc/gke-metadata) for a
// short-lived OpenAI access token via Workload Identity Federation. The
// result is a Bearer token suitable for Authorization against the OpenAI API.
//
// Flow on each refresh:
//  1. Fetch the OIDC JWT from tokenSource.Token(ctx). Re-read every time —
//     projected k8s tokens and GitHub Actions OIDC tokens rotate ahead of
//     their nominal exp, and OpenAI's access token never outlives the subject
//     token used for the exchange.
//  2. POST JSON to https://auth.openai.com/oauth/token with the RFC 8693
//     token-exchange grant, the subject token, and the operator's identity
//     provider + service account IDs.
//  3. Parse the OAuth response and return an oauth2.Token; the wrapping
//     ReuseTokenSource caches it until expiry-soon.
//
// The audience is NOT a member of the exchange body. OpenAI validates it from
// the subject token's aud claim against the provider config, so the audience
// (canonically https://api.openai.com/v1) is configured on the TokenSource,
// not here. Likewise org/project are bound by the OpenAI service-account
// mapping server-side, so neither appears in the request.
//
// Concurrency is provided entirely by oauth2.ReuseTokenSource's built-in
// single-flight; OpenAIWIFSource itself holds no mutable state after
// construction (tokenURL is set once at NewOpenAIWIFSource time and is only
// mutated by tests before use).
//
// No new SDK dependencies — OpenAI's OAuth endpoint is plain HTTPS JSON; the
// request is hand-rolled to keep the dependency surface small, consistent
// with the rest of the credential package.
type OpenAIWIFSource struct {
	tokenSource        TokenSource
	identityProviderID string
	serviceAccountID   string
	subjectTokenType   string

	httpClient *http.Client
	tokenURL   string // overridable for testing
}

// NewOpenAIWIFSource constructs an OpenAI WIF source. ts supplies the OIDC
// proof; identityProviderID and serviceAccountID parameterise the
// token-exchange request. subjectTokenType may be empty, in which case the
// JWT default (urn:ietf:params:oauth:token-type:jwt) is applied.
func NewOpenAIWIFSource(ts TokenSource, identityProviderID, serviceAccountID, subjectTokenType string) *OpenAIWIFSource {
	if subjectTokenType == "" {
		subjectTokenType = openAIDefaultSubjectTokenType
	}
	return &OpenAIWIFSource{
		tokenSource:        ts,
		identityProviderID: identityProviderID,
		serviceAccountID:   serviceAccountID,
		subjectTokenType:   subjectTokenType,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
			},
		},
		tokenURL: openAIOAuthURL,
	}
}

// Resolve validates the configured fields and returns a Resolved whose
// BearerToken closure performs the token exchange lazily on first
// invocation, wrapped in oauth2.ReuseTokenSource for cache + single-flight
// refresh.
//
// The "OpenAI WIF:" error prefix is a deliberate log-greppability convention
// shared with the other federation sources so federation errors group
// together in operator dashboards.
//
//nolint:staticcheck // ST1005: capitalized prefix is intentional, see above.
func (o *OpenAIWIFSource) Resolve(_ context.Context) (*Resolved, error) {
	if o.tokenSource == nil {
		return nil, fmt.Errorf("OpenAI WIF: token source is required")
	}
	if o.identityProviderID == "" {
		return nil, fmt.Errorf("OpenAI WIF: identity_provider_id is required")
	}
	if o.serviceAccountID == "" {
		return nil, fmt.Errorf("OpenAI WIF: service_account_id is required")
	}

	inner := &openAIWIFTokenSource{src: o}
	cached := oauth2.ReuseTokenSource(nil, inner)
	return &Resolved{BearerToken: bearerFromTokenSource(cached)}, nil
}

// openAIWIFTokenSource implements oauth2.TokenSource. Token() runs the full
// OAuth exchange flow and returns a token whose Expiry the ReuseTokenSource
// wrapper inspects to decide when to call us again.
type openAIWIFTokenSource struct {
	src *OpenAIWIFSource
}

// openAIOAuthRequest is the JSON body documented at
// https://developers.openai.com/api/reference/workload-identity-federation.
// There is no audience / organization / project / workspace field: the
// audience rides on the subject token's aud claim, and org/project are bound
// by the service-account mapping server-side.
type openAIOAuthRequest struct {
	GrantType          string `json:"grant_type"`
	SubjectTokenType   string `json:"subject_token_type"`
	SubjectToken       string `json:"subject_token"`
	IdentityProviderID string `json:"identity_provider_id"`
	ServiceAccountID   string `json:"service_account_id"`
}

// Token performs the OAuth token-exchange. Implements oauth2.TokenSource.
//
// The request body is OpenAI-specific (RFC 8693 token-exchange shape); the
// transport, bounded response read, error decoration, and expiry calculation
// are delegated to doJSONTokenExchange, shared with the Anthropic WIF source.
// OpenAI does not document a correlation header on the exchange endpoint, so
// x-request-id is read opportunistically and surfaced only when present.
//
// The "OpenAI WIF:" error prefix is a deliberate convention shared with the
// other federation sources so failures group together in dashboard searches.
//
//nolint:staticcheck // ST1005: capitalized prefix is intentional, see above.
func (t *openAIWIFTokenSource) Token() (*oauth2.Token, error) {
	// Internal context — the oauth2.TokenSource contract has no caller ctx.
	// A cancelled adapter request must not poison subsequent refreshes.
	// 30 seconds covers the single round-trip with comfortable headroom.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	jwt, err := t.src.tokenSource.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("OpenAI WIF: fetch OIDC token: %w", err)
	}
	if len(jwt) == 0 {
		return nil, fmt.Errorf("OpenAI WIF: token source returned empty token")
	}

	body, err := json.Marshal(&openAIOAuthRequest{
		GrantType:          openAITokenExchangeGrantType,
		SubjectTokenType:   t.src.subjectTokenType,
		SubjectToken:       string(jwt),
		IdentityProviderID: t.src.identityProviderID,
		ServiceAccountID:   t.src.serviceAccountID,
	})
	if err != nil {
		return nil, fmt.Errorf("OpenAI WIF: marshal token request: %w", err)
	}

	return doJSONTokenExchange(ctx, t.src.httpClient, t.src.tokenURL, map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json",
	}, body, "OpenAI WIF", "x-request-id")
}
