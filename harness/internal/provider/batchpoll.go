package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
)

const (
	// anthropicBatchAPIBaseURL is the Anthropic API root for the
	// /v1/messages/batches endpoints. The polling client appends the
	// resource path on each request. baseURL is overridable on the
	// struct so httptest fixtures can swap in a mock server.
	anthropicBatchAPIBaseURL = "https://api.anthropic.com"

	// batchPollMaxInterval caps the exponential backoff between polls.
	// Five minutes mirrors the batch_waiting heartbeat cadence the
	// streaming wrapper emits — there is no value in polling faster
	// than the heartbeat once a batch has been in flight for a while.
	batchPollMaxInterval = 5 * time.Minute

	// batchCancelTimeout bounds the best-effort cancel call issued on
	// timeout / ctx-cancel. Short by design — the cancel is fire-and-
	// forget, and a slow API call here would delay the surfacing of the
	// underlying expiration / cancellation back to the caller.
	batchCancelTimeout = 10 * time.Second
)

// batchPollInitialIntervalNs holds the first poll interval in
// nanoseconds. Stored as an atomic so tests can lower it without
// racing the loop in any concurrently-running test. The production
// value (10s) matches the cadence Anthropic recommends in their
// Message Batches docs for the first few polls.
var batchPollInitialIntervalNs atomic.Int64

// batchPollJitterDisabled toggles the ±20% jitter applied to each
// sleep duration. Tests flip this to true so the exponential
// progression is deterministic; production always leaves it false.
var batchPollJitterDisabled atomic.Bool

