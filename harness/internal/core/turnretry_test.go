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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err, "NewMetricsForTesting")
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
	err := h.reader.Collect(context.Background(), &rm)
	require.NoError(t, err, "collect metrics")
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
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome)
	assert.Equal(t, 3, prov.callCount())

	turns := h.turns()
	require.Len(t, turns, 2, "a retried attempt must not add a turn")
	assert.Equal(t, 0, turns[0].Retries)
	assert.Equal(t, 1, turns[1].Retries)
	assert.Equal(t, "end_turn", turns[1].StopReason)

	records := h.turnRecords()
	require.Len(t, records, 2)
	for _, block := range records[1].ModelOutput {
		assert.NotContains(t, block.Text, "discarded partial", "retried turn kept output from the discarded attempt")
	}

	warnings := h.retryWarnings()
	require.Len(t, warnings, 1, "transport output:\n%s", h.transport.String())
	assert.Contains(t, warnings[0], "turn 1 attempt 1", "warning names the discarded attempt")
	assert.Contains(t, warnings[0], "overloaded_error", "warning names the error")

	assert.Equal(t, map[string]int64{turnRetryOutcomeSucceeded: 1}, h.turnRetryOutcomes(t))
}

func TestLoop_TurnRetry_PreStreamRetryableErrorThenSuccess(t *testing.T) {
	prov := &scriptedTurnProvider{scripts: []turnScript{
		{err: types.MarkRetryable(errors.New("anthropic API returned status 529"))},
		answerScript(),
	}}
	h := newRetryHarness(t, prov)

	runTrace, err := h.loop.Run(context.Background(), retryConfig(3))
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome)

	turns := h.turns()
	require.Len(t, turns, 1)
	require.Equal(t, 1, turns[0].Retries)
	assert.Len(t, h.retryWarnings(), 1)
}

func TestLoop_TurnRetry_NonRetryableErrorEndsRunOnFirstFailure(t *testing.T) {
	cases := []struct {
		name   string
		script turnScript
	}{
		{"401 before the stream", turnScript{err: errors.New("anthropic API returned status 401: invalid x-api-key")}},
		{"400 before the stream", turnScript{err: errors.New("anthropic API returned status 400: prompt is too long")}},
		{"insufficient quota 429 before the stream", turnScript{err: errors.New("openai API returned status 429: You exceeded your current quota")}},
		{"unclassified stream error", turnScript{events: []types.StreamEvent{{Type: "error", Error: errors.New("parse tool input JSON: unexpected end")}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &scriptedTurnProvider{scripts: []turnScript{tc.script, answerScript()}}
			h := newRetryHarness(t, prov)

			runTrace, err := h.loop.Run(context.Background(), retryConfig(3))
			require.NoError(t, err)
			require.Equal(t, "error", runTrace.Outcome)
			require.Equal(t, 1, prov.callCount(), "stream calls")
			require.Empty(t, h.retryWarnings(), "no retry warning for a non-retryable failure")
			require.Empty(t, h.turnRetryOutcomes(t), "no turn retry outcome for a turn that never hit a retryable failure")
		})
	}
}

func TestLoop_TurnRetry_StopsAtAttemptCount(t *testing.T) {
	cases := []struct {
		name        string
		maxAttempts int
		wantCalls   int
	}{
		{"max attempts 2 allows one retry", 2, 2},
		{"max attempts 3 allows two retries", 3, 3},
		{"max attempts 5 is capped at two retries", 5, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := &scriptedTurnProvider{scripts: []turnScript{overloadedMidStream()}}
			h := newRetryHarness(t, prov)

			runTrace, err := h.loop.Run(context.Background(), retryConfig(tc.maxAttempts))
			require.NoError(t, err)
			require.Equal(t, "error", runTrace.Outcome)
			require.Equal(t, tc.wantCalls, prov.callCount(), "stream calls")

			turns := h.turns()
			require.Len(t, turns, 1)
			require.Equal(t, "error", turns[0].StopReason)
			require.Equal(t, tc.wantCalls-1, turns[0].Retries)
			require.Len(t, h.retryWarnings(), tc.wantCalls-1)
			require.Equal(t, map[string]int64{turnRetryOutcomeExhausted: 1}, h.turnRetryOutcomes(t))
		})
	}
}

func TestLoop_TurnRetry_DisabledWhenMaxAttemptsIsOne(t *testing.T) {
	prov := &scriptedTurnProvider{scripts: []turnScript{overloadedMidStream(), answerScript()}}
	h := newRetryHarness(t, prov)

	runTrace, err := h.loop.Run(context.Background(), retryConfig(1))
	require.NoError(t, err)
	assert.Equal(t, "error", runTrace.Outcome)
	assert.Equal(t, 1, prov.callCount(), "stream calls")
	assert.Empty(t, h.turnRetryOutcomes(t), "turn retry is disabled")
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
	require.NoError(t, err)
	assert.Equal(t, "error", runTrace.Outcome)
	assert.Equal(t, 2, prov.callCount(), "stream calls")
	assert.Equal(t, map[string]int64{turnRetryOutcomeBudgetExhausted: 1}, h.turnRetryOutcomes(t))
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
	require.NoError(t, err)
	require.LessOrEqual(t, time.Since(start), 10*time.Second, "the run deadline must interrupt the backoff")
	assert.Equal(t, "timeout", runTrace.Outcome)
	assert.Equal(t, 1, prov.callCount(), "stream calls")
	assert.Equal(t, map[string]int64{turnRetryOutcomeContextDone: 1}, h.turnRetryOutcomes(t))
}

