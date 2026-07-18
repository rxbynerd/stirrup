package provider

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// RetryPolicy is the resolved, validated retry configuration passed
// into the helper. A zero RetryPolicy makes exactly one attempt with
// no backoff and no wall-clock budget.
type RetryPolicy struct {
	MaxAttempts     int
	InitialDelay    time.Duration
	MaxDelay        time.Duration
	WallClockBudget time.Duration
}

// RetryPolicyFromConfig converts a (post-defaulting) types.ProviderRetryConfig
// into a RetryPolicy. Callers must call types.ValidateRunConfig before
// invoking this so the input is fully populated.
func RetryPolicyFromConfig(cfg *types.ProviderRetryConfig) RetryPolicy {
	if cfg == nil {
		return RetryPolicy{}
	}
	initialDelay := time.Duration(cfg.InitialDelayMs) * time.Millisecond
	if cfg.InitialDelayMs == 0 {
		// A zero InitialDelay would make backoffDelay return zero on
		// every attempt, producing a tight retry loop. Embedders that
		// bypass ValidateRunConfig still get the safe default here.
		initialDelay = defaultInitialDelayFallback
	}
	return RetryPolicy{
		MaxAttempts:     cfg.MaxAttempts,
		InitialDelay:    initialDelay,
		MaxDelay:        time.Duration(cfg.MaxDelayMs) * time.Millisecond,
		WallClockBudget: time.Duration(cfg.WallClockBudgetMs) * time.Millisecond,
	}
}

// defaultInitialDelayFallback mirrors types.defaultProviderRetryInitialDelayMs
// (500ms), duplicated because that constant is unexported.
const defaultInitialDelayFallback = 500 * time.Millisecond

// retryableStatus reports whether status code s warrants a retry.
// List is the cross-SDK consensus: 408, 409, 429, 500, 502, 503, 504.
func retryableStatus(s int) bool {
	switch s {
	case 408, 409, 429, 500, 502, 503, 504:
		return true
	default:
		return false
	}
}

// transientErr reports whether a transport-level error is retryable.
// Timeouts always qualify; io.EOF qualifies only on the first attempt,
// where it most likely indicates a stale keepalive connection rather
// than a server-side condition the next attempt would also hit.
func transientErr(err error, attempt int) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if attempt == 0 && errors.Is(err, io.EOF) {
		return true
	}
	return false
}

// maxRetryAfterHint ceilings Retry-After/-Ms values before conversion to
// time.Duration, guarding against int64 overflow on the ms multiplication
// and against a hostile upstream advertising an absurd delay. The caller
// still clamps against policy.MaxDelay separately.
const maxRetryAfterHint = 60 * time.Second

// parseRetryAfter returns a delay derived from response headers and
// the source it was derived from (one of the delaySource* constants,
// or "" when no usable hint was present). Order: retry-after-ms
// (Azure ext, integer ms), retry-after (RFC 9110 delta-seconds),
// retry-after (HTTP-date against now). The caller caps at
// policy.MaxDelay.
//
// A malformed, non-positive, or above-ceiling Retry-After-Ms value
// falls through to Retry-After rather than being treated as a
// zero-delay retry signal, to avoid tight loops against misbehaving
// upstreams.
func parseRetryAfter(h http.Header, now time.Time) (time.Duration, string) {
	if v := h.Get("Retry-After-Ms"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			d := time.Duration(ms) * time.Millisecond
			if d <= maxRetryAfterHint {
				return d, delaySourceRetryAfterMs
			}
		}
	}
	v := h.Get("Retry-After")
	if v == "" {
		return 0, ""
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		d := time.Duration(secs) * time.Second
		if d <= maxRetryAfterHint {
			return d, delaySourceRetryAfter
		}
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		if d > 0 && d <= maxRetryAfterHint {
			return d, delaySourceRetryAfter
		}
	}
	return 0, ""
}

// backoffDelay returns the algorithmic backoff for attempt n (zero-based).
// Full jitter against min(initial * 2^n, maxDelay). Full jitter is
// preferred over centred jitter because it minimises the probability of
// synchronised retry storms when many concurrent runs hit the same
// upstream rate limit at once.
func backoffDelay(n int, policy RetryPolicy, r *rand.Rand) time.Duration {
	if policy.InitialDelay <= 0 {
		return 0
	}
	upper := policy.InitialDelay << n
	if upper <= 0 || upper > policy.MaxDelay {
		upper = policy.MaxDelay
	}
	if upper <= 0 {
		return 0
	}
	return time.Duration(r.Int64N(int64(upper)))
}

