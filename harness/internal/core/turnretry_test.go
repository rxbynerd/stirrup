package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// turnScript is one scripted Stream call: a pre-stream error, or a stream
// of events delivered after an optional delay.
type turnScript struct {
	err    error
	events []types.StreamEvent
	delay  time.Duration
}

// scriptedTurnProvider plays one turnScript per Stream call, repeating the
// last script once the list runs out.
type scriptedTurnProvider struct {
	mu      sync.Mutex
	scripts []turnScript
	calls   int
}

func (p *scriptedTurnProvider) Stream(ctx context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	p.mu.Lock()
	idx := min(p.calls, len(p.scripts)-1)
	p.calls++
	script := p.scripts[idx]
	p.mu.Unlock()

	if script.delay > 0 {
		select {
		case <-time.After(script.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if script.err != nil {
		return nil, script.err
	}
	ch := make(chan types.StreamEvent, len(script.events))
	for _, e := range script.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func (p *scriptedTurnProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func toolCallScript() turnScript {
	return turnScript{events: []types.StreamEvent{
		{Type: "tool_call", ID: "tc_1", Name: "test_tool", Input: map[string]any{}},
		{Type: "message_complete", StopReason: "tool_use"},
	}}
}

func answerScript() turnScript {
	return turnScript{events: []types.StreamEvent{
		{Type: "text_delta", Text: "All done."},
		{Type: "message_complete", StopReason: "end_turn", InputTokens: 100, OutputTokens: 10},
	}}
}

// overloadedMidStream streams partial text and then fails the way the
// Anthropic adapter reports an SSE overloaded_error event.
func overloadedMidStream() turnScript {
	return turnScript{events: []types.StreamEvent{
		{Type: "text_delta", Text: "discarded partial"},
		{Type: "error", Error: types.MarkRetryable(errors.New("anthropic API stream error (overloaded_error): Overloaded"))},
	}}
}

type retryHarness struct {
	loop      *AgenticLoop
	trace     *usageTraceEmitter
	transport *bytes.Buffer
	reader    *sdkmetric.ManualReader
}

func newRetryHarness(t *testing.T, prov provider.ProviderAdapter) *retryHarness {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	metrics, err := observability.NewMetricsForTesting(mp)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}
	h := &retryHarness{
		trace:     &usageTraceEmitter{agg: trace.NewJSONLTraceEmitter(&bytes.Buffer{}, false)},
		transport: &bytes.Buffer{},
		reader:    reader,
	}
	h.loop = buildTestLoop(nil)
	h.loop.Provider = prov
	h.loop.Trace = h.trace
	h.loop.Transport = transport.NewStdioTransport(h.transport, &bytes.Buffer{})
	h.loop.Metrics = metrics
	h.loop.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return h
}

// retryConfig enables turn retry with millisecond backoff.
func retryConfig(maxAttempts int) *types.RunConfig {
	config := buildTestConfig()
	config.Provider.Retry = &types.ProviderRetryConfig{
		MaxAttempts:       maxAttempts,
		InitialDelayMs:    1,
		MaxDelayMs:        2,
		WallClockBudgetMs: 10_000,
	}
	return config
}

func (h *retryHarness) turns() []types.TurnTrace {
	h.trace.mu.Lock()
	defer h.trace.mu.Unlock()
	return append([]types.TurnTrace(nil), h.trace.turns...)
}

func (h *retryHarness) turnRecords() []types.TurnRecord {
	h.trace.mu.Lock()
	defer h.trace.mu.Unlock()
	return append([]types.TurnRecord(nil), h.trace.turnRecords...)
}

// retryWarnings returns the transport warning events announcing a turn
// retry.
func (h *retryHarness) retryWarnings() []string {
	var out []string
	for _, line := range strings.Split(h.transport.String(), "\n") {
		if strings.Contains(line, `"type":"warning"`) && strings.Contains(line, "the turn is retried") {
			out = append(out, line)
		}
	}
	return out
}

// turnRetryOutcomes sums scope=turn provider_retry_outcomes by outcome.
func (h *retryHarness) turnRetryOutcomes(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
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
				if scope, _ := dp.Attributes.Value("scope"); scope.AsString() != "turn" {
					continue
				}
				outcome, _ := dp.Attributes.Value("provider.retry.outcome")
				out[outcome.AsString()] += dp.Value
			}
		}
	}
	return out
}

func TestLoop_TurnRetry_OverloadedMidStreamThenSuccess(t *testing.T) {
	prov := &scriptedTurnProvider{scripts: []turnScript{toolCallScript(), overloadedMidStream(), answerScript()}}
	h := newRetryHarness(t, prov)

	runTrace, err := h.loop.Run(context.Background(), retryConfig(3))
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	if n := prov.callCount(); n != 3 {
		t.Errorf("Stream calls = %d, want 3", n)
	}

	turns := h.turns()
	if len(turns) != 2 {
		t.Fatalf("recorded %d turns, want 2: a retried attempt must not add a turn", len(turns))
	}
	if turns[0].Retries != 0 || turns[1].Retries != 1 {
		t.Errorf("turn retries = [%d %d], want [0 1]", turns[0].Retries, turns[1].Retries)
	}
	if turns[1].StopReason != "end_turn" {
		t.Errorf("retried turn StopReason = %q, want end_turn", turns[1].StopReason)
	}

	records := h.turnRecords()
	if len(records) != 2 {
		t.Fatalf("recorded %d turn records, want 2", len(records))
	}
	for _, block := range records[1].ModelOutput {
		if strings.Contains(block.Text, "discarded partial") {
			t.Errorf("retried turn kept output from the discarded attempt: %+v", records[1].ModelOutput)
		}
	}

	warnings := h.retryWarnings()
	if len(warnings) != 1 {
		t.Fatalf("retry warnings = %d, want 1:\n%s", len(warnings), h.transport.String())
	}
	if !strings.Contains(warnings[0], "turn 1 attempt 1") || !strings.Contains(warnings[0], "overloaded_error") {
		t.Errorf("warning does not name the discarded attempt and its error: %s", warnings[0])
	}
	if got := h.turnRetryOutcomes(t); got[turnRetryOutcomeSucceeded] != 1 || len(got) != 1 {
		t.Errorf("turn retry outcomes = %v, want exactly one succeeded", got)
	}
}

func TestLoop_TurnRetry_PreStreamRetryableErrorThenSuccess(t *testing.T) {
	prov := &scriptedTurnProvider{scripts: []turnScript{
		{err: types.MarkRetryable(errors.New("anthropic API returned status 529"))},
		answerScript(),
	}}
	h := newRetryHarness(t, prov)

	runTrace, err := h.loop.Run(context.Background(), retryConfig(3))
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	turns := h.turns()
	if len(turns) != 1 || turns[0].Retries != 1 {
		t.Fatalf("turns = %+v, want one turn with Retries 1", turns)
	}
	if len(h.retryWarnings()) != 1 {
		t.Errorf("retry warnings = %d, want 1", len(h.retryWarnings()))
	}
}

func TestLoop_TurnRetry_NonRetryableErrorEndsRunOnFirstFailure(t *testing.T) {
	cases := []struct {
		name   string
		script turnScript
	}{
		{"401 before the stream", turnScript{err: errors.New("anthropic API returned status 401: invalid x-api-key")}},
		{"400 before the stream", turnScript{err: errors.New("anthropic API returned status 400: prompt is too long")}},
		{"insufficient_quota 429 before the stream", turnScript{err: errors.New("openai API returned status 429: You exceeded your current quota")}},
		{"unclassified stream error", turnScript{events: []types.StreamEvent{{Type: "error", Error: errors.New("parse tool input JSON: unexpected end")}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &scriptedTurnProvider{scripts: []turnScript{tc.script, answerScript()}}
			h := newRetryHarness(t, prov)

			runTrace, err := h.loop.Run(context.Background(), retryConfig(3))
			if err != nil {
				t.Fatalf("Run() error: %v", err)
			}
			if runTrace.Outcome != "error" {
				t.Errorf("Outcome = %q, want error", runTrace.Outcome)
			}
			if n := prov.callCount(); n != 1 {
				t.Errorf("Stream calls = %d, want 1", n)
			}
			if w := h.retryWarnings(); len(w) != 0 {
				t.Errorf("unexpected retry warnings: %v", w)
			}
			if got := h.turnRetryOutcomes(t); len(got) != 0 {
				t.Errorf("turn retry outcomes = %v, want none for a turn that never hit a retryable failure", got)
			}
		})
	}
}

func TestLoop_TurnRetry_StopsAtAttemptCount(t *testing.T) {
	cases := []struct {
		name        string
		maxAttempts int
		wantCalls   int
	}{
		{"maxAttempts 2 allows one retry", 2, 2},
		{"maxAttempts 3 allows two retries", 3, 3},
		{"maxAttempts 5 is capped at two retries", 5, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &scriptedTurnProvider{scripts: []turnScript{overloadedMidStream()}}
			h := newRetryHarness(t, prov)

			runTrace, err := h.loop.Run(context.Background(), retryConfig(tc.maxAttempts))
			if err != nil {
				t.Fatalf("Run() error: %v", err)
			}
			if runTrace.Outcome != "error" {
				t.Errorf("Outcome = %q, want error", runTrace.Outcome)
			}
			if n := prov.callCount(); n != tc.wantCalls {
				t.Errorf("Stream calls = %d, want %d", n, tc.wantCalls)
			}
			turns := h.turns()
			if len(turns) != 1 || turns[0].StopReason != "error" || turns[0].Retries != tc.wantCalls-1 {
				t.Errorf("turns = %+v, want one error turn with Retries %d", turns, tc.wantCalls-1)
			}
			if w := h.retryWarnings(); len(w) != tc.wantCalls-1 {
				t.Errorf("retry warnings = %d, want %d", len(w), tc.wantCalls-1)
			}
			if got := h.turnRetryOutcomes(t); got[turnRetryOutcomeExhausted] != 1 || len(got) != 1 {
				t.Errorf("turn retry outcomes = %v, want exactly one exhausted", got)
			}
		})
	}
}

func TestLoop_TurnRetry_DisabledWhenMaxAttemptsIsOne(t *testing.T) {
	prov := &scriptedTurnProvider{scripts: []turnScript{overloadedMidStream(), answerScript()}}
	h := newRetryHarness(t, prov)

	runTrace, err := h.loop.Run(context.Background(), retryConfig(1))
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "error" || prov.callCount() != 1 {
		t.Errorf("Outcome = %q after %d calls, want error after 1", runTrace.Outcome, prov.callCount())
	}
	if got := h.turnRetryOutcomes(t); len(got) != 0 {
		t.Errorf("turn retry outcomes = %v, want none when turn retry is disabled", got)
	}
}

func TestLoop_TurnRetry_StopsAtWallClockBudget(t *testing.T) {
	// Each attempt takes 50 ms and every backoff is under 10 ms, so the
	// first retry fits a 25 ms budget measured from the first failure and
	// the second does not.
	failing := overloadedMidStream()
	failing.delay = 50 * time.Millisecond
	prov := &scriptedTurnProvider{scripts: []turnScript{failing}}
	h := newRetryHarness(t, prov)
	config := retryConfig(5)
	config.Provider.Retry.InitialDelayMs = 10
	config.Provider.Retry.MaxDelayMs = 10
	config.Provider.Retry.WallClockBudgetMs = 25

	runTrace, err := h.loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "error" {
		t.Errorf("Outcome = %q, want error", runTrace.Outcome)
	}
	if n := prov.callCount(); n != 2 {
		t.Errorf("Stream calls = %d, want 2", n)
	}
	if got := h.turnRetryOutcomes(t); got[turnRetryOutcomeBudgetExhausted] != 1 || len(got) != 1 {
		t.Errorf("turn retry outcomes = %v, want exactly one budget_exhausted", got)
	}
}

func TestLoop_TurnRetry_RunDeadlineWinsDuringBackoff(t *testing.T) {
	prov := &scriptedTurnProvider{scripts: []turnScript{overloadedMidStream(), answerScript()}}
	h := newRetryHarness(t, prov)
	config := retryConfig(3)
	// Equal jitter keeps every backoff at or above 30 minutes.
	config.Provider.Retry.InitialDelayMs = 3_600_000
	config.Provider.Retry.MaxDelayMs = 3_600_000
	config.Provider.Retry.WallClockBudgetMs = 7_200_000

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	runTrace, err := h.loop.Run(ctx, config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Run took %v; the run deadline did not interrupt the backoff", elapsed)
	}
	if runTrace.Outcome != "timeout" {
		t.Errorf("Outcome = %q, want timeout", runTrace.Outcome)
	}
	if n := prov.callCount(); n != 1 {
		t.Errorf("Stream calls = %d, want 1", n)
	}
	if got := h.turnRetryOutcomes(t); got[turnRetryOutcomeContextDone] != 1 || len(got) != 1 {
		t.Errorf("turn retry outcomes = %v, want exactly one context_done", got)
	}
}

func TestLoop_TurnRetry_CancelledRunIsNotRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := &cancelThenRetryableProvider{cancel: cancel}
	h := newRetryHarness(t, prov)

	runTrace, err := h.loop.Run(ctx, retryConfig(3))
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "cancelled" {
		t.Errorf("Outcome = %q, want cancelled", runTrace.Outcome)
	}
	if n := prov.calls.Load(); n != 1 {
		t.Errorf("Stream calls = %d, want 1", n)
	}
	if got := h.turnRetryOutcomes(t); len(got) != 0 {
		t.Errorf("turn retry outcomes = %v, want none", got)
	}
}

