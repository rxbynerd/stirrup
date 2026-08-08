package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/security"
)

// probeBodyLimit caps how much of a non-2xx probe response body is read
// into the returned error so a hostile or misconfigured intermediary
// cannot pin a large buffer just to construct the error message.
const probeBodyLimit = 4096

// Probe hits the models metadata endpoint, never the completion path, so
// a dry-run cannot spend tokens. Auth-header selection mirrors Stream so
// the probe authenticates identically.
func (a *AnthropicAdapter) Probe(ctx context.Context) error {
	apiKey, err := resolveBearer(ctx, a.bearer)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, anthropicModelsURL(a.baseURL), nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	switch a.authMode {
	case AuthModeBearer:
		req.Header.Set("Authorization", "Bearer "+apiKey)
	default:
		req.Header.Set("x-api-key", apiKey)
	}
	req.Header.Set("anthropic-version", anthropicAPIVersion)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		// a.baseURL is operator-configurable and may carry a credential in
		// its query string; unwrap the *url.Error so its embedded URL never
		// leaks into the returned probe error (CWE-532).
		return fmt.Errorf("execute request: %w", security.UnwrapURLError(err))
	}
	return checkProbeStatus("anthropic", resp)
}

// anthropicModelsURL derives the models metadata URL from the messages
// base URL by swapping a trailing "/messages" for "/models", so the probe
// targets a metadata path rather than the completion path. A custom
// baseURL without that suffix joins "/models" onto its API root.
func anthropicModelsURL(baseURL string) string {
	if strings.HasSuffix(baseURL, "/messages") {
		return strings.TrimSuffix(baseURL, "/messages") + "/models"
	}
	return strings.TrimRight(baseURL, "/") + "/models"
}

// Probe hits GET {baseURL}/models, never /chat/completions, so a dry-run
// spends no tokens.
func (o *OpenAICompatibleAdapter) Probe(ctx context.Context) error {
	return probeOpenAIModels(ctx, "openai-compatible", o.httpClient, o.bearer, o.baseURL, o.apiKeyHeader, o.queryParams)
}

// Probe hits GET {baseURL}/models — the Responses API shares the Chat
// Completions metadata route — and never /responses, so a dry-run spends
// no tokens.
func (o *OpenAIResponsesAdapter) Probe(ctx context.Context) error {
	return probeOpenAIModels(ctx, "openai-responses", o.httpClient, o.bearer, o.baseURL, o.apiKeyHeader, o.queryParams)
}

// probeOpenAIModels is the shared models-metadata GET for the two OpenAI
// dialects: identical auth/URL composition, differing only in the
// provider label carried into the error.
func probeOpenAIModels(
	ctx context.Context,
	providerLabel string,
	httpClient *http.Client,
	bearer credential.BearerTokenFunc,
	baseURL, apiKeyHeader string,
	queryParams map[string]string,
) error {
	apiKey, err := resolveBearer(ctx, bearer)
	if err != nil {
		return err
	}
	requestURL, err := composeOpenAIURL(baseURL, "/models", queryParams)
	if err != nil {
		return fmt.Errorf("compose request URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	setOpenAIAuthHeader(req, apiKey, apiKeyHeader)

	resp, err := httpClient.Do(req)
	if err != nil {
		// The composed URL carries baseURL and queryParams, either of which
		// may hold a gateway credential; unwrap the *url.Error so its
		// embedded URL never leaks into the returned probe error (CWE-532).
		return fmt.Errorf("execute request: %w", security.UnwrapURLError(err))
	}
	return checkProbeStatus(providerLabel, resp)
}

// Probe GETs the publisher-model collection rather than the
// :streamGenerateContent action, so no completion is requested and no
// tokens are spent.
func (g *GeminiAdapter) Probe(ctx context.Context) error {
	token, err := resolveBearer(ctx, g.bearer)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.modelsURL(), nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	return checkProbeStatus("gemini", resp)
}

// checkProbeStatus closes resp.Body and returns nil for a 2xx status,
// draining the body first so the keep-alive connection can be reused by a
// subsequent probe on the same client. A non-2xx status carries up to
// probeBodyLimit bytes of the body so a 401/403 surfaces the provider's
// diagnostic without exfiltrating an unbounded payload.
func checkProbeStatus(provider string, resp *http.Response) error {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit))
	if len(body) > 0 {
		return fmt.Errorf("%s metadata endpoint returned status %d: %s", provider, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return fmt.Errorf("%s metadata endpoint returned status %d", provider, resp.StatusCode)
}
