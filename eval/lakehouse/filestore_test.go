package lakehouse

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func makeTrace(id, outcome, mode, model string, started time.Time, durationMs int64, turns int, tokens types.TokenUsage) types.RunTrace {
	return types.RunTrace{
		ID:      id,
		Outcome: outcome,
		Config: types.RunConfig{
			Mode: mode,
			ModelRouter: types.ModelRouterConfig{
				Model: model,
			},
		},
		StartedAt:   started,
		CompletedAt: started.Add(time.Duration(durationMs) * time.Millisecond),
		Turns:       turns,
		TokenUsage:  tokens,
	}
}

func makeRecording(runID string, trace types.RunTrace) types.RunRecording {
	return types.RunRecording{
		RunID:        runID,
		Config:       trace.Config,
		FinalOutcome: trace,
	}
}

func TestStoreTrace_RoundTripsThroughQuery(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	trace := makeTrace("t1", "success", "execution", "claude-sonnet-4-6", time.Now(), 1000, 3, types.TokenUsage{Input: 100, Output: 200})

	require.NoError(t, fs.StoreTrace(ctx, trace))

	results, err := fs.QueryTraces(ctx, types.TraceFilter{})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "t1", results[0].ID)
}

func TestStoreRecording_RoundTripsThroughQuery(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	trace := makeTrace("r1", "success", "execution", "claude-sonnet-4-6", time.Now(), 500, 2, types.TokenUsage{Input: 50, Output: 100})
	rec := makeRecording("r1", trace)

	require.NoError(t, fs.StoreRecording(ctx, rec))

	results, err := fs.QueryRecordings(ctx, types.TraceFilter{})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "r1", results[0].RunID)
}

func TestStoreTrace_RejectsEmptyID(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)

	err = fs.StoreTrace(context.Background(), types.RunTrace{})
	require.Error(t, err)
}

func TestStoreRecording_RejectsEmptyRunID(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)

	err = fs.StoreRecording(context.Background(), types.RunRecording{})
	require.Error(t, err)
}

func TestQueryTraces_EmptyStoreReturnsNone(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)

	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{})
	require.NoError(t, err)
	require.Empty(t, results)
}

func TestQueryRecordings_EmptyStoreReturnsNone(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)

	results, err := fs.QueryRecordings(context.Background(), types.TraceFilter{})
	require.NoError(t, err)
	require.Empty(t, results)
}

func seedTraces(t *testing.T, fs *FileStore) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	traces := []types.RunTrace{
		makeTrace("t1", "success", "execution", "claude-sonnet-4-6", base, 1000, 3, types.TokenUsage{Input: 100, Output: 200}),
		makeTrace("t2", "error", "planning", "claude-haiku-3", base.Add(1*time.Hour), 2000, 5, types.TokenUsage{Input: 200, Output: 400}),
		makeTrace("t3", "success", "execution", "claude-sonnet-4-6", base.Add(2*time.Hour), 3000, 7, types.TokenUsage{Input: 300, Output: 600}),
		makeTrace("t4", "max_turns", "review", "claude-opus-4", base.Add(3*time.Hour), 4000, 10, types.TokenUsage{Input: 400, Output: 800}),
		makeTrace("t5", "success", "execution", "claude-sonnet-4-6", base.Add(4*time.Hour), 5000, 2, types.TokenUsage{Input: 500, Output: 1000}),
	}
	for _, tr := range traces {
		require.NoError(t, fs.StoreTrace(ctx, tr), "seed StoreTrace %s", tr.ID)
	}
}

func TestQueryTraces_FilterAfter(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	seedTraces(t, fs)

	after := time.Date(2025, 1, 1, 2, 0, 0, 0, time.UTC)
	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{After: &after})
	require.NoError(t, err)
	// t3 is exactly at the boundary (not strictly after), t4 and t5 are after
	require.Len(t, results, 2)
}

