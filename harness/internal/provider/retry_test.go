package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
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
	"github.com/rxbynerd/stirrup/types"
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

// retryOutcomeFromMetrics extracts the recorded provider_retry_outcomes
// counter data points keyed by outcome.
func retryOutcomeFromMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := make(map[string]int64)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "stirrup.harness.provider_retry_outcomes" {
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

// testOpts builds a RetryOptions struct from the most common test
// inputs. Keeps the per-test call sites short.
func testOpts(policy RetryPolicy, m *observability.Metrics) RetryOptions {
	return RetryOptions{
		Policy:       policy,
		Logger:       discardLogger(),
		Metrics:      m,
		ProviderType: "openai",
		Model:        "gpt-test",
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
		testOpts(defaultTestPolicy(), m))
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
		testOpts(policy, m))
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
		testOpts(policy, m))
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
		testOpts(policy, m))
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
		testOpts(policy, m))
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
		testOpts(defaultTestPolicy(), m))
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
		testOpts(policy, m))
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
		testOpts(defaultTestPolicy(), m))
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
		testOpts(defaultTestPolicy(), nil))
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

// --- B3: transientErr + transport error coverage ---

// timeoutOpError synthesises a net.OpError whose underlying error
// reports Timeout()==true. The retry helper classifies transport
// errors via errors.As(*net.Error) + Timeout(); this is the
// shortest path to a value that satisfies that contract.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestTransientErr(t *testing.T) {
	t.Run("nil error is not transient", func(t *testing.T) {
		if transientErr(nil, 0) {
			t.Fatal("transientErr(nil, 0) = true, want false")
		}
	})
	t.Run("net.OpError with timeout is transient", func(t *testing.T) {
		opErr := &net.OpError{Op: "dial", Net: "tcp", Err: timeoutError{}}
		if !transientErr(opErr, 0) {
			t.Fatal("transientErr(*net.OpError{Timeout=true}, 0) = false, want true")
		}
		// Also retryable on subsequent attempts.
		if !transientErr(opErr, 1) {
			t.Fatal("transientErr(*net.OpError{Timeout=true}, 1) = false, want true")
		}
	})
	t.Run("io.EOF on first attempt is transient", func(t *testing.T) {
		if !transientErr(io.EOF, 0) {
			t.Fatal("transientErr(io.EOF, 0) = false, want true")
		}
	})
	t.Run("io.EOF on second attempt is not transient", func(t *testing.T) {
		// Guards against regressing the "io.EOF only on first attempt"
		// rule. A repeat EOF likely indicates a server-side condition
		// the next attempt would also hit, not a stale keepalive.
		if transientErr(io.EOF, 1) {
			t.Fatal("transientErr(io.EOF, 1) = true, want false")
		}
	})
	t.Run("plain error is not transient", func(t *testing.T) {
		if transientErr(errors.New("connection reset"), 0) {
			t.Fatal("transientErr(plain, 0) = true, want false")
		}
	})
}

// hijackAndCloseHandler returns an http.HandlerFunc that hijacks the
// connection and closes it immediately, simulating a stale-keepalive
// EOF on the client. If alwaysClose is false, the handler closes only
// on the first request and answers 200 OK on every subsequent one.
func hijackAndCloseHandler(t *testing.T, alwaysClose bool, count *int32) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(count, 1)
		if alwaysClose || n == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("ResponseWriter does not support Hijacker")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("Hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func TestDoWithRetry_TransientNetworkError(t *testing.T) {
	var count int32
	srv := httptest.NewServer(hijackAndCloseHandler(t, false, &count))
	defer srv.Close()

	m, reader := newTestMetrics(t)

	// http.Client with no keep-alive disabled — we want connection
	// re-use so the first request sees the close-after-hijack and the
	// second attempt opens a fresh connection.
	client := &http.Client{Timeout: 5 * time.Second}

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(context.Background(), client, req, testOpts(defaultTestPolicy(), m))
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&count); got != 2 {
		t.Fatalf("server hit count: got %d, want 2", got)
	}

	outcomes := retryOutcomeFromMetrics(t, reader)
	if outcomes[retryOutcomeSucceeded] != 1 {
		t.Errorf("succeeded counter: got %d, want 1 (all: %+v)", outcomes[retryOutcomeSucceeded], outcomes)
	}
}

