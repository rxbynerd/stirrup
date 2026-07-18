package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

const (
	// anthropicBatchAPIBaseURL is the Anthropic API root for the
	// /v1/messages/batches endpoints; overridable on the struct so
	// httptest fixtures can swap in a mock server.
	anthropicBatchAPIBaseURL = "https://api.anthropic.com"

	// batchPollMaxIntervalDefault caps the exponential poll backoff.
	// Five minutes mirrors the batch_waiting heartbeat cadence, since
	// polling faster than the heartbeat has no value.
	batchPollMaxIntervalDefault = 5 * time.Minute

	// batchCancelTimeout bounds the best-effort cancel call issued on
	// timeout / ctx-cancel; kept short since the cancel is fire-and-
	// forget and must not delay surfacing the underlying error.
	batchCancelTimeout = 10 * time.Second
)

// batchPollInitialIntervalNs holds the first poll interval in
// nanoseconds; atomic so tests can lower it without racing concurrent
// tests. The production value (10s) matches Anthropic's recommended
// cadence for the first few polls.
var batchPollInitialIntervalNs atomic.Int64

// batchPollJitterDisabled toggles the ±20% jitter applied to each
// sleep duration; tests set true for deterministic timing.
var batchPollJitterDisabled atomic.Bool

// batchPollMaxIntervalNs holds the live polling cap in nanoseconds,
// lowerable via swapBatchPollMaxInterval to exercise the cap branch
// within a test's time budget.
var batchPollMaxIntervalNs atomic.Int64

func init() {
	batchPollInitialIntervalNs.Store(int64(10 * time.Second))
	batchPollMaxIntervalNs.Store(int64(batchPollMaxIntervalDefault))
}

func getBatchPollInitialInterval() time.Duration {
	return time.Duration(batchPollInitialIntervalNs.Load())
}

func setBatchPollInitialInterval(d time.Duration) time.Duration {
	prev := batchPollInitialIntervalNs.Swap(int64(d))
	return time.Duration(prev)
}

func setBatchPollJitterDisabled(disabled bool) bool {
	return batchPollJitterDisabled.Swap(disabled)
}

func getBatchPollMaxInterval() time.Duration {
	return time.Duration(batchPollMaxIntervalNs.Load())
}

// swapBatchPollMaxInterval lowers the polling cap for tests; returns
// the prior value so the caller can restore it on teardown.
func swapBatchPollMaxInterval(d time.Duration) time.Duration {
	prev := batchPollMaxIntervalNs.Swap(int64(d))
	return time.Duration(prev)
}

// harnessPollingBatchClient implements BatchClient by talking directly to
// the provider's batch HTTP API. Used in stdio mode (no control plane)
// when the operator opts in via Batch.HarnessSidePolling=true.
//
// Submit/Result dispatch on providerType: "anthropic" uses the Messages
// Batches API; "openai-compatible" and "openai-responses" use the OpenAI
// Batch API (two-step file upload + batch creation).
type harnessPollingBatchClient struct {
	httpClient   *http.Client
	credSource   credential.Source
	providerType string
	// apiKeyRef is the secret:// reference logged as the keyRef
	// attribute in bestEffortCancel's failure warns; safe to log
	// because Redact() strips secret references from persisted traces.
	apiKeyRef string
	baseURL   string
	// apiKeyHeader overrides the OpenAI auth header ("api-key" for
	// Azure-style gateways); empty means "Authorization: Bearer <key>".
	// Ignored on the Anthropic branch, which always uses x-api-key.
	apiKeyHeader string
	maxWait      time.Duration
	logger       *slog.Logger

	// lastSubmittedCustomID retains the customID from the most recent
	// Submit so resultOpenAI can key terminal-status / top-level-failure
	// BatchResults under it — the OpenAI poll/result endpoints address
	// only by batchID, but the BatchAdapter looks up by customID.
	lastSubmittedCustomID atomic.Value // string
}

// HarnessBatchClientOptions configures NewHarnessPollingBatchClient.
type HarnessBatchClientOptions struct {
	// ProviderType selects the wire dialect. One of "anthropic" |
	// "openai-compatible" | "openai-responses". Required.
	ProviderType string

	// APIKeyRef is the secret:// reference logged as the keyRef
	// attribute in cancel-path warnings; the key value itself is
	// fetched through CredSource.
	APIKeyRef string

	// CredSource resolves the bearer credential on every HTTP request,
	// keeping the client compatible with future rotating sources.
	CredSource credential.Source

	// BaseURL overrides the per-provider default API root. Empty falls
	// back to the production URL for the chosen provider.
	BaseURL string

	// APIKeyHeader overrides the OpenAI auth header for Azure-style
	// gateways (e.g. "api-key"); empty preserves the "Authorization:
	// Bearer <key>" default. Ignored when ProviderType is "anthropic".
	APIKeyHeader string

	// MaxWait is the wall-clock cap on a single Result call. An
	// expiration returns an error wrapping errBatchExpired so
	// BatchAdapter's FallbackOnTimeout branch routes correctly.
	// Zero or negative falls back to types.DefaultBatchMaxWaitSeconds.
	MaxWait time.Duration

	// Logger is the run-scoped slog logger used for cancel-path,
	// credential-resolution, and unrecognised-status warnings. Nil
	// falls back to slog.Default().
	Logger *slog.Logger
}