func TestQueryTraces_FilterBefore(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	seedTraces(t, fs)

	before := time.Date(2025, 1, 1, 2, 0, 0, 0, time.UTC)
	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{Before: &before})
	require.NoError(t, err)
	// t1 (00:00) and t2 (01:00) are before 02:00; t3 is exactly at boundary (not strictly before)
	require.Len(t, results, 2)
}

func TestQueryTraces_FilterOutcome(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	seedTraces(t, fs)

	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{Outcome: "success"})
	require.NoError(t, err)
	require.Len(t, results, 3)
}

func TestQueryTraces_FilterMode(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	seedTraces(t, fs)

	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{Mode: "execution"})
	require.NoError(t, err)
	require.Len(t, results, 3)
}

func TestQueryTraces_FilterModel(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	seedTraces(t, fs)

	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{Model: "claude-opus-4"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "t4", results[0].ID)
}

// TestQueryTraces_FilterProvider pins that TraceFilter.Provider is
// honoured by the FileStore; a regression would silently let through
// traces from the wrong provider.
func TestQueryTraces_FilterProvider(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		id       string
		provider string
		model    string
	}{
		{"p1", "anthropic", "claude-sonnet-4-6"},
		{"p2", "anthropic", "claude-haiku-3"},
		{"p3", "openai-responses", "gpt-4o"},
		{"p4", "gemini", "gemini-2.5-pro"},
	} {
		tr := makeTrace(tc.id, "success", "execution", tc.model, base, 1000, 1, types.TokenUsage{Input: 10, Output: 20})
		tr.Config.Provider.Type = tc.provider
		require.NoError(t, fs.StoreTrace(ctx, tr), "StoreTrace %s", tc.id)
	}

	cases := []struct {
		provider string
		wantIDs  []string
	}{
		{"anthropic", []string{"p1", "p2"}},
		{"openai-responses", []string{"p3"}},
		{"gemini", []string{"p4"}},
		{"bedrock", nil},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			got, err := fs.QueryTraces(ctx, types.TraceFilter{Provider: tc.provider})
			require.NoError(t, err)
			gotIDs := make([]string, 0, len(got))
			for _, tr := range got {
				gotIDs = append(gotIDs, tr.ID)
			}
			require.ElementsMatch(t, tc.wantIDs, gotIDs)
		})
	}
}

func TestQueryTraces_CombinedFilters(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	seedTraces(t, fs)

	after := time.Date(2025, 1, 1, 0, 30, 0, 0, time.UTC)
	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{
		After:   &after,
		Outcome: "success",
		Mode:    "execution",
		Model:   "claude-sonnet-4-6",
	})
	require.NoError(t, err)
	// t3 (02:00, success, execution, sonnet) and t5 (04:00, success, execution, sonnet)
	require.Len(t, results, 2)
}

func TestQueryTraces_Limit(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	seedTraces(t, fs)

	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{Limit: 2})
	require.NoError(t, err)
	require.Len(t, results, 2)
	// Should be sorted descending by StartedAt, so t5 first, then t4
	require.Equal(t, "t5", results[0].ID)
	require.Equal(t, "t4", results[1].ID)
}

func TestQueryTraces_LimitZeroReturnsAll(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	seedTraces(t, fs)

	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{Limit: 0})
	require.NoError(t, err)
	require.Len(t, results, 5)
}

func TestQueryTraces_SortsNewestFirst(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	seedTraces(t, fs)

	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{})
	require.NoError(t, err)
	for i := 1; i < len(results); i++ {
		require.False(t, results[i].StartedAt.After(results[i-1].StartedAt),
			"results not sorted descending at index %d: %v after %v", i, results[i].StartedAt, results[i-1].StartedAt)
	}
}

func TestMetrics_EmptyStoreReportsZero(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)

	m, err := fs.Metrics(context.Background(), types.TraceFilter{})
	require.NoError(t, err)
	require.Zero(t, m.Count)
	require.Zero(t, m.PassRate)
}