func init() {
	batchPollInitialIntervalNs.Store(int64(10 * time.Second))
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

// harnessPollingBatchClient implements BatchClient by talking directly to
// the Anthropic Message Batches API over HTTP. Used when the harness runs
// in stdio mode (no control plane is available to own the batch lifecycle)
// and the operator opts in via Batch.HarnessSidePolling=true.
//
// v1 supports Anthropic only — OpenAI Chat Completions and Responses
// batch are scheduled for phase 6 (#139). The factory rejects any
// stdio-batch config whose provider type is not "anthropic" before
// reaching here.
type harnessPollingBatchClient struct {
	httpClient *http.Client
	credSource credential.Source
	apiKeyRef  string //nolint:unused // captured for diagnostic provenance; resolver consumes it via credSource
	baseURL    string
	maxWait    time.Duration
	logger     *slog.Logger
}

// NewHarnessPollingBatchClient constructs a polling client for the
// Anthropic Message Batches API. credSource is invoked once per HTTP
// request (Submit and each poll tick) so a refresh-aware source —
// today only static, in future possibly an OAuth source — stays fresh
// across long waits.
//
// apiKeyRef is captured for diagnostic provenance; the credential
// resolver consumes the underlying secret reference via credSource.
// maxWait is the wall-clock cap on a single Result call; expiration
// returns an error wrapping errBatchExpired so BatchAdapter's
// timeout-fallback branch routes correctly.
func NewHarnessPollingBatchClient(
	apiKeyRef string,
	credSource credential.Source,
	maxWait time.Duration,
) *harnessPollingBatchClient {
	return &harnessPollingBatchClient{
		httpClient: &http.Client{
			// 30s aligns with CLAUDE.md's non-streaming HTTP timeout
			// guidance; the streaming adapter's 120s is reserved for
			// long-lived SSE responses, which the batch endpoints do
			// not produce.
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 20 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
		credSource: credSource,
		apiKeyRef:  apiKeyRef,
		baseURL:    anthropicBatchAPIBaseURL,
		maxWait:    maxWait,
		logger:     slog.Default(),
	}
}

// resolveAPIKey looks up the current API key via the configured
// credential.Source. Called per HTTP request so credential rotation
// (e.g. a future OAuth source) takes effect mid-wait without rebuilding
// the client. Static sources cache internally so the per-call cost is
// effectively a function pointer dereference.
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
// requires. Centralised so the per-request call sites cannot drift
// (e.g. forget the version header on the polling GET).
func setAuthHeaders(req *http.Request, apiKey string) {
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", anthropicAPIVersion)
	req.Header.Set("Content-Type", "application/json")
}

// anthropicBatchSubmitRequest is the POST body for /v1/messages/batches.
// The "requests" key carries one entry per submission; the polling
// client always submits exactly one (size-1 contract with BatchAdapter).
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

// Submit POSTs a single-entry batch to /v1/messages/batches. The phase-4
// contract matches the phase-2 controlPlaneBatchClient: exactly one
// entry per call. Multi-entry submission is reserved for the future
// OpenAI file-upload flow (phase 6).
func (c *harnessPollingBatchClient) Submit(ctx context.Context, entries []BatchEntry) (string, error) {
	if len(entries) != 1 {
		return "", fmt.Errorf("harnessPollingBatchClient: expected exactly 1 entry, got %d", len(entries))
	}
	entry := entries[0]

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
		return "", fmt.Errorf("submit batch: %w", err)
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

// Result polls /v1/messages/batches/{batchID} until the batch resolves,
// maxWait fires, or ctx is cancelled. On either of the latter two an
// errBatchExpired-wrapped error is returned (timeout) or ctx.Err()
// (cancel) — the BatchAdapter discriminates via errors.Is(err,
// errBatchExpired) so the wrapping is load-bearing for the
// FallbackOnTimeout branch. A best-effort cancel call is issued on
// both exit paths so the operator is not billed for a batch nobody
// will read.
func (c *harnessPollingBatchClient) Result(ctx context.Context, batchID string) (map[string]*BatchResult, error) {
	deadline := time.Now().Add(c.maxWait)
	interval := getBatchPollInitialInterval()

	for {
		obj, err := c.pollOnce(ctx, batchID)
		if err != nil {
			// Context-derived errors short-circuit immediately; HTTP-level
			// errors mid-poll are surfaced as-is because Anthropic's batch
			// API is durable — retrying a transient 5xx is the upstream
			// transport's job (the HTTP client has timeouts but no retry
			// budget). Surfacing keeps the caller's error chain honest.
			if ctx.Err() != nil {
				c.bestEffortCancel(batchID)
				return nil, ctx.Err()
			}
			return nil, err
		}

		if obj.ProcessingStatus == "ended" {
			if obj.ResultsURL == "" {
				return nil, fmt.Errorf("anthropic batch %s ended without results_url", batchID)
			}
			// Validate before fetch: fetchResults sends the x-api-key header
			// to results_url unconditionally. A compromised upstream response
			// (or a misbehaving test fixture) that returns an off-domain URL
			// would otherwise exfiltrate the credential to that host.
			if err := validateResultsURL(obj.ResultsURL, c.baseURL); err != nil {
				return nil, fmt.Errorf("anthropic batch %s: %w", batchID, err)
			}
			return c.fetchResults(ctx, obj.ResultsURL, batchID)
		}

		// Compute next sleep with jitter, then cap-checked against the
		// remaining deadline so the timeout fires on the documented cap
		// instead of one full interval past it.
		sleep := jitter(interval)
		if remaining := time.Until(deadline); remaining <= 0 {
			c.bestEffortCancel(batchID)
			return nil, fmt.Errorf("%w: harness polling timeout after %s (batchID=%s)", errBatchExpired, c.maxWait, batchID)
		} else if sleep > remaining {
			sleep = remaining
		}

		select {
		case <-ctx.Done():
			c.bestEffortCancel(batchID)
			return nil, ctx.Err()
		case <-time.After(sleep):
		}

		// Exponential backoff with a cap. Doubling matches Anthropic's
		// "exponential backoff" guidance; the cap stops a long-running
		// batch from polling at hour-plus intervals.
		interval *= 2
		if interval > batchPollMaxInterval {
			interval = batchPollMaxInterval
		}

		// Final deadline check — the sleep may have consumed the entire
		// remaining budget without yet triggering the loop-top poll.
		if time.Now().After(deadline) {
			c.bestEffortCancel(batchID)
			return nil, fmt.Errorf("%w: harness polling timeout after %s (batchID=%s)", errBatchExpired, c.maxWait, batchID)
		}
	}
}

// pollOnce issues a single GET /v1/messages/batches/{id}. Credential
// resolution is per-call so a rotating source stays fresh across
// long waits.
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
		return nil, fmt.Errorf("poll batch: %w", err)
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
// entry whose custom_id matches a submitted one. The polling client
// always submits exactly one entry, so the returned map carries one
// key — but we still walk the full JSONL response because Anthropic
// does not guarantee ordering.
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
		return nil, fmt.Errorf("fetch batch results: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("anthropic batch results GET returned status %d: %s", resp.StatusCode, errBody)
	}

	out := make(map[string]*BatchResult)
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, maxBatchResponseBytes))
	// JSONL lines can exceed bufio's default 64 KiB buffer for large
	// Messages responses; lift the per-line cap to match the response
	// cap so a single fat assistant turn does not silently truncate.
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
// onto the harness-side BatchResult shape. The error-type strings
// ("server_error", "batch_cancelled", "batch_expired") mirror
// BatchResultError.Type's documented vocabulary.
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

// bestEffortCancel issues a POST /v1/messages/batches/{id}/cancel with
// a short, independent deadline so the cancel does not inherit a
// just-cancelled parent ctx. Errors are logged at warn and swallowed:
// surfacing them would mask the timeout / cancel that triggered the
// cancel call in the first place.
func (c *harnessPollingBatchClient) bestEffortCancel(batchID string) {
	ctx, cancel := context.WithTimeout(context.Background(), batchCancelTimeout)
	defer cancel()

	apiKey, err := c.resolveAPIKey(ctx)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("batch cancel: resolve credential", "batchID", batchID, "error", err)
		}
		return
	}

	cancelURL := c.baseURL + "/v1/messages/batches/" + url.PathEscape(batchID) + "/cancel"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cancelURL, nil)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("batch cancel: build request", "batchID", batchID, "error", err)
		}
		return
	}
	setAuthHeaders(req, apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("batch cancel: send", "batchID", batchID, "error", err)
		}
		return
	}
	_ = resp.Body.Close()
}

