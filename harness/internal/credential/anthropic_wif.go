package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

// anthropicOAuthURL is the Anthropic OAuth token-exchange endpoint,
// copied into each source's tokenURL so tests can swap in an
// httptest.Server without mutating shared state.
const anthropicOAuthURL = "https://api.anthropic.com/v1/oauth/token"

// anthropicVersionHeader is required by Anthropic on every request to
// /v1/oauth/token.
const anthropicVersionHeader = "2023-06-01"

// anthropicJWTBearerGrantType is the OAuth 2.0 grant type for JWT
// bearer assertion (RFC 7523).
const anthropicJWTBearerGrantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"

// AnthropicWIFSource exchanges an OIDC identity token for a short-lived
// Anthropic access token via Workload Identity Federation. See
// docs/anthropic-wif.md for the refresh flow and risk mitigations.
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
// BearerToken closure performs the token exchange lazily, cached via
// oauth2.ReuseTokenSource.
//
//nolint:staticcheck // ST1005: capitalized "Anthropic WIF:" prefix is intentional, shared convention across federation sources.
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

// anthropicWIFTokenSource implements oauth2.TokenSource.
type anthropicWIFTokenSource struct {
	src *AnthropicWIFSource
}

// anthropicOAuthRequest is the JSON body shape documented at
// https://platform.claude.com/docs/en/manage-claude/wif-reference.
// workspace_id must be absent (not empty-string) when the federation
// rule is bound to a single workspace, hence omitempty.
type anthropicOAuthRequest struct {
	GrantType        string `json:"grant_type"`
	Assertion        string `json:"assertion"`
	FederationRuleID string `json:"federation_rule_id"`
	OrganizationID   string `json:"organization_id"`
	ServiceAccountID string `json:"service_account_id"`
	WorkspaceID      string `json:"workspace_id,omitempty"`
}

// Token performs the OAuth token-exchange. Implements oauth2.TokenSource.
// The transport, bounded response read, error decoration, and expiry
// calculation are delegated to doJSONTokenExchange, shared with the
// OpenAI WIF source.
//
//nolint:staticcheck // ST1005: capitalized "Anthropic WIF:" prefix is intentional, shared convention across federation sources.
func (t *anthropicWIFTokenSource) Token() (*oauth2.Token, error) {
	// context.Background(), not the Resolve ctx: the oauth2.TokenSource
	// contract has no caller ctx, and a cancelled adapter request must
	// not poison subsequent refreshes.
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

	return doJSONTokenExchange(ctx, t.src.httpClient, t.src.tokenURL, map[string]string{
		"Content-Type":      "application/json",
		"Accept":            "application/json",
		"anthropic-version": anthropicVersionHeader,
	}, body, "Anthropic WIF", "request-id")
}