func TestMetrics_ComputesAggregates(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	seedTraces(t, fs)

	m, err := fs.Metrics(context.Background(), types.TraceFilter{})
	require.NoError(t, err)

	require.Equal(t, 5, m.Count)

	// 3 out of 5 are success
	require.InDelta(t, 3.0/5.0, m.PassRate, 0.001)

	// Mean turns: (3+5+7+10+2) / 5 = 5.4
	require.InDelta(t, 5.4, m.MeanTurns, 0.001)

	// Mean tokens: total input+output per trace = 300+600+900+1200+1500 = 4500; mean = 900
	require.InDelta(t, 900.0, m.MeanTokens, 0.001)

	// Durations sorted: 1000, 2000, 3000, 4000, 5000
	// P50: index = 0.5 * 4 = 2.0 -> sorted[2] = 3000
	require.InDelta(t, 3000, m.P50Duration, 0.001)

	// P95: index = 0.95 * 4 = 3.8 -> sorted[3] + 0.8*(sorted[4]-sorted[3]) = 4000 + 800 = 4800
	require.InDelta(t, 4800, m.P95Duration, 0.001)
}

func TestMetrics_AppliesFilter(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	seedTraces(t, fs)

	m, err := fs.Metrics(context.Background(), types.TraceFilter{Outcome: "success"})
	require.NoError(t, err)

	require.Equal(t, 3, m.Count)
	require.InDelta(t, 1.0, m.PassRate, 0.001)
}

func TestQueryRecordings_FilterByOutcome(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)

	ctx := context.Background()
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	t1 := makeTrace("r1", "success", "execution", "claude-sonnet-4-6", base, 1000, 3, types.TokenUsage{Input: 100, Output: 200})
	t2 := makeTrace("r2", "error", "execution", "claude-sonnet-4-6", base.Add(time.Hour), 2000, 5, types.TokenUsage{Input: 200, Output: 400})

	require.NoError(t, fs.StoreRecording(ctx, makeRecording("r1", t1)))
	require.NoError(t, fs.StoreRecording(ctx, makeRecording("r2", t2)))

	results, err := fs.QueryRecordings(ctx, types.TraceFilter{Outcome: "success"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "r1", results[0].RunID)
}

// TestParentOnlyToolCalls_FiltersForwardedSubAgentEntries asserts the
// helper rejects tool call summaries forwarded from a sub-agent run;
// without filtering, any aggregate over ToolCalls double-counts
// sub-agent activity against the parent run.
func TestParentOnlyToolCalls_FiltersForwardedSubAgentEntries(t *testing.T) {
	trace := types.RunTrace{
		ID: "parent-1",
		ToolCalls: []types.ToolCallSummary{
			// parent-only entry: empty RunID/ParentRunID
			{Name: "read_file", DurationMs: 10, Success: true},
			// parent-only entry: RunID equal to trace.ID
			{Name: "run_command", DurationMs: 20, Success: true, RunID: "parent-1"},
			// forwarded sub-agent entry: ParentRunID set
			{Name: "read_file", DurationMs: 30, Success: true, RunID: "sub-1", ParentRunID: "parent-1"},
			// forwarded sub-agent entry: only RunID set, distinct
			{Name: "run_command", DurationMs: 40, Success: true, RunID: "sub-2"},
		},
	}

	got := parentOnlyToolCalls(trace)
	require.Len(t, got, 2, "parent-only tool calls: %+v", got)
	for _, tc := range got {
		assert.Empty(t, tc.ParentRunID, "parentOnlyToolCalls returned forwarded entry: %+v", tc)
		if tc.RunID != "" {
			assert.Equal(t, trace.ID, tc.RunID, "parentOnlyToolCalls returned cross-run entry: %+v", tc)
		}
	}
}