// retry outcome attribute values. Closed set: every DoWithRetry path
// that reaches recordOutcome uses one of these constants.
const (
	retryOutcomeSucceeded       = "succeeded"
	retryOutcomeExhausted       = "exhausted"
	retryOutcomeNonRetryable    = "non_retryable"
	retryOutcomeBudgetExhausted = "budget_exhausted"
	retryOutcomeContextDone     = "context_done"
	retryOutcomeRewindFailed    = "rewind_failed"
)

// retry delay-source attribute values.
const (
	delaySourceRetryAfterMs = "retry-after-ms"
	delaySourceRetryAfter   = "retry-after"
	delaySourceBackoff      = "backoff"
)

// RetryOptions bundles the non-request configuration DoWithRetry
// needs. Policy zero value produces single-attempt behaviour; Logger
// nil falls back to slog.Default(); Metrics nil disables outcome
// recording. ShouldRetry, when non-nil, is consulted before the
// default retryableStatus heuristic: consumed=true makes its
// retryable value final, consumed=false falls through to the
// heuristic. Transport errors always bypass ShouldRetry and are
// routed through transientErr.
type RetryOptions struct {
	Policy       RetryPolicy
	Logger       *slog.Logger
	Metrics      *observability.Metrics
	ProviderType string
	Model        string
	ShouldRetry  func(*http.Response) (retryable bool, consumed bool)
}