func TestDoWithRetry_TransientNetworkError_RecordsSpanAttr(t *testing.T) {
	// Same hijack-and-close pattern as TestDoWithRetry_TransientNetworkError
	// but with a recording span attached so the span-error-attribute
	// branch in the retry log/span block is exercised.
	var count int32
	srv := httptest.NewServer(hijackAndCloseHandler(t, false, &count))
	defer srv.Close()

	m, _ := newTestMetrics(t)
	ctx, exporter, span := withRecordingSpan(t)

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(ctx, &http.Client{Timeout: 5 * time.Second}, req, testOpts(defaultTestPolicy(), m))
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	span.End()
	events := collectSpanEvents(exporter, "provider_retry_attempt")
	if len(events) != 1 {
		t.Fatalf("expected 1 provider_retry_attempt event, got %d", len(events))
	}
	// The event must carry the unwrapped error string. We don't
	// assert on its exact content (varies by platform: "EOF",
	// "connection reset by peer") but it must exist on the
	// transport-error path.
	var foundErrAttr bool
	for _, kv := range events[0].Attributes {
		if string(kv.Key) == "error" {
			foundErrAttr = true
			if strings.Contains(kv.Value.AsString(), srv.URL) {
				t.Errorf("span error attribute leaks URL: %q", kv.Value.AsString())
			}
		}
	}
	if !foundErrAttr {
		t.Errorf("provider_retry_attempt event missing error attribute")
	}
}

func TestDoWithRetry_PersistentEOF(t *testing.T) {
	var count int32
	srv := httptest.NewServer(hijackAndCloseHandler(t, true, &count))
	defer srv.Close()

	m, reader := newTestMetrics(t)

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(defaultTestPolicy(), m))
	if err == nil {
		// Drain and close even on unexpected success so the server's
		// finished its work.
		_ = resp.Body.Close()
		t.Fatal("expected non-nil error from persistent EOF, got nil")
	}

	// Exactly two attempts: first sees EOF (retry), second sees EOF
	// with attempt>0 so transientErr returns false, helper surfaces
	// the error as non-retryable.
	if got := atomic.LoadInt32(&count); got != 2 {
		t.Fatalf("server hit count: got %d, want 2", got)
	}

	outcomes := retryOutcomeFromMetrics(t, reader)
	if outcomes[retryOutcomeNonRetryable] != 1 {
		t.Errorf("non_retryable counter: got %d, want 1 (all: %+v)", outcomes[retryOutcomeNonRetryable], outcomes)
	}
}

// --- B4: RetryPolicyFromConfig ---

func TestRetryPolicyFromConfig(t *testing.T) {
	t.Run("nil cfg returns zero policy", func(t *testing.T) {
		got := RetryPolicyFromConfig(nil)
		if got != (RetryPolicy{}) {
			t.Errorf("RetryPolicyFromConfig(nil) = %+v, want zero RetryPolicy", got)
		}
	})
	t.Run("populated cfg converts ms fields to durations", func(t *testing.T) {
		cfg := &types.ProviderRetryConfig{
			MaxAttempts:       3,
			InitialDelayMs:    200,
			MaxDelayMs:        5000,
			WallClockBudgetMs: 30000,
		}
		got := RetryPolicyFromConfig(cfg)
		want := RetryPolicy{
			MaxAttempts:     3,
			InitialDelay:    200 * time.Millisecond,
			MaxDelay:        5 * time.Second,
			WallClockBudget: 30 * time.Second,
		}
		if got != want {
			t.Errorf("RetryPolicyFromConfig() = %+v, want %+v", got, want)
		}
	})
}

// TestRetryPolicyFromConfig_ZeroInitialDelay asserts that a caller who
// constructs a ProviderRetryConfig with InitialDelayMs=0 and bypasses
// ValidateRunConfig still gets the safe canonical default rather than a
// zero InitialDelay (which would produce a tight retry loop when
// backoffDelay returns zero on every attempt).
func TestRetryPolicyFromConfig_ZeroInitialDelay(t *testing.T) {
	cfg := &types.ProviderRetryConfig{
		MaxAttempts:       3,
		InitialDelayMs:    0,
		MaxDelayMs:        5000,
		WallClockBudgetMs: 30000,
	}
	got := RetryPolicyFromConfig(cfg)
	if got.InitialDelay != 500*time.Millisecond {
		t.Errorf("RetryPolicyFromConfig with zero InitialDelayMs: InitialDelay = %v, want 500ms (defence-in-depth fallback)", got.InitialDelay)
	}
	// Other fields should still convert normally.
	if got.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", got.MaxAttempts)
	}
	if got.MaxDelay != 5*time.Second {
		t.Errorf("MaxDelay = %v, want 5s", got.MaxDelay)
	}
	if got.WallClockBudget != 30*time.Second {
		t.Errorf("WallClockBudget = %v, want 30s", got.WallClockBudget)
	}
}