// TestComputeMetrics_SubAgentToolCallsDoNotInflate verifies that
// running computeMetrics over a trace whose ToolCalls slice contains
// sub-agent forwarded entries does not affect the returned aggregate
// shape.
func TestComputeMetrics_SubAgentToolCallsDoNotInflate(t *testing.T) {
	started := time.Now()
	parentOnly := types.RunTrace{
		ID:          "parent-only",
		Outcome:     "success",
		StartedAt:   started,
		CompletedAt: started.Add(1 * time.Second),
		Turns:       3,
		TokenUsage:  types.TokenUsage{Input: 100, Output: 200},
		ToolCalls: []types.ToolCallSummary{
			{Name: "read_file", DurationMs: 10, Success: true},
			{Name: "run_command", DurationMs: 20, Success: true},
		},
	}
	withSubAgent := parentOnly
	withSubAgent.ID = "with-subagent"
	withSubAgent.ToolCalls = []types.ToolCallSummary{
		{Name: "read_file", DurationMs: 10, Success: true},
		{Name: "run_command", DurationMs: 20, Success: true},
		// Forwarded sub-agent entries that must not affect aggregates.
		{Name: "read_file", DurationMs: 30, Success: true, RunID: "sub-1", ParentRunID: "with-subagent"},
		{Name: "run_command", DurationMs: 40, Success: true, RunID: "sub-1", ParentRunID: "with-subagent"},
		{Name: "read_file", DurationMs: 50, Success: true, RunID: "sub-1", ParentRunID: "with-subagent"},
	}

	mParent := computeMetrics([]types.RunTrace{parentOnly})
	mSub := computeMetrics([]types.RunTrace{withSubAgent})

	assert.Equal(t, mParent.Count, mSub.Count, "Count")
	assert.InDelta(t, mParent.MeanTurns, mSub.MeanTurns, 1e-9, "MeanTurns")
	assert.InDelta(t, mParent.MeanTokens, mSub.MeanTokens, 1e-9, "MeanTokens")
	assert.InDelta(t, mParent.PassRate, mSub.PassRate, 1e-9, "PassRate")
}

// makeBatchTrace is makeTrace's twin with Batch enabled on the
// Provider config, centralising the BatchProviderConfig wiring
// computeMetrics keys its bucketing on.
func makeBatchTrace(id, outcome, model string, started time.Time, durationMs int64, turns int, tokens types.TokenUsage) types.RunTrace {
	tr := makeTrace(id, outcome, "execution", model, started, durationMs, turns, tokens)
	tr.Config.Provider = types.ProviderConfig{
		Type:  "anthropic",
		Batch: &types.BatchProviderConfig{Enabled: true},
	}
	return tr
}

// TestMetrics_BucketsStreamingAndBatchSeparately pins that a mixed
// window of streaming and batch runs produces two independent
// duration percentile pairs, so batch queue time does not skew the
// streaming latency signal.
func TestMetrics_BucketsStreamingAndBatchSeparately(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)

	ctx := context.Background()
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	// Streaming: 100ms, 200ms, 300ms — P50 = 200, P95 ≈ 290.
	// Batch:     1000ms, 5000ms, 10000ms — P50 = 5000, P95 ≈ 9500.
	traces := []types.RunTrace{
		makeTrace("s1", "success", "execution", "claude-sonnet-4-6", base, 100, 1, types.TokenUsage{}),
		makeTrace("s2", "success", "execution", "claude-sonnet-4-6", base.Add(time.Hour), 200, 1, types.TokenUsage{}),
		makeTrace("s3", "success", "execution", "claude-sonnet-4-6", base.Add(2*time.Hour), 300, 1, types.TokenUsage{}),
		makeBatchTrace("b1", "success", "claude-sonnet-4-6", base.Add(3*time.Hour), 1000, 1, types.TokenUsage{}),
		makeBatchTrace("b2", "success", "claude-sonnet-4-6", base.Add(4*time.Hour), 5000, 1, types.TokenUsage{}),
		makeBatchTrace("b3", "success", "claude-sonnet-4-6", base.Add(5*time.Hour), 10000, 1, types.TokenUsage{}),
	}
	for _, tr := range traces {
		require.NoError(t, fs.StoreTrace(ctx, tr), "StoreTrace %s", tr.ID)
	}

	m, err := fs.Metrics(ctx, types.TraceFilter{})
	require.NoError(t, err)

	assert.Equal(t, 6, m.Count, "Count")
	assert.InDelta(t, 200, m.P50Duration, 0.001, "streaming P50")
	// P95 of [100,200,300] using rank=0.95*2=1.9 -> 200 + 0.9*100 = 290
	assert.InDelta(t, 290, m.P95Duration, 0.001, "streaming P95")
	assert.InDelta(t, 5000, m.BatchP50Duration, 0.001, "batch P50")
	// P95 of [1000,5000,10000] using rank=0.95*2=1.9 -> 5000 + 0.9*5000 = 9500
	assert.InDelta(t, 9500, m.BatchP95Duration, 0.001, "batch P95")
}

