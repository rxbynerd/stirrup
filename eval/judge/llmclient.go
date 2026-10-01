package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// DefaultLLMModel and DefaultLLMKeyRef are the Anthropic defaults applied
// when a diff-review judge has no `llm` block and the invocation supplies no
// overrides.
const (
	DefaultLLMModel  = "claude-haiku-4-5-20251001"
	DefaultLLMKeyRef = "secret://ANTHROPIC_API_KEY"
)

// Stop reasons as normalised by the clients. Provider-specific values that
// have no counterpart here pass through unchanged.
const (
	stopEndTurn   = "end_turn"
	stopMaxTokens = "max_tokens"
	stopRefusal   = "refusal"
)

const (
	// maxResponseBytes bounds how much of a provider response is read.
	maxResponseBytes = 4 << 20

	// maxErrorBodyBytes bounds the provider error text carried into a
	// verdict reason.
	maxErrorBodyBytes = 1024
)

// JudgeClient sends one judge prompt to a model and returns its reply.
type JudgeClient interface {
	Complete(ctx context.Context, req JudgeRequest) (JudgeResponse, error)
}

// JudgeRequest is a provider-neutral judge prompt.
type JudgeRequest struct {
	System string
	User   string

	// Schema, when non-empty, is a JSON Schema the provider is asked to
	// constrain the reply to. Empty relies on the prompt alone.
	Schema json.RawMessage

	MaxTokens int

	// Temperature is sent only when non-nil.
	Temperature *float64
}

// JudgeResponse is the model's reply.
type JudgeResponse struct {
	Text string

	// Model is the identifier the provider reports having served.
	Model string

	// StopReason is "end_turn", "max_tokens" or "refusal" when the
	// provider's reason maps onto them.
	StopReason string

	InputTokens  int
	OutputTokens int
}

// ClientFactory builds a JudgeClient for a resolved configuration and an
// already-resolved API key.
type ClientFactory func(cfg types.JudgeLLMConfig, apiKey string) (JudgeClient, error)

// Options carries invocation-scoped settings for LLM-backed judges, set by
// the CLI rather than the suite file.
type Options struct {
	// LLMDefaults supplies the model configuration for diff-review judges
	// that have no `llm` block. Nil keeps the built-in Anthropic default.
	LLMDefaults *types.JudgeLLMConfig

	// NewClient overrides client construction, for tests. Nil uses
	// NewClient's HTTP implementations.
	NewClient ClientFactory
}

// ResolveLLMConfig produces the configuration for one diff-review judge. An
// explicit `llm` block is used as written; otherwise the invocation defaults
// are layered over the built-in Anthropic default. The Anthropic model and
// key reference are only defaulted for the Anthropic provider, so an Anthropic
// key is never sent to another provider's endpoint.
func ResolveLLMConfig(explicit, defaults *types.JudgeLLMConfig) (types.JudgeLLMConfig, error) {
	var cfg types.JudgeLLMConfig
	switch {
	case explicit != nil:
		cfg = *explicit
	case defaults != nil:
		cfg = *defaults
	}
	cfg.Provider = cfg.EffectiveProvider()
	if cfg.Provider == types.JudgeProviderAnthropic {
		if cfg.Model == "" && explicit == nil {
			cfg.Model = DefaultLLMModel
		}
		if cfg.APIKeyRef == "" {
			cfg.APIKeyRef = DefaultLLMKeyRef
		}
	}
	if err := cfg.Validate(); err != nil {
		return types.JudgeLLMConfig{}, fmt.Errorf("invalid judge llm configuration: %w", err)
	}
	return cfg, nil
}

// NewClient builds the HTTP client for cfg's provider. The per-call timeout
// is cfg's TimeoutSeconds.
func NewClient(cfg types.JudgeLLMConfig, apiKey string) (JudgeClient, error) {
	httpClient := &http.Client{Timeout: time.Duration(cfg.EffectiveTimeoutSeconds()) * time.Second}
	switch cfg.EffectiveProvider() {
	case types.JudgeProviderAnthropic:
		return newAnthropicClient(httpClient, cfg.BaseURL, apiKey, cfg.Model)
	case types.JudgeProviderOpenAICompatible:
		return newOpenAIClient(httpClient, cfg.BaseURL, apiKey, cfg.Model)
	default:
		return nil, fmt.Errorf("unsupported judge provider %q", cfg.Provider)
	}
}

// joinEndpoint appends path to the base URL's own path, preserving any query
// string the operator configured.
func joinEndpoint(baseURL, path string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", errors.New("judge base URL is not a valid URL")
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawPath = ""
	return u.String(), nil
}

// postJSON POSTs payload and returns the response body on HTTP 200. Errors
// never carry the request URL, which may hold a gateway credential in its
// query string.
func postJSON(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("build request: invalid endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil, fmt.Errorf("provider returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("provider response exceeds %d bytes", maxResponseBytes)
	}
	return body, nil
}
