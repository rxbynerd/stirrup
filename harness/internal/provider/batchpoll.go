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
	// /v1/messages/batches endpoints. The polling client appends the
	// resource path on each request. baseURL is overridable on the
	// struct so httptest fixtures can swap in a mock server.
	anthropicBatchAPIBaseURL = "https://api.anthropic.com"

	// batchPollMaxIntervalDefault caps the exponential backoff between
	// polls. Five minutes mirrors the batch_waiting heartbeat cadence
	// the streaming wrapper emits — there is no value in polling faster
	// than the heartbeat once a batch has been in flight for a while.
	// The live cap is stored in batchPollMaxIntervalNs so tests can
	// lower it without subjecting the test runner to a 5-minute wait.
	batchPollMaxIntervalDefault = 5 * time.Minute

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

// batchPollMaxIntervalNs holds the live polling cap in nanoseconds.
// Stored as an atomic so tests can lower it (via swapBatchPollMaxInterval)
// to exercise the cap branch in well under a minute. Production runs
// always read the documented default (5 minutes).
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

// swapBatchPollMaxInterval lets tests lower the polling cap below the
// 5-minute production default so the cap branch can be exercised
// within the test runner's budget. Returns the prior value so the
// caller can restore it on teardown.
func swapBatchPollMaxInterval(d time.Duration) time.Duration {
	prev := batchPollMaxIntervalNs.Swap(int64(d))
	return time.Duration(prev)
}

// harnessPollingBatchClient implements BatchClient by talking directly to
// the provider's batch HTTP API. Used when the harness runs in stdio mode
// (no control plane is available to own the batch lifecycle) and the
// operator opts in via Batch.HarnessSidePolling=true.
//
// Supported provider types are dispatched in Submit/Result based on
// providerType: "anthropic" uses the Messages Batches API; the
// "openai-compatible" and "openai-responses" types use the OpenAI Batch
// API (two-step file upload + batch creation, /v1/files + /v1/batches).
type harnessPollingBatchClient struct {
	httpClient   *http.Client
	credSource   credential.Source
	providerType string
	// apiKeyRef is the secret:// reference string captured for diagnostic
	// provenance — logged as the keyRef attribute in bestEffortCancel's
	// failure warns. The resolver consumes the underlying secret value
	// via credSource; the reference string itself is safe to log because
	// Redact() strips secret references from any persisted trace.
	apiKeyRef string
	baseURL   string
	// apiKeyHeader overrides the OpenAI auth header. Empty means
	// "Authorization: Bearer <key>" (OpenAI default). Setting it to
	// "api-key" routes through the Azure-style header path. Ignored on
	// the Anthropic branch — that branch always uses x-api-key.
	apiKeyHeader string
	maxWait      time.Duration
	logger       *slog.Logger

	// lastSubmittedCustomID retains the customID supplied to the most
	// recent Submit so resultOpenAI can key terminal-status / top-level-
	// failure BatchResults under the originating customID. The OpenAI
	// poll/result endpoints address by batchID (no customID round-trip)
	// and the BatchAdapter looks up by customID; without this bridge,
	// cancelled / failed-without-error-file outcomes surface as the
	// opaque "missing entry" error rather than the typed BatchResultError.
	// Safe for the single-in-flight contract: harnessPollingBatchClient is
	// constructed per-run and Submit accepts exactly one entry; future
	// multi-entry support will need a batchID → customID map keyed off
	// the upload payload (deferred to phase 7 follow-up).
	lastSubmittedCustomID atomic.Value // string
}

