package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
)

// --- unit tests ---

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		headers    http.Header
		want       time.Duration
		wantSource string
	}{
		{
			name:       "retry-after-ms integer",
			headers:    http.Header{"Retry-After-Ms": []string{"1500"}},
			want:       1500 * time.Millisecond,
			wantSource: delaySourceRetryAfterMs,
		},
		{
			name:       "retry-after integer seconds",
			headers:    http.Header{"Retry-After": []string{"2"}},
			want:       2 * time.Second,
			wantSource: delaySourceRetryAfter,
		},
		{
			name:       "retry-after http-date",
			headers:    http.Header{"Retry-After": []string{now.Add(5 * time.Second).Format(http.TimeFormat)}},
			want:       5 * time.Second,
			wantSource: delaySourceRetryAfter,
		},
		{
			name: "retry-after-ms wins over retry-after",
			headers: http.Header{
				"Retry-After-Ms": []string{"500"},
				"Retry-After":    []string{"10"},
			},
			want:       500 * time.Millisecond,
			wantSource: delaySourceRetryAfterMs,
		},
		{
			name:       "negative retry-after returns zero",
			headers:    http.Header{"Retry-After": []string{"-1"}},
			want:       0,
			wantSource: "",
		},
		{
			name:       "garbage retry-after returns zero",
			headers:    http.Header{"Retry-After": []string{"not-a-number"}},
			want:       0,
			wantSource: "",
		},
		{
			name:       "negative retry-after-ms returns zero",
			headers:    http.Header{"Retry-After-Ms": []string{"-100"}},
			want:       0,
			wantSource: "",
		},
		{
			name:       "no headers returns zero",
			headers:    http.Header{},
			want:       0,
			wantSource: "",
		},
		{
			// Per parseRetryAfter contract: Retry-After-Ms: 0 is
			// treated as "ignore this hint and fall through" rather
			// than "retry immediately", so the source is the
			// downstream Retry-After header.
			name: "zero retry-after-ms falls through to retry-after",
			headers: http.Header{
				"Retry-After-Ms": []string{"0"},
				"Retry-After":    []string{"5"},
			},
			want:       5 * time.Second,
			wantSource: delaySourceRetryAfter,
		},
		{
			name: "negative retry-after-ms falls through to retry-after",
			headers: http.Header{
				"Retry-After-Ms": []string{"-1"},
				"Retry-After":    []string{"5"},
			},
			want:       5 * time.Second,
			wantSource: delaySourceRetryAfter,
		},
		{
			// At ceiling: 60000 ms == maxRetryAfterHint exactly.
			name:       "retry-after-ms at ceiling returns 60s",
			headers:    http.Header{"Retry-After-Ms": []string{"60000"}},
			want:       60 * time.Second,
			wantSource: delaySourceRetryAfterMs,
		},
		{
			// Just above ceiling: falls through to Retry-After
			// (which is empty here, so returns zero).
			name:       "retry-after-ms above ceiling falls through",
			headers:    http.Header{"Retry-After-Ms": []string{"60001"}},
			want:       0,
			wantSource: "",
		},
		{
			// Just above ceiling with Retry-After fallback present.
			name: "retry-after-ms above ceiling falls through to retry-after",
			headers: http.Header{
				"Retry-After-Ms": []string{"60001"},
				"Retry-After":    []string{"5"},
			},
			want:       5 * time.Second,
			wantSource: delaySourceRetryAfter,
		},
		{
			// Near-overflow: 9223372036954776 ms * 1e6 (ns/ms)
			// would wrap int64. Cap rejects the value first; no
			// wrap occurs.
			name:       "retry-after-ms near int64 overflow returns zero",
			headers:    http.Header{"Retry-After-Ms": []string{"9223372036954776"}},
			want:       0,
			wantSource: "",
		},
		{
			// Retry-After seconds above ceiling: rejected.
			name:       "retry-after seconds above ceiling returns zero",
			headers:    http.Header{"Retry-After": []string{"3600"}},
			want:       0,
			wantSource: "",
		},
		{
			name:       "retry-after seconds at ceiling returns 60s",
			headers:    http.Header{"Retry-After": []string{"60"}},
			want:       60 * time.Second,
			wantSource: delaySourceRetryAfter,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, source := parseRetryAfter(tt.headers, now)
			if got != tt.want {
				t.Errorf("parseRetryAfter() duration = %v, want %v", got, tt.want)
			}
			if source != tt.wantSource {
				t.Errorf("parseRetryAfter() source = %q, want %q", source, tt.wantSource)
			}
		})
	}
}

