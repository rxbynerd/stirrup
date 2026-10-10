package core

import (
	"context"
	"math/rand/v2"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/rxbynerd/stirrup/types"
)

// maxTurnRetries caps how many times one model turn is re-issued after a
// retryable provider failure, whatever provider.retry.maxAttempts allows.
// Each attempt may itself make up to maxAttempts pre-stream HTTP requests.
const maxTurnRetries = 2

// turnRetryInitialDelayFallback and turnRetryMaxDelayFallback apply when
// an embedder bypasses ValidateRunConfig and leaves initialDelayMs or
// maxDelayMs at zero; they mirror the RunConfig defaults.
const (
	turnRetryInitialDelayFallback = 500 * time.Millisecond
	turnRetryMaxDelayFallback     = 16 * time.Second
)

// Turn-scope values of the provider.retry.outcome attribute on
// provider_retry_outcomes, recorded once per turn that hit a retryable
// failure while turn retry was enabled.
const (
	turnRetryOutcomeSucceeded       = "succeeded"
	turnRetryOutcomeExhausted       = "exhausted"
	turnRetryOutcomeBudgetExhausted = "budget_exhausted"
	turnRetryOutcomeNonRetryable    = "non_retryable"
	turnRetryOutcomeContextDone     = "context_done"
)

// turnRetryPolicy bounds re-issuing a model turn. A zero policy disables
// turn retry.
type turnRetryPolicy struct {
	maxRetries   int
	initialDelay time.Duration
	maxDelay     time.Duration
	// budget bounds the time from a turn's first retryable failure to
	// the start of its last retry; zero means unbounded.
	budget time.Duration
}

// turnRetryPolicyFor derives the turn retry policy from the retry
// settings of the provider the router selected, falling back to the
// default provider's.
func turnRetryPolicyFor(config *types.RunConfig, providerName string) turnRetryPolicy {
	cfg := config.Provider.Retry
	if named, ok := config.Providers[providerName]; ok && named.Retry != nil {
		cfg = named.Retry
	}
	if cfg == nil || cfg.MaxAttempts <= 1 {
		return turnRetryPolicy{}
	}
	initial := time.Duration(cfg.InitialDelayMs) * time.Millisecond
	if initial <= 0 {
		initial = turnRetryInitialDelayFallback
	}
	maxDelay := time.Duration(cfg.MaxDelayMs) * time.Millisecond
	if maxDelay <= 0 {
		maxDelay = turnRetryMaxDelayFallback
	}
	return turnRetryPolicy{
		maxRetries:   min(cfg.MaxAttempts-1, maxTurnRetries),
		initialDelay: initial,
		maxDelay:     maxDelay,
		budget:       time.Duration(cfg.WallClockBudgetMs) * time.Millisecond,
	}
}

// delay returns the wait before retry n (zero-based): equal jitter over
// min(initialDelay*2^n, maxDelay), so every retry waits at least half
// the bound and an overloaded upstream gets time to recover.
func (p turnRetryPolicy) delay(n int) time.Duration {
	upper := p.initialDelay << n
	if upper <= 0 || upper > p.maxDelay {
		upper = p.maxDelay
	}
	if upper <= 0 {
		return 0
	}
	half := upper / 2
	return half + time.Duration(rand.Int64N(int64(upper-half)))
}

// turnRetrier tracks the retries of one model turn.
type turnRetrier struct {
	policy       turnRetryPolicy
	retries      int
	engaged      bool
	firstFailure time.Time
}

// next decides whether to retry after a retryable failure observed at
// now. It returns the delay to wait when retry is true, otherwise the
// terminal turn-scope outcome ("" when turn retry is disabled).
func (r *turnRetrier) next(now time.Time) (delay time.Duration, outcome string, retry bool) {
	if r.policy.maxRetries == 0 {
		return 0, "", false
	}
	if !r.engaged {
		r.engaged = true
		r.firstFailure = now
	}
	if r.retries >= r.policy.maxRetries {
		return 0, turnRetryOutcomeExhausted, false
	}
	delay = r.policy.delay(r.retries)
	if r.policy.budget > 0 && now.Sub(r.firstFailure)+delay > r.policy.budget {
		return 0, turnRetryOutcomeBudgetExhausted, false
	}
	r.retries++
	return delay, "", true
}

// maxAttempts is the total attempts the policy allows for one turn.
func (r *turnRetrier) maxAttempts() int {
	return r.policy.maxRetries + 1
}

// sleepCtx waits for d or until ctx is done, reporting whether the full
// wait elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// recordTurnRetryOutcome increments provider_retry_outcomes for a turn
// that hit a retryable failure.
func (l *AgenticLoop) recordTurnRetryOutcome(ctx context.Context, providerType, model, outcome string) {
	l.Metrics.ProviderRetryOutcomes.Add(ctx, 1, l.metricAttrs(
		attribute.String("provider.type", providerType),
		attribute.String("provider.model", model),
		attribute.String("provider.retry.outcome", outcome),
		attribute.String("scope", "turn"),
	))
}