// HarnessBatchClientOptions configures NewHarnessPollingBatchClient. The
// struct form keeps the constructor signature stable as new provider
// branches are wired in: callers fill the fields they need and the rest
// get zero-value defaults appropriate to the provider type.
type HarnessBatchClientOptions struct {
	// ProviderType selects the wire dialect. One of "anthropic" |
	// "openai-compatible" | "openai-responses". Required.
	ProviderType string

	// APIKeyRef is the secret:// reference; captured for diagnostic
	// provenance and logged as the keyRef attribute in cancel-path
	// warnings. The actual key value is fetched through CredSource.
	APIKeyRef string

	// CredSource resolves the bearer credential on every HTTP request.
	// Per-request resolution keeps the client compatible with future
	// rotating sources (today's StaticSource caches internally so the
	// per-call cost is a function pointer dereference).
	CredSource credential.Source

	// BaseURL overrides the per-provider default API root. Empty falls
	// back to the documented production URL for the chosen provider
	// (api.anthropic.com or api.openai.com/v1).
	BaseURL string

	// APIKeyHeader overrides the OpenAI auth header for Azure-style
	// gateways. Empty preserves the "Authorization: Bearer <key>"
	// default. Example non-default value: "api-key" (Azure OpenAI
	// Service). See setOpenAIAuthHeader in openai.go for the
	// implementation. Ignored when ProviderType is "anthropic".
	APIKeyHeader string

	// MaxWait is the wall-clock cap on a single Result call. An
	// expiration returns an error wrapping errBatchExpired so
	// BatchAdapter's FallbackOnTimeout branch routes correctly.
	// Zero or negative values fall back to
	// types.DefaultBatchMaxWaitSeconds.
	MaxWait time.Duration

	// Logger is the run-scoped slog logger used for cancel-path
	// warnings, credential-resolution failures, and unrecognised
	// status warnings. Nil falls back to slog.Default(); the factory
	// injects the run-scoped logger so events flow through the
	// ScrubHandler with the runID attribute attached.
	Logger *slog.Logger
}

// NewHarnessPollingBatchClient constructs a polling client for the
// provider batch API matching opts.ProviderType. The opts.CredSource
// closure is invoked once per HTTP request (Submit and each poll tick)
// so a refresh-aware source — today only static, in future possibly an
// OAuth source — stays fresh across long waits.
//
// opts.APIKeyRef is captured for diagnostic provenance; the credential
// resolver consumes the underlying secret reference via CredSource.
// opts.MaxWait is the wall-clock cap on a single Result call; zero or
// negative falls back to types.DefaultBatchMaxWaitSeconds. opts.Logger
// is the run-scoped slog logger; nil falls back to slog.Default().
//
// Panics if opts.CredSource is nil — the field is required and a
// deferred nil deref on the first HTTP request is harder to diagnose
// than a clear constructor-time panic. The panic form matches the
// harness convention for nil required dependencies (see retry.go).
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
	// Trim trailing slash so URL joining stays consistent regardless of
	// whether the operator supplied a base with or without one.
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