// NewHarnessPollingBatchClient constructs a polling client for the
// provider batch API matching opts.ProviderType. opts.CredSource is
// invoked once per HTTP request (Submit and each poll tick) so a
// refresh-aware source stays fresh across long waits.
//
// Panics if opts.CredSource is nil — a clear constructor-time panic is
// easier to diagnose than a deferred nil deref on the first request.
func NewHarnessPollingBatchClient(opts HarnessBatchClientOptions) *harnessPollingBatchClient {
	if opts.CredSource == nil {
		panic("HarnessBatchClientOptions.CredSource must not be nil")
	}
	baseURL := opts.BaseURL
	if baseURL == "" {
		switch opts.ProviderType {
		case "openai-compatible", "openai-responses":
			baseURL = openaiDefaultBaseURL
		default:
			baseURL = anthropicBatchAPIBaseURL
		}
	}

	baseURL = strings.TrimRight(baseURL, "/")
	maxWait := opts.MaxWait
	if maxWait <= 0 {
		maxWait = time.Duration(types.DefaultBatchMaxWaitSeconds) * time.Second
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &harnessPollingBatchClient{
		httpClient: &http.Client{
			// 30s non-streaming timeout; batch endpoints don't produce
			// the long-lived SSE responses the 120s streaming client covers.
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 20 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
		credSource:   opts.CredSource,
		providerType: opts.ProviderType,
		apiKeyRef:    opts.APIKeyRef,
		baseURL:      baseURL,
		apiKeyHeader: opts.APIKeyHeader,
		maxWait:      maxWait,
		logger:       logger,
	}
}

// resolveAPIKey looks up the current API key via the configured
// credential.Source. Called per HTTP request so credential rotation
// takes effect mid-wait without rebuilding the client.
func (c *harnessPollingBatchClient) resolveAPIKey(ctx context.Context) (string, error) {
	resolved, err := c.credSource.Resolve(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve credential: %w", err)
	}
	if resolved.BearerToken == nil {
		return "", fmt.Errorf("credential source produced no bearer token")
	}
	key, err := resolved.BearerToken(ctx)
	if err != nil {
		return "", fmt.Errorf("fetch bearer token: %w", err)
	}
	return key, nil
}

// setAuthHeaders pins the three headers every Anthropic API call
// requires, centralised so per-request call sites cannot drift.
func setAuthHeaders(req *http.Request, apiKey string) {
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", anthropicAPIVersion)
	req.Header.Set("Content-Type", "application/json")
}

// anthropicBatchSubmitRequest is the POST body for /v1/messages/batches.
// The polling client always submits exactly one entry (size-1 contract
// with BatchAdapter).
type anthropicBatchSubmitRequest struct {
	Requests []anthropicBatchSubmitEntry `json:"requests"`
}

type anthropicBatchSubmitEntry struct {
	CustomID string          `json:"custom_id"`
	Params   json.RawMessage `json:"params"`
}

// anthropicBatchObject is the response shape returned by both POST
// /v1/messages/batches and GET /v1/messages/batches/{id}. Only the
// fields the polling loop consumes are decoded; the API returns more.
type anthropicBatchObject struct {
	ID               string `json:"id"`
	ProcessingStatus string `json:"processing_status"`
	ResultsURL       string `json:"results_url"`
}

// anthropicBatchResultLine is a single JSONL entry returned from
// results_url. Each line corresponds to one submitted custom_id.
type anthropicBatchResultLine struct {
	CustomID string                     `json:"custom_id"`
	Result   anthropicBatchResultOutput `json:"result"`
}

// anthropicBatchResultOutput discriminates the four outcome types
// Anthropic documents for a batch entry. Message is the raw provider
// response body on success — preserved as RawMessage so the
// fabrication path (fabricateAnthropicStream) sees exactly what the
// streaming endpoint would have returned.
type anthropicBatchResultOutput struct {
	Type    string                   `json:"type"` // "succeeded" | "errored" | "canceled" | "expired"
	Message json.RawMessage          `json:"message,omitempty"`
	Error   *anthropicBatchResultErr `json:"error,omitempty"`
}

type anthropicBatchResultErr struct {
	Type    string `json:"type,omitempty"`
	Message string `json:"message,omitempty"`
}

// Submit routes the single-entry batch to the configured provider's
// submission endpoint. BatchAdapter always submits one entry per turn.
func (c *harnessPollingBatchClient) Submit(ctx context.Context, entries []BatchEntry) (string, error) {
	if len(entries) != 1 {
		return "", fmt.Errorf("harnessPollingBatchClient: expected exactly 1 entry, got %d", len(entries))
	}
	switch c.providerType {
	case "openai-compatible", "openai-responses":
		return c.submitOpenAI(ctx, entries[0])
	case "anthropic", "":
		// Empty providerType defaults to "anthropic" for callers that
		// bypass the validator without setting it explicitly.
		return c.submitAnthropic(ctx, entries[0])
	default:
		return "", fmt.Errorf("harnessPollingBatchClient: unsupported provider type %q", c.providerType)
	}
}

// submitAnthropic POSTs a size-1 batch to /v1/messages/batches and
// returns the assigned batch id (msgbatch_...).
func (c *harnessPollingBatchClient) submitAnthropic(ctx context.Context, entry BatchEntry) (string, error) {
	body := anthropicBatchSubmitRequest{
		Requests: []anthropicBatchSubmitEntry{{
			CustomID: entry.CustomID,
			Params:   entry.Body,
		}},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal batch submit body: %w", err)
	}

	apiKey, err := c.resolveAPIKey(ctx)
	if err != nil {
		return "", err
	}

	submitURL := c.baseURL + "/v1/messages/batches"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, submitURL, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build batch submit request: %w", err)
	}
	setAuthHeaders(req, apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("submit batch: %w", security.UnwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("anthropic batch submit returned status %d: %s", resp.StatusCode, errBody)
	}

	var obj anthropicBatchObject
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBatchResponseBytes)).Decode(&obj); err != nil {
		return "", fmt.Errorf("decode batch submit response: %w", err)
	}
	if obj.ID == "" {
		return "", fmt.Errorf("anthropic batch submit response missing id")
	}
	return obj.ID, nil
}

