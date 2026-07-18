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
// endpoint (the auth plane, not api.openai.com).
const openAIOAuthURL = "https://auth.openai.com/oauth/token"

// openAITokenExchangeGrantType is the RFC 8693 OAuth 2.0 Token Exchange
// grant OpenAI WIF uses, unlike Anthropic WIF's RFC 7523 jwt-bearer grant.
const openAITokenExchangeGrantType = "urn:ietf:params:oauth:grant-type:token-exchange"

// openAIDefaultSubjectTokenType is the default subject_token_type;
// operators may override it (e.g. to id_token) through config.
const openAIDefaultSubjectTokenType = "urn:ietf:params:oauth:token-type:jwt"

// OpenAIWIFSource exchanges an OIDC identity token (from any TokenSource)
// for a short-lived OpenAI access token via Workload Identity Federation.
// See docs/openai-wif.md for the exchange flow and wire contract.
//
// The audience is not part of the exchange body: OpenAI validates it from
// the subject token's aud claim against the provider config, so it is
// configured on the TokenSource, not here.
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
// BearerToken closure performs the token exchange lazily, wrapped in
// oauth2.ReuseTokenSource for cache + single-flight refresh.
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
// Transport, bounded response read, error decoration, and expiry
// calculation are delegated to doJSONTokenExchange, shared with the
// Anthropic WIF source; x-request-id is read opportunistically since
// OpenAI does not document a correlation header on this endpoint.
//
// The "OpenAI WIF:" error prefix is a deliberate convention shared with the
// other federation sources so failures group together in dashboard searches.
//
//nolint:staticcheck // ST1005: capitalized prefix is intentional, see above.
func (t *openAIWIFTokenSource) Token() (*oauth2.Token, error) {
	// Internal context: the oauth2.TokenSource contract has no caller
	// ctx, and a cancelled adapter request must not poison later refreshes.
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
