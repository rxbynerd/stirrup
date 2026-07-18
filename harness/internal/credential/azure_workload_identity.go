package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// azureDefaultScope is the OAuth2 scope used for Azure OpenAI /
// Cognitive Services; operators may override it for other audiences.
const azureDefaultScope = "https://cognitiveservices.azure.com/.default"

// azureTokenURLTemplate is a printf template for the Microsoft Entra ID
// token endpoint (global Azure cloud); `%s` is the URL-escaped tenant
// UUID. Sovereign clouds use the CredentialConfig.AzureTokenURL
// override — see docs/azure-workload-identity.md.
const azureTokenURLTemplate = "https://login.microsoftonline.com/%s/oauth2/v2.0/token"

// AzureWorkloadIdentitySource exchanges an OIDC identity token (from any
// TokenSource) for a short-lived Microsoft Entra ID access token via the
// OAuth2 client_credentials grant with a JWT client assertion. See
// docs/azure-workload-identity.md for the exchange flow.
//
// The wire format differs from GCP STS in two ways: the body is
// form-encoded rather than JSON, and errors carry a `correlation_id`
// (or camelCase `correlationId`) that the exchange surfaces in the
// wrapped error when present — the operator's handle for a Microsoft
// support ticket.
type AzureWorkloadIdentitySource struct {
	tokenSource TokenSource
	tenantID    string
	clientID    string
	scope       string

	httpClient *http.Client
	tokenURL   string // overridable for testing
}

// NewAzureWorkloadIdentitySource constructs an Azure WIF source. ts
// supplies the OIDC proof, tenantID is the Azure AD tenant UUID,
// clientID identifies the App Registration / federated identity, and
// scope is the OAuth2 audience (empty defaults to Azure OpenAI /
// Cognitive Services).
//
// The optional variadic tokenURLOverride lets sovereign-cloud
// deployments (Azure Government, China, Germany) point the exchange at
// a non-default authority; the first non-empty entry wins. Variadic
// rather than positional so existing four-arg call sites are unchanged.
func NewAzureWorkloadIdentitySource(ts TokenSource, tenantID, clientID, scope string, tokenURLOverride ...string) *AzureWorkloadIdentitySource {
	if scope == "" {
		scope = azureDefaultScope
	}
	tokenURL := fmt.Sprintf(azureTokenURLTemplate, url.PathEscape(tenantID))
	for _, override := range tokenURLOverride {
		if override != "" {
			tokenURL = override
			break
		}
	}
	return &AzureWorkloadIdentitySource{
		tokenSource: ts,
		tenantID:    tenantID,
		clientID:    clientID,
		scope:       scope,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
			},
		},
		tokenURL: tokenURL,
	}
}

// Resolve validates the configured fields and returns a Resolved whose
// BearerToken closure performs the Entra token exchange lazily, wrapped
// in oauth2.ReuseTokenSource so subsequent calls return the cached
// access token until expiry. tenantID/clientID emptiness is also
// rejected by types.ValidateRunConfig; checking again here covers
// programmatic callers that bypass the config layer.
func (a *AzureWorkloadIdentitySource) Resolve(_ context.Context) (*Resolved, error) {
	if a.tokenSource == nil {
		return nil, fmt.Errorf("azure-workload-identity: token source is required")
	}
	if a.tenantID == "" {
		return nil, fmt.Errorf("azure-workload-identity: tenantID is required")
	}
	if a.clientID == "" {
		return nil, fmt.Errorf("azure-workload-identity: clientID is required")
	}

	// Refresh runs on its own context: a cancelled Resolve ctx must not
	// poison subsequent token refreshes triggered by adapter calls.
	inner := &azureTokenSource{src: a}
	cached := oauth2.ReuseTokenSource(nil, inner)
	return &Resolved{BearerToken: bearerFromTokenSource(cached)}, nil
}

// azureTokenSource implements oauth2.TokenSource, running the full
// client_credentials exchange. ReuseTokenSource serialises concurrent
// callers internally, so no mutex is needed here.
type azureTokenSource struct {
	src *AzureWorkloadIdentitySource
}

// azureTokenResponse mirrors the documented success response. We parse
// only the fields we care about; trailing fields (`ext_expires_in`)
// are ignored.
type azureTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

// azureErrorResponse captures the OAuth2-shaped error envelope
// Microsoft returns. Both snake_case and camelCase correlation IDs
// have been observed in the wild — both fields are populated and we
// pick whichever is non-empty.
type azureErrorResponse struct {
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
	CorrelationID    string `json:"correlation_id,omitempty"`
	CorrelationIDAlt string `json:"correlationId,omitempty"`
}

func (a *azureTokenSource) Token() (*oauth2.Token, error) {
	// Internal context: the oauth2 contract has no caller ctx.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	jwt, err := a.src.tokenSource.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("azure-workload-identity: fetch OIDC token: %w", err)
	}
	if len(jwt) == 0 {
		return nil, fmt.Errorf("azure-workload-identity: token source returned empty subject token")
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", a.src.clientID)
	form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
	form.Set("client_assertion", string(jwt))
	form.Set("scope", a.src.scope)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.src.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("azure-workload-identity: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.src.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("azure-workload-identity: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, stsResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("azure-workload-identity: read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("azure-workload-identity: token endpoint returned %d%s: %s",
			resp.StatusCode,
			correlationIDSuffix(respBody),
			truncateForError(respBody),
		)
	}

	var parsed azureTokenResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("azure-workload-identity: parse token response: %w", err)
	}
	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("azure-workload-identity: token endpoint returned empty access_token")
	}

	tokenType := parsed.TokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}

	// Fall back to a 1-hour assumption if the server omitted expires_in
	// — without a non-zero expiry, ReuseTokenSource would treat the
	// token as already expired and re-hit Entra on every request.
	lifetime := time.Duration(parsed.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = time.Hour
	}
	return &oauth2.Token{
		AccessToken: parsed.AccessToken,
		TokenType:   tokenType,
		Expiry:      time.Now().Add(lifetime),
	}, nil
}

// maxCorrelationIDLen caps the correlation_id surfaced through error
// messages. Real Entra correlation IDs are 36-byte UUIDs; 64 bounds the
// worst case from a hostile or malfunctioning endpoint.
const maxCorrelationIDLen = 64

// correlationIDSuffix returns " (correlation_id=<id>)" when the body
// parses as JSON and carries a correlation_id (snake_case or
// camelCase), or "" otherwise. The id passes through
// sanitiseCorrelationID so control characters, ANSI escapes, and
// oversized values cannot land in slog / OTel / terminal output verbatim.
func correlationIDSuffix(body []byte) string {
	var parsed azureErrorResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	id := parsed.CorrelationID
	if id == "" {
		id = parsed.CorrelationIDAlt
	}
	if id == "" {
		return ""
	}
	return fmt.Sprintf(" (correlation_id=%s)", sanitiseCorrelationID(id))
}

// sanitiseCorrelationID strips non-printable bytes (control chars, ANSI
// escapes, embedded NULs) and caps the length at maxCorrelationIDLen so
// the result is safe to embed verbatim in slog, OTel, and terminal output.
func sanitiseCorrelationID(id string) string {
	var b strings.Builder
	b.Grow(len(id))
	for _, r := range id {
		// Restrict to printable ASCII; the documented UUID shape means
		// any byte outside that range is anomalous.
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) > maxCorrelationIDLen {
		s = s[:maxCorrelationIDLen]
	}
	return s
}