// Result polls the configured provider's batch endpoint until the batch
// resolves, maxWait fires, or ctx is cancelled. On timeout the error
// wraps errBatchExpired (load-bearing for BatchAdapter's
// errors.Is(err, errBatchExpired) FallbackOnTimeout check); on cancel
// it is ctx.Err(). A best-effort cancel call is issued on both exit
// paths so the operator is not billed for a batch nobody will read.
func (c *harnessPollingBatchClient) Result(ctx context.Context, batchID string) (map[string]*BatchResult, error) {
	switch c.providerType {
	case "openai-compatible", "openai-responses":
		return c.resultOpenAI(ctx, batchID)
	case "anthropic", "":
		return c.resultAnthropic(ctx, batchID)
	default:
		return nil, fmt.Errorf("harnessPollingBatchClient: unsupported provider type %q", c.providerType)
	}
}

// resultAnthropic polls /v1/messages/batches/{batchID} until the
// Anthropic batch resolves and fetches the JSONL results document.
func (c *harnessPollingBatchClient) resultAnthropic(ctx context.Context, batchID string) (map[string]*BatchResult, error) {
	deadline := time.Now().Add(c.maxWait)
	interval := getBatchPollInitialInterval()

	for {
		obj, err := c.pollOnce(ctx, batchID)
		if err != nil {
			// errors.Is over ctx.Err()!=nil so a per-request
			// http.Client.Timeout (which wraps context.DeadlineExceeded on
			// the request context but does not cancel the parent ctx)
			// still routes to the bestEffortCancel branch.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				go c.bestEffortCancel(batchID)
				return nil, err
			}
			return nil, err
		}

		switch obj.ProcessingStatus {
		case "ended":
			if obj.ResultsURL == "" {
				return nil, fmt.Errorf("anthropic batch %s ended without results_url", batchID)
			}
			// fetchResults sends x-api-key to results_url unconditionally,
			// so an off-domain URL must be rejected before the credential
			// is exposed to it.
			if err := validateResultsURL(obj.ResultsURL, c.baseURL); err != nil {
				return nil, fmt.Errorf("anthropic batch %s: %w", batchID, err)
			}
			return c.fetchResults(ctx, obj.ResultsURL, batchID)
		case "in_progress", "canceling":
			// Documented intermediate statuses; continue polling.
		default:
			// Unrecognised status: warn but keep polling since the
			// deadline still bounds the loop.
			if c.logger != nil {
				c.logger.Warn(
					"batch poll: unrecognised processing_status; continuing to poll",
					"batchID", batchID,
					"status", obj.ProcessingStatus,
				)
			}
		}

		// Cap sleep against the remaining deadline so timeout fires on
		// the documented cap instead of one full interval past it.
		sleep := jitter(interval)
		if remaining := time.Until(deadline); remaining <= 0 {
			go c.bestEffortCancel(batchID)
			return nil, fmt.Errorf("%w: harness polling timeout after %s (batchID=%s)", errBatchExpired, c.maxWait, batchID)
		} else if sleep > remaining {
			sleep = remaining
		}

		select {
		case <-ctx.Done():
			go c.bestEffortCancel(batchID)
			return nil, ctx.Err()
		case <-time.After(sleep):
		}

		interval *= 2
		if maxInterval := getBatchPollMaxInterval(); interval > maxInterval {
			interval = maxInterval
		}

		// The sleep may have consumed the entire remaining budget
		// without yet triggering the loop-top poll.
		if time.Now().After(deadline) {
			go c.bestEffortCancel(batchID)
			return nil, fmt.Errorf("%w: harness polling timeout after %s (batchID=%s)", errBatchExpired, c.maxWait, batchID)
		}
	}
}