// DoWithRetry issues req using client, retrying on retryable statuses
// and transient transport errors per opts.Policy. The caller MUST
// set req.GetBody so the body is rewindable; the helper panics if
// GetBody is nil.
//
// On terminal failure (non-retryable status, attempts exhausted,
// budget exhausted, ctx cancelled, rewind failure), returns the last
// response or error. Caller owns closing the returned response body
// whenever resp != nil, including on terminal non-success statuses
// returned without an error.
func DoWithRetry(
	ctx context.Context,
	client *http.Client,
	req *http.Request,
	opts RetryOptions,
) (*http.Response, error) {
	if req.GetBody == nil && req.Body != nil {
		// A nil GetBody with a non-nil Body is a caller wiring bug, not
		// a runtime condition — fail loudly instead of silently sending
		// an empty payload on retry.
		panic("DoWithRetry: req.GetBody must be set (internal contract violation)")
	}

	logger := opts.Logger
	if logger == nil {
		// Wrap the process-wide slog default with a ScrubHandler so a
		// caller that omits Logger still gets retry warnings redacted.
		logger = slog.New(observability.NewScrubHandler(slog.Default().Handler()))
	}
	policy := opts.Policy
	metrics := opts.Metrics
	providerType := opts.ProviderType
	model := opts.Model
	shouldRetry := opts.ShouldRetry

	seed := uint64(time.Now().UnixNano())
	prng := rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))

	start := time.Now()
	deadline := start.Add(policy.WallClockBudget)
	span := oteltrace.SpanFromContext(ctx)

	var (
		lastResp *http.Response
		lastErr  error
	)

	maxAttempts := policy.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if req.GetBody != nil {
				body, err := req.GetBody()
				if err != nil {
					// Rewind failure is distinct from exhaustion: retries
					// were not used up, the body just cannot be replayed.
					// Drain and close the previous response so the
					// connection can be reused.
					if lastResp != nil {
						_, _ = io.Copy(io.Discard, io.LimitReader(lastResp.Body, 4096))
						_ = lastResp.Body.Close()
					}
					logger.Warn("provider_retry_rewind_failed",
						"event", "provider_retry_rewind_failed",
						"provider", providerType,
						"model", model,
						"attempt", attempt+1,
						"error", security.Scrub(err.Error()),
					)
					recordOutcome(ctx, metrics, providerType, model, retryOutcomeRewindFailed)
					return nil, err
				}
				req.Body = body
			}
		}

		resp, err := client.Do(req)

		retryable := false
		var delay time.Duration
		var delaySource string

		switch {
		case err != nil:
			lastResp = nil
			lastErr = err
			if transientErr(err, attempt) {
				retryable = true
				delay = backoffDelay(attempt, policy, prng)
				delaySource = delaySourceBackoff
			}
		case classifyRetryable(resp, shouldRetry):
			lastResp = resp
			lastErr = nil
			retryable = true
			now := time.Now()
			hint, source := parseRetryAfter(resp.Header, now)
			if hint > 0 {
				if hint > policy.MaxDelay {
					hint = policy.MaxDelay
				}
				delay = hint
				delaySource = source
			} else {
				delay = backoffDelay(attempt, policy, prng)
				delaySource = delaySourceBackoff
			}
		default:
			// Non-retryable: success or terminal client/server error.
			if err == nil && (resp.StatusCode >= 200 && resp.StatusCode < 300) {
				recordOutcome(ctx, metrics, providerType, model, retryOutcomeSucceeded)
			} else {
				recordOutcome(ctx, metrics, providerType, model, retryOutcomeNonRetryable)
			}
			return resp, nil
		}

		if !retryable {

			recordOutcome(ctx, metrics, providerType, model, retryOutcomeNonRetryable)
			return nil, err
		}

		if attempt == maxAttempts-1 {
			recordOutcome(ctx, metrics, providerType, model, retryOutcomeExhausted)
			return lastResp, lastErr
		}

		if policy.WallClockBudget > 0 && time.Now().Add(delay).After(deadline) {
			recordOutcome(ctx, metrics, providerType, model, retryOutcomeBudgetExhausted)
			return lastResp, lastErr
		}

		attemptNum := attempt + 1
		// *url.Error's Error() embeds the full request URL, including
		// sensitive query parameters. Unwrap before logging so the URL
		// never reaches the slog handler or the OTel span.
		var unwrappedErrStr string
		if err != nil {
			unwrappedErrStr = err.Error()
			var ue *url.Error
			if errors.As(err, &ue) {
				unwrappedErrStr = ue.Err.Error()
			}
		}
		logAttrs := []any{
			"event", "provider_retry",
			"provider", providerType,
			"model", model,
			"attempt", attemptNum,
			"delay_ms", delay.Milliseconds(),
			"delay_source", delaySource,
		}
		if err != nil {
			logAttrs = append(logAttrs, "error", security.Scrub(unwrappedErrStr))
		} else if lastResp != nil {
			logAttrs = append(logAttrs, "status", lastResp.StatusCode)
		}
		logger.Warn("provider_retry", logAttrs...)

		if span.IsRecording() {
			spanAttrs := []attribute.KeyValue{
				attribute.String("provider.type", providerType),
				attribute.String("provider.model", model),
				attribute.Int("attempt", attemptNum),
				attribute.Int64("delay_ms", delay.Milliseconds()),
				attribute.String("delay_source", delaySource),
			}
			if err != nil {
				// Mirror the slog scrubbing above so the OTel span
				// exporter never receives an unredacted value.
				spanAttrs = append(spanAttrs, attribute.String("error", security.Scrub(unwrappedErrStr)))
			} else if lastResp != nil {
				spanAttrs = append(spanAttrs, attribute.Int("status", lastResp.StatusCode))
			}
			span.AddEvent("provider_retry_attempt", oteltrace.WithAttributes(spanAttrs...))
		}

		// Bound the drain at 4 KB so a hostile upstream cannot stall
		// progress by streaming an unbounded body.
		if lastResp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(lastResp.Body, 4096))
			_ = lastResp.Body.Close()
			lastResp = nil
		}

		// Stop drains the timer on cancellation, avoiding a leaked
		// goroutine (time.After would not allow this).
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			recordOutcome(ctx, metrics, providerType, model, retryOutcomeContextDone)
			return nil, ctx.Err()
		}
	}

	// The for-loop returns on every iteration (success, non-retryable,
	// exhausted, budget, context-done). Reaching here would mean
	// maxAttempts was zero after the < 1 clamp, which is impossible.
	panic("unreachable")
}

// classifyRetryable decides whether resp should be retried. The
// optional shouldRetry callback gets first pass: when consumed=true
// its retryable value is final; when consumed=false the call falls
// through to the default retryableStatus heuristic.
func classifyRetryable(resp *http.Response, shouldRetry func(*http.Response) (bool, bool)) bool {
	if shouldRetry != nil {
		if retryable, consumed := shouldRetry(resp); consumed {
			return retryable
		}
	}
	return retryableStatus(resp.StatusCode)
}

func recordOutcome(ctx context.Context, m *observability.Metrics, providerType, model, outcome string) {
	if m == nil {
		return
	}
	m.ProviderRetryOutcomes.Add(ctx, 1, metric.WithAttributes(
		attribute.String("provider.type", providerType),
		attribute.String("provider.model", model),
		attribute.String("provider.retry.outcome", outcome),
	))
}
