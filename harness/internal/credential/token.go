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

// azureIMDSDefaultBase is the link-local address of the Azure Instance
// Metadata Service. It is fixed by the Azure platform and not
// configurable per-VM; we expose baseURL only so tests can swap in an
// httptest.Server.
const azureIMDSDefaultBase = "http://169.254.169.254"

// azureIMDSAPIVersion pins the Azure IMDS identity API version. Azure
// has shipped several IMDS versions over the years; 2018-02-01 is the
// canonical, broadly-deployed version for the
// /metadata/identity/oauth2/token endpoint and is the version Microsoft
// uses in their official documentation and SDKs as of 2024. Newer
// versions (e.g. 2019-08-15) add fields but are not universally
// available across all Azure regions and managed-identity flavours, so
// we pin the lowest-common-denominator that works everywhere.
const azureIMDSAPIVersion = "2018-02-01"

// metadataResponseLimit caps the response body when reading from cloud
// metadata services. IMDS in particular has been observed returning
// large HTML error pages on misconfiguration; bounding the read prevents
// a hostile or buggy metadata endpoint from exhausting memory.
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
// Identity / IRSA mounts into pods at runtime. It is a thin convenience
// wrapper over FileTokenSource that reads the path from the standard
// AWS_WEB_IDENTITY_TOKEN_FILE environment variable. Reading the env var
// in Token() rather than the constructor lets us produce a clear error
// at credential-resolution time when the runtime is misconfigured (the
// pod is running outside an IRSA-enabled service account).
type AWSIRSATokenSource struct{}

// Token reads the IRSA-projected service account token. The
// AWS_WEB_IDENTITY_TOKEN_FILE env var is injected by the EKS Pod
// Identity webhook; if it is unset we return an error that names the
// var so operators have an obvious starting point for diagnosis.
func (a *AWSIRSATokenSource) Token(ctx context.Context) ([]byte, error) {
	path := os.Getenv("AWS_WEB_IDENTITY_TOKEN_FILE")
	if path == "" {
		return nil, fmt.Errorf("AWS_WEB_IDENTITY_TOKEN_FILE is unset; this token source only works inside an EKS pod with IRSA configured")
	}
	return (&FileTokenSource{path: path}).Token(ctx)
}

// AzureIMDSTokenSource fetches an Azure AD access token from the Azure
// Instance Metadata Service (IMDS). The token is for the resource (an
// Azure AD app registration URI) supplied at construction time;
// downstream credential sources that perform cross-cloud federation can
// use this token as the OIDC proof in their token-exchange step.
type AzureIMDSTokenSource struct {
	resource   string // required: Azure AD resource URI (e.g. "https://management.azure.com/")
	clientID   string // optional: client ID of a user-assigned managed identity
	baseURL    string // overridable for testing; defaults to azureIMDSDefaultBase
	httpClient *http.Client
}

// NewAzureIMDSTokenSource creates a token source that calls the Azure
// IMDS identity endpoint. baseURL can be empty to use the default link-
// local metadata address. clientID can be empty to use the system-
// assigned managed identity; set it to the client ID of a user-assigned
// managed identity if the VM has more than one identity attached.
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
// GitHub Actions runner's token-issuance endpoint. The runner injects
// two environment variables into every workflow step:
//
//   - ACTIONS_ID_TOKEN_REQUEST_URL: the issuance endpoint, already
//     carrying an api-version query parameter.
//   - ACTIONS_ID_TOKEN_REQUEST_TOKEN: a short-lived bearer that
//     authenticates the request to the runner.
//
// Both env vars are only present when the workflow declares
// `permissions: id-token: write`. The URL is read and validated at
// construction time so a malicious sidecar that mutates the env after
// the harness has started cannot redirect subsequent token refreshes
// to an attacker-controlled host. The bearer token continues to be
// read at call time (the runner refreshes it).
type GitHubActionsOIDCTokenSource struct {
	audience   string
	requestURL string // captured + validated at construction time
	httpClient *http.Client
}

// NewGitHubActionsOIDCTokenSource creates a token source that requests
// a JWT from the GitHub Actions runner, scoped to the given audience.
// The audience is the value the downstream relying party (e.g. AWS STS,
// GCP STS, an OIDC-enabled Anthropic/Azure exchange) expects to see in
// the `aud` claim — choose it to match the policy on the relying party.
//
// ACTIONS_ID_TOKEN_REQUEST_URL is read and validated here rather than
// in Token() to (a) fail fast with a clear error when the workflow is
// misconfigured (`permissions: id-token: write` not set) and (b) close
// the SSRF window where a process with write access to the runner's
// environment can swap the URL between Token() calls. The URL must
// parse and use the https scheme — sending the runner bearer token
// over plain HTTP would let any on-path attacker on a self-hosted
// runner exfiltrate it and exchange it for a valid OIDC JWT (CWE-319,
// CWE-918).
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
// already contains a `?api-version=...` query parameter (set by the
// runner), so we append the audience with `&audience=...`. The URL was
// validated and frozen at construction time; only the bearer token is
// re-read on each call (the runner rotates it).
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
