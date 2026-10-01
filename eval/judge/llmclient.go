package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
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
	stopEndTurn      = "end_turn"
	stopStopSequence = "stop_sequence"
	stopMaxTokens    = "max_tokens"
	stopRefusal      = "refusal"
)

const (
	// maxResponseBytes bounds how much of a provider response is read.
	maxResponseBytes = 4 << 20

	// maxErrorBodyBytes bounds the provider error text carried into a
	// verdict reason.
	maxErrorBodyBytes = 1024

	// minRedactedSecretBytes is the shortest API key that is redacted;
	// replacing shorter values would mangle unrelated text.
	minRedactedSecretBytes = 8

	redactedSecret = "[redacted]"
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

	// ClientFactory overrides client construction, for tests. Nil uses
	// NewClient's HTTP implementations.
	ClientFactory ClientFactory
}

// ResolveLLMConfig produces the configuration for one diff-review judge: an
// explicit `llm` block as written, otherwise the invocation defaults over the
// Anthropic default. The default key reference is applied only when the
// endpoint is the Anthropic API, so that key never reaches another host; an
// anthropic gateway must name its own api_key_ref.
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
		if cfg.APIKeyRef == "" && isAnthropicAPI(cfg.BaseURL) {
			cfg.APIKeyRef = DefaultLLMKeyRef
		}
	}
	if err := cfg.Validate(); err != nil {
		return types.JudgeLLMConfig{}, fmt.Errorf("invalid judge llm configuration: %w", err)
	}
	if cfg.Provider == types.JudgeProviderAnthropic && cfg.APIKeyRef == "" {
		return types.JudgeLLMConfig{}, fmt.Errorf("invalid judge llm configuration: api_key_ref is required when base_url is not %s; %s is sent only to the Anthropic API",
			anthropicDefaultBaseURL, DefaultLLMKeyRef)
	}
	return cfg, nil
}

// isAnthropicAPI reports whether baseURL is empty or names the Anthropic
// API host.
func isAnthropicAPI(baseURL string) bool {
	if baseURL == "" {
		return true
	}
	u, err := url.Parse(baseURL)
	return err == nil && strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), "api.anthropic.com")
}

// CheckEndpoint resolves cfg's base_url host and applies
// types.CheckJudgeEndpointAddr to every address, so a refused endpoint
// fails before any task runs. The client repeats the check on the address
// it connects to; behind a proxy that address is the proxy's.
func CheckEndpoint(ctx context.Context, cfg types.JudgeLLMConfig) error {
	return checkEndpoint(ctx, cfg, net.DefaultResolver.LookupNetIP)
}

func checkEndpoint(ctx context.Context, cfg types.JudgeLLMConfig, lookup func(ctx context.Context, network, host string) ([]netip.Addr, error)) error {
	if cfg.BaseURL == "" {
		return nil
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return errors.New("judge base URL is not a valid URL")
	}
	host := u.Hostname()
	keyOverHTTP := u.Scheme == "http" && cfg.APIKeyRef != ""
	if addr, err := netip.ParseAddr(host); err == nil {
		return types.CheckJudgeEndpointAddr(addr, keyOverHTTP)
	}
	addrs, err := lookup(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolving judge base_url host %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("judge base_url host %q resolves to no address", host)
	}
	for _, addr := range addrs {
		if err := types.CheckJudgeEndpointAddr(addr, keyOverHTTP); err != nil {
			return fmt.Errorf("judge base_url host %q: %w", host, err)
		}
	}
	return nil
}

// NewClient builds the HTTP client for cfg's provider. The per-call timeout
// is cfg's TimeoutSeconds.
func NewClient(cfg types.JudgeLLMConfig, apiKey string) (JudgeClient, error) {
	keyOverHTTP := apiKey != "" && strings.HasPrefix(strings.ToLower(cfg.BaseURL), "http://")
	httpClient := &http.Client{
		Timeout:   time.Duration(cfg.EffectiveTimeoutSeconds()) * time.Second,
		Transport: judgeTransport(keyOverHTTP),
		// A redirect would resend the API key to a host the operator never
		// configured.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	switch cfg.EffectiveProvider() {
	case types.JudgeProviderAnthropic:
		return newAnthropicClient(httpClient, cfg.BaseURL, apiKey, cfg.Model)
	case types.JudgeProviderOpenAICompatible:
		return newOpenAIClient(httpClient, cfg.BaseURL, apiKey, cfg.Model)
	default:
		return nil, fmt.Errorf("unsupported judge provider %q", cfg.Provider)
	}
}

// judgeTransport is the default transport with types.CheckJudgeEndpointAddr
// applied to every address it connects to, after DNS resolution, so a
// hostname that resolves or rebinds to a refused address is caught.
func judgeTransport(keyOverHTTP bool) *http.Transport {
	t := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		t = base.Clone()
	}
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			return checkDialAddress(address, keyOverHTTP)
		},
	}
	t.DialContext = dialer.DialContext
	return t
}