// --- M4: backoffDelay zero-guard paths ---

func TestBackoffDelay_ZeroInitialDelay(t *testing.T) {
	policy := RetryPolicy{
		InitialDelay: 0,
		MaxDelay:     1 * time.Second,
	}
	prng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 100; i++ {
		got := backoffDelay(i%5, policy, prng)
		if got != 0 {
			t.Fatalf("backoffDelay with zero InitialDelay = %v, want 0", got)
		}
	}
}

func TestBackoffDelay_ZeroMaxDelay(t *testing.T) {
	policy := RetryPolicy{
		InitialDelay: 100 * time.Millisecond,
		MaxDelay:     0,
	}
	prng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 100; i++ {
		// Any non-zero shift would still produce a positive `upper`
		// initially, but the post-cap guard (`upper > policy.MaxDelay`
		// → `upper = policy.MaxDelay = 0`) drives the second guard
		// (`upper <= 0`) into returning zero. The test would panic if
		// the second guard were ever removed.
		got := backoffDelay(i%5, policy, prng)
		if got != 0 {
			t.Fatalf("backoffDelay with zero MaxDelay = %v, want 0", got)
		}
	}
}

// --- M5: Retry-After capping ---

func TestDoWithRetry_RetryAfterMsCapped(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&count, 1)
		if n == 1 {
			// Within parseRetryAfter's 60 s ceiling, but well above
			// the test policy's 100 ms MaxDelay — exercises the cap.
			w.Header().Set("Retry-After-Ms", "5000")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := newTestMetrics(t)

	policy := RetryPolicy{
		MaxAttempts:     3,
		InitialDelay:    1 * time.Millisecond,
		MaxDelay:        100 * time.Millisecond,
		WallClockBudget: 5 * time.Second,
	}

	req := newPostReq(t, srv.URL, `{}`)
	start := time.Now()
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(policy, m))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	// Cap at 100 ms; tolerate scheduler overhead. The unbounded
	// uncapped path would sleep ~5 s.
	if elapsed > 1*time.Second {
		t.Errorf("elapsed = %v, want capped near MaxDelay=100ms", elapsed)
	}
}

func TestDoWithRetry_RetryAfterSecondsCapped(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&count, 1)
		if n == 1 {
			// parseRetryAfter caps Retry-After at 60 s (its own
			// ceiling). Use 30 s so the value reaches the dispatch
			// switch's MaxDelay cap unmodified by the header parser.
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := newTestMetrics(t)

	policy := RetryPolicy{
		MaxAttempts:     3,
		InitialDelay:    1 * time.Millisecond,
		MaxDelay:        100 * time.Millisecond,
		WallClockBudget: 5 * time.Second,
	}

	req := newPostReq(t, srv.URL, `{}`)
	start := time.Now()
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(policy, m))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	// Cap at 100 ms; uncapped would sleep ~30 s.
	if elapsed > 1*time.Second {
		t.Errorf("elapsed = %v, want capped near MaxDelay=100ms", elapsed)
	}
}

// --- M7: small coverage gaps ---

func TestDoWithRetry_MaxAttemptsZeroNormalisedToOne(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := newTestMetrics(t)

	// MaxAttempts == 0 must be normalised to one attempt by the
	// helper. The for-loop bound is the only protection against a
	// zero-attempts misconfiguration causing the loop to never run.
	policy := RetryPolicy{MaxAttempts: 0, InitialDelay: 1 * time.Millisecond}

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(policy, m))
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("server hits: got %d, want 1", got)
	}
}

