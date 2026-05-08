package credential

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// stsResponseLimit caps the response body when reading from the STS or
// IAM Credentials endpoints. Real responses are well under 8 KiB; the
// cap exists so a misconfigured proxy or hostile endpoint cannot
// exhaust memory by streaming an unbounded payload into the JSON
// parser. 64 KiB matches the bound used by other federation paths
// (`metadataResponseLimit`, `maxServiceAccountKeyBytes`).
const stsResponseLimit = 64 * 1024

// stsErrorBodyLimit bounds how much of an error response body is
// included verbatim in the wrapped error. Keeps log lines tractable.
const stsErrorBodyLimit = 1024

// gcpSTSURL is the public Google STS token-exchange endpoint. Exposed
// as a package-level var (not const) so tests can swap in an
// httptest.Server. Production code never mutates it.
const gcpSTSURL = "https://sts.googleapis.com/v1/token"

// gcpIAMCredURLTemplate is a printf template for the
// generateAccessToken endpoint on iamcredentials.googleapis.com. The
// `%s` is filled with the URL-escaped target service-account email.
const gcpIAMCredURLTemplate = "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/%s:generateAccessToken"

// federatedAudiencePattern is duplicated from types/runconfig.go
// (gcpWIFAudiencePattern) so the credential source can validate at
// Resolve time without taking a `types` import dependency. The two
// regexes must stay in sync; both expressions describe the same
// closed shape.
var federatedAudiencePattern = regexp.MustCompile(
	`^//iam\.googleapis\.com/projects/[0-9]+/locations/global/workloadIdentityPools/[a-z][a-z0-9-]{2,30}[a-z0-9]/providers/[a-z][a-z0-9-]{2,30}[a-z0-9]$`,
)

// GCPWorkloadIdentityFederationSource exchanges an OIDC identity token
// (from any TokenSource — IRSA, Azure IMDS, GHA, file, env, …) for a
// short-lived GCP access token via the Workload Identity Federation
// flow. The result is suitable for Vertex AI Gemini auth on a non-GCP
// runtime.
//
// Flow:
//  1. Fetch the OIDC token from tokenSource.Token(ctx).
//  2. POST JSON to https://sts.googleapis.com/v1/token to exchange it
//     for a federated access token bound to the configured audience.
//  3. (Optional) If serviceAccount is set, POST to
//     https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/{SA}:generateAccessToken
//     to impersonate the target service account, allowing operators to
//     grant narrower IAM than the federated identity itself holds.
//  4. Wrap the result in oauth2.ReuseTokenSource so the access token
//     is cached and refreshed lazily; the BearerToken closure can be
//     invoked freely on every provider request without re-hitting STS.
//
// Audience MUST match the form
//
//	//iam.googleapis.com/projects/{N}/locations/global/workloadIdentityPools/{POOL}/providers/{PROVIDER}
//
// Resolve rejects ill-formed audiences up front so an operator gets a
// precise error rather than a 400 from STS at first use.
//
// No new SDK dependencies — STS and iamcredentials are plain HTTPS
// endpoints; we hand-roll the requests to keep the dependency surface
// small (consistent with the rest of the credential package).
type GCPWorkloadIdentityFederationSource struct {
	tokenSource    TokenSource
	audience       string
	serviceAccount string // optional; empty means "use federated token directly"
	scope          string

	httpClient *http.Client
	stsURL     string // overridable for testing
	iamCredURL string // overridable for testing (printf template; %s = SA email)
}

// NewGCPWorkloadIdentityFederationSource constructs a federation
// source. ts supplies the OIDC proof, audience names the WIF provider,
// and serviceAccount (optional) is the SA email to impersonate after
// the STS exchange.
func NewGCPWorkloadIdentityFederationSource(ts TokenSource, audience, serviceAccount string) *GCPWorkloadIdentityFederationSource {
	return &GCPWorkloadIdentityFederationSource{
		tokenSource:    ts,
		audience:       audience,
		serviceAccount: serviceAccount,
		scope:          cloudPlatformScope,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 10 * time.Second,
			},
		},
		stsURL:     gcpSTSURL,
		iamCredURL: gcpIAMCredURLTemplate,
	}
}

// Resolve validates the configured audience and returns a Resolved
// whose BearerToken closure performs the STS exchange (and optional
// impersonation) lazily on first invocation. The closure is wrapped
// in oauth2.ReuseTokenSource so subsequent calls return the cached
// access token until expiry.
func (g *GCPWorkloadIdentityFederationSource) Resolve(_ context.Context) (*Resolved, error) {
	if g.tokenSource == nil {
		return nil, fmt.Errorf("GCP WIF: token source is required")
	}
	if g.audience == "" {
		return nil, fmt.Errorf("GCP WIF: audience is required")
	}
	if !federatedAudiencePattern.MatchString(g.audience) {
		return nil, fmt.Errorf(
			"GCP WIF: audience %q must match //iam.googleapis.com/projects/{N}/locations/global/workloadIdentityPools/{POOL}/providers/{PROVIDER}",
			g.audience,
		)
	}

	// Refresh runs on its own context (see ServiceAccountKeySource for
	// the same B1-style rationale): a cancelled Resolve ctx must not
	// poison subsequent token refreshes triggered by adapter calls
	// inside the agentic loop.
	inner := &federationTokenSource{src: g}
	cached := oauth2.ReuseTokenSource(nil, inner)
	return &Resolved{BearerToken: bearerFromTokenSource(cached)}, nil
}

