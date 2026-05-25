package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// probeBodyLimit caps how much of a non-2xx probe response body is read
// into the returned error. A metadata endpoint should answer in a few
// hundred bytes; 4 KiB is generous headroom without letting a hostile or
// misconfigured intermediary pin a large buffer.
const probeBodyLimit = 4096

// Probe issues a cheap, read-only reachability and authentication check
// against the Anthropic API. It hits the models metadata endpoint
// (GET /v1/models) and never the completion path (/v1/messages), so a
// dry-run cannot spend tokens. A non-2xx status (notably 401 for a bad
// key) is surfaced as an error; the caller turns that into a failed
// preflight step.
//
// The same httpClient and auth-header selection the Stream path uses are
// reused so the probe authenticates identically to a real request.
func (a *AnthropicAdapter) Probe(ctx context.Context) error {
	apiKey, err := a.bearer(ctx)
	if err != nil {
		return fmt.Errorf("resolve bearer token: %w", err)
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
		return fmt.Errorf("execute request: %w", err)
	}
	return checkProbeStatus("anthropic", resp)
}

// anthropicModelsURL derives the models metadata URL from the configured
// messages base URL by swapping the trailing "/messages" path for
// "/models". A custom baseURL without that suffix falls back to joining
// "/models" onto its API root so test servers and gateways still resolve
// to a metadata path rather than the completion path.
func anthropicModelsURL(baseURL string) string {
	if strings.HasSuffix(baseURL, "/messages") {
		return strings.TrimSuffix(baseURL, "/messages") + "/models"
	}
	return strings.TrimRight(baseURL, "/") + "/models"
}

// Probe issues a read-only reachability and authentication check against
// the OpenAI-compatible Chat Completions endpoint. It hits the models
// metadata endpoint (GET {baseURL}/models) and never /chat/completions,
// so a dry-run cannot spend tokens. The configured auth header and query
// parameters are applied exactly as on the Stream path.
func (o *OpenAICompatibleAdapter) Probe(ctx context.Context) error {
	apiKey, err := resolveBearer(ctx, o.bearer)
	if err != nil {
		return err
	}
	requestURL, err := composeOpenAIURL(o.baseURL, "/models", o.queryParams)
	if err != nil {
		return fmt.Errorf("compose request URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	setOpenAIAuthHeader(req, apiKey, o.apiKeyHeader)

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	return checkProbeStatus("openai-compatible", resp)
}

// Probe issues a read-only reachability and authentication check against
// the OpenAI Responses endpoint. The Responses API shares the same
// /models metadata route as Chat Completions, so the probe hits
// GET {baseURL}/models and never /responses.
func (o *OpenAIResponsesAdapter) Probe(ctx context.Context) error {
	apiKey, err := resolveBearer(ctx, o.bearer)
	if err != nil {
		return err
	}
	requestURL, err := composeOpenAIURL(o.baseURL, "/models", o.queryParams)
	if err != nil {
		return fmt.Errorf("compose request URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	setOpenAIAuthHeader(req, apiKey, o.apiKeyHeader)

	resp, err := o.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	return checkProbeStatus("openai-responses", resp)
}

// Probe issues a read-only reachability and authentication check against
// Vertex AI. It performs a GET on the publisher-model resource
// (.../publishers/google/models) — the list-models route — rather than
// the :streamGenerateContent action, so no completion is requested and no
// tokens are spent. The OAuth2 bearer is acquired through the same
// closure the Stream path uses.
func (g *GeminiAdapter) Probe(ctx context.Context) error {
	token, err := g.bearer(ctx)
	if err != nil {
		return fmt.Errorf("resolve bearer token: %w", err)
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

// checkProbeStatus closes resp.Body and returns nil for a 2xx status. A
// non-2xx status is rendered into an error carrying up to probeBodyLimit
// bytes of the body so a 401/403 surfaces the provider's diagnostic
// without exfiltrating an unbounded payload.
func checkProbeStatus(provider string, resp *http.Response) error {
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit))
	if len(body) > 0 {
		return fmt.Errorf("%s metadata endpoint returned status %d: %s", provider, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return fmt.Errorf("%s metadata endpoint returned status %d", provider, resp.StatusCode)
}