func TestDoWithRetry_NilMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Pass nil metrics; recordOutcome's nil guard must prevent panic.
	req := newPostReq(t, srv.URL, `{}`)
	opts := RetryOptions{
		Policy:       defaultTestPolicy(),
		Logger:       discardLogger(),
		Metrics:      nil,
		ProviderType: "openai",
		Model:        "gpt-test",
	}
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req, opts)
	if err != nil {
		t.Fatalf("DoWithRetry with nil metrics: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
}

// --- M1: rewind failure path ---

// flakyGetBody is a counter-backed GetBody implementation that returns
// the body successfully on first call (for http.NewRequest's initial
// read) and fails on subsequent calls. Used to drive the
// retryOutcomeRewindFailed path.
type flakyGetBody struct {
	body    []byte
	calls   int32
	failAt  int32
	failErr error
}

func (f *flakyGetBody) get() (io.ReadCloser, error) {
	n := atomic.AddInt32(&f.calls, 1)
	if n >= f.failAt {
		return nil, f.failErr
	}
	return io.NopCloser(bytes.NewReader(f.body)), nil
}

func TestDoWithRetry_GetBodyError(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	m, reader := newTestMetrics(t)

	bodyBytes := []byte(`{"x":1}`)
	injected := errors.New("rewind failed: source exhausted")
	// failAt=1 → fail on the first GetBody call (which happens at
	// attempt 1 to rewind for the retry). Attempt 0 uses req.Body
	// directly without consulting GetBody.
	flaky := &flakyGetBody{body: bodyBytes, failAt: 1, failErr: injected}

	req, err := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.GetBody = flaky.get

	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req,
		testOpts(defaultTestPolicy(), m))
	if !errors.Is(err, injected) {
		t.Fatalf("err: got %v, want %v", err, injected)
	}
	if resp != nil {
		t.Errorf("expected nil resp on rewind failure, got %v", resp)
		_ = resp.Body.Close()
	}

	// Exactly one server hit: the first attempt succeeded in sending,
	// the response was 429, then GetBody failed before attempt 2 could
	// be dispatched.
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Errorf("server hits: got %d, want 1", got)
	}

	outcomes := retryOutcomeFromMetrics(t, reader)
	if outcomes[retryOutcomeRewindFailed] != 1 {
		t.Errorf("rewind_failed counter: got %d, want 1 (all: %+v)", outcomes[retryOutcomeRewindFailed], outcomes)
	}
	// And specifically NOT exhausted — that was the M2 mislabel.
	if outcomes[retryOutcomeExhausted] != 0 {
		t.Errorf("exhausted counter: got %d, want 0 — rewind failure must not record exhaustion", outcomes[retryOutcomeExhausted])
	}
}

// --- ShouldRetry classifier coverage ---

func TestDoWithRetry_ShouldRetry_ConsumesAndRetries(t *testing.T) {
	// ShouldRetry returns (retryable=true, consumed=true) on the
	// first attempt's 200 response. Exercises the "consumed=true"
	// branch overriding the default heuristic (which would treat
	// 200 as success).
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&count, 1)
		if n == 1 {
			w.Header().Set("x-force-retry", "1")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := newTestMetrics(t)

	opts := RetryOptions{
		Policy:       defaultTestPolicy(),
		Logger:       discardLogger(),
		Metrics:      m,
		ProviderType: "anthropic",
		Model:        "claude-test",
		ShouldRetry: func(resp *http.Response) (bool, bool) {
			if resp.Header.Get("x-force-retry") == "1" {
				return true, true
			}
			return false, false
		},
	}

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req, opts)
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if got := atomic.LoadInt32(&count); got != 2 {
		t.Errorf("server hits: got %d, want 2", got)
	}
}

func TestDoWithRetry_ShouldRetry_FallsThroughOnNotConsumed(t *testing.T) {
	// ShouldRetry returns (false, false=not consumed) for every
	// response. The default retryableStatus heuristic should apply
	// — first 429 retries, second 200 succeeds.
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&count, 1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := newTestMetrics(t)

	var shouldRetryCalls int32
	opts := RetryOptions{
		Policy:       defaultTestPolicy(),
		Logger:       discardLogger(),
		Metrics:      m,
		ProviderType: "anthropic",
		Model:        "claude-test",
		ShouldRetry: func(resp *http.Response) (bool, bool) {
			atomic.AddInt32(&shouldRetryCalls, 1)
			return false, false
		},
	}

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req, opts)
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if got := atomic.LoadInt32(&count); got != 2 {
		t.Errorf("server hits: got %d, want 2", got)
	}
	// ShouldRetry consulted on the 429 response (fall-through), AND
	// again on the second 200 (also fall-through, treated as
	// success).
	if got := atomic.LoadInt32(&shouldRetryCalls); got < 1 {
		t.Errorf("shouldRetry calls: got %d, want at least 1", got)
	}
}

