package sandboxidentity

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// MaxTokenRequests bounds the sandbox_token_requests a run may send,
// matching the control plane's per-stream cap (hairpin accepts eight). The
// initial exchange counts, so at most seven refreshes follow.
const MaxTokenRequests = 8

// refreshFraction schedules a refresh once this fraction of the token's
// remaining lifetime has elapsed; the rest is slack for a slow control
// plane and clock skew between it and the harness.
const refreshFraction = 0.8

// TokenWriter delivers a token into the running sandbox. The executors
// satisfy it through executor.SandboxIdentityTokenWriter.
type TokenWriter interface {
	WriteSandboxIdentityToken(ctx context.Context, token string) error
}

// RefresherConfig wires a Refresher.
type RefresherConfig struct {
	Exchanger *Exchanger
	Writer    TokenWriter
	// Transport receives the "warning" HarnessEvent a terminal refresh
	// failure emits.
	Transport Transport
	Logger    *slog.Logger
	Audience  string
	// Timeout bounds each exchange; DefaultTimeout when non-positive.
	Timeout time.Duration
	// ExpiresAt is the Unix-seconds expiry of the token currently in the
	// sandbox. Nil schedules nothing: without an expiry there is no
	// lifetime to refresh against, and the token as issued stands.
	ExpiresAt *int64
	// BudgetDeadline is the run's wall-clock deadline, zero when unbounded.
	// A token outliving it makes an exhausted request budget unremarkable.
	BudgetDeadline time.Time
	// Now is injectable for tests; nil selects time.Now.
	Now func() time.Time
}

// Refresher re-requests the sandbox identity token ahead of expiry and
// delivers each new token through the TokenWriter, decoupling the token's
// lifetime from the run's wall-clock budget. Every terminal outcome that
// leaves a token in the sandbox past its expiry — a declined or failed
// exchange, a failed delivery, an exhausted request budget — is reported
// as a "warning" HarnessEvent and a log line, never silently.
//
// The agentic loop is unaware of it: the factory starts it once the initial
// token is delivered and stops it through Close with the loop's other owned
// resources.
type Refresher struct {
	cfg    RefresherConfig
	cancel context.CancelFunc
	done   chan struct{}
}

// NewRefresher validates cfg's required members and returns a stopped
// Refresher.
func NewRefresher(cfg RefresherConfig) (*Refresher, error) {
	if cfg.Exchanger == nil || cfg.Writer == nil || cfg.Transport == nil {
		return nil, fmt.Errorf("sandboxidentity: refresher requires an Exchanger, a TokenWriter, and a Transport")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Refresher{cfg: cfg}, nil
}

// Start launches the refresh schedule on a goroutine bounded by ctx. It is
// a no-op beyond a log line when the initial token reported no expiry.
func (r *Refresher) Start(ctx context.Context) {
	if r.cfg.ExpiresAt == nil {
		r.cfg.Logger.Info("sandbox identity token reports no expiry; refresh not scheduled")
		return
	}
	ctx, r.cancel = context.WithCancel(ctx)
	r.done = make(chan struct{})
	go r.run(ctx, *r.cfg.ExpiresAt)
}

// Close stops the schedule and waits for an in-flight refresh to abandon
// its exchange. Safe to call without Start.
func (r *Refresher) Close() error {
	if r.cancel == nil {
		return nil
	}
	r.cancel()
	<-r.done
	return nil
}

func (r *Refresher) run(ctx context.Context, expiresAt int64) {
	defer close(r.done)

	for {
		if r.cfg.Exchanger.Requests() >= MaxTokenRequests {
			r.exhausted(expiresAt)
			return
		}

		timer := time.NewTimer(refreshDelay(r.cfg.Now(), expiresAt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		result, err := r.cfg.Exchanger.Exchange(ctx, r.cfg.Audience, r.cfg.Timeout)
		if err != nil {
			r.failed(ctx, expiresAt, err)
			return
		}
		if err := r.cfg.Writer.WriteSandboxIdentityToken(ctx, result.Token); err != nil {
			r.failed(ctx, expiresAt, fmt.Errorf("deliver refreshed token: %w", err))
			return
		}

		requests := r.cfg.Exchanger.Requests()
		if result.ExpiresAt == nil {
			r.cfg.Logger.Info("sandbox identity token refreshed; the control plane reported no expiry, so no further refresh is scheduled",
				"requests", requests)
			return
		}
		expiresAt = *result.ExpiresAt
		r.cfg.Logger.Info("sandbox identity token refreshed",
			"requests", requests,
			"tokenExpiresAtUnix", expiresAt)
	}
}

// failed reports a refresh that left the previous token in place. A ctx
// that has already ended means the run is over, which is not a failure.
func (r *Refresher) failed(ctx context.Context, expiresAt int64, err error) {
	if ctx.Err() != nil {
		r.cfg.Logger.Debug("sandbox identity token refresh abandoned; run context ended", "error", err)
		return
	}
	r.warn(fmt.Sprintf(
		"sandbox identity token refresh failed: %v; the sandbox keeps its previous token, which expires at %d (Unix seconds), and git operations after that will fail authentication",
		err, expiresAt),
		"error", err, "tokenExpiresAtUnix", expiresAt)
}

// exhausted reports that no further request may be sent. It stays quiet
// when the current token outlives the run's known budget.
func (r *Refresher) exhausted(expiresAt int64) {
	if !r.cfg.BudgetDeadline.IsZero() && expiresAt >= r.cfg.BudgetDeadline.Unix() {
		r.cfg.Logger.Info("sandbox identity token request budget exhausted; the current token outlives the run's wall-clock budget",
			"requests", MaxTokenRequests, "tokenExpiresAtUnix", expiresAt)
		return
	}
	r.warn(fmt.Sprintf(
		"sandbox identity token request budget exhausted after %d requests; the current token expires at %d (Unix seconds), and git operations after that will fail authentication",
		MaxTokenRequests, expiresAt),
		"requests", MaxTokenRequests, "tokenExpiresAtUnix", expiresAt)
}

func (r *Refresher) warn(message string, logArgs ...any) {
	r.cfg.Logger.Warn(message, logArgs...)
	if err := r.cfg.Transport.Emit(types.HarnessEvent{Type: "warning", Message: message}); err != nil {
		r.cfg.Logger.Warn("transport emit failed", "event", "warning", "error", err)
	}
}

// refreshDelay is how long to wait before refreshing a token expiring at
// expiresAt: refreshFraction of the remaining lifetime, or nothing for a
// token that has already expired.
func refreshDelay(now time.Time, expiresAt int64) time.Duration {
	remaining := time.Unix(expiresAt, 0).Sub(now)
	if remaining <= 0 {
		return 0
	}
	return time.Duration(float64(remaining) * refreshFraction)
}