// checkDialAddress applies types.CheckJudgeEndpointAddr to a dialer's
// "ip:port" address.
func checkDialAddress(address string, keyOverHTTP bool) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("judge endpoint address %q: %w", address, err)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("judge endpoint address %q is not an IP address", host)
	}
	return types.CheckJudgeEndpointAddr(addr, keyOverHTTP)
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

// Retry policy for judge requests. Only 429 and 5xx responses are
// retried, and every attempt and wait shares one deadline.
const (
	maxAttempts    = 3
	retryBaseDelay = 500 * time.Millisecond

	// maxRetryAfter bounds a provider-requested wait when the request has
	// no deadline to bound it.
	maxRetryAfter = time.Minute
)

// jsonRequest is one judge request's transport settings.
type jsonRequest struct {
	client   *http.Client
	endpoint string
	headers  map[string]string

	// secret is redacted from any provider text an error quotes.
	secret string

	// sleep waits between attempts; nil waits on a timer.
	sleep func(context.Context, time.Duration) error
}

// statusError is a non-200 provider response.
type statusError struct {
	code       int
	retryAfter time.Duration
	msg        string
}

func (e *statusError) Error() string { return e.msg }

func (e *statusError) retryable() bool {
	return e.code == http.StatusTooManyRequests || e.code >= 500
}

// postJSON POSTs payload and returns the response body on HTTP 200,
// retrying 429 and 5xx responses with jittered exponential backoff or the
// provider's Retry-After. The client's Timeout bounds all attempts and
// waits together; a wait that would outlast it is not taken. Errors never
// carry the request URL, which may hold a gateway credential in its query
// string, and have the secret redacted from any provider text they quote.
func postJSON(ctx context.Context, r jsonRequest, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	if r.client.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.client.Timeout)
		defer cancel()
	}
	sleep := r.sleep
	if sleep == nil {
		sleep = sleepContext
	}
	for attempt := 1; ; attempt++ {
		body, err := postOnce(ctx, r, data)
		var se *statusError
		if err == nil || !errors.As(err, &se) || !se.retryable() {
			return body, err
		}
		if attempt == maxAttempts {
			return nil, fmt.Errorf("%w (after %d attempts)", err, attempt)
		}
		wait := se.retryAfter
		if wait <= 0 {
			wait = backoff(attempt)
		}
		if !waitFits(ctx, wait) {
			return nil, fmt.Errorf("%w (not retried: a %s wait exceeds the remaining timeout)", err, wait)
		}
		if serr := sleep(ctx, wait); serr != nil {
			return nil, fmt.Errorf("%w (retry abandoned: %v)", err, serr)
		}
	}
}

// postOnce sends one attempt of a judge request.
func postOnce(ctx context.Context, r jsonRequest, data []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("build request: invalid endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, redactError(fmt.Errorf("request failed: %w", err), r.secret)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, fmt.Errorf("provider returned HTTP %d; redirects are not followed", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, int64(maxErrorBodyBytes+len(r.secret))))
		return nil, &statusError{
			code:       resp.StatusCode,
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
			msg:        fmt.Sprintf("provider returned HTTP %d: %s", resp.StatusCode, providerText(string(body), r.secret)),
		}
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

// backoff is the jittered wait before retry number attempt: uniform in
// [d/2, d] for d = retryBaseDelay * 2^(attempt-1).
func backoff(attempt int) time.Duration {
	d := retryBaseDelay << (attempt - 1)
	return d/2 + rand.N(d/2+1)
}

// parseRetryAfter reads a Retry-After header in delay-seconds or HTTP-date
// form. Zero means absent, malformed or already past.
func parseRetryAfter(h string, now time.Time) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(h, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(min(secs, int64(time.Hour/time.Second))) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		return max(t.Sub(now), 0)
	}
	return 0
}

// waitFits reports whether waiting d leaves time before ctx's deadline, or,
// without a deadline, whether d is within maxRetryAfter.
func waitFits(ctx context.Context, d time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return d <= maxRetryAfter
	}
	return time.Until(deadline) > d
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// providerText prepares provider-authored text for an error message:
// secret redacted, then flattened to one line and bounded to
// maxErrorBodyBytes. Redaction runs first so truncation cannot leave a
// fragment of the secret behind.
func providerText(s, secret string) string {
	s = redactSecret(s, secret)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxErrorBodyBytes {
		s = string(trimPartialRune([]byte(s[:maxErrorBodyBytes])))
	}
	return s
}

// redactSecret replaces secret in s, raw or in its JSON- or Go-escaped
// string form, with "[redacted]".
func redactSecret(s, secret string) string {
	if len(secret) < minRedactedSecretBytes || s == "" {
		return s
	}
	forms := []string{secret}
	if j, err := json.Marshal(secret); err == nil {
		forms = append(forms, string(j[1:len(j)-1]))
	}
	q := strconv.Quote(secret)
	forms = append(forms, q[1:len(q)-1])
	for _, f := range forms {
		s = strings.ReplaceAll(s, f, redactedSecret)
	}
	return s
}

// redactError returns err with secret redacted from its message, or err
// unchanged when the message does not contain it.
func redactError(err error, secret string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if red := redactSecret(msg, secret); red != msg {
		return errors.New(red)
	}
	return err
}