// --- Nil-logger default fallback ---

// TestDoWithRetry_NilLoggerFallback drives a 429-then-200 exchange so
// the nil-logger fallback path actually emits a `provider_retry` warn
// record. The slog.Default() handler is replaced for the duration of
// the test with a JSON handler writing to an in-memory buffer; the
// assertion verifies the buffer (i) received at least one record,
// confirming the fallback is wired, and (ii) does not contain the
// raw scrubbable test sentinel — confirming the fallback wraps the
// default handler in a ScrubHandler. Without the wrapper the sentinel
// would land verbatim.
func TestDoWithRetry_NilLoggerFallback(t *testing.T) {
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := atomic.AddInt32(&count, 1); n == 1 {
			w.Header().Set("Retry-After-Ms", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m, _ := newTestMetrics(t)

	// Swap the process-global slog default for the duration of the
	// test so we can inspect what reaches the fallback handler.
	prevDefault := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevDefault) })
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	// Logger left zero so DoWithRetry's nil-fallback branch fires.
	opts := RetryOptions{
		Policy:       defaultTestPolicy(),
		Metrics:      m,
		ProviderType: "openai",
		Model:        "gpt-test-sk-ant-test-cafebabe1234567890",
	}

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(context.Background(), &http.Client{Timeout: 5 * time.Second}, req, opts)
	if err != nil {
		t.Fatalf("DoWithRetry with nil Logger: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
	logOutput := buf.String()
	if !strings.Contains(logOutput, "provider_retry") {
		t.Fatalf("nil-logger fallback emitted no provider_retry record:\n%s", logOutput)
	}
	// The sentinel is a known scrubbable Anthropic key prefix embedded
	// in the Model field, which surfaces in the warn record. If the
	// ScrubHandler wrapper is missing it lands raw — assert it does
	// not.
	if strings.Contains(logOutput, "sk-ant-test-cafebabe1234567890") {
		t.Errorf("nil-logger fallback leaked scrubbable sentinel; ScrubHandler wrap may be missing:\n%s", logOutput)
	}
	if !strings.Contains(logOutput, "[REDACTED]") {
		t.Errorf("nil-logger fallback should run sentinel through ScrubHandler, expected [REDACTED] marker:\n%s", logOutput)
	}
}

// --- B1: URL scrubbing ---

func TestDoWithRetry_TransportError_URLNotLogged(t *testing.T) {
	// Pick a port that nothing is listening on. The connection
	// attempt fails fast, producing a *url.Error wrapping the dial
	// failure.
	target := "http://127.0.0.1:1/sensitive-path?api_key=should-not-appear"

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	m, _ := newTestMetrics(t)

	bodyBytes := []byte(`{}`)
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(bodyBytes)), nil
	}

	// Use a short-timeout dialer so the test does not block on
	// connection retry timeouts.
	client := &http.Client{
		Timeout: 500 * time.Millisecond,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext,
		},
	}

	policy := RetryPolicy{
		MaxAttempts:     2,
		InitialDelay:    1 * time.Millisecond,
		MaxDelay:        5 * time.Millisecond,
		WallClockBudget: 2 * time.Second,
	}

	opts := RetryOptions{
		Policy:       policy,
		Logger:       logger,
		Metrics:      m,
		ProviderType: "openai",
		Model:        "gpt-test",
	}

	// transientErr is false for plain "connection refused" so the
	// helper will surface the error after the first attempt. The
	// slog handler will be called via the non_retryable path only if
	// a provider_retry warn happened — which requires a transient
	// classification. To force a retry log, simulate the EOF path
	// with a hijack-and-close server.

	// Reroute: use the hijack-and-close pattern but pick a target
	// whose URL contains the secrets we want to test against.
	var hits int32
	srv := httptest.NewServer(hijackAndCloseHandler(t, false, &hits))
	defer srv.Close()

	secretURL := srv.URL + "/path?api_key=topsecret&token=alsosecret"
	req2, err := http.NewRequest(http.MethodPost, secretURL, bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("NewRequest secret: %v", err)
	}
	req2.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(bodyBytes)), nil
	}

	buf.Reset()
	resp, err := DoWithRetry(context.Background(), client, req2, opts)
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	logOutput := buf.String()
	if strings.Contains(logOutput, "topsecret") {
		t.Errorf("log output contains api_key value 'topsecret':\n%s", logOutput)
	}
	if strings.Contains(logOutput, "alsosecret") {
		t.Errorf("log output contains token value 'alsosecret':\n%s", logOutput)
	}
	// The server-side host:port is also part of the *url.Error
	// string. Asserting on its absence proves the unwrap occurred —
	// the inner net error message does not contain the URL.
	host := strings.TrimPrefix(srv.URL, "http://")
	if strings.Contains(logOutput, host) {
		t.Errorf("log output contains server URL host %q:\n%s", host, logOutput)
	}
}