// pollOnce issues a single GET /v1/messages/batches/{id}.
func (c *harnessPollingBatchClient) pollOnce(ctx context.Context, batchID string) (*anthropicBatchObject, error) {
	apiKey, err := c.resolveAPIKey(ctx)
	if err != nil {
		return nil, err
	}

	pollURL := c.baseURL + "/v1/messages/batches/" + url.PathEscape(batchID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build batch poll request: %w", err)
	}
	setAuthHeaders(req, apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("poll batch: %w", security.UnwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("anthropic batch poll returned status %d: %s", resp.StatusCode, errBody)
	}

	var obj anthropicBatchObject
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBatchResponseBytes)).Decode(&obj); err != nil {
		return nil, fmt.Errorf("decode batch poll response: %w", err)
	}
	return &obj, nil
}

// fetchResults GETs the JSONL document at resultsURL and locates the
// entry whose custom_id matches a submitted one. The full document is
// walked because Anthropic does not guarantee line ordering.
func (c *harnessPollingBatchClient) fetchResults(ctx context.Context, resultsURL, batchID string) (map[string]*BatchResult, error) {
	apiKey, err := c.resolveAPIKey(ctx)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resultsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build results GET request: %w", err)
	}
	setAuthHeaders(req, apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// resultsURL is provider-returned and is frequently a presigned
		// download URL whose signature lives in the query string; unwrap
		// the *url.Error so it cannot leak into this error (CWE-532).
		return nil, fmt.Errorf("fetch batch results: %w", security.UnwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("anthropic batch results GET returned status %d: %s", resp.StatusCode, errBody)
	}

	out := make(map[string]*BatchResult)
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, maxBatchResponseBytes))
	// Lift the per-line cap above bufio's 64 KiB default so a single
	// fat assistant turn does not silently truncate.
	scanner.Buffer(make([]byte, 0, 64*1024), maxBatchResponseBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry anthropicBatchResultLine
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("decode batch result line: %w", err)
		}
		out[entry.CustomID] = mapBatchResultLine(entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read batch results body: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("anthropic batch %s results document is empty", batchID)
	}
	return out, nil
}

// mapBatchResultLine projects Anthropic's per-entry result discriminator
// onto the harness-side BatchResult shape.
func mapBatchResultLine(line anthropicBatchResultLine) *BatchResult {
	switch line.Result.Type {
	case "succeeded":
		return &BatchResult{Response: line.Result.Message}
	case "errored":
		msg := ""
		if line.Result.Error != nil {
			msg = line.Result.Error.Message
		}
		return &BatchResult{Err: &BatchResultError{Type: "server_error", Message: msg}}
	case "canceled":
		return &BatchResult{Err: &BatchResultError{Type: "batch_cancelled"}}
	case "expired":
		return &BatchResult{Err: &BatchResultError{Type: "batch_expired"}}
	default:
		return &BatchResult{Err: &BatchResultError{
			Type:    "server_error",
			Message: fmt.Sprintf("unknown result type %q", line.Result.Type),
		}}
	}
}