func TestLoop_TurnRetry_CancelledRunIsNotRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := &cancelThenRetryableProvider{cancel: cancel}
	h := newRetryHarness(t, prov)

	runTrace, err := h.loop.Run(ctx, retryConfig(3))
	require.NoError(t, err)
	assert.Equal(t, "cancelled", runTrace.Outcome)
	assert.EqualValues(t, 1, prov.calls.Load(), "stream calls")
	assert.Empty(t, h.turnRetryOutcomes(t))
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
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome)

	turns := h.turns()
	require.Len(t, turns, 1)
	assert.Equal(t, 140, turns[0].Tokens.Input, "turn input tokens include the discarded attempt")
	assert.Equal(t, 14, turns[0].Tokens.Output, "turn output tokens include the discarded attempt")
	assert.Equal(t, 140, runTrace.TokenUsage.Input, "run input tokens")
	assert.Equal(t, 14, runTrace.TokenUsage.Output, "run output tokens")
}

func TestLoop_TurnRetry_UsesSelectedProviderPolicy(t *testing.T) {
	config := retryConfig(3)
	config.Providers = map[string]types.ProviderConfig{
		"secondary": {Type: "openai-compatible", Retry: &types.ProviderRetryConfig{MaxAttempts: 2, InitialDelayMs: 7, MaxDelayMs: 7}},
		"bare":      {Type: "openai-compatible"},
	}

	t.Run("selected provider uses its own retry block", func(t *testing.T) {
		got := turnRetryPolicyFor(config, "secondary")
		require.Equal(t, 1, got.maxRetries)
		require.Equal(t, 7*time.Millisecond, got.initialDelay)
	})
	t.Run("provider without a retry block uses the default provider's block", func(t *testing.T) {
		got := turnRetryPolicyFor(config, "bare")
		require.Equal(t, 2, got.maxRetries)
		require.Equal(t, time.Millisecond, got.initialDelay)
	})
	t.Run("default provider uses the top-level retry block", func(t *testing.T) {
		got := turnRetryPolicyFor(config, "anthropic")
		require.Equal(t, 2, got.maxRetries)
	})
	t.Run("nil retry block disables turn retry", func(t *testing.T) {
		config.Provider.Retry = nil
		require.Equal(t, turnRetryPolicy{}, turnRetryPolicyFor(config, "anthropic"))
	})
}

func TestTurnRetryPolicyFor_ZeroDelaysFallBackToDefaults(t *testing.T) {
	config := buildTestConfig()
	config.Provider.Retry = &types.ProviderRetryConfig{MaxAttempts: 3}
	got := turnRetryPolicyFor(config, "anthropic")
	require.Equal(t, turnRetryInitialDelayFallback, got.initialDelay)
	require.Equal(t, turnRetryMaxDelayFallback, got.maxDelay)
	assert.GreaterOrEqual(t, got.delay(0), turnRetryInitialDelayFallback/2, "first retry must not be back to back")
}

func TestTurnRetryPolicy_DelayUsesEqualJitter(t *testing.T) {
	p := turnRetryPolicy{maxRetries: 2, initialDelay: 100 * time.Millisecond, maxDelay: 300 * time.Millisecond}
	for range 200 {
		first := p.delay(0)
		require.GreaterOrEqual(t, first, 50*time.Millisecond, "delay(0) is at least half the initial delay")
		require.Less(t, first, 100*time.Millisecond, "delay(0) is below the initial delay")

		capped := p.delay(2)
		require.GreaterOrEqual(t, capped, 150*time.Millisecond, "delay(2) is at least half the capped delay")
		require.Less(t, capped, 300*time.Millisecond, "delay(2) is below the capped delay")
	}
	assert.Zero(t, (turnRetryPolicy{initialDelay: time.Second}).delay(0), "delay with zero maxDelay")
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
			if assert.NoError(t, err, "hijack the connection") {
				_ = conn.Close()
			}
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
	require.NoError(t, err)
	require.Equal(t, "success", runTrace.Outcome)
	assert.EqualValues(t, 2, hits.Load(), "server requests")

	turns := h.turns()
	require.Len(t, turns, 1)
	assert.Equal(t, 1, turns[0].Retries)
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
		{"insufficient quota 429", http.StatusTooManyRequests, `{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`},
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
			require.NoError(t, err)
			require.Equal(t, "error", runTrace.Outcome)
			require.EqualValues(t, 1, hits.Load(), "server requests")
		})
	}
}

// batchTurnProvider reports itself as a batch adapter and fails every
// turn with a retryable error.
type batchTurnProvider struct {
	scriptedTurnProvider
}

func (p *batchTurnProvider) LastBatchID() string { return "msgbatch_test" }

func TestLoop_TurnRetry_SkipsBatchTurns(t *testing.T) {
	prov := &batchTurnProvider{scriptedTurnProvider{scripts: []turnScript{overloadedMidStream(), answerScript()}}}
	h := newRetryHarness(t, prov)

	runTrace, err := h.loop.Run(context.Background(), retryConfig(3))
	require.NoError(t, err)
	assert.Equal(t, "error", runTrace.Outcome)
	assert.Equal(t, 1, prov.callCount(), "a batch turn must not be resubmitted")
}