// federationTokenSource implements oauth2.TokenSource. Token() runs
// the full STS-exchange (+ optional impersonation) flow, returning a
// token whose Expiry the ReuseTokenSource wrapper inspects to decide
// whether to call us again.
type federationTokenSource struct {
	src *GCPWorkloadIdentityFederationSource
}

// stsRequest is the JSON body shape documented at
// https://cloud.google.com/iam/docs/reference/sts/rest/v1/TopLevel/token.
// All fields are required for the token-exchange grant type.
type stsRequest struct {
	Audience           string `json:"audience"`
	GrantType          string `json:"grantType"`
	RequestedTokenType string `json:"requestedTokenType"`
	Scope              string `json:"scope"`
	SubjectTokenType   string `json:"subjectTokenType"`
	SubjectToken       string `json:"subjectToken"`
}

// stsResponse mirrors the documented success response. We parse only
// the fields we care about; trailing fields (`issued_token_type`) are
// ignored.
type stsResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

// iamCredRequest is the JSON body for generateAccessToken. The full
// API supports a `lifetime` string and a `delegates` list; neither is
// required for the WIF use case so we omit them and let the API apply
// its 1-hour default.
type iamCredRequest struct {
	Scope []string `json:"scope"`
}

// iamCredResponse mirrors the documented response: an access token and
// an RFC3339 expiry timestamp.
type iamCredResponse struct {
	AccessToken string `json:"accessToken"`
	ExpireTime  string `json:"expireTime"`
}

func (f *federationTokenSource) Token() (*oauth2.Token, error) {
	// Internal context — the oauth2 contract has no caller ctx. A
	// 30-second budget covers both the STS round-trip and the
	// optional impersonation hop with comfortable headroom.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	oidc, err := f.src.tokenSource.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("GCP WIF: fetch OIDC token: %w", err)
	}
	if len(oidc) == 0 {
		return nil, fmt.Errorf("GCP WIF: token source returned empty token")
	}

	stsTok, stsExpiry, err := f.exchangeAtSTS(ctx, string(oidc))
	if err != nil {
		return nil, err
	}

	if f.src.serviceAccount == "" {
		return &oauth2.Token{
			AccessToken: stsTok,
			TokenType:   "Bearer",
			Expiry:      stsExpiry,
		}, nil
	}

	saTok, saExpiry, err := f.impersonate(ctx, stsTok)
	if err != nil {
		return nil, err
	}
	return &oauth2.Token{
		AccessToken: saTok,
		TokenType:   "Bearer",
		Expiry:      saExpiry,
	}, nil
}

// exchangeAtSTS performs the token-exchange POST. Returns the
// federated access token and an expiry computed from `expires_in`.
func (f *federationTokenSource) exchangeAtSTS(ctx context.Context, oidc string) (string, time.Time, error) {
	body, err := json.Marshal(&stsRequest{
		Audience:           f.src.audience,
		GrantType:          "urn:ietf:params:oauth:grant-type:token-exchange",
		RequestedTokenType: "urn:ietf:params:oauth:token-type:access_token",
		Scope:              f.src.scope,
		SubjectTokenType:   "urn:ietf:params:oauth:token-type:jwt",
		SubjectToken:       oidc,
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("GCP WIF: marshal STS request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.src.stsURL, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("GCP WIF: build STS request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := f.src.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("GCP WIF: STS request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, stsResponseLimit))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("GCP WIF: read STS response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf(
			"GCP WIF: STS returned %d: %s",
			resp.StatusCode,
			truncateForError(respBody),
		)
	}

	var parsed stsResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", time.Time{}, fmt.Errorf("GCP WIF: parse STS response: %w", err)
	}
	if parsed.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("GCP WIF: STS returned empty access_token")
	}

	// STS always returns expires_in in seconds (typically 3600). Fall
	// back to a 1-hour assumption if the server omitted it for any
	// reason, matching the documented default and keeping
	// ReuseTokenSource able to refresh.
	lifetime := time.Duration(parsed.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = time.Hour
	}
	return parsed.AccessToken, time.Now().Add(lifetime), nil
}

// impersonate exchanges the federated access token for a service-
// account access token via iamcredentials.generateAccessToken.
func (f *federationTokenSource) impersonate(ctx context.Context, federatedToken string) (string, time.Time, error) {
	endpoint := fmt.Sprintf(f.src.iamCredURL, url.PathEscape(f.src.serviceAccount))

	body, err := json.Marshal(&iamCredRequest{Scope: []string{f.src.scope}})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("GCP WIF: marshal impersonation request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("GCP WIF: build impersonation request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+federatedToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := f.src.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("GCP WIF: impersonation request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, stsResponseLimit))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("GCP WIF: read impersonation response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf(
			"GCP WIF: impersonation returned %d: %s",
			resp.StatusCode,
			truncateForError(respBody),
		)
	}

	var parsed iamCredResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", time.Time{}, fmt.Errorf("GCP WIF: parse impersonation response: %w", err)
	}
	if parsed.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("GCP WIF: impersonation returned empty accessToken")
	}

	expiry, err := time.Parse(time.RFC3339, parsed.ExpireTime)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("GCP WIF: parse impersonation expireTime %q: %w", parsed.ExpireTime, err)
	}
	return parsed.AccessToken, expiry, nil
}

// truncateForError shrinks an error body excerpt to a fixed cap and
// strips surrounding whitespace so the wrapped error stays a single,
// searchable log line.
func truncateForError(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > stsErrorBodyLimit {
		s = s[:stsErrorBodyLimit] + "…"
	}
	return s
}
