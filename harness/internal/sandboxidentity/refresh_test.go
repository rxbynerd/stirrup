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
			ExpiresAt: int64Ptr(time.Now().Add(ttl).Unix()),
		}, true
	}
	return &refreshHarness{
		mt:        mt,
		writer:    newRecordingWriter(),
		logs:      &syncBuffer{},
		exchanger: NewExchanger(mt),
	}
}

func tokenFor(n int) string {
	return "secret-jwt-" + strings.Repeat("x", n) + "-token"
}

func (h *refreshHarness) start(t *testing.T, ctx context.Context, initial Result, deadline time.Time) *Refresher {
	t.Helper()
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
	if err != nil {
		t.Fatalf("NewRefresher() error: %v", err)
	}
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
	logs := h.logs.String()
	h.mt.mu.Lock()
	emitted := append([]types.HarnessEvent(nil), h.mt.emitted...)
	h.mt.mu.Unlock()
	for _, tok := range h.writer.all() {
		if strings.Contains(logs, tok) {
			t.Errorf("log output contains a sandbox identity token: %q", logs)
		}
		for _, e := range emitted {
			raw, _ := json.Marshal(e)
			if strings.Contains(string(raw), tok) {
				t.Errorf("emitted event contains a sandbox identity token: %s", raw)
			}
		}
	}
}

func waitFor(t *testing.T, ch <-chan string, timeout time.Duration) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(timeout):
		t.Fatal("timed out waiting for a token delivery")
		return ""
	}
}