// cancelThenRetryableProvider cancels the run and then fails with an error that
// would otherwise be retried.
type cancelThenRetryableProvider struct {
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (p *cancelThenRetryableProvider) Stream(context.Context, types.StreamParams) (<-chan types.StreamEvent, error) {
	p.calls.Add(1)
	p.cancel()
	return nil, types.MarkRetryable(errors.New("connection reset by peer"))
}

func TestLoop_TurnRetry_ChargesUsageReportedByDiscardedAttempt(t *testing.T) {
	failing := turnScript{events: []types.StreamEvent{
		{Type: "message_complete", InputTokens: 40, OutputTokens: 4},
		{Type: "error", Error: types.MarkRetryable(errors.New("read SSE stream: connection reset by peer"))},
	}}
	prov := &scriptedTurnProvider{scripts: []turnScript{failing, answerScript()}}
	h := newRetryHarness(t, prov)

	runTrace, err := h.loop.Run(context.Background(), retryConfig(3))
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	turns := h.turns()
	if len(turns) != 1 {
		t.Fatalf("recorded %d turns, want 1", len(turns))
	}
	if turns[0].Tokens.Input != 140 || turns[0].Tokens.Output != 14 {
		t.Errorf("turn tokens = %+v, want input 140 and output 14 including the discarded attempt", turns[0].Tokens)
	}
	if runTrace.TokenUsage.Input != 140 || runTrace.TokenUsage.Output != 14 {
		t.Errorf("run tokens = %+v, want input 140 and output 14", runTrace.TokenUsage)
	}
}

func TestLoop_TurnRetry_UsesSelectedProviderPolicy(t *testing.T) {
	config := retryConfig(3)
	config.Providers = map[string]types.ProviderConfig{
		"secondary": {Type: "openai-compatible", Retry: &types.ProviderRetryConfig{MaxAttempts: 2, InitialDelayMs: 7, MaxDelayMs: 7}},
		"bare":      {Type: "openai-compatible"},
	}
	if got := turnRetryPolicyFor(config, "secondary"); got.maxRetries != 1 || got.initialDelay != 7*time.Millisecond {
		t.Errorf("secondary policy = %+v, want its own retry block", got)
	}
	if got := turnRetryPolicyFor(config, "bare"); got.maxRetries != 2 || got.initialDelay != time.Millisecond {
		t.Errorf("bare policy = %+v, want the default provider's retry block", got)
	}
	if got := turnRetryPolicyFor(config, "anthropic"); got.maxRetries != 2 {
		t.Errorf("default provider policy = %+v, want two retries", got)
	}
	config.Provider.Retry = nil
	if got := turnRetryPolicyFor(config, "anthropic"); got != (turnRetryPolicy{}) {
		t.Errorf("nil retry block policy = %+v, want turn retry disabled", got)
	}
}

func TestTurnRetryPolicy_DelayUsesEqualJitter(t *testing.T) {
	p := turnRetryPolicy{maxRetries: 2, initialDelay: 100 * time.Millisecond, maxDelay: 300 * time.Millisecond}
	for range 200 {
		if d := p.delay(0); d < 50*time.Millisecond || d >= 100*time.Millisecond {
			t.Fatalf("delay(0) = %v, want within [50ms, 100ms)", d)
		}
		if d := p.delay(2); d < 150*time.Millisecond || d >= 300*time.Millisecond {
			t.Fatalf("delay(2) = %v, want within [150ms, 300ms) after the maxDelay cap", d)
		}
	}
	if d := (turnRetryPolicy{initialDelay: time.Second}).delay(0); d != 0 {
		t.Errorf("delay with zero maxDelay = %v, want 0", d)
	}
}

// TestLoop_TurnRetry_DroppedConnectionWithRealAdapter drives the loop
// against the openai-compatible adapter: the first response drops the
// connection mid-stream and the second completes.
func TestLoop_TurnRetry_DroppedConnectionWithRealAdapter(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if hits.Add(1) == 1 {
			_, _ = fmt.Fprint(w, `data: {"id":"c","choices":[{"index":0,"delta":{"content":"discarded partial"},"finish_reason":null}]}`+"\n\n")
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("Hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = fmt.Fprint(w,
			`data: {"id":"c","choices":[{"index":0,"delta":{"content":"All done."},"finish_reason":null}]}`+"\n\n"+
				`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+
				"data: [DONE]\n\n")
	}))
	defer srv.Close()

	bearer := func(context.Context) (string, error) { return "test-key", nil }
	adapter := provider.NewOpenAICompatibleAdapter(bearer, srv.URL, provider.OpenAIAuthConfig{}, provider.RetryPolicy{})
	h := newRetryHarness(t, adapter)

	runTrace, err := h.loop.Run(context.Background(), retryConfig(3))
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("Outcome = %q, want success", runTrace.Outcome)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("server saw %d requests, want 2", n)
	}
	if turns := h.turns(); len(turns) != 1 || turns[0].Retries != 1 {
		t.Errorf("turns = %+v, want one turn with Retries 1", turns)
	}
}

// TestLoop_TurnRetry_TerminalStatusWithRealAdapter pins that a 401, a 400
// and a quota-exhausted 429 from a real adapter end the run after one
// request, with turn retry and pre-stream retry both enabled.
func TestLoop_TurnRetry_TerminalStatusWithRealAdapter(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"401", http.StatusUnauthorized, `{"error":{"message":"Incorrect API key provided"}}`},
		{"400", http.StatusBadRequest, `{"error":{"message":"Unknown parameter: foo","type":"invalid_request_error"}}`},
		{"insufficient_quota 429", http.StatusTooManyRequests, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()

			bearer := func(context.Context) (string, error) { return "test-key", nil }
			adapter := provider.NewOpenAICompatibleAdapter(bearer, srv.URL, provider.OpenAIAuthConfig{},
				provider.RetryPolicy{MaxAttempts: 3, InitialDelay: time.Millisecond, MaxDelay: time.Millisecond, WallClockBudget: 5 * time.Second})
			h := newRetryHarness(t, adapter)

			runTrace, err := h.loop.Run(context.Background(), retryConfig(3))
			if err != nil {
				t.Fatalf("Run() error: %v", err)
			}
			if runTrace.Outcome != "error" {
				t.Errorf("Outcome = %q, want error", runTrace.Outcome)
			}
			if n := hits.Load(); n != 1 {
				t.Errorf("server saw %d requests, want 1", n)
			}
		})
	}
}