// Submit routes the single-entry batch to the configured provider's
// submission endpoint. The single-entry contract matches the phase-2
// controlPlaneBatchClient (the BatchAdapter always submits one entry
// per turn); the slice shape is preserved so a future caller batching
// multiple turns has somewhere to grow.
func (c *harnessPollingBatchClient) Submit(ctx context.Context, entries []BatchEntry) (string, error) {
	if len(entries) != 1 {
		return "", fmt.Errorf("harnessPollingBatchClient: expected exactly 1 entry, got %d", len(entries))
	}
	switch c.providerType {
	case "openai-compatible", "openai-responses":
		return c.submitOpenAI(ctx, entries[0])
	case "anthropic", "":
		// Empty providerType is treated as "anthropic" for backwards
		// compatibility with any caller that bypasses the validator and
		// constructs the client without setting it.
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

// Result polls the configured provider's batch endpoint until the batch
// resolves, maxWait fires, or ctx is cancelled. On either of the latter
// two an errBatchExpired-wrapped error is returned (timeout) or
// ctx.Err() (cancel) — the BatchAdapter discriminates via
// errors.Is(err, errBatchExpired) so the wrapping is load-bearing for
// the FallbackOnTimeout branch. A best-effort cancel call is issued on
// both exit paths so the operator is not billed for a batch nobody
// will read.
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
			// Context-derived errors short-circuit immediately; HTTP-level
			// errors mid-poll are surfaced as-is because Anthropic's batch
			// API is durable — retrying a transient 5xx is the upstream
			// transport's job (the HTTP client has timeouts but no retry
			// budget). Surfacing keeps the caller's error chain honest.
			//
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
			// Validate before fetch: fetchResults sends the x-api-key header
			// to results_url unconditionally. A compromised upstream response
			// (or a misbehaving test fixture) that returns an off-domain URL
			// would otherwise exfiltrate the credential to that host.
			if err := validateResultsURL(obj.ResultsURL, c.baseURL); err != nil {
				return nil, fmt.Errorf("anthropic batch %s: %w", batchID, err)
			}
			return c.fetchResults(ctx, obj.ResultsURL, batchID)
		case "in_progress", "canceling":
			// Documented intermediate statuses; continue polling.
		default:
			// Anthropic may add new terminal or intermediate statuses
			// over time (e.g. "cancelled"); a silent infinite poll until
			// maxWait fires obscures the cause. Warn once per poll and
			// continue — the deadline still bounds the loop.
			if c.logger != nil {
				c.logger.Warn(
					"batch poll: unrecognised processing_status; continuing to poll",
					"batchID", batchID,
					"status", obj.ProcessingStatus,
				)
			}
		}

		// Compute next sleep with jitter, then cap-checked against the
		// remaining deadline so the timeout fires on the documented cap
		// instead of one full interval past it.
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

		// Exponential backoff with a cap. Doubling matches Anthropic's
		// "exponential backoff" guidance; the cap stops a long-running
		// batch from polling at hour-plus intervals.
		interval *= 2
		if maxInterval := getBatchPollMaxInterval(); interval > maxInterval {
			interval = maxInterval
		}

		// Final deadline check — the sleep may have consumed the entire
		// remaining budget without yet triggering the loop-top poll.
		if time.Now().After(deadline) {
			go c.bestEffortCancel(batchID)
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

// bestEffortCancel issues a provider-specific cancel POST with a short,
// independent deadline so the cancel does not inherit a just-cancelled
// parent ctx. Errors are logged at warn and swallowed: surfacing them
// would mask the timeout / cancel that triggered the cancel call in
// the first place.
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
			c.logger.Warn("batch cancel: send", "batchID", batchID, "error", err, "keyRef", c.apiKeyRef)
		}
		return
	}
	// Drain the body so the transport can reuse the TCP connection on the
	// next polling tick. Cap at 4 KiB because the cancel endpoint returns
	// a small JSON object and a misbehaving server should not be allowed
	// to push the harness into reading an unbounded body.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK && c.logger != nil {
		c.logger.Warn("batch cancel: unexpected status", "batchID", batchID, "status", resp.StatusCode, "keyRef", c.apiKeyRef)
	}
}