// waitForWarning blocks until the refresher has emitted a warning, so a
// test can observe a terminal outcome before Close cancels the schedule.
func (h *refreshHarness) waitForWarning(t *testing.T) types.HarnessEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if warnings := h.emittedOfType("warning"); len(warnings) > 0 {
			return warnings[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for a warning")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRefresher_RefreshesBeforeExpiry drives the load-bearing path: the
// initial token expires shortly, a second sandbox_token_request goes out
// before that expiry, and the sandbox receives the new token.
func TestRefresher_RefreshesBeforeExpiry(t *testing.T) {
	const ttl = 500 * time.Millisecond
	h := newRefreshHarness(t, ttl, nil)

	initial, err := h.exchanger.Exchange(context.Background(), "https://haybale.internal", time.Second)
	if err != nil {
		t.Fatalf("initial Exchange() error: %v", err)
	}
	initialExpiry := time.Unix(*initial.ExpiresAt, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := h.start(t, ctx, initial, time.Time{})
	defer func() { _ = r.Close() }()

	got := waitFor(t, h.writer.written, 2*time.Second)
	refreshedAt := time.Now()

	if got != tokenFor(2) {
		t.Errorf("delivered token = %q, want the second issued token %q", got, tokenFor(2))
	}
	if got == initial.Token {
		t.Error("the refreshed token equals the initial token; the file content must change")
	}
	// Unix-second expiry granularity makes the "before expiry" bound
	// coarse; the refresh must at least precede the expiry second's end.
	if !refreshedAt.Before(initialExpiry.Add(time.Second)) {
		t.Errorf("refresh landed at %s, after the initial expiry %s", refreshedAt, initialExpiry)
	}
	if reqs := h.emittedOfType("sandbox_token_request"); len(reqs) < 2 {
		t.Fatalf("expected at least 2 sandbox_token_requests (initial + refresh), got %d", len(reqs))
	} else if reqs[0].RequestID == reqs[1].RequestID {
		t.Errorf("refresh reused request ID %q; each request must be uniquely correlated", reqs[1].RequestID)
	}
	if warnings := h.emittedOfType("warning"); len(warnings) != 0 {
		t.Errorf("unexpected warning(s) on a successful refresh: %+v", warnings)
	}
	h.assertNoTokenLeak(t)
}

// TestRefresher_CapsRequestsPerRun asserts refreshing stops once
// MaxTokenRequests sandbox_token_requests have been sent, counting the
// initial exchange, and that the stop is reported when the last token
// expires before the run's wall-clock budget.
func TestRefresher_CapsRequestsPerRun(t *testing.T) {
	const ttl = 40 * time.Millisecond
	h := newRefreshHarness(t, ttl, nil)

	initial, err := h.exchanger.Exchange(context.Background(), "https://haybale.internal", time.Second)
	if err != nil {
		t.Fatalf("initial Exchange() error: %v", err)
	}

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
		t.Fatal("Refresher did not stop after exhausting its request budget")
	}

	if got := h.exchanger.Requests(); got != MaxTokenRequests {
		t.Errorf("Requests() = %d, want exactly %d", got, MaxTokenRequests)
	}
	if reqs := h.emittedOfType("sandbox_token_request"); len(reqs) != MaxTokenRequests {
		t.Errorf("emitted %d sandbox_token_requests, want %d", len(reqs), MaxTokenRequests)
	}
	if got := len(h.writer.all()); got != MaxTokenRequests-1 {
		t.Errorf("delivered %d refreshed tokens, want %d", got, MaxTokenRequests-1)
	}
	warnings := h.emittedOfType("warning")
	if len(warnings) != 1 {
		t.Fatalf("expected exactly one warning once the budget is exhausted, got %d: %+v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0].Message, "budget exhausted") {
		t.Errorf("warning message %q should name the exhausted budget", warnings[0].Message)
	}
	h.assertNoTokenLeak(t)
}

// TestRefresher_ExhaustedBudgetSilentWhenTokenOutlivesRun asserts the
// exhausted-budget report is informational, not a warning, when the last
// token already outlives the run's deadline.
func TestRefresher_ExhaustedBudgetSilentWhenTokenOutlivesRun(t *testing.T) {
	h := newRefreshHarness(t, time.Hour, nil)
	// Exhaust the budget up front so the refresher stops on its first pass.
	for i := 0; i < MaxTokenRequests; i++ {
		if _, err := h.exchanger.Exchange(context.Background(), "aud", time.Second); err != nil {
			t.Fatalf("Exchange() %d error: %v", i, err)
		}
	}

	expires := time.Now().Add(time.Hour).Unix()
	r := h.start(t, context.Background(), Result{ExpiresAt: &expires}, time.Now().Add(time.Minute))
	_ = r.Close()

	if warnings := h.emittedOfType("warning"); len(warnings) != 0 {
		t.Errorf("unexpected warning when the token outlives the run: %+v", warnings)
	}
	if !strings.Contains(h.logs.String(), "outlives the run") {
		t.Errorf("expected an informational log line, got: %q", h.logs.String())
	}
}

// TestRefresher_DeclinedRefreshWarns asserts a control-plane decline on a
// refresh is reported as a warning HarnessEvent and a log line, and that
// the schedule stops rather than retrying.
func TestRefresher_DeclinedRefreshWarns(t *testing.T) {
	const ttl = 40 * time.Millisecond
	h := newRefreshHarness(t, ttl, func(n int, requestID string) (types.ControlEvent, bool) {
		if n == 1 {
			return types.ControlEvent{
				Type:      "sandbox_token_response",
				RequestID: requestID,
				Token:     tokenFor(1),
				ExpiresAt: int64Ptr(time.Now().Add(ttl).Unix()),
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
	if err != nil {
		t.Fatalf("initial Exchange() error: %v", err)
	}

	r := h.start(t, context.Background(), initial, time.Time{})
	h.waitForWarning(t)
	if err := r.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}

	warnings := h.emittedOfType("warning")
	if len(warnings) != 1 {
		t.Fatalf("expected exactly one warning for a declined refresh, got %d: %+v", len(warnings), warnings)
	}
	for _, want := range []string{"refresh failed", "declined", "run-scoped issuer revoked", "fail authentication"} {
		if !strings.Contains(warnings[0].Message, want) {
			t.Errorf("warning message %q should contain %q", warnings[0].Message, want)
		}
	}
	if !strings.Contains(h.logs.String(), "refresh failed") {
		t.Errorf("expected a warning log line, got: %q", h.logs.String())
	}
	if got := len(h.writer.all()); got != 0 {
		t.Errorf("a declined refresh must deliver nothing, delivered %d token(s)", got)
	}
	if reqs := h.emittedOfType("sandbox_token_request"); len(reqs) != 2 {
		t.Errorf("expected exactly 2 requests (initial + the declined refresh, no retry), got %d", len(reqs))
	}
	h.assertNoTokenLeak(t)
}

// TestRefresher_DeliveryFailureWarns asserts a token that was issued but
// could not be written into the sandbox is reported the same way as a
// declined exchange, and that its value never reaches the warning.
func TestRefresher_DeliveryFailureWarns(t *testing.T) {
	const ttl = 40 * time.Millisecond
	h := newRefreshHarness(t, ttl, nil)
	h.writer.failErr = errors.New("exec: container not running")

	initial, err := h.exchanger.Exchange(context.Background(), "aud", time.Second)
	if err != nil {
		t.Fatalf("initial Exchange() error: %v", err)
	}

	r := h.start(t, context.Background(), initial, time.Time{})
	h.waitForWarning(t)
	if err := r.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}

	warnings := h.emittedOfType("warning")
	if len(warnings) != 1 {
		t.Fatalf("expected exactly one warning for a failed delivery, got %d: %+v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0].Message, "deliver refreshed token") || !strings.Contains(warnings[0].Message, "container not running") {
		t.Errorf("warning message %q should name the delivery failure", warnings[0].Message)
	}
	if strings.Contains(warnings[0].Message, tokenFor(2)) || strings.Contains(h.logs.String(), tokenFor(2)) {
		t.Error("the undelivered token leaked into the warning or the log")
	}
}

// TestRefresher_NoExpiryNoRefresh asserts a control plane that never sets
// expires_at gets exactly the pre-refresh behaviour: one request, no
// schedule, no warning.
func TestRefresher_NoExpiryNoRefresh(t *testing.T) {
	h := newRefreshHarness(t, 0, func(_ int, requestID string) (types.ControlEvent, bool) {
		return types.ControlEvent{Type: "sandbox_token_response", RequestID: requestID, Token: tokenFor(1)}, true
	})

	initial, err := h.exchanger.Exchange(context.Background(), "aud", time.Second)
	if err != nil {
		t.Fatalf("initial Exchange() error: %v", err)
	}
	if initial.ExpiresAt != nil {
		t.Fatalf("precondition: ExpiresAt must be nil, got %d", *initial.ExpiresAt)
	}

	r := h.start(t, context.Background(), initial, time.Time{})
	time.Sleep(50 * time.Millisecond)
	if err := r.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}

	if reqs := h.emittedOfType("sandbox_token_request"); len(reqs) != 1 {
		t.Errorf("expected exactly 1 sandbox_token_request without an expiry, got %d", len(reqs))
	}
	if warnings := h.emittedOfType("warning"); len(warnings) != 0 {
		t.Errorf("unexpected warning(s) without an expiry: %+v", warnings)
	}
	if !strings.Contains(h.logs.String(), "refresh not scheduled") {
		t.Errorf("expected the no-expiry log line, got: %q", h.logs.String())
	}
}

// TestRefresher_RefreshedTokenWithoutExpiryStopsSchedule asserts that a
// refreshed token reported without expires_at is delivered and then ends
// the schedule, since there is no longer a lifetime to plan against.
func TestRefresher_RefreshedTokenWithoutExpiryStopsSchedule(t *testing.T) {
	const ttl = 40 * time.Millisecond
	h := newRefreshHarness(t, ttl, func(n int, requestID string) (types.ControlEvent, bool) {
		ev := types.ControlEvent{Type: "sandbox_token_response", RequestID: requestID, Token: tokenFor(n)}
		if n == 1 {
			ev.ExpiresAt = int64Ptr(time.Now().Add(ttl).Unix())
		}
		return ev, true
	})

	initial, err := h.exchanger.Exchange(context.Background(), "aud", time.Second)
	if err != nil {
		t.Fatalf("initial Exchange() error: %v", err)
	}

	r := h.start(t, context.Background(), initial, time.Time{})
	if got := waitFor(t, h.writer.written, 2*time.Second); got != tokenFor(2) {
		t.Errorf("delivered %q, want %q", got, tokenFor(2))
	}
	closed := make(chan struct{})
	go func() {
		_ = r.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Refresher did not stop after a refreshed token without expiry")
	}
	if reqs := h.emittedOfType("sandbox_token_request"); len(reqs) != 2 {
		t.Errorf("expected exactly 2 requests, got %d", len(reqs))
	}
	if warnings := h.emittedOfType("warning"); len(warnings) != 0 {
		t.Errorf("unexpected warning(s): %+v", warnings)
	}
}

// TestRefresher_CloseCancelsInFlightExchange asserts Close returns promptly
// while a refresh is waiting on a control plane that never answers, and
// that the abandoned exchange is not reported as a failure.
func TestRefresher_CloseCancelsInFlightExchange(t *testing.T) {
	const ttl = 20 * time.Millisecond
	h := newRefreshHarness(t, ttl, func(n int, requestID string) (types.ControlEvent, bool) {
		if n == 1 {
			return types.ControlEvent{
				Type:      "sandbox_token_response",
				RequestID: requestID,
				Token:     tokenFor(1),
				ExpiresAt: int64Ptr(time.Now().Add(ttl).Unix()),
			}, true
		}
		return types.ControlEvent{}, false
	})

	initial, err := h.exchanger.Exchange(context.Background(), "aud", time.Second)
	if err != nil {
		t.Fatalf("initial Exchange() error: %v", err)
	}

	r, err := NewRefresher(RefresherConfig{
		Exchanger: h.exchanger,
		Writer:    h.writer,
		Transport: h.mt,
		Logger:    slog.New(slog.NewTextHandler(h.logs, nil)),
		Timeout:   time.Minute,
		ExpiresAt: initial.ExpiresAt,
	})
	if err != nil {
		t.Fatalf("NewRefresher() error: %v", err)
	}
	r.Start(context.Background())

	// Wait until the refresh request is in flight.
	deadline := time.Now().Add(2 * time.Second)
	for len(h.emittedOfType("sandbox_token_request")) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("refresh request never went out")
		}
		time.Sleep(5 * time.Millisecond)
	}

	start := time.Now()
	if err := r.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Close() took %s; it must cancel the in-flight exchange rather than wait out its timeout", elapsed)
	}
	if warnings := h.emittedOfType("warning"); len(warnings) != 0 {
		t.Errorf("an exchange abandoned by Close must not warn, got: %+v", warnings)
	}
}

// TestRefresher_CloseWithoutStart pins that Close is safe on a Refresher
// that was never started (the factory's cleanup path on a build failure).
func TestRefresher_CloseWithoutStart(t *testing.T) {
	h := newRefreshHarness(t, time.Minute, nil)
	r, err := NewRefresher(RefresherConfig{Exchanger: h.exchanger, Writer: h.writer, Transport: h.mt})
	if err != nil {
		t.Fatalf("NewRefresher() error: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close() before Start error: %v", err)
	}
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
			if _, err := NewRefresher(cfg); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestRefreshDelay(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	// A fifteen-minute token refreshes around the twelve-minute mark, with
	// jitter of at most 5% either way and always inside the lifetime.
	for i := 0; i < 50; i++ {
		got := refreshDelay(now, now.Add(15*time.Minute).Unix())
		if got < 11*time.Minute+24*time.Second || got > 12*time.Minute+36*time.Second {
			t.Fatalf("refreshDelay(15m) = %s, want within 5%% of 12m", got)
		}
	}
	for _, expiresIn := range []time.Duration{-time.Minute, 0} {
		if got := refreshDelay(now, now.Add(expiresIn).Unix()); got != 0 {
			t.Errorf("refreshDelay(expires in %s) = %s, want an immediate refresh", expiresIn, got)
		}
	}
}

func TestRetryDelay(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	if got, ok := retryDelay(now, now.Add(4*time.Minute).Unix()); !ok || got != 2*time.Minute {
		t.Errorf("retryDelay(4m) = (%s, %v), want (2m, true)", got, ok)
	}
	if _, ok := retryDelay(now, now.Add(-time.Second).Unix()); ok {
		t.Error("retryDelay on an expired token must report no lifetime left")
	}
}

// TestRefresher_RetriesTransientFailure drives the slack the schedule
// reserves: a refresh whose exchange times out is retried within the
// token's remaining lifetime and succeeds without any warning.
func TestRefresher_RetriesTransientFailure(t *testing.T) {
	const ttl = 2 * time.Second
	h := newRefreshHarness(t, ttl, func(n int, requestID string) (types.ControlEvent, bool) {
		if n == 2 {
			return types.ControlEvent{}, false
		}
		return types.ControlEvent{
			Type:      "sandbox_token_response",
			RequestID: requestID,
			Token:     tokenFor(n),
			ExpiresAt: int64Ptr(time.Now().Add(ttl).Unix()),
		}, true
	})

	initial, err := h.exchanger.Exchange(context.Background(), "aud", time.Second)
	if err != nil {
		t.Fatalf("initial Exchange() error: %v", err)
	}

	r, err := NewRefresher(RefresherConfig{
		Exchanger: h.exchanger,
		Writer:    h.writer,
		Transport: h.mt,
		Logger:    slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Timeout:   30 * time.Millisecond,
		ExpiresAt: initial.ExpiresAt,
	})
	if err != nil {
		t.Fatalf("NewRefresher() error: %v", err)
	}
	r.Start(context.Background())
	defer func() { _ = r.Close() }()

	if got := waitFor(t, h.writer.written, 5*time.Second); got != tokenFor(3) {
		t.Errorf("delivered %q, want the token issued on the retried request %q", got, tokenFor(3))
	}
	if reqs := h.emittedOfType("sandbox_token_request"); len(reqs) != 3 {
		t.Errorf("expected 3 requests (initial, timed-out refresh, retry), got %d", len(reqs))
	}
	if warnings := h.emittedOfType("warning"); len(warnings) != 0 {
		t.Errorf("a retried transient failure must not warn, got %+v", warnings)
	}
	if !strings.Contains(h.logs.String(), "retrying within the token's remaining lifetime") {
		t.Errorf("expected a retry log line, got: %q", h.logs.String())
	}
	h.assertNoTokenLeak(t)
}

// TestRefresher_TransientFailureWithNoLifetimeLeftWarns asserts a timeout
// on an already-expired token is not retried into the void: with no
// lifetime left the schedule stops and warns.
func TestRefresher_TransientFailureWithNoLifetimeLeftWarns(t *testing.T) {
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
	if err != nil {
		t.Fatalf("NewRefresher() error: %v", err)
	}
	r.Start(context.Background())
	warning := h.waitForWarning(t)
	_ = r.Close()

	if !strings.Contains(warning.Message, "timed out") {
		t.Errorf("warning %q should name the timeout", warning.Message)
	}
	if reqs := h.emittedOfType("sandbox_token_request"); len(reqs) != 1 {
		t.Errorf("expected exactly 1 request with no lifetime left to retry in, got %d", len(reqs))
	}
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
	prev := closeGrace
	closeGrace = 50 * time.Millisecond
	defer func() { closeGrace = prev }()

	h := newRefreshHarness(t, 40*time.Millisecond, nil)
	initial, err := h.exchanger.Exchange(context.Background(), "aud", time.Second)
	if err != nil {
		t.Fatalf("initial Exchange() error: %v", err)
	}
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
	if err != nil {
		t.Fatalf("NewRefresher() error: %v", err)
	}
	r.Start(context.Background())

	select {
	case <-writer.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh never reached the writer")
	}

	start := time.Now()
	err = r.Close()
	elapsed := time.Since(start)
	if err == nil {
		t.Error("Close() must report that the refresher did not stop in time")
	}
	if elapsed > time.Second {
		t.Errorf("Close() took %s; it must give up after closeGrace", elapsed)
	}
	if !strings.Contains(h.logs.String(), "did not stop within the close grace period") {
		t.Errorf("expected the abandonment log line, got: %q", h.logs.String())
	}
}