// bestEffortCancel issues a provider-specific cancel POST with a short,
// independent deadline so the cancel does not inherit a just-cancelled
// parent ctx. Errors are logged at warn and swallowed so they don't mask
// the timeout / cancel that triggered the call.
func (c *harnessPollingBatchClient) bestEffortCancel(batchID string) {
	ctx, cancel := context.WithTimeout(context.Background(), batchCancelTimeout)
	defer cancel()

	apiKey, err := c.resolveAPIKey(ctx)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("batch cancel: resolve credential", "batchID", batchID, "error", err, "keyRef", c.apiKeyRef)
		}
		return
	}

	var cancelURL string
	switch c.providerType {
	case "openai-compatible", "openai-responses":
		cancelURL = c.baseURL + "/batches/" + url.PathEscape(batchID) + "/cancel"
	default:
		cancelURL = c.baseURL + "/v1/messages/batches/" + url.PathEscape(batchID) + "/cancel"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cancelURL, nil)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("batch cancel: build request", "batchID", batchID, "error", err, "keyRef", c.apiKeyRef)
		}
		return
	}
	c.applyAuthHeaders(req, apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if c.logger != nil {
			// Unwrap the *url.Error: it may embed a credential query
			// string Go does not redact (CWE-532).
			c.logger.Warn("batch cancel: send", "batchID", batchID, "error", security.UnwrapURLError(err), "keyRef", c.apiKeyRef)
		}
		return
	}
	// Drain (bounded) so the transport can reuse the connection.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK && c.logger != nil {
		c.logger.Warn("batch cancel: unexpected status", "batchID", batchID, "status", resp.StatusCode, "keyRef", c.apiKeyRef)
	}
}

// applyAuthHeaders pins the auth-related headers for the configured
// provider. The OpenAI branch deliberately does not set Content-Type
// here so multipart uploaders can attach their own boundary-bearing
// value without it being clobbered; JSON-body OpenAI callers set
// Content-Type at the call site.
func (c *harnessPollingBatchClient) applyAuthHeaders(req *http.Request, apiKey string) {
	switch c.providerType {
	case "openai-compatible", "openai-responses":
		setOpenAIAuthHeader(req, apiKey, c.apiKeyHeader)
	default:
		setAuthHeaders(req, apiKey)
	}
}

// validateResultsURL guards against sending the run's Anthropic
// credentials to an off-domain results_url. See docs/batch.md for the
// exfiltration rationale and the loopback test-server exception.
func validateResultsURL(raw, baseURL string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("results_url is not a valid URL: %w", err)
	}
	if isBaseURLLoopback(baseURL) {
		base, perr := url.Parse(baseURL)
		if perr == nil && u.Host == base.Host {
			return nil
		}

	}
	if u.Scheme != "https" {
		return fmt.Errorf("results_url scheme must be https, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host != "anthropic.com" && !strings.HasSuffix(host, ".anthropic.com") {
		return fmt.Errorf("results_url host %q is not on anthropic.com", host)
	}
	return nil
}

// isBaseURLLoopback reports whether baseURL is a loopback address —
// the signal that an httptest server is wired in for validateResultsURL.
func isBaseURLLoopback(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	return false
}

// jitter applies ±20% randomisation to d so concurrent harnesses do not
// synchronise their poll cadences. Non-cryptographic by design (no
// security boundary).
func jitter(d time.Duration) time.Duration {
	if batchPollJitterDisabled.Load() {
		return d
	}

	delta := time.Duration(rand.Int63n(int64(d)/5*2+1)) - d/5
	return d + delta
}

// openaiBatchObject mirrors the relevant fields of an OpenAI batch
// object as returned by POST /v1/batches and GET /v1/batches/{id}.
// Statuses progress: validating → in_progress → finalizing → completed
// (or failed / expired / cancelling / cancelled).
type openaiBatchObject struct {
	ID            string             `json:"id"`
	Status        string             `json:"status"`
	OutputFileID  string             `json:"output_file_id,omitempty"`
	ErrorFileID   string             `json:"error_file_id,omitempty"`
	Errors        *openaiBatchErrors `json:"errors,omitempty"`
	RequestCounts *struct {
		Total     int `json:"total"`
		Completed int `json:"completed"`
		Failed    int `json:"failed"`
	} `json:"request_counts,omitempty"`
}

// openaiBatchErrors is the inline errors object returned on top-level
// batch failures (distinct from per-entry errors in error_file_id).
type openaiBatchErrors struct {
	Data []struct {
		Code    string `json:"code,omitempty"`
		Message string `json:"message,omitempty"`
		Line    *int   `json:"line,omitempty"`
	} `json:"data,omitempty"`
}

