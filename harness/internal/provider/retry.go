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
// into the helper. Constructed from types.ProviderRetryConfig by the
// factory in Wave 3.
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
	return RetryPolicy{
		MaxAttempts:     cfg.MaxAttempts,
		InitialDelay:    time.Duration(cfg.InitialDelayMs) * time.Millisecond,
		MaxDelay:        time.Duration(cfg.MaxDelayMs) * time.Millisecond,
		WallClockBudget: time.Duration(cfg.WallClockBudgetMs) * time.Millisecond,
	}
}

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

// maxRetryAfterHint is an absolute ceiling applied to Retry-After
// and Retry-After-Ms values before they are converted to a
// time.Duration. It defends against two failure modes:
//
//   - Integer overflow: strconv.Atoi returns int (64-bit on 64-bit
//     hosts). Multiplying by time.Millisecond (1e6) or time.Second
//     (1e9) wraps int64 for very large inputs and the wrapped value
//     could satisfy (0, MaxDelay) and silently bypass the cap.
//   - Defence-in-depth against a hostile upstream advertising an
//     absurd value to monopolise client time; the cap forces all
//     hints into a bounded range before MaxDelay clamping runs.
//
// The caller still clamps against policy.MaxDelay; this is the
// upper-bound on what parseRetryAfter is willing to consider in
// the first place.
const maxRetryAfterHint = 60 * time.Second

// parseRetryAfter returns a delay derived from response headers and
// the source it was derived from (one of the delaySource* constants,
// or "" when no usable hint was present). Order: retry-after-ms
// (Azure ext, integer ms), retry-after (RFC 9110 delta-seconds),
// retry-after (HTTP-date against now). The caller caps at
// policy.MaxDelay.
//
// A malformed or non-positive Retry-After-Ms (zero, negative, or
// non-numeric) is treated as "ignore this hint and fall through to
// Retry-After" rather than "retry immediately" — interpreting a
// zero/garbage ms value as a zero-delay retry signal would risk tight
// loops against misbehaving upstreams. Values above maxRetryAfterHint
// fall through to the next header for the same reason.
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

// retry outcome attribute values.
const (
	retryOutcomeSucceeded       = "succeeded"
	retryOutcomeExhausted       = "exhausted"
	retryOutcomeNonRetryable    = "non_retryable"
	retryOutcomeBudgetExhausted = "budget_exhausted"
	retryOutcomeContextDone     = "context_done"
)

// retry delay-source attribute values.
const (
	delaySourceRetryAfterMs = "retry-after-ms"
	delaySourceRetryAfter   = "retry-after"
	delaySourceBackoff      = "backoff"
)

// DoWithRetry issues req using client, retrying on retryable statuses
// and transient transport errors per policy. The caller MUST set
// req.GetBody so the body is rewindable; the helper panics if GetBody
// is nil (programmer error, 100% internal contract).
//
// On terminal failure (non-retryable status, attempts exhausted,
// budget exhausted, ctx cancelled), returns the last response or
// error. Caller owns closing the returned response body on success.
//
// The slog logger is required; pass slog.Default() if no specific
// logger is available. The metrics value is optional — pass nil for
// no instrumentation. The current OTel span is read from ctx via
// oteltrace.SpanFromContext; intermediate-attempt events are added
// to that span when present.
func DoWithRetry(
	ctx context.Context,
	client *http.Client,
	req *http.Request,
	policy RetryPolicy,
	logger *slog.Logger,
	metrics *observability.Metrics,
	providerType string,
	model string,
) (*http.Response, error) {
	if req.GetBody == nil && req.Body != nil {
		// Internal contract: every caller is expected to set GetBody so
		// the helper can rewind the body before each retry. A nil
		// GetBody with a non-nil Body indicates a wiring bug, not a
		// runtime condition — fail loudly instead of silently consuming
		// the body on the first attempt and sending an empty payload on
		// the next.
		panic("DoWithRetry: req.GetBody must be set (internal contract violation)")
	}

	// Per-call PRNG. math/rand/v2 has no global lock, but a per-call
	// source still avoids contention if a future change moves to a
	// pool-of-sources model. Seeded from wall clock — the consequences
	// of a predictable jitter sequence here are negligible (the
	// downside would be marginally less effective storm-avoidance).
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
					recordOutcome(ctx, metrics, providerType, model, retryOutcomeExhausted)
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
		case retryableStatus(resp.StatusCode):
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
			// Transport error with no transient classification: surface
			// immediately as non-retryable.
			recordOutcome(ctx, metrics, providerType, model, retryOutcomeNonRetryable)
			return nil, err
		}

		// We have a retryable outcome. If this was the final attempt,
		// surface the last response/error and record exhaustion without
		// emitting an attempt event (the caller's existing rate_limited
		// event covers the user-visible failure).
		if attempt == maxAttempts-1 {
			recordOutcome(ctx, metrics, providerType, model, retryOutcomeExhausted)
			return lastResp, lastErr
		}

		// Wall-clock budget check: if sleeping now would push us past
		// the deadline, abort.
		if policy.WallClockBudget > 0 && time.Now().Add(delay).After(deadline) {
			recordOutcome(ctx, metrics, providerType, model, retryOutcomeBudgetExhausted)
			return lastResp, lastErr
		}

		attemptNum := attempt + 1
		// Transport errors from http.Client.Do are *url.Error values
		// whose Error() string embeds the full request URL (including
		// any sensitive query parameters). Unwrap before logging so
		// the URL never reaches the slog handler or the OTel span.
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
				spanAttrs = append(spanAttrs, attribute.String("error", unwrappedErrStr))
			} else if lastResp != nil {
				spanAttrs = append(spanAttrs, attribute.Int("status", lastResp.StatusCode))
			}
			span.AddEvent("provider_retry_attempt", oteltrace.WithAttributes(spanAttrs...))
		}

		// Drain the previous response body before retrying so the
		// connection can be reused. Skip for transport errors where
		// resp is nil.
		if lastResp != nil {
			_, _ = io.Copy(io.Discard, lastResp.Body)
			_ = lastResp.Body.Close()
			lastResp = nil
		}

		// time.NewTimer + Stop drains the underlying timer if the
		// context is cancelled mid-sleep — avoids leaking the
		// goroutine spawned by time.After.
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

	// Unreachable: the loop returns on every path. Guarded for the
	// compiler.
	recordOutcome(ctx, metrics, providerType, model, retryOutcomeExhausted)
	return lastResp, lastErr
}

func recordOutcome(ctx context.Context, m *observability.Metrics, providerType, model, outcome string) {
	if m == nil {
		return
	}
	m.ProviderRetries.Add(ctx, 1, metric.WithAttributes(
		attribute.String("provider.type", providerType),
		attribute.String("provider.model", model),
		attribute.String("provider.retry.outcome", outcome),
	))
}