// applyAuthHeaders pins the auth-related headers appropriate to the
// configured provider. Anthropic's setAuthHeaders also sets
// Content-Type to application/json because all three Anthropic batch
// endpoints (submit, poll, cancel) consume JSON bodies; the OpenAI
// branch deliberately does NOT set Content-Type here so multipart
// uploaders can attach their boundary-bearing Content-Type without
// being clobbered. JSON-body OpenAI callers set Content-Type at the
// call site.
func (c *harnessPollingBatchClient) applyAuthHeaders(req *http.Request, apiKey string) {
	switch c.providerType {
	case "openai-compatible", "openai-responses":
		setOpenAIAuthHeader(req, apiKey, c.apiKeyHeader)
	default:
		setAuthHeaders(req, apiKey)
	}
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

// -----------------------------------------------------------------------------
// OpenAI batch path
// -----------------------------------------------------------------------------

// openaiBatchObject mirrors the relevant fields of an OpenAI batch
// object as returned by POST /v1/batches and GET /v1/batches/{id}.
// Statuses progress: validating → in_progress → finalizing → completed
// (or failed / expired / cancelling / cancelled). Only the fields the
// polling loop consumes are decoded; the API returns more.
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
// batch failures (distinct from per-entry errors carried in
// error_file_id). The object's "data" array holds free-form error
// records; we extract the first message for surfacing in the
// BatchResultError on a generic failure.
type openaiBatchErrors struct {
	Data []struct {
		Code    string `json:"code,omitempty"`
		Message string `json:"message,omitempty"`
		Line    *int   `json:"line,omitempty"`
	} `json:"data,omitempty"`
}

// openaiBatchInputLine is one line of the JSONL document uploaded to
// /v1/files with purpose=batch. The "url" field is the endpoint each
// entry's request will be dispatched to (/v1/chat/completions or
// /v1/responses); "body" carries the same wire body the streaming
// adapter would have POSTed.
type openaiBatchInputLine struct {
	CustomID string          `json:"custom_id"`
	Method   string          `json:"method"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}

// openaiBatchOutputLine is one line of the JSONL document downloaded
// from /v1/files/{output_file_id}/content. Each entry's "response"
// field carries the same shape the streaming adapter would have read
// from the HTTP response body — fabricateStream consumes
// response.body verbatim.
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
// batch endpoint string. Centralised so submit and result decoders
// agree on which URL each entry targets.
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
	// Record the customID so resultOpenAI can key terminal-status /
	// top-level-failure BatchResults under it (the OpenAI batch poll
	// endpoints carry only the batchID; the BatchAdapter looks up by
	// customID — see the lastSubmittedCustomID field doc).
	c.lastSubmittedCustomID.Store(entry.CustomID)

	// Step 1: upload the JSONL input file.
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

	// Step 2: create the batch referencing the uploaded file.
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
		return "", fmt.Errorf("submit openai batch: %w", err)
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
// The Content-Type (including the boundary) is written by mime/multipart;
// the caller must not override it.
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
	// Content-Type carries the multipart boundary the writer emits; set
	// it before applyAuthHeaders. (applyAuthHeaders no longer touches
	// Content-Type on the OpenAI branch precisely so this call site can
	// participate in the same auth dispatch as the JSON-body callers.)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c.applyAuthHeaders(req, apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload openai batch file: %w", err)
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
//
// The customID parameter is read from the most recent successful
// submitOpenAI (stored on the client via lastSubmittedCustomID). It is
// used to key terminal-status results (cancelled / cancelling) and
// top-level failure fallbacks so the BatchAdapter's customID lookup
// surfaces the typed BatchResultError rather than the opaque
// "missing entry" message.
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
			// Per-entry errors live in error_file_id when the API
			// could route the failure to a specific submission; the
			// top-level errors.data field is reserved for batch-wide
			// failures (e.g. malformed input file). Either way the
			// caller receives a single BatchResult keyed by the
			// originating customID so the BatchAdapter's lookup
			// surfaces the typed BatchResultError.
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
		return nil, fmt.Errorf("poll openai batch: %w", err)
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
// BatchResult keyed by custom_id. A per-entry error in the line maps
// to BatchResultError; a successful line surfaces response.body as
// the BatchResult.Response (the same JSON the streaming adapter
// would have read from /v1/chat/completions or /v1/responses).
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
// harness-side BatchResult shape. A non-2xx response.status_code or
// a populated top-level error block surfaces as a server_error.
//
// status_code == 0 (the JSON zero value, i.e. the field was absent or
// malformed) is treated as a server_error rather than silently
// fabricating a stream from an unverified body — OpenAI always sets
// status_code on a valid output line.
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

// fetchOpenAIFailure handles the "failed" terminal status. When
// error_file_id is set we surface the first per-entry error keyed by
// the failing custom_id. When only the top-level errors object is
// populated (e.g. malformed input file rejected before per-entry
// dispatch), we synthesise a server_error keyed under the originating
// customID so the BatchAdapter's lookup surfaces the typed
// BatchResultError rather than the opaque "missing entry" error.
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

	// No per-entry error file (or it was empty). Fall back to the
	// top-level errors object and surface as a synthesised server_error
	// keyed under the originating customID so BatchAdapter sees a typed
	// entry rather than an opaque "missing entry" error.
	msg := fmt.Sprintf("openai batch %s failed upstream", batchID)
	if obj.Errors != nil && len(obj.Errors.Data) > 0 && obj.Errors.Data[0].Message != "" {
		msg = obj.Errors.Data[0].Message
	}
	return c.synthesizeOpenAIFailure(batchID, customID, "server_error", msg), nil
}

// synthesizeOpenAIFailure produces a single-entry map for a terminal
// failure status that does not carry a per-entry error file (cancelled
// / cancelling, or a "failed" top-level error with no error_file_id).
// The result is keyed under the originating customID (captured from the
// last successful Submit) so the BatchAdapter's customID lookup
// surfaces the typed BatchResultError. If customID is empty (a
// pathological Result-without-prior-Submit call), the batchID is used
// as a fallback so the map is non-empty and the BatchAdapter still
// returns a typed error instead of "missing entry".
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
		return nil, fmt.Errorf("download openai file: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("openai file download returned status %d: %s", resp.StatusCode, security.Scrub(string(errBody)))
	}
	return resp.Body, nil
}