// openaiBatchInputLine is one line of the JSONL document uploaded to
// /v1/files with purpose=batch. "url" is the endpoint the entry's
// request is dispatched to; "body" is the same wire body the streaming
// adapter would have POSTed.
type openaiBatchInputLine struct {
	CustomID string          `json:"custom_id"`
	Method   string          `json:"method"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}

// openaiBatchOutputLine is one line of the JSONL document downloaded
// from /v1/files/{output_file_id}/content; "response" carries the same
// shape the streaming adapter would have read from the HTTP body.
type openaiBatchOutputLine struct {
	CustomID string `json:"custom_id"`
	Response *struct {
		StatusCode int             `json:"status_code"`
		Body       json.RawMessage `json:"body"`
	} `json:"response,omitempty"`
	Error *struct {
		Code    string `json:"code,omitempty"`
		Message string `json:"message,omitempty"`
	} `json:"error,omitempty"`
}

// openaiBatchCreateRequest is the JSON body POSTed to /v1/batches once
// the input file is uploaded.
type openaiBatchCreateRequest struct {
	InputFileID      string `json:"input_file_id"`
	Endpoint         string `json:"endpoint"`
	CompletionWindow string `json:"completion_window"`
}

// openaiFileObject mirrors the relevant fields of the /v1/files upload
// response and the polled file-status object.
type openaiFileObject struct {
	ID string `json:"id"`
}

// openaiEndpointFor maps the harness-side provider type to the OpenAI
// batch endpoint string.
func openaiEndpointFor(providerType string) string {
	if providerType == "openai-responses" {
		return "/v1/responses"
	}
	return "/v1/chat/completions"
}

// submitOpenAI runs the two-step OpenAI batch flow: upload the entry
// as a single-line JSONL file to /v1/files (purpose=batch), then
// POST /v1/batches referencing the uploaded file. Returns the
// assigned batch id (batch_...).
func (c *harnessPollingBatchClient) submitOpenAI(ctx context.Context, entry BatchEntry) (string, error) {

	c.lastSubmittedCustomID.Store(entry.CustomID)

	inputLine := openaiBatchInputLine{
		CustomID: entry.CustomID,
		Method:   "POST",
		URL:      openaiEndpointFor(c.providerType),
		Body:     entry.Body,
	}
	lineBytes, err := json.Marshal(inputLine)
	if err != nil {
		return "", fmt.Errorf("marshal openai batch input line: %w", err)
	}
	jsonlPayload := append(lineBytes, '\n')

	fileID, err := c.uploadOpenAIBatchFile(ctx, jsonlPayload)
	if err != nil {
		return "", err
	}

	createBody := openaiBatchCreateRequest{
		InputFileID:      fileID,
		Endpoint:         openaiEndpointFor(c.providerType),
		CompletionWindow: "24h",
	}
	createPayload, err := json.Marshal(createBody)
	if err != nil {
		return "", fmt.Errorf("marshal openai batch create body: %w", err)
	}

	apiKey, err := c.resolveAPIKey(ctx)
	if err != nil {
		return "", err
	}
	createURL := c.baseURL + "/batches"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, createURL, bytes.NewReader(createPayload))
	if err != nil {
		return "", fmt.Errorf("build openai batch create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.applyAuthHeaders(req, apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("submit openai batch: %w", security.UnwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("openai batch create returned status %d: %s", resp.StatusCode, security.Scrub(string(errBody)))
	}

	var obj openaiBatchObject
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBatchResponseBytes)).Decode(&obj); err != nil {
		return "", fmt.Errorf("decode openai batch create response: %w", err)
	}
	if obj.ID == "" {
		return "", fmt.Errorf("openai batch create response missing id")
	}
	return obj.ID, nil
}

// uploadOpenAIBatchFile POSTs the JSONL payload as a multipart upload
// to /v1/files with purpose=batch and returns the assigned file id.
func (c *harnessPollingBatchClient) uploadOpenAIBatchFile(ctx context.Context, jsonl []byte) (string, error) {
	apiKey, err := c.resolveAPIKey(ctx)
	if err != nil {
		return "", err
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("purpose", "batch"); err != nil {
		return "", fmt.Errorf("write purpose field: %w", err)
	}
	filePart, err := writer.CreateFormFile("file", "batch.jsonl")
	if err != nil {
		return "", fmt.Errorf("create file part: %w", err)
	}
	if _, err := filePart.Write(jsonl); err != nil {
		return "", fmt.Errorf("write file part: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("close multipart writer: %w", err)
	}

	uploadURL := c.baseURL + "/files"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, &body)
	if err != nil {
		return "", fmt.Errorf("build openai file upload request: %w", err)
	}
	// Content-Type carries the multipart boundary; set before
	// applyAuthHeaders, which deliberately leaves it untouched.
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c.applyAuthHeaders(req, apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload openai batch file: %w", security.UnwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("openai file upload returned status %d: %s", resp.StatusCode, security.Scrub(string(errBody)))
	}

	var obj openaiFileObject
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBatchResponseBytes)).Decode(&obj); err != nil {
		return "", fmt.Errorf("decode openai file upload response: %w", err)
	}
	if obj.ID == "" {
		return "", fmt.Errorf("openai file upload response missing id")
	}
	return obj.ID, nil
}

// resultOpenAI polls GET /v1/batches/{batchID} until the batch
// terminal status is observed, maxWait fires, or ctx is cancelled.
// On completion the output file's JSONL is fetched and the line
// matching the submitted custom_id is projected onto BatchResult.
// customID (from the last successful submitOpenAI) keys terminal-status
// and top-level-failure results so the BatchAdapter's lookup surfaces a
// typed BatchResultError rather than an opaque "missing entry".
func (c *harnessPollingBatchClient) resultOpenAI(ctx context.Context, batchID string) (map[string]*BatchResult, error) {
	customID, _ := c.lastSubmittedCustomID.Load().(string)
	deadline := time.Now().Add(c.maxWait)
	interval := getBatchPollInitialInterval()

	for {
		obj, err := c.pollOnceOpenAI(ctx, batchID)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				go c.bestEffortCancel(batchID)
				return nil, err
			}
			return nil, err
		}

		switch obj.Status {
		case "completed":
			if obj.OutputFileID == "" {
				return nil, fmt.Errorf("openai batch %s completed without output_file_id", batchID)
			}
			return c.fetchOpenAIResults(ctx, obj.OutputFileID, batchID)
		case "failed":

			return c.fetchOpenAIFailure(ctx, *obj, batchID, customID)
		case "expired":
			return nil, fmt.Errorf("%w: openai batch %s expired upstream", errBatchExpired, batchID)
		case "cancelled", "cancelling":
			return c.synthesizeOpenAIFailure(batchID, customID, "batch_cancelled", "openai batch was cancelled upstream"), nil
		case "validating", "in_progress", "finalizing":
			// Documented intermediate statuses; continue polling.
		default:
			if c.logger != nil {
				c.logger.Warn(
					"batch poll: unrecognised openai status; continuing to poll",
					"batchID", batchID,
					"status", obj.Status,
				)
			}
		}

		sleep := jitter(interval)
		if remaining := time.Until(deadline); remaining <= 0 {
			go c.bestEffortCancel(batchID)
			return nil, fmt.Errorf("%w: harness polling timeout after %s (batchID=%s)", errBatchExpired, c.maxWait, batchID)
		} else if sleep > remaining {
			sleep = remaining
		}

		select {
		case <-ctx.Done():
			go c.bestEffortCancel(batchID)
			return nil, ctx.Err()
		case <-time.After(sleep):
		}

		interval *= 2
		if maxInterval := getBatchPollMaxInterval(); interval > maxInterval {
			interval = maxInterval
		}
		if time.Now().After(deadline) {
			go c.bestEffortCancel(batchID)
			return nil, fmt.Errorf("%w: harness polling timeout after %s (batchID=%s)", errBatchExpired, c.maxWait, batchID)
		}
	}
}

// pollOnceOpenAI issues a single GET /v1/batches/{id}.
func (c *harnessPollingBatchClient) pollOnceOpenAI(ctx context.Context, batchID string) (*openaiBatchObject, error) {
	apiKey, err := c.resolveAPIKey(ctx)
	if err != nil {
		return nil, err
	}

	pollURL := c.baseURL + "/batches/" + url.PathEscape(batchID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build openai batch poll request: %w", err)
	}
	c.applyAuthHeaders(req, apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("poll openai batch: %w", security.UnwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("openai batch poll returned status %d: %s", resp.StatusCode, security.Scrub(string(errBody)))
	}

	var obj openaiBatchObject
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBatchResponseBytes)).Decode(&obj); err != nil {
		return nil, fmt.Errorf("decode openai batch poll response: %w", err)
	}
	return &obj, nil
}

// fetchOpenAIResults GETs the JSONL document at
// /v1/files/{output_file_id}/content and projects each line onto a
// BatchResult keyed by custom_id.
func (c *harnessPollingBatchClient) fetchOpenAIResults(ctx context.Context, fileID, batchID string) (map[string]*BatchResult, error) {
	body, err := c.downloadOpenAIFile(ctx, fileID)
	if err != nil {
		return nil, fmt.Errorf("openai batch %s: fetch output file: %w", batchID, err)
	}
	defer func() { _ = body.Close() }()

	out := make(map[string]*BatchResult)
	scanner := bufio.NewScanner(io.LimitReader(body, maxBatchResponseBytes))
	scanner.Buffer(make([]byte, 0, 64*1024), maxBatchResponseBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry openaiBatchOutputLine
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("decode openai batch output line: %w", err)
		}
		out[entry.CustomID] = mapOpenAIOutputLine(entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read openai batch output: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("openai batch %s output document is empty", batchID)
	}
	return out, nil
}

// mapOpenAIOutputLine projects an /v1/files content line onto the
// harness-side BatchResult shape. A non-2xx response.status_code
// (including the zero value, i.e. an absent/malformed field — OpenAI
// always sets it on a valid line) or a populated error block surfaces
// as a server_error rather than fabricating a stream from an
// unverified body.
func mapOpenAIOutputLine(line openaiBatchOutputLine) *BatchResult {
	if line.Error != nil && line.Error.Message != "" {
		return &BatchResult{Err: &BatchResultError{Type: "server_error", Message: line.Error.Message}}
	}
	if line.Response == nil {
		return &BatchResult{Err: &BatchResultError{Type: "server_error", Message: "openai batch output line carried no response"}}
	}
	if line.Response.StatusCode < 200 || line.Response.StatusCode >= 300 {
		return &BatchResult{Err: &BatchResultError{
			Type:    "server_error",
			Message: fmt.Sprintf("openai batch entry returned status %d", line.Response.StatusCode),
		}}
	}
	return &BatchResult{Response: line.Response.Body}
}

// fetchOpenAIFailure handles the "failed" terminal status: per-entry
// errors from error_file_id are surfaced keyed by custom_id; when only
// the top-level errors object is populated (e.g. malformed input file
// rejected before per-entry dispatch), a server_error is synthesised
// under the originating customID instead.
func (c *harnessPollingBatchClient) fetchOpenAIFailure(ctx context.Context, obj openaiBatchObject, batchID, customID string) (map[string]*BatchResult, error) {
	if obj.ErrorFileID != "" {
		body, err := c.downloadOpenAIFile(ctx, obj.ErrorFileID)
		if err != nil {
			return nil, fmt.Errorf("openai batch %s failed; fetch error file: %w", batchID, err)
		}
		defer func() { _ = body.Close() }()

		out := make(map[string]*BatchResult)
		scanner := bufio.NewScanner(io.LimitReader(body, maxBatchResponseBytes))
		scanner.Buffer(make([]byte, 0, 64*1024), maxBatchResponseBytes)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var entry openaiBatchOutputLine
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				return nil, fmt.Errorf("decode openai batch error line: %w", err)
			}
			out[entry.CustomID] = mapOpenAIOutputLine(entry)
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("read openai batch error file: %w", err)
		}
		if len(out) > 0 {
			return out, nil
		}
	}

	msg := fmt.Sprintf("openai batch %s failed upstream", batchID)
	if obj.Errors != nil && len(obj.Errors.Data) > 0 && obj.Errors.Data[0].Message != "" {
		msg = obj.Errors.Data[0].Message
	}
	return c.synthesizeOpenAIFailure(batchID, customID, "server_error", msg), nil
}

// synthesizeOpenAIFailure produces a single-entry map for a terminal
// failure status that carries no per-entry error file, keyed under
// customID (or batchID as a fallback if customID is empty) so the
// BatchAdapter always sees a typed error rather than "missing entry".
func (c *harnessPollingBatchClient) synthesizeOpenAIFailure(batchID, customID, errType, message string) map[string]*BatchResult {
	if c.logger != nil {
		c.logger.Warn(
			"openai batch terminal failure",
			"batchID", batchID,
			"customID", customID,
			"type", errType,
			"message", message,
		)
	}
	key := customID
	if key == "" {
		key = batchID
	}
	return map[string]*BatchResult{
		key: {Err: &BatchResultError{Type: errType, Message: message}},
	}
}

// downloadOpenAIFile GETs /v1/files/{fileID}/content with auth headers
// and returns the body for streaming JSONL decode. The caller is
// responsible for closing the returned ReadCloser.
func (c *harnessPollingBatchClient) downloadOpenAIFile(ctx context.Context, fileID string) (io.ReadCloser, error) {
	apiKey, err := c.resolveAPIKey(ctx)
	if err != nil {
		return nil, err
	}
	dlURL := c.baseURL + "/files/" + url.PathEscape(fileID) + "/content"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dlURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build openai file download request: %w", err)
	}
	c.applyAuthHeaders(req, apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download openai file: %w", security.UnwrapURLError(err))
	}
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("openai file download returned status %d: %s", resp.StatusCode, security.Scrub(string(errBody)))
	}
	return resp.Body, nil
}
