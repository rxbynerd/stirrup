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

// azureDefaultScope is the OAuth2 scope used for Azure OpenAI / Cognitive
// Services. Operators MAY override this for non-default Azure audiences,
// but the vast majority of stirrup deployments target Azure OpenAI and
// the default keeps RunConfigs concise.
const azureDefaultScope = "https://cognitiveservices.azure.com/.default"

// azureTokenURLTemplate is a printf template for the Microsoft Entra ID
// token endpoint. The `%s` is filled with the URL-escaped tenant UUID.
// This is the global Azure cloud endpoint; sovereign clouds
// (login.microsoftonline.us / .cn / .de) are not yet exposed via
// RunConfig (tracked as future work in issue #118).
const azureTokenURLTemplate = "https://login.microsoftonline.com/%s/oauth2/v2.0/token"

// AzureWorkloadIdentitySource exchanges an OIDC identity token (from any
// TokenSource — projected k8s file, IRSA, Azure IMDS, GHA, env, …) for a
// short-lived Microsoft Entra ID access token via the OAuth2
// client_credentials grant with a JWT client assertion.
//
// Flow:
//  1. Fetch the OIDC token from tokenSource.Token(ctx). The token is
//     re-read on every refresh so projected-volume rotations and IRSA
//     file updates are picked up automatically.
//  2. POST application/x-www-form-urlencoded to
//     https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token with
//     grant_type=client_credentials and client_assertion={JWT}.
//  3. Wrap the result in oauth2.ReuseTokenSource so the access token is
//     cached and refreshed lazily; the returned BearerToken closure can
//     be invoked freely on every provider request without re-hitting
//     Entra. ReuseTokenSource also provides single-flight semantics, so
//     no separate sync.Mutex is required here.
//
// The wire format differs from GCP STS in two material ways:
//   - Body is form-encoded, not JSON (Microsoft's documented contract).
//   - Errors carry a `correlation_id` (or sometimes `correlationId`)
//     which is the operator's only handle when filing a Microsoft
//     support ticket. The exchange surfaces it in the wrapped error
//     when present.
//
// No new SDK dependencies — the Entra token endpoint is plain HTTPS
// with form-encoded bodies; we hand-roll the request to keep the
// dependency surface small (consistent with the rest of the credential
// package).
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
func NewAzureWorkloadIdentitySource(ts TokenSource, tenantID, clientID, scope string) *AzureWorkloadIdentitySource {
	if scope == "" {
		scope = azureDefaultScope
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
		tokenURL: fmt.Sprintf(azureTokenURLTemplate, url.PathEscape(tenantID)),
	}
}

// Resolve validates the configured fields and returns a Resolved whose
// BearerToken closure performs the Entra token exchange lazily on first
// invocation. The closure is wrapped in oauth2.ReuseTokenSource so
// subsequent calls return the cached access token until expiry.
//
// Defence-in-depth: tenantID/clientID emptiness is also rejected by
// types.ValidateRunConfig, but checking again here means a programmatic
// caller of NewAzureWorkloadIdentitySource that bypasses the config
// layer still gets a clear error rather than a malformed request to
// Entra.
func (a *AzureWorkloadIdentitySource) Resolve(_ context.Context) (*Resolved, error) {
	if a.tokenSource == nil {
		return nil, fmt.Errorf("Azure WIF: token source is required")
	}
	if a.tenantID == "" {
		return nil, fmt.Errorf("Azure WIF: tenantID is required")
	}
	if a.clientID == "" {
		return nil, fmt.Errorf("Azure WIF: clientID is required")
	}

	// Refresh runs on its own context (mirrors federationTokenSource):
	// a cancelled Resolve ctx must not poison subsequent token
	// refreshes triggered by adapter calls inside the agentic loop.
	inner := &azureTokenSource{src: a}
	cached := oauth2.ReuseTokenSource(nil, inner)
	return &Resolved{BearerToken: bearerFromTokenSource(cached)}, nil
}

// azureTokenSource implements oauth2.TokenSource. Token() runs the
// full client_credentials exchange, returning a token whose Expiry
// the ReuseTokenSource wrapper inspects to decide whether to call us
// again. ReuseTokenSource serialises concurrent callers internally,
// so no mutex is needed here.
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
	// Internal context — the oauth2 contract has no caller ctx. A
	// 30-second budget covers the round-trip to Entra with comfortable
	// headroom; matches GCP federation precedent.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	jwt, err := a.src.tokenSource.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("Azure WIF: fetch OIDC token: %w", err)
	}
	if len(jwt) == 0 {
		return nil, fmt.Errorf("Azure WIF: token source returned empty subject token")
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", a.src.clientID)
	form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
	form.Set("client_assertion", string(jwt))
	form.Set("scope", a.src.scope)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.src.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("Azure WIF: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.src.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Azure WIF: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Reuse the GCP federation body cap (64 KiB). Real Entra responses
	// are well under 8 KiB; the cap exists so a misconfigured proxy
	// cannot exhaust memory streaming an unbounded payload into the
	// JSON parser.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, stsResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("Azure WIF: read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Azure WIF: token endpoint returned %d%s: %s",
			resp.StatusCode,
			correlationIDSuffix(respBody),
			truncateForError(respBody),
		)
	}

	var parsed azureTokenResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("Azure WIF: parse token response: %w", err)
	}
	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("Azure WIF: token endpoint returned empty access_token")
	}

	tokenType := parsed.TokenType
	if tokenType == "" {
		tokenType = "Bearer"
	}

	// Entra always returns expires_in in seconds (typically 3599 or
	// 5399). Fall back to a 1-hour assumption if the server omitted it
	// for any reason — without a non-zero expiry, ReuseTokenSource
	// would treat the token as already expired and re-hit Entra on
	// every adapter request, exhausting tenant rate limits.
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
// messages. Real Entra correlation IDs are 36-byte UUIDs; 64 leaves
// modest headroom while bounding the worst case if a hostile or
// malfunctioning endpoint returns a much larger value.
const maxCorrelationIDLen = 64

// correlationIDSuffix returns " (correlation_id=<id>)" when the body
// parses as JSON and carries a correlation_id (snake_case or
// camelCase). On non-JSON or absent IDs it returns the empty string —
// json.Unmarshal handles non-JSON bodies cleanly without panicking, so
// no defensive recover is required. The correlation ID is the only
// handle operators have when filing a Microsoft support ticket; the
// JWT itself never appears in error output.
//
// The id passes through sanitiseCorrelationID before formatting so
// control characters, ANSI escapes, and oversized values from a
// hostile or malfunctioning endpoint cannot land in slog / OTel /
// terminal output verbatim.
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
// escapes, embedded NULs) and caps the length at maxCorrelationIDLen.
// The resulting string is safe to embed verbatim in slog attributes,
// OTel span events, and terminal output — none of which expects to
// receive control sequences from a remote authority server.
func sanitiseCorrelationID(id string) string {
	var b strings.Builder
	b.Grow(len(id))
	for _, r := range id {
		// Restrict to printable ASCII (space through tilde). The Entra
		// correlation_id is documented as a UUID, so any byte outside
		// that range is anomalous and we drop it rather than try to
		// preserve it.
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