func TestRetryableStatus(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{200, false},
		{201, false},
		{301, false},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{408, true},
		{409, true},
		{410, false},
		{422, false},
		{429, true},
		{500, true},
		{501, false},
		{502, true},
		{503, true},
		{504, true},
		{505, false},
	}
	for _, tt := range tests {
		got := retryableStatus(tt.status)
		if got != tt.want {
			t.Errorf("retryableStatus(%d) = %v, want %v", tt.status, got, tt.want)
		}
	}
}

func TestBackoffDelay_Distribution(t *testing.T) {
	policy := RetryPolicy{
		InitialDelay: 100 * time.Millisecond,
		MaxDelay:     5 * time.Second,
	}
	prng := rand.New(rand.NewPCG(1, 2))

	for n := 0; n < 8; n++ {
		expectedCap := policy.InitialDelay << n
		if expectedCap > policy.MaxDelay || expectedCap <= 0 {
			expectedCap = policy.MaxDelay
		}
		for i := 0; i < 1000; i++ {
			got := backoffDelay(n, policy, prng)
			if got < 0 || got >= expectedCap {
				t.Fatalf("attempt %d sample %d: delay %v out of [0, %v)", n, i, got, expectedCap)
			}
		}
	}
}

// --- integration tests ---

// newTestMetrics returns a Metrics instance backed by a ManualReader so
// assertions can inspect the recorded counter increments.
func newTestMetrics(t *testing.T) (*observability.Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m, err := observability.NewMetricsForTesting(provider)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	return m, reader
}

// retryOutcomeFromMetrics extracts the recorded provider_retries counter
// data points keyed by outcome.
func retryOutcomeFromMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := make(map[string]int64)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "stirrup.harness.provider_retries" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				outcome, _ := dp.Attributes.Value("provider.retry.outcome")
				out[outcome.AsString()] += dp.Value
			}
		}
	}
	return out
}

func newPostReq(t *testing.T, url, body string) *http.Request {
	t.Helper()
	bodyBytes := []byte(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(bodyBytes)), nil
	}
	return req
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func defaultTestPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:     3,
		InitialDelay:    5 * time.Millisecond,
		MaxDelay:        50 * time.Millisecond,
		WallClockBudget: 5 * time.Second,
	}
}

// withRecordingSpan returns a context carrying an OTel span backed by an
// in-memory exporter so tests can assert on span events.
func withRecordingSpan(t *testing.T) (context.Context, *tracetest.InMemoryExporter, oteltrace.Span) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)
	tracer := tp.Tracer("retry-test")
	ctx, span := tracer.Start(context.Background(), "test-span")
	return ctx, exporter, span
}

func TestDoWithRetry_429ThenSuccess(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, reader := newTestMetrics(t)
	ctx, exporter, span := withRecordingSpan(t)

	req := newPostReq(t, srv.URL, `{"x":1}`)
	resp, err := DoWithRetry(ctx, &http.Client{Timeout: 5 * time.Second}, req,
		defaultTestPolicy(), discardLogger(), m, "openai", "gpt-test")
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("attempts: got %d, want 2", atomic.LoadInt32(&attempts))
	}

	span.End()
	events := collectSpanEvents(exporter, "provider_retry_attempt")
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 provider_retry_attempt event, got %d", len(events))
	}

	outcomes := retryOutcomeFromMetrics(t, reader)
	if outcomes[retryOutcomeSucceeded] != 1 {
		t.Errorf("succeeded counter: got %d, want 1", outcomes[retryOutcomeSucceeded])
	}
}

func TestDoWithRetry_RetryAfterSecondsHonoured(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := newTestMetrics(t)

	policy := defaultTestPolicy()
	policy.MaxDelay = 2 * time.Second

	req := newPostReq(t, srv.URL, `{}`)
	start := time.Now()
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		policy, discardLogger(), m, "openai", "gpt-test")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if elapsed < 800*time.Millisecond || elapsed > 1200*time.Millisecond {
		t.Errorf("elapsed = %v, want ~1s ±200ms", elapsed)
	}
}

func TestDoWithRetry_RetryAfterMsHonoured(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.Header().Set("Retry-After-Ms", "250")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := newTestMetrics(t)

	policy := defaultTestPolicy()
	policy.MaxDelay = 1 * time.Second

	req := newPostReq(t, srv.URL, `{}`)
	start := time.Now()
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		policy, discardLogger(), m, "openai", "gpt-test")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if elapsed < 150*time.Millisecond || elapsed > 450*time.Millisecond {
		t.Errorf("elapsed = %v, want ~250ms ±100ms", elapsed)
	}
}

