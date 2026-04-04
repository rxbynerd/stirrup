package credential

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const gkeMetadataDefaultBase = "http://metadata.google.internal"

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

	token, err := io.ReadAll(resp.Body)
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