// scrubbableTransport is a stub http.RoundTripper that fails on the
// first call with a *net.OpError whose embedded error message contains
// a known scrub pattern (the sk-ant- prefix). transientErr classifies
// net.OpError timeouts as retryable; we set Op="dial" + Err=timeout so
// the helper logs a retry attempt rather than surfacing the error
// immediately.
type scrubbableTransport struct {
	mu       sync.Mutex
	calls    int
	innerErr error
	delegate http.RoundTripper
}

func (t *scrubbableTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	if t.calls == 1 {
		return nil, t.innerErr
	}
	return t.delegate.RoundTrip(req)
}

// scrubTimeoutErr satisfies net.Error.Timeout()==true so transientErr
// classifies it as retryable. Embedding a scrub-pattern in the message
// is the load-bearing piece — it lets the span attribute assertion
// verify that the scrubber runs on the value the OTel exporter sees.
type scrubTimeoutErr struct{ msg string }

func (e *scrubTimeoutErr) Error() string   { return e.msg }
func (e *scrubTimeoutErr) Timeout() bool   { return true }
func (e *scrubTimeoutErr) Temporary() bool { return true }

// TestDoWithRetry_SpanErrorAttributeIsScrubbed asserts the OTel span's
// `error` attribute is run through security.Scrub before export, so a
// scrubbable secret pattern surfacing in a transport-error message
// never reaches the span exporter unredacted. This mirrors the
// existing slog scrubbing behaviour at the same code site.
func TestDoWithRetry_SpanErrorAttributeIsScrubbed(t *testing.T) {
	// Use a known scrubbable pattern (Anthropic API key prefix). The
	// scrubber replaces it with "[REDACTED]" — asserting on the
	// presence of the literal token in the span attribute would tell
	// us the scrubber did not run.
	secret := "sk-ant-test-abcdef1234567890"
	transport := &scrubbableTransport{
		innerErr: &scrubTimeoutErr{msg: "dial tcp: " + secret + " timed out"},
		delegate: http.DefaultTransport,
	}

	// Need a real server for attempt 2 (helper retries on the timeout).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &http.Client{Timeout: 5 * time.Second, Transport: transport}
	m, _ := newTestMetrics(t)
	ctx, exporter, span := withRecordingSpan(t)

	req := newPostReq(t, srv.URL, `{}`)
	resp, err := DoWithRetry(ctx, client, req, testOpts(defaultTestPolicy(), m))
	if err != nil {
		t.Fatalf("DoWithRetry: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	span.End()
	events := collectSpanEvents(exporter, "provider_retry_attempt")
	if len(events) != 1 {
		t.Fatalf("expected 1 provider_retry_attempt event, got %d", len(events))
	}
	var got string
	var foundErrAttr bool
	for _, kv := range events[0].Attributes {
		if string(kv.Key) == "error" {
			foundErrAttr = true
			got = kv.Value.AsString()
		}
	}
	if !foundErrAttr {
		t.Fatalf("provider_retry_attempt event missing error attribute")
	}
	if strings.Contains(got, secret) {
		t.Errorf("span error attribute leaks the scrubbable token %q: %q", secret, got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("span error attribute should contain [REDACTED] marker, got: %q", got)
	}
	// Belt-and-braces: explicitly compare against the scrubber output
	// so a future divergence between log and span sinks fails here.
	wantPrefix := "dial tcp: "
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("span error attribute should retain the non-secret prefix %q, got: %q", wantPrefix, got)
	}
}