func TestDoWithRetry_Exhausted(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	m, reader := newTestMetrics(t)

	policy := RetryPolicy{
		MaxAttempts:     3,
		InitialDelay:    1 * time.Millisecond,
		MaxDelay:        5 * time.Millisecond,
		WallClockBudget: 5 * time.Second,
	}

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		policy, discardLogger(), m, "openai", "gpt-test")
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status: got %d, want 429", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts: got %d, want 3", got)
	}

	outcomes := retryOutcomeFromMetrics(t, reader)
	if outcomes[retryOutcomeExhausted] != 1 {
		t.Errorf("exhausted counter: got %d, want 1", outcomes[retryOutcomeExhausted])
	}
}

func TestDoWithRetry_BudgetExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Force a large server-side hint so the first sleep would
		// overshoot the budget.
		w.Header().Set("Retry-After-Ms", "5000")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	m, reader := newTestMetrics(t)

	policy := RetryPolicy{
		MaxAttempts:     5,
		InitialDelay:    10 * time.Millisecond,
		MaxDelay:        10 * time.Second,
		WallClockBudget: 50 * time.Millisecond,
	}

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		policy, discardLogger(), m, "openai", "gpt-test")
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status: got %d, want 429", resp.StatusCode)
	}

	outcomes := retryOutcomeFromMetrics(t, reader)
	if outcomes[retryOutcomeBudgetExhausted] != 1 {
		t.Errorf("budget_exhausted counter: got %d, want 1 (all: %+v)", outcomes[retryOutcomeBudgetExhausted], outcomes)
	}
}

func TestDoWithRetry_NonRetryable(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	m, reader := newTestMetrics(t)

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		defaultTestPolicy(), discardLogger(), m, "openai", "gpt-test")
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts: got %d, want 1", got)
	}

	outcomes := retryOutcomeFromMetrics(t, reader)
	if outcomes[retryOutcomeNonRetryable] != 1 {
		t.Errorf("non_retryable counter: got %d, want 1", outcomes[retryOutcomeNonRetryable])
	}
}

func TestDoWithRetry_ContextCancelDuringSleep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After-Ms", "5000")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	m, reader := newTestMetrics(t)

	policy := RetryPolicy{
		MaxAttempts:     5,
		InitialDelay:    10 * time.Millisecond,
		MaxDelay:        10 * time.Second,
		WallClockBudget: 10 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	req := newPostReq(t, srv.URL, `{}`)
	start := time.Now()
	resp, err := DoWithRetry(ctx, &http.Client{Timeout: 5 * time.Second}, req,
		policy, discardLogger(), m, "openai", "gpt-test")
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err: got %v, want context.Canceled", err)
	}
	if resp != nil {
		t.Errorf("expected nil resp on cancel, got %v", resp)
		_ = resp.Body.Close()
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("cancel took %v, want <200ms", elapsed)
	}

	outcomes := retryOutcomeFromMetrics(t, reader)
	if outcomes[retryOutcomeContextDone] != 1 {
		t.Errorf("context_done counter: got %d, want 1", outcomes[retryOutcomeContextDone])
	}
}

func TestDoWithRetry_BodyRewoundOnRetry(t *testing.T) {
	var (
		mu     sync.Mutex
		bodies []string
		count  int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(buf))
		mu.Unlock()
		n := atomic.AddInt32(&count, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := newTestMetrics(t)

	bodyContent := `{"prompt":"hello world"}`
	req := newPostReq(t, srv.URL, bodyContent)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		defaultTestPolicy(), discardLogger(), m, "openai", "gpt-test")
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("expected 2 bodies recorded, got %d", len(bodies))
	}
	if bodies[0] != bodyContent || bodies[1] != bodyContent {
		t.Errorf("bodies differ: %q vs %q (expected both %q)", bodies[0], bodies[1], bodyContent)
	}
}

func TestDoWithRetry_PanicsWhenGetBodyMissing(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic, got none")
		}
		msg := fmt.Sprint(r)
		if !strings.Contains(msg, "DoWithRetry: req.GetBody must be set") {
			t.Errorf("panic message %q does not contain expected substring", msg)
		}
	}()

	req, err := http.NewRequest(http.MethodPost, "http://example.invalid", strings.NewReader("body"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	// http.NewRequest with strings.NewReader sets GetBody automatically;
	// clear it to exercise the contract check.
	req.GetBody = nil

	_, _ = DoWithRetry(context.Background(), &http.Client{}, req,
		defaultTestPolicy(), discardLogger(), nil, "openai", "gpt-test")
}

// collectSpanEvents extracts events with the given name across all spans
// captured by the in-memory exporter.
func collectSpanEvents(exporter *tracetest.InMemoryExporter, name string) []sdktrace.Event {
	var out []sdktrace.Event
	for _, sp := range exporter.GetSpans() {
		for _, ev := range sp.Events {
			if ev.Name == name {
				out = append(out, ev)
			}
		}
	}
	return out
}