// TestMetrics_BatchBucketsZeroWhenNoBatchTraces pins the zero-value
// convention called out in the TraceMetrics doc: an empty batch
// bucket reports 0 (not NaN, not nil-equivalent) so downstream JSON
// consumers see a stable shape.
func TestMetrics_BatchBucketsZeroWhenNoBatchTraces(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	seedTraces(t, fs)

	m, err := fs.Metrics(context.Background(), types.TraceFilter{})
	require.NoError(t, err)
	assert.Zero(t, m.BatchP50Duration, "batch P50 with no batch traces seeded")
	assert.Zero(t, m.BatchP95Duration, "batch P95 with no batch traces seeded")
}

// TestMetrics_LegacyTracesCountAsStreaming pins the backward-compat
// promise: a RunTrace whose Provider.Batch is nil (or Enabled=false)
// falls into the streaming bucket. Without this guarantee, lakehouses
// that migrated from a pre-#138 schema would drop legacy traces out
// of every duration percentile pair.
func TestMetrics_LegacyTracesCountAsStreaming(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)

	ctx := context.Background()
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	// Legacy: no Provider field populated at all.
	legacy := types.RunTrace{
		ID:          "legacy-1",
		Outcome:     "success",
		StartedAt:   base,
		CompletedAt: base.Add(500 * time.Millisecond),
		Turns:       2,
	}
	// Explicit Batch=nil-equivalent: Provider with no Batch.
	explicitStreaming := makeTrace("stream-1", "success", "execution", "claude-sonnet-4-6", base.Add(time.Hour), 700, 2, types.TokenUsage{})
	// Batch present but disabled — must still bucket as streaming.
	disabledBatch := makeTrace("disabled-1", "success", "execution", "claude-sonnet-4-6", base.Add(2*time.Hour), 900, 2, types.TokenUsage{})
	disabledBatch.Config.Provider = types.ProviderConfig{
		Type:  "anthropic",
		Batch: &types.BatchProviderConfig{Enabled: false},
	}
	for _, tr := range []types.RunTrace{legacy, explicitStreaming, disabledBatch} {
		require.NoError(t, fs.StoreTrace(ctx, tr), "StoreTrace %s", tr.ID)
	}

	m, err := fs.Metrics(ctx, types.TraceFilter{})
	require.NoError(t, err)
	// All three landed in streaming. Median of [500,700,900] = 700.
	assert.InDelta(t, 700, m.P50Duration, 0.001, "streaming P50")
	assert.Zero(t, m.BatchP50Duration, "batch P50 with no enabled-batch traces")
	assert.Zero(t, m.BatchP95Duration, "batch P95 with no enabled-batch traces")
}