// validateResultsURL is a defence-in-depth guard on the results_url that
// the polling loop is about to GET with the run's Anthropic credentials
// attached. Anthropic's documented endpoints sit on api.anthropic.com (and
// future regional subdomains under *.anthropic.com), so a results_url
// pointing anywhere else is either a misframed upstream response or an
// active exfiltration attempt — either way the credential must not be
// sent. The check is split into scheme + host so the operator-facing
// error names which invariant failed.
//
// Test-server relaxation: when the client's baseURL is overridden to a
// loopback / httptest address, accept any URL sharing the same host so
// the existing httptest-based suite (which serves /results off the same
// server as /v1/messages/batches) continues to work. The override is
// scoped to the per-instance baseURL field — production constructions
// always point at api.anthropic.com and trigger the strict branch.
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
		// Fall through to strict checks if hosts differ — the test
		// override is not a blanket bypass.
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

// isBaseURLLoopback reports whether the client's baseURL has been
// overridden to a loopback address — the signal that an httptest server
// is wired in and the strict origin check should accept same-host
// results_url responses.
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
// synchronise their poll cadences (thundering-herd avoidance). Tests
// can disable jitter to keep timings deterministic; the randomness
// here is non-cryptographic by design (no security boundary).
func jitter(d time.Duration) time.Duration {
	if batchPollJitterDisabled.Load() {
		return d
	}
	// ±20%: pick a delta in [-0.2d, +0.2d].
	delta := time.Duration(rand.Int63n(int64(d)/5*2+1)) - d/5
	return d + delta
}
