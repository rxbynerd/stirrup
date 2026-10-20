package sandboxidentity

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// recordingWriter is a TokenWriter that records every delivered token and
// can be made to fail.
type recordingWriter struct {
	mu      sync.Mutex
	tokens  []string
	failErr error
	written chan string
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{written: make(chan string, MaxTokenRequests+1)}
}

func (w *recordingWriter) WriteSandboxIdentityToken(_ context.Context, token string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failErr != nil {
		return w.failErr
	}
	w.tokens = append(w.tokens, token)
	w.written <- token
	return nil
}

func (w *recordingWriter) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.tokens...)
}

// syncBuffer is a log sink safe to read while the refresher goroutine is
// still writing to it.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// refreshHarness assembles a Refresher over a mockTransport whose control
// plane answers each request with a token named after its ordinal and an
// expiry ttl after "now". Log output is captured for leak assertions.
type refreshHarness struct {
	mt        *mockTransport
	writer    *recordingWriter
	logs      *syncBuffer
	exchanger *Exchanger
}

func newRefreshHarness(t *testing.T, ttl time.Duration, respond func(n int, requestID string) (types.ControlEvent, bool)) *refreshHarness {
	t.Helper()
	var (
		mu sync.Mutex
		n  int
	)
	mt := &mockTransport{}
	mt.respond = func(requestID string) (types.ControlEvent, bool) {
		mu.Lock()
		n++
		ordinal := n
		mu.Unlock()
		if respond != nil {
			return respond(ordinal, requestID)
		}
		return types.ControlEvent{
			Type:      "sandbox_token_response",
			RequestID: requestID,
			Token:     tokenFor(ordinal),
			ExpiresAt: int64Ptr(unixCeil(time.Now().Add(ttl))),
		}, true
	}
	return &refreshHarness{
		mt:        mt,
		writer:    newRecordingWriter(),
		logs:      &syncBuffer{},
		exchanger: NewExchanger(mt),
	}
}

// unixCeil rounds t up to whole Unix seconds. Truncating would mint a
// token that the refresher already sees as expired whenever t falls in the
// second half of a second, so it would refresh in a tight loop to the
// request budget instead of waiting for the scheduled refresh.
func unixCeil(t time.Time) int64 {
	if t.Nanosecond() == 0 {
		return t.Unix()
	}
	return t.Unix() + 1
}

func tokenFor(n int) string {
	return "secret-jwt-" + strings.Repeat("x", n) + "-token"
}

func (h *refreshHarness) start(t *testing.T, ctx context.Context, initial Result, deadline time.Time) *Refresher {
	t.Helper()
	must := require.New(t)
	r, err := NewRefresher(RefresherConfig{
		Exchanger:      h.exchanger,
		Writer:         h.writer,
		Transport:      h.mt,
		Logger:         slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Audience:       "https://haybale.internal",
		Timeout:        time.Second,
		ExpiresAt:      initial.ExpiresAt,
		BudgetDeadline: deadline,
	})
	must.NoError(err, "NewRefresher()")
	r.Start(ctx)
	return r
}

func (h *refreshHarness) emittedOfType(eventType string) []types.HarnessEvent {
	h.mt.mu.Lock()
	defer h.mt.mu.Unlock()
	var out []types.HarnessEvent
	for _, e := range h.mt.emitted {
		if e.Type == eventType {
			out = append(out, e)
		}
	}
	return out
}

// assertNoTokenLeak fails if any delivered token appears in a log line or
// in any emitted HarnessEvent's JSON form.
func (h *refreshHarness) assertNoTokenLeak(t *testing.T) {
	t.Helper()
	check := assert.New(t)
	logs := h.logs.String()
	h.mt.mu.Lock()
	emitted := append([]types.HarnessEvent(nil), h.mt.emitted...)
	h.mt.mu.Unlock()
	for _, tok := range h.writer.all() {
		check.NotContains(logs, tok, "log output contains a sandbox identity token")
		for _, e := range emitted {
			raw, _ := json.Marshal(e)
			check.NotContains(string(raw), tok, "emitted event contains a sandbox identity token")
		}
	}
}