// TestIsBatchRun_ClassifiesByBatchEnabledFlag exercises the classifier
// directly so a future refactor that swaps the predicate has a single
// failure point rather than ricocheting through every bucketing test.
func TestIsBatchRun_ClassifiesByBatchEnabledFlag(t *testing.T) {
	cases := []struct {
		name  string
		trace types.RunTrace
		want  bool
	}{
		{"nil provider", types.RunTrace{}, false},
		{
			"provider without batch",
			types.RunTrace{Config: types.RunConfig{Provider: types.ProviderConfig{Type: "anthropic"}}},
			false,
		},
		{
			"batch disabled",
			types.RunTrace{Config: types.RunConfig{Provider: types.ProviderConfig{
				Batch: &types.BatchProviderConfig{Enabled: false},
			}}},
			false,
		},
		{
			"batch enabled",
			types.RunTrace{Config: types.RunConfig{Provider: types.ProviderConfig{
				Batch: &types.BatchProviderConfig{Enabled: true},
			}}},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isBatchRun(tc.trace))
		})
	}
}

// TestComputeMetrics_SingleTrace covers the percentile single-element
// branch (the n==1 short-circuit). Every existing bucketing test
// uses 3+ element datasets, so the early return that prevents an
// out-of-range index on a one-trace window was untested. Pin both
// P50 and P95 to the lone trace's duration — interpolation must
// degenerate to the only available value.
func TestComputeMetrics_SingleTrace(t *testing.T) {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	traces := []types.RunTrace{
		makeTrace("only", "success", "execution", "claude-sonnet-4-6", base, 750, 1, types.TokenUsage{}),
	}

	m := computeMetrics(traces)

	assert.Equal(t, 1, m.Count)
	assert.InDelta(t, 750, m.P50Duration, 0.001, "single-element P50")
	assert.InDelta(t, 750, m.P95Duration, 0.001, "single-element P95")
}

func TestFileStore_Close_ReturnsNil(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, fs.Close())
}

// TestWriteJSON_ConcurrentStoreTraceIsAtomic pins that two goroutines
// writing the same trace ID concurrently leave a valid JSON document
// on disk, never a torn file with a JSON prefix followed by stale
// trailing bytes.
func TestWriteJSON_ConcurrentStoreTraceIsAtomic(t *testing.T) {
	fs, err := NewFileStore(t.TempDir())
	require.NoError(t, err)
	defer func() { _ = fs.Close() }()

	id := "run-concurrent"
	traceA := makeTrace(id, "success", "execution", "claude-3-5-sonnet", time.Now(), 100, 1, types.TokenUsage{Input: 10, Output: 5})
	traceB := makeTrace(id, "error", "execution", "claude-3-5-sonnet", time.Now(), 200, 2, types.TokenUsage{Input: 20, Output: 8})

	const iters = 50
	done := make(chan struct{}, 2)
	go func() {
		for i := 0; i < iters; i++ {
			_ = fs.StoreTrace(context.Background(), traceA)
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < iters; i++ {
			_ = fs.StoreTrace(context.Background(), traceB)
		}
		done <- struct{}{}
	}()
	<-done
	<-done

	// The final file must parse as valid JSON matching one of the two
	// inputs (the last successful rename wins, but either is valid).
	got, err := fs.QueryTraces(context.Background(), types.TraceFilter{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, id, got[0].ID)
	require.Contains(t, []string{"success", "error"}, got[0].Outcome)
}

// TestWriteJSON_TempFileCleanedOnRenameFailure pins that a failure on
// the rename path leaves no leaked .tmp-*.json files. The repro uses
// a target directory the rename cannot succeed against (a path under
// a removed directory) and asserts the temp file is gone afterwards.
func TestWriteJSON_TempFileCleanedOnRenameFailure(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	require.NoError(t, err)
	defer func() { _ = fs.Close() }()

	// Force a rename failure by passing a path under a non-existent
	// subdirectory. os.CreateTemp succeeds (it creates inside the
	// existing root), but the rename to the bad path fails.
	badPath := filepath.Join(dir, "missing", "out.json")
	require.Error(t, fs.writeJSON(badPath, map[string]any{"x": 1}))

	// Walk the root: no .tmp-*.json should remain.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var leaked []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp") {
			leaked = append(leaked, e.Name())
		}
	}
	require.Empty(t, leaked, "temp files left after rename failure")
}
