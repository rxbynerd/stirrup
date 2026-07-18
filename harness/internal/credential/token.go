package credential

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const gkeMetadataDefaultBase = "http://metadata.google.internal"

// azureIMDSDefaultBase is the fixed link-local address of the Azure
// Instance Metadata Service; baseURL exists only so tests can swap in
// an httptest.Server.
const azureIMDSDefaultBase = "http://169.254.169.254"

// azureIMDSAPIVersion pins the lowest-common-denominator IMDS identity
// API version available across all Azure regions and managed-identity
// flavours.
const azureIMDSAPIVersion = "2018-02-01"

// metadataResponseLimit bounds reads from cloud metadata services so a
// hostile or misconfigured endpoint cannot exhaust memory.
const metadataResponseLimit = 64 * 1024 // 64 KiB

// GKEMetadataTokenSource fetches OIDC identity tokens from the GKE
// Workload Identity metadata server. The returned token can be exchanged
// for credentials in another cloud (e.g. AWS STS AssumeRoleWithWebIdentity).
type GKEMetadataTokenSource struct {
	audience   string
	baseURL    string // overridable for testing; defaults to gkeMetadataDefaultBase
	httpClient *http.Client
}

// NewGKEMetadataTokenSource creates a token source that calls the GKE
// metadata server. baseURL can be empty to use the default metadata endpoint.
func NewGKEMetadataTokenSource(audience, baseURL string) *GKEMetadataTokenSource {
	if baseURL == "" {
		baseURL = gkeMetadataDefaultBase
	}
	return &GKEMetadataTokenSource{
		audience: audience,
		baseURL:  strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

func (g *GKEMetadataTokenSource) Token(ctx context.Context) ([]byte, error) {
	endpoint := fmt.Sprintf(
		"%s/computeMetadata/v1/instance/service-accounts/default/identity?audience=%s",
		g.baseURL,
		url.QueryEscape(g.audience),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build GKE metadata request: %w", err)
	}
	req.Header.Set("Metadata-Flavor", "Google")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GKE metadata request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("GKE metadata returned %d: %s", resp.StatusCode, string(body))
	}

	token, err := io.ReadAll(io.LimitReader(resp.Body, metadataResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("read GKE identity token: %w", err)
	}

	token = []byte(strings.TrimSpace(string(token)))
	if len(token) == 0 {
		return nil, fmt.Errorf("GKE metadata returned empty identity token")
	}
	return token, nil
}

// FileTokenSource reads an identity token from a file. This is useful for
// Kubernetes projected service account token volumes.
type FileTokenSource struct {
	path string
}

func (f *FileTokenSource) Token(_ context.Context) ([]byte, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return nil, fmt.Errorf("read token file %q: %w", f.path, err)
	}
	token := []byte(strings.TrimSpace(string(data)))
	if len(token) == 0 {
		return nil, fmt.Errorf("token file %q is empty", f.path)
	}
	return token, nil
}

// EnvTokenSource reads an identity token from an environment variable.
type EnvTokenSource struct {
	envVar string
}

func (e *EnvTokenSource) Token(_ context.Context) ([]byte, error) {
	val := os.Getenv(e.envVar)
	if val == "" {
		return nil, fmt.Errorf("environment variable %q is empty or unset", e.envVar)
	}
	return []byte(val), nil
}

// AWSIRSATokenSource resolves the projected token file that EKS Pod
// Identity / IRSA mounts into pods, reading the path from
// AWS_WEB_IDENTITY_TOKEN_FILE at call time so a misconfigured runtime
// fails with a clear error at credential-resolution time.
type AWSIRSATokenSource struct{}

// Token reads the IRSA-projected service account token.
func (a *AWSIRSATokenSource) Token(ctx context.Context) ([]byte, error) {
	path := os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE")
	if path == "" {
		return nil, fmt.Errorf("AWS_WEB_IDENTITY_TOKEN_FILE is unset; this token source only works inside an EKS pod with IRSA configured")
	}
	return (&FileTokenSource{path: path}).Token(ctx)
}

// AzureIMDSTokenSource fetches an Azure AD access token for the
// configured resource from the Azure Instance Metadata Service (IMDS).
type AzureIMDSTokenSource struct {
	resource   string // required: Azure AD resource URI (e.g. "https://management.azure.com/")
	clientID   string // optional: client ID of a user-assigned managed identity
	baseURL    string // overridable for testing; defaults to azureIMDSDefaultBase
	httpClient *http.Client
}

// NewAzureIMDSTokenSource creates a token source that calls the Azure
// IMDS identity endpoint. baseURL empty uses the default metadata
// address; clientID empty uses the system-assigned managed identity.
func NewAzureIMDSTokenSource(resource, clientID, baseURL string) *AzureIMDSTokenSource {
	if baseURL == "" {
		baseURL = azureIMDSDefaultBase
	}
	return &AzureIMDSTokenSource{
		resource: resource,
		clientID: clientID,
		baseURL:  strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// azureIMDSResponse is the documented JSON shape of the IMDS identity
// endpoint. Only access_token is consumed; other fields are ignored.
type azureIMDSResponse struct {
	AccessToken string `json:"access_token"`
}

// Token fetches an Azure AD access token from IMDS. The "Metadata: true"
// header is mandatory — IMDS rejects requests without it as a basic SSRF
// defence (an attacker who can persuade a VM-resident process to make a
// request still cannot reach IMDS unless they can also set arbitrary
// headers).
func (a *AzureIMDSTokenSource) Token(ctx context.Context) ([]byte, error) {
	q := url.Values{}
	q.Set("api-version", azureIMDSAPIVersion)
	q.Set("resource", a.resource)
	if a.clientID != "" {
		q.Set("client_id", a.clientID)
	}
	endpoint := fmt.Sprintf("%s/metadata/identity/oauth2/token?%s", a.baseURL, q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build Azure IMDS request: %w", err)
	}
	req.Header.Set("Metadata", "true")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("azure IMDS request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, metadataResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("read Azure IMDS response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("azure IMDS returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed azureIMDSResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse Azure IMDS response: %w", err)
	}
	if parsed.AccessToken == "" {
		return nil, fmt.Errorf("azure IMDS returned empty access_token")
	}
	return []byte(parsed.AccessToken), nil
}

// GitHubActionsOIDCTokenSource fetches an OIDC identity token from the
// GitHub Actions runner's token-issuance endpoint. Both
// ACTIONS_ID_TOKEN_REQUEST_URL and ACTIONS_ID_TOKEN_REQUEST_TOKEN are
// only present when the workflow declares `permissions: id-token:
// write`. The request URL is captured and validated at construction
// time (see docs/credential-federation.md); the bearer token is
// re-read on each call since the runner rotates it.
type GitHubActionsOIDCTokenSource struct {
	audience   string
	requestURL string
	httpClient *http.Client
}

// NewGitHubActionsOIDCTokenSource creates a token source that requests
// a JWT from the GitHub Actions runner, scoped to audience — the value
// the downstream relying party expects in the `aud` claim.
func NewGitHubActionsOIDCTokenSource(audience string) (*GitHubActionsOIDCTokenSource, error) {
	requestURL := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_URL")
	if requestURL == "" {
		return nil, fmt.Errorf("ACTIONS_ID_TOKEN_REQUEST_URL is unset; this token source only works in a GitHub Actions workflow with `permissions: id-token: write`")
	}
	parsed, err := url.Parse(requestURL)
	if err != nil {
		return nil, fmt.Errorf("ACTIONS_ID_TOKEN_REQUEST_URL must be an https URL, got %q: %w", requestURL, err)
	}
	if parsed.Scheme != "https" {
		return nil, fmt.Errorf("ACTIONS_ID_TOKEN_REQUEST_URL must be an https URL, got %q", requestURL)
	}

	return &GitHubActionsOIDCTokenSource{
		audience:   audience,
		requestURL: requestURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}, nil
}

// ghaOIDCResponse is the documented JSON shape returned by the GitHub
// Actions OIDC token endpoint. Only "value" is consumed.
type ghaOIDCResponse struct {
	Value string `json:"value"`
}

// Token fetches a fresh OIDC token from the runner. The request URL
// already carries a `?api-version=...` query parameter set by the
// runner; the audience is appended with `&audience=...`.
func (g *GitHubActionsOIDCTokenSource) Token(ctx context.Context) ([]byte, error) {
	requestToken := os.Getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	if requestToken == "" {
		return nil, fmt.Errorf("ACTIONS_ID_TOKEN_REQUEST_TOKEN is unset; this token source only works in a GitHub Actions workflow with `permissions: id-token: write`")
	}

	endpoint := g.requestURL + "&audience=" + url.QueryEscape(g.audience)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build GitHub Actions OIDC request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+requestToken)
	req.Header.Set("Accept", "application/json")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub Actions OIDC request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, metadataResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("read GitHub Actions OIDC response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub Actions OIDC returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed ghaOIDCResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse GitHub Actions OIDC response: %w", err)
	}
	if parsed.Value == "" {
		return nil, fmt.Errorf("GitHub Actions OIDC returned empty value")
	}
	return []byte(parsed.Value), nil
}