func waitFor(t *testing.T, ch <-chan string, timeout time.Duration) string {
	t.Helper()
	must := require.New(t)
	select {
	case v := <-ch:
		return v
	case <-time.After(timeout):
		must.FailNow("timed out waiting for a token delivery")
		return ""
	}
}

// waitForWarning blocks until the refresher has emitted a warning, so a
// test can observe a terminal outcome before Close cancels the schedule.
func (h *refreshHarness) waitForWarning(t *testing.T) types.HarnessEvent {
	t.Helper()
	must := require.New(t)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if warnings := h.emittedOfType("warning"); len(warnings) > 0 {
			return warnings[0]
		}
		if time.Now().After(deadline) {
			must.FailNow("timed out waiting for a warning")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRefresher_RefreshesBeforeExpiry drives the load-bearing path: the
// initial token expires shortly, a second sandbox_token_request goes out
// before that expiry, and the sandbox receives the new token.
func TestRefresher_RefreshesBeforeExpiry(t *testing.T) {
	must := require.New(t)
	const ttl = 500 * time.Millisecond
	h := newRefreshHarness(t, ttl, nil)

	initial, err := h.exchanger.Exchange(context.Background(), "https://haybale.internal", time.Second)
	must.NoError(err, "initial Exchange()")
	initialExpiry := time.Unix(*initial.ExpiresAt, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := h.start(t, ctx, initial, time.Time{})
	defer func() { _ = r.Close() }()

	got := waitFor(t, h.writer.written, 2*time.Second)
	refreshedAt := time.Now()

	must.Equal(tokenFor(2), got, "delivered token should be the second issued token")
	must.NotEqual(initial.Token, got, "the sandbox file content must change on refresh")
	// Unix-second expiry granularity makes the "before expiry" bound
	// coarse; the refresh must at least precede the expiry second's end.
	must.True(refreshedAt.Before(initialExpiry.Add(time.Second)),
		"refresh landed at %s, after the initial expiry %s", refreshedAt, initialExpiry)
	reqs := h.emittedOfType("sandbox_token_request")
	must.GreaterOrEqual(len(reqs), 2, "expected the initial exchange and the refresh")
	must.NotEqual(reqs[0].RequestID, reqs[1].RequestID, "each request must be uniquely correlated")
	must.Empty(h.emittedOfType("warning"), "a successful refresh must not warn")
	h.assertNoTokenLeak(t)
}

// TestRefresher_CapsRequestsPerRun asserts refreshing stops once
// MaxTokenRequests sandbox_token_requests have been sent, counting the
// initial exchange, and that the stop is reported when the last token
// expires before the run's wall-clock budget.
func TestRefresher_CapsRequestsPerRun(t *testing.T) {
	must := require.New(t)
	const ttl = 40 * time.Millisecond
	h := newRefreshHarness(t, ttl, nil)

	initial, err := h.exchanger.Exchange(context.Background(), "https://haybale.internal", time.Second)
	must.NoError(err, "initial Exchange()")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := h.start(t, ctx, initial, time.Now().Add(time.Hour))

	for i := 0; i < MaxTokenRequests-1; i++ {
		waitFor(t, h.writer.written, 2*time.Second)
	}
	// Run to completion: with the budget exhausted the goroutine returns
	// on its own, so Close must not need the ctx cancelled to finish.
	closed := make(chan struct{})
	go func() {
		_ = r.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		must.FailNow("Refresher did not stop after exhausting its request budget")
	}

	must.Equal(MaxTokenRequests, h.exchanger.Requests(), "exchanger request count")
	must.Len(h.emittedOfType("sandbox_token_request"), MaxTokenRequests)
	must.Len(h.writer.all(), MaxTokenRequests-1, "refreshed tokens delivered")
	warnings := h.emittedOfType("warning")
	must.Len(warnings, 1, "expected exactly one warning once the budget is exhausted")
	must.Contains(warnings[0].Message, "budget exhausted")
	h.assertNoTokenLeak(t)
}

// TestRefresher_ExhaustedBudgetSilentWhenTokenOutlivesRun asserts the
// exhausted-budget report is informational, not a warning, when the last
// token already outlives the run's deadline.
func TestRefresher_ExhaustedBudgetSilentWhenTokenOutlivesRun(t *testing.T) {
	must := require.New(t)
	h := newRefreshHarness(t, time.Hour, nil)
	// Exhaust the budget up front so the refresher stops on its first pass.
	for i := 0; i < MaxTokenRequests; i++ {
		_, err := h.exchanger.Exchange(context.Background(), "aud", time.Second)
		must.NoError(err, "Exchange() %d", i)
	}

	expires := time.Now().Add(time.Hour).Unix()
	r := h.start(t, context.Background(), Result{ExpiresAt: &expires}, time.Now().Add(time.Minute))
	_ = r.Close()

	must.Empty(h.emittedOfType("warning"), "a token that outlives the run must not warn")
	must.Contains(h.logs.String(), "outlives the run", "expected an informational log line")
}

// TestRefresher_DeclinedRefreshWarns asserts a control-plane decline on a
// refresh is reported as a warning HarnessEvent and a log line, and that
// the schedule stops rather than retrying.
func TestRefresher_DeclinedRefreshWarns(t *testing.T) {
	must := require.New(t)
	const ttl = 40 * time.Millisecond
	h := newRefreshHarness(t, ttl, func(n int, requestID string) (types.ControlEvent, bool) {
		if n == 1 {
			return types.ControlEvent{
				Type:      "sandbox_token_response",
				RequestID: requestID,
				Token:     tokenFor(1),
				ExpiresAt: int64Ptr(unixCeil(time.Now().Add(ttl))),
			}, true
		}
		return types.ControlEvent{
			Type:      "sandbox_token_response",
			RequestID: requestID,
			IsError:   boolPtr(true),
			Reason:    "run-scoped issuer revoked",
		}, true
	})

	initial, err := h.exchanger.Exchange(context.Background(), "aud", time.Second)
	must.NoError(err, "initial Exchange()")

	r := h.start(t, context.Background(), initial, time.Time{})
	h.waitForWarning(t)
	must.NoError(r.Close(), "Close()")

	warnings := h.emittedOfType("warning")
	must.Len(warnings, 1, "a declined refresh warns exactly once")
	for _, want := range []string{"refresh failed", "declined", "run-scoped issuer revoked", "fail authentication"} {
		t.Run(want, func(t *testing.T) {
			must := require.New(t)
			must.Contains(warnings[0].Message, want)
		})
	}
	must.Contains(h.logs.String(), "refresh failed", "expected a warning log line")
	must.Empty(h.writer.all(), "a declined refresh must deliver nothing")
	must.Len(h.emittedOfType("sandbox_token_request"), 2, "initial request plus the declined refresh, no retry")
	h.assertNoTokenLeak(t)
}

// TestRefresher_DeliveryFailureWarns asserts a token that was issued but
// could not be written into the sandbox is reported the same way as a
// declined exchange, and that its value never reaches the warning.
func TestRefresher_DeliveryFailureWarns(t *testing.T) {
	must := require.New(t)
	const ttl = 40 * time.Millisecond
	h := newRefreshHarness(t, ttl, nil)
	h.writer.failErr = errors.New("exec: container not running")

	initial, err := h.exchanger.Exchange(context.Background(), "aud", time.Second)
	must.NoError(err, "initial Exchange()")

	r := h.start(t, context.Background(), initial, time.Time{})
	h.waitForWarning(t)
	must.NoError(r.Close(), "Close()")

	warnings := h.emittedOfType("warning")
	must.Len(warnings, 1, "a failed delivery warns exactly once")
	must.Contains(warnings[0].Message, "deliver refreshed token")
	must.Contains(warnings[0].Message, "container not running")
	must.NotContains(warnings[0].Message, tokenFor(2), "the undelivered token must not reach the warning")
	must.NotContains(h.logs.String(), tokenFor(2), "the undelivered token must not reach the log")
}

// TestRefresher_NoExpiryNoRefresh asserts a control plane that never sets
// expires_at gets exactly the pre-refresh behaviour: one request, no
// schedule, no warning.
func TestRefresher_NoExpiryNoRefresh(t *testing.T) {
	must := require.New(t)
	h := newRefreshHarness(t, 0, func(_ int, requestID string) (types.ControlEvent, bool) {
		return types.ControlEvent{Type: "sandbox_token_response", RequestID: requestID, Token: tokenFor(1)}, true
	})

	initial, err := h.exchanger.Exchange(context.Background(), "aud", time.Second)
	must.NoError(err, "initial Exchange()")
	must.Nil(initial.ExpiresAt, "precondition: ExpiresAt must be nil")

	r := h.start(t, context.Background(), initial, time.Time{})
	time.Sleep(50 * time.Millisecond)
	must.NoError(r.Close(), "Close()")

	must.Len(h.emittedOfType("sandbox_token_request"), 1, "one request without an expiry")
	must.Empty(h.emittedOfType("warning"), "no warning without an expiry")
	must.Contains(h.logs.String(), "refresh not scheduled", "expected the no-expiry log line")
}

// TestRefresher_RefreshedTokenWithoutExpiryStopsSchedule asserts that a
// refreshed token reported without expires_at is delivered and then ends
// the schedule, since there is no longer a lifetime to plan against.
func TestRefresher_RefreshedTokenWithoutExpiryStopsSchedule(t *testing.T) {
	must := require.New(t)
	const ttl = 40 * time.Millisecond
	h := newRefreshHarness(t, ttl, func(n int, requestID string) (types.ControlEvent, bool) {
		ev := types.ControlEvent{Type: "sandbox_token_response", RequestID: requestID, Token: tokenFor(n)}
		if n == 1 {
			ev.ExpiresAt = int64Ptr(time.Now().Add(ttl).Unix())
		}
		return ev, true
	})

	initial, err := h.exchanger.Exchange(context.Background(), "aud", time.Second)
	must.NoError(err, "initial Exchange()")

	r := h.start(t, context.Background(), initial, time.Time{})
	got := waitFor(t, h.writer.written, 2*time.Second)
	must.Equal(tokenFor(2), got, "delivered token")
	closed := make(chan struct{})
	go func() {
		_ = r.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		must.FailNow("Refresher did not stop after a refreshed token without expiry")
	}
	must.Len(h.emittedOfType("sandbox_token_request"), 2)
	must.Empty(h.emittedOfType("warning"), "a refreshed token without expiry must not warn")
}

// TestRefresher_CloseCancelsInFlightExchange asserts Close returns promptly
// while a refresh is waiting on a control plane that never answers, and
// that the abandoned exchange is not reported as a failure.
func TestRefresher_CloseCancelsInFlightExchange(t *testing.T) {
	must := require.New(t)
	const ttl = 20 * time.Millisecond
	h := newRefreshHarness(t, ttl, func(n int, requestID string) (types.ControlEvent, bool) {
		if n == 1 {
			return types.ControlEvent{
				Type:      "sandbox_token_response",
				RequestID: requestID,
				Token:     tokenFor(1),
				ExpiresAt: int64Ptr(unixCeil(time.Now().Add(ttl))),
			}, true
		}
		return types.ControlEvent{}, false
	})

	initial, err := h.exchanger.Exchange(context.Background(), "aud", time.Second)
	must.NoError(err, "initial Exchange()")

	r, err := NewRefresher(RefresherConfig{
		Exchanger: h.exchanger,
		Writer:    h.writer,
		Transport: h.mt,
		Logger:    slog.New(slog.NewTextHandler(h.logs, nil)),
		Timeout:   time.Minute,
		ExpiresAt: initial.ExpiresAt,
	})
	must.NoError(err, "NewRefresher()")
	r.Start(context.Background())

	// Wait until the refresh request is in flight.
	deadline := time.Now().Add(2 * time.Second)
	for len(h.emittedOfType("sandbox_token_request")) < 2 {
		if time.Now().After(deadline) {
			must.FailNow("refresh request never went out")
		}
		time.Sleep(5 * time.Millisecond)
	}

	start := time.Now()
	must.NoError(r.Close(), "Close()")
	must.LessOrEqual(time.Since(start), time.Second,
		"Close must cancel the in-flight exchange rather than wait out its timeout")
	must.Empty(h.emittedOfType("warning"), "an exchange abandoned by Close must not warn")
}

// TestRefresher_CloseWithoutStart pins that Close is safe on a Refresher
// that was never started (the factory's cleanup path on a build failure).
func TestRefresher_CloseWithoutStart(t *testing.T) {
	must := require.New(t)
	h := newRefreshHarness(t, time.Minute, nil)
	r, err := NewRefresher(RefresherConfig{Exchanger: h.exchanger, Writer: h.writer, Transport: h.mt})
	must.NoError(err, "NewRefresher()")
	must.NoError(r.Close(), "Close() before Start")
}

func TestNewRefresher_RequiresDependencies(t *testing.T) {
	h := newRefreshHarness(t, time.Minute, nil)
	cases := map[string]RefresherConfig{
		"no exchanger": {Writer: h.writer, Transport: h.mt},
		"no writer":    {Exchanger: h.exchanger, Transport: h.mt},
		"no transport": {Exchanger: h.exchanger, Writer: h.writer},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			_, err := NewRefresher(cfg)
			must.Error(err)
		})
	}
}

func TestRefreshDelay_JitteredFractionOfLifetimeOrImmediate(t *testing.T) {
	must := require.New(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	// A fifteen-minute token refreshes around the twelve-minute mark, with
	// jitter of at most 5% either way and always inside the lifetime.
	for i := 0; i < 50; i++ {
		got := refreshDelay(now, now.Add(15*time.Minute).Unix())
		must.GreaterOrEqual(got, 11*time.Minute+24*time.Second, "refreshDelay(15m) should be within 5% of 12m")
		must.LessOrEqual(got, 12*time.Minute+36*time.Second, "refreshDelay(15m) should be within 5% of 12m")
	}

	cases := []struct {
		name      string
		expiresIn time.Duration
	}{
		{name: "expired a minute ago", expiresIn: -time.Minute},
		{name: "expires now", expiresIn: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Zero(refreshDelay(now, now.Add(tc.expiresIn).Unix()), "an expired token should refresh immediately")
		})
	}
}

func TestRetryDelay_HalfOfRemainingLifetimeOrNone(t *testing.T) {
	must := require.New(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	got, ok := retryDelay(now, now.Add(4*time.Minute).Unix())
	must.True(ok, "retryDelay(4m) should report lifetime left")
	must.Equal(2*time.Minute, got)

	_, ok = retryDelay(now, now.Add(-time.Second).Unix())
	must.False(ok, "retryDelay on an expired token must report no lifetime left")
}

// TestRefresher_RetriesTransientFailure drives the slack the schedule
// reserves: a refresh whose exchange times out is retried within the
// token's remaining lifetime and succeeds without any warning.
func TestRefresher_RetriesTransientFailure(t *testing.T) {
	must := require.New(t)
	const ttl = 2 * time.Second
	h := newRefreshHarness(t, ttl, func(n int, requestID string) (types.ControlEvent, bool) {
		if n == 2 {
			return types.ControlEvent{}, false
		}
		return types.ControlEvent{
			Type:      "sandbox_token_response",
			RequestID: requestID,
			Token:     tokenFor(n),
			ExpiresAt: int64Ptr(unixCeil(time.Now().Add(ttl))),
		}, true
	})

	initial, err := h.exchanger.Exchange(context.Background(), "aud", time.Second)
	must.NoError(err, "initial Exchange()")

	r, err := NewRefresher(RefresherConfig{
		Exchanger: h.exchanger,
		Writer:    h.writer,
		Transport: h.mt,
		Logger:    slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Timeout:   30 * time.Millisecond,
		ExpiresAt: initial.ExpiresAt,
	})
	must.NoError(err, "NewRefresher()")
	r.Start(context.Background())
	defer func() { _ = r.Close() }()

	must.Equal(tokenFor(3), waitFor(t, h.writer.written, 5*time.Second), "token issued on the retried request")
	must.Len(h.emittedOfType("sandbox_token_request"), 3, "initial, timed-out refresh, and retry")
	must.Empty(h.emittedOfType("warning"), "a retried transient failure must not warn")
	must.Contains(h.logs.String(), "retrying within the token's remaining lifetime", "expected a retry log line")
	h.assertNoTokenLeak(t)
}

// TestRefresher_TransientFailureWithNoLifetimeLeftWarns asserts a timeout
// on an already-expired token is not retried into the void: with no
// lifetime left the schedule stops and warns.
func TestRefresher_TransientFailureWithNoLifetimeLeftWarns(t *testing.T) {
	must := require.New(t)
	h := newRefreshHarness(t, 0, func(_ int, _ string) (types.ControlEvent, bool) {
		return types.ControlEvent{}, false
	})
	expired := time.Now().Add(-time.Minute).Unix()

	r, err := NewRefresher(RefresherConfig{
		Exchanger: h.exchanger,
		Writer:    h.writer,
		Transport: h.mt,
		Logger:    slog.New(slog.NewTextHandler(h.logs, nil)),
		Timeout:   20 * time.Millisecond,
		ExpiresAt: &expired,
	})
	must.NoError(err, "NewRefresher()")
	r.Start(context.Background())
	warning := h.waitForWarning(t)
	_ = r.Close()

	must.Contains(warning.Message, "timed out", "the warning should name the timeout")
	must.Len(h.emittedOfType("sandbox_token_request"), 1, "no lifetime left to retry in")
}

// blockingWriter ignores ctx and blocks until released, standing in for a
// wedged engine write.
type blockingWriter struct {
	enteredOnce sync.Once
	entered     chan struct{}
	release     chan struct{}
}

func (w *blockingWriter) WriteSandboxIdentityToken(context.Context, string) error {
	w.enteredOnce.Do(func() { close(w.entered) })
	<-w.release
	return nil
}

// TestRefresher_CloseIsBounded asserts Close gives up on a delivery that
// ignores cancellation once closeGrace elapses, so a wedged engine cannot
// hold the loop's shutdown.
func TestRefresher_CloseIsBounded(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	prev := closeGrace
	closeGrace = 50 * time.Millisecond
	defer func() { closeGrace = prev }()

	h := newRefreshHarness(t, 40*time.Millisecond, nil)
	initial, err := h.exchanger.Exchange(context.Background(), "aud", time.Second)
	must.NoError(err, "initial exchange")
	writer := &blockingWriter{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(writer.release)

	r, err := NewRefresher(RefresherConfig{
		Exchanger: h.exchanger,
		Writer:    writer,
		Transport: h.mt,
		Logger:    slog.New(slog.NewTextHandler(h.logs, nil)),
		Timeout:   time.Second,
		ExpiresAt: initial.ExpiresAt,
	})
	must.NoError(err, "NewRefresher")
	r.Start(context.Background())

	select {
	case <-writer.entered:
	case <-time.After(2 * time.Second):
		must.Fail("refresh never reached the writer")
	}

	start := time.Now()
	err = r.Close()
	elapsed := time.Since(start)
	must.Error(err, "Close() must report that the refresher did not stop in time")
	check.LessOrEqual(elapsed, time.Second, "Close() must give up after closeGrace")
	check.Contains(h.logs.String(), "did not stop within the close grace period",
		"expected the abandonment log line")
}
