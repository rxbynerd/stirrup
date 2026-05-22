package lakehouse

import (
	"context"
	"math"
	"testing"
	"time"

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

func TestStoreAndQueryTrace(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	trace := makeTrace("t1", "success", "execution", "claude-sonnet-4-6", time.Now(), 1000, 3, types.TokenUsage{Input: 100, Output: 200})

	if err := fs.StoreTrace(ctx, trace); err != nil {
		t.Fatalf("StoreTrace: %v", err)
	}

	results, err := fs.QueryTraces(ctx, types.TraceFilter{})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(results))
	}
	if results[0].ID != "t1" {
		t.Fatalf("expected trace ID t1, got %s", results[0].ID)
	}
}

func TestStoreAndQueryRecording(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = fs.Close() }()

	ctx := context.Background()
	trace := makeTrace("r1", "success", "execution", "claude-sonnet-4-6", time.Now(), 500, 2, types.TokenUsage{Input: 50, Output: 100})
	rec := makeRecording("r1", trace)

	if err := fs.StoreRecording(ctx, rec); err != nil {
		t.Fatalf("StoreRecording: %v", err)
	}

	results, err := fs.QueryRecordings(ctx, types.TraceFilter{})
	if err != nil {
		t.Fatalf("QueryRecordings: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 recording, got %d", len(results))
	}
	if results[0].RunID != "r1" {
		t.Fatalf("expected recording RunID r1, got %s", results[0].RunID)
	}
}

func TestStoreTrace_EmptyID(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	err = fs.StoreTrace(context.Background(), types.RunTrace{})
	if err == nil {
		t.Fatal("expected error for empty trace ID")
	}
}

func TestStoreRecording_EmptyRunID(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	err = fs.StoreRecording(context.Background(), types.RunRecording{})
	if err == nil {
		t.Fatal("expected error for empty recording RunID")
	}
}

// TestStoreTrace_RejectsTraversalID pins the path-traversal guard:
// a trace whose ID contains `..` or path separators must be rejected
// before filepath.Join resolution would escape the lakehouse root.
// Mirrors the equivalent guard in StoreRecording.
func TestStoreTrace_RejectsTraversalID(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = fs.Close() }()

	for _, id := range []string{"../evil", "../../etc/passwd", "a/b", "a\\b", "with\x00null", "has space"} {
		if err := fs.StoreTrace(context.Background(), types.RunTrace{ID: id}); err == nil {
			t.Errorf("StoreTrace(%q) = nil, want validation error", id)
		}
		if err := fs.StoreRecording(context.Background(), types.RunRecording{RunID: id}); err == nil {
			t.Errorf("StoreRecording(%q) = nil, want validation error", id)
		}
	}
}

func TestQueryTraces_EmptyStore(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 traces, got %d", len(results))
	}
}

func TestQueryRecordings_EmptyStore(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	results, err := fs.QueryRecordings(context.Background(), types.TraceFilter{})
	if err != nil {
		t.Fatalf("QueryRecordings: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 recordings, got %d", len(results))
	}
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
		if err := fs.StoreTrace(ctx, tr); err != nil {
			t.Fatalf("seed StoreTrace %s: %v", tr.ID, err)
		}
	}
}

func TestQueryTraces_FilterAfter(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	seedTraces(t, fs)

	after := time.Date(2025, 1, 1, 2, 0, 0, 0, time.UTC)
	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{After: &after})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	// t3 is exactly at the boundary (not strictly after), t4 and t5 are after
	if len(results) != 2 {
		t.Fatalf("expected 2 traces after boundary, got %d", len(results))
	}
}

func TestQueryTraces_FilterBefore(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	seedTraces(t, fs)

	before := time.Date(2025, 1, 1, 2, 0, 0, 0, time.UTC)
	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{Before: &before})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	// t1 (00:00) and t2 (01:00) are before 02:00; t3 is exactly at boundary (not strictly before)
	if len(results) != 2 {
		t.Fatalf("expected 2 traces before boundary, got %d", len(results))
	}
}

func TestQueryTraces_FilterOutcome(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	seedTraces(t, fs)

	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{Outcome: "success"})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 success traces, got %d", len(results))
	}
}

func TestQueryTraces_FilterMode(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	seedTraces(t, fs)

	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{Mode: "execution"})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 execution traces, got %d", len(results))
	}
}

func TestQueryTraces_FilterModel(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	seedTraces(t, fs)

	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{Model: "claude-opus-4"})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 opus trace, got %d", len(results))
	}
	if results[0].ID != "t4" {
		t.Fatalf("expected t4, got %s", results[0].ID)
	}
}

func TestQueryTraces_CombinedFilters(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	seedTraces(t, fs)

	after := time.Date(2025, 1, 1, 0, 30, 0, 0, time.UTC)
	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{
		After:   &after,
		Outcome: "success",
		Mode:    "execution",
		Model:   "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	// t3 (02:00, success, execution, sonnet) and t5 (04:00, success, execution, sonnet)
	if len(results) != 2 {
		t.Fatalf("expected 2 traces, got %d", len(results))
	}
}

func TestQueryTraces_Limit(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	seedTraces(t, fs)

	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{Limit: 2})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 traces, got %d", len(results))
	}
	// Should be sorted descending by StartedAt, so t5 first, then t4
	if results[0].ID != "t5" {
		t.Fatalf("expected first result t5, got %s", results[0].ID)
	}
	if results[1].ID != "t4" {
		t.Fatalf("expected second result t4, got %s", results[1].ID)
	}
}

func TestQueryTraces_LimitZeroReturnsAll(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	seedTraces(t, fs)

	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{Limit: 0})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	if len(results) != 5 {
		t.Fatalf("expected 5 traces, got %d", len(results))
	}
}

func TestQueryTraces_SortOrder(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	seedTraces(t, fs)

	results, err := fs.QueryTraces(context.Background(), types.TraceFilter{})
	if err != nil {
		t.Fatalf("QueryTraces: %v", err)
	}
	for i := 1; i < len(results); i++ {
		if results[i].StartedAt.After(results[i-1].StartedAt) {
			t.Fatalf("results not sorted descending at index %d: %v after %v", i, results[i].StartedAt, results[i-1].StartedAt)
		}
	}
}

func TestMetrics_EmptyStore(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	m, err := fs.Metrics(context.Background(), types.TraceFilter{})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if m.Count != 0 {
		t.Fatalf("expected count 0, got %d", m.Count)
	}
	if m.PassRate != 0 {
		t.Fatalf("expected pass rate 0, got %f", m.PassRate)
	}
}

func approxEqual(a, b, epsilon float64) bool {
	return math.Abs(a-b) < epsilon
}

func TestMetrics_Computation(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	seedTraces(t, fs)

	m, err := fs.Metrics(context.Background(), types.TraceFilter{})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}

	if m.Count != 5 {
		t.Fatalf("expected count 5, got %d", m.Count)
	}

	// 3 out of 5 are success
	expectedPassRate := 3.0 / 5.0
	if !approxEqual(m.PassRate, expectedPassRate, 0.001) {
		t.Fatalf("expected pass rate %f, got %f", expectedPassRate, m.PassRate)
	}

	// Mean turns: (3+5+7+10+2) / 5 = 5.4
	if !approxEqual(m.MeanTurns, 5.4, 0.001) {
		t.Fatalf("expected mean turns 5.4, got %f", m.MeanTurns)
	}

	// Mean tokens: total input+output per trace = 300+600+900+1200+1500 = 4500; mean = 900
	if !approxEqual(m.MeanTokens, 900.0, 0.001) {
		t.Fatalf("expected mean tokens 900, got %f", m.MeanTokens)
	}

	// Durations sorted: 1000, 2000, 3000, 4000, 5000
	// P50: index = 0.5 * 4 = 2.0 -> sorted[2] = 3000
	if !approxEqual(m.P50Duration, 3000, 0.001) {
		t.Fatalf("expected P50 3000, got %f", m.P50Duration)
	}

	// P95: index = 0.95 * 4 = 3.8 -> sorted[3] + 0.8*(sorted[4]-sorted[3]) = 4000 + 800 = 4800
	if !approxEqual(m.P95Duration, 4800, 0.001) {
		t.Fatalf("expected P95 4800, got %f", m.P95Duration)
	}
}

func TestMetrics_WithFilter(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	seedTraces(t, fs)

	m, err := fs.Metrics(context.Background(), types.TraceFilter{Outcome: "success"})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}

	if m.Count != 3 {
		t.Fatalf("expected count 3, got %d", m.Count)
	}
	if !approxEqual(m.PassRate, 1.0, 0.001) {
		t.Fatalf("expected pass rate 1.0, got %f", m.PassRate)
	}
}

func TestQueryRecordings_FilterByOutcome(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	ctx := context.Background()
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	t1 := makeTrace("r1", "success", "execution", "claude-sonnet-4-6", base, 1000, 3, types.TokenUsage{Input: 100, Output: 200})
	t2 := makeTrace("r2", "error", "execution", "claude-sonnet-4-6", base.Add(time.Hour), 2000, 5, types.TokenUsage{Input: 200, Output: 400})

	if err := fs.StoreRecording(ctx, makeRecording("r1", t1)); err != nil {
		t.Fatalf("StoreRecording: %v", err)
	}
	if err := fs.StoreRecording(ctx, makeRecording("r2", t2)); err != nil {
		t.Fatalf("StoreRecording: %v", err)
	}

	results, err := fs.QueryRecordings(ctx, types.TraceFilter{Outcome: "success"})
	if err != nil {
		t.Fatalf("QueryRecordings: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 recording, got %d", len(results))
	}
	if results[0].RunID != "r1" {
		t.Fatalf("expected r1, got %s", results[0].RunID)
	}
}

// TestParentOnlyToolCalls_FiltersForwardedSubAgentEntries asserts the
// helper rejects tool call summaries that were forwarded from a sub-
// agent run (#55). The mixed-source contract is documented on
// types.RunTrace.ToolCalls; without filtering, any aggregate over
// ToolCalls double-counts sub-agent activity against the parent run.
func TestParentOnlyToolCalls_FiltersForwardedSubAgentEntries(t *testing.T) {
	trace := types.RunTrace{
		ID: "parent-1",
		ToolCalls: []types.ToolCallSummary{
			// parent-only entry: empty RunID/ParentRunID
			{Name: "read_file", DurationMs: 10, Success: true},
			// parent-only entry: RunID equal to trace.ID (forwarder
			// path tags both parent and child the same way once a
			// future writer normalises the wire shape)
			{Name: "run_command", DurationMs: 20, Success: true, RunID: "parent-1"},
			// forwarded sub-agent entry: ParentRunID set
			{Name: "read_file", DurationMs: 30, Success: true, RunID: "sub-1", ParentRunID: "parent-1"},
			// forwarded sub-agent entry: only RunID set, distinct
			{Name: "run_command", DurationMs: 40, Success: true, RunID: "sub-2"},
		},
	}

	got := parentOnlyToolCalls(trace)
	if len(got) != 2 {
		t.Fatalf("expected 2 parent-only tool calls, got %d (%+v)", len(got), got)
	}
	for _, tc := range got {
		if tc.ParentRunID != "" {
			t.Errorf("parentOnlyToolCalls returned forwarded entry: %+v", tc)
		}
		if tc.RunID != "" && tc.RunID != trace.ID {
			t.Errorf("parentOnlyToolCalls returned cross-run entry: %+v", tc)
		}
	}
}

// TestComputeMetrics_SubAgentToolCallsDoNotInflate verifies that
// running computeMetrics over a trace whose ToolCalls slice contains
// sub-agent forwarded entries does not affect the returned aggregate
// shape. This is the regression guard for the contract described on
// types.RunTrace.ToolCalls: any future per-run tool-count aggregate
// added to TraceMetrics must filter via parentOnlyToolCalls (#55).
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

	if mParent.Count != mSub.Count {
		t.Errorf("Count: parent=%d sub=%d (forwarded entries must not inflate)", mParent.Count, mSub.Count)
	}
	if mParent.MeanTurns != mSub.MeanTurns {
		t.Errorf("MeanTurns: parent=%v sub=%v", mParent.MeanTurns, mSub.MeanTurns)
	}
	if mParent.MeanTokens != mSub.MeanTokens {
		t.Errorf("MeanTokens: parent=%v sub=%v", mParent.MeanTokens, mSub.MeanTokens)
	}
	if mParent.PassRate != mSub.PassRate {
		t.Errorf("PassRate: parent=%v sub=%v", mParent.PassRate, mSub.PassRate)
	}
}

// makeBatchTrace is makeTrace's twin with Batch enabled on the
// Provider config. computeMetrics keys the bucketing on
// Config.Provider.Batch.Enabled, so the helper centralises the
// classifier wiring rather than scattering BatchProviderConfig
// constructions across each #138 test case.
func makeBatchTrace(id, outcome, model string, started time.Time, durationMs int64, turns int, tokens types.TokenUsage) types.RunTrace {
	tr := makeTrace(id, outcome, "execution", model, started, durationMs, turns, tokens)
	tr.Config.Provider = types.ProviderConfig{
		Type:  "anthropic",
		Batch: &types.BatchProviderConfig{Enabled: true},
	}
	return tr
}

// TestMetrics_BucketsStreamingAndBatchSeparately is the core
// regression guard for #138: a mixed window of streaming and batch
// runs must produce two independent duration percentile pairs so
// batch queue time does not skew the streaming latency signal.
func TestMetrics_BucketsStreamingAndBatchSeparately(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

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
		if err := fs.StoreTrace(ctx, tr); err != nil {
			t.Fatalf("StoreTrace %s: %v", tr.ID, err)
		}
	}

	m, err := fs.Metrics(ctx, types.TraceFilter{})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}

	if m.Count != 6 {
		t.Errorf("Count = %d, want 6", m.Count)
	}
	if !approxEqual(m.P50Duration, 200, 0.001) {
		t.Errorf("streaming P50 = %v, want 200", m.P50Duration)
	}
	// P95 of [100,200,300] using rank=0.95*2=1.9 -> 200 + 0.9*100 = 290
	if !approxEqual(m.P95Duration, 290, 0.001) {
		t.Errorf("streaming P95 = %v, want 290", m.P95Duration)
	}
	if !approxEqual(m.BatchP50Duration, 5000, 0.001) {
		t.Errorf("batch P50 = %v, want 5000", m.BatchP50Duration)
	}
	// P95 of [1000,5000,10000] using rank=0.95*2=1.9 -> 5000 + 0.9*5000 = 9500
	if !approxEqual(m.BatchP95Duration, 9500, 0.001) {
		t.Errorf("batch P95 = %v, want 9500", m.BatchP95Duration)
	}
}

// TestMetrics_BatchBucketsZeroWhenNoBatchTraces pins the zero-value
// convention called out in the TraceMetrics doc: an empty batch
// bucket reports 0 (not NaN, not nil-equivalent) so downstream JSON
// consumers see a stable shape.
func TestMetrics_BatchBucketsZeroWhenNoBatchTraces(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	seedTraces(t, fs)

	m, err := fs.Metrics(context.Background(), types.TraceFilter{})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if m.BatchP50Duration != 0 {
		t.Errorf("BatchP50Duration = %v, want 0 (no batch traces seeded)", m.BatchP50Duration)
	}
	if m.BatchP95Duration != 0 {
		t.Errorf("BatchP95Duration = %v, want 0 (no batch traces seeded)", m.BatchP95Duration)
	}
}

// TestMetrics_LegacyTracesCountAsStreaming pins the backward-compat
// promise: a RunTrace whose Provider.Batch is nil (or Enabled=false)
// falls into the streaming bucket. Without this guarantee, lakehouses
// that migrated from a pre-#138 schema would drop legacy traces out
// of every duration percentile pair.
func TestMetrics_LegacyTracesCountAsStreaming(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

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
		if err := fs.StoreTrace(ctx, tr); err != nil {
			t.Fatalf("StoreTrace %s: %v", tr.ID, err)
		}
	}

	m, err := fs.Metrics(ctx, types.TraceFilter{})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	// All three landed in streaming. Median of [500,700,900] = 700.
	if !approxEqual(m.P50Duration, 700, 0.001) {
		t.Errorf("streaming P50 = %v, want 700", m.P50Duration)
	}
	if m.BatchP50Duration != 0 || m.BatchP95Duration != 0 {
		t.Errorf("batch buckets non-zero with no enabled-batch traces: P50=%v P95=%v", m.BatchP50Duration, m.BatchP95Duration)
	}
}

// TestIsBatchRun exercises the classifier directly so a future
// refactor that swaps the predicate has a single failure point
// rather than ricocheting through every bucketing test.
func TestIsBatchRun(t *testing.T) {
	cases := []struct {
		name string
		t    types.RunTrace
		want bool
	}{
		{"nil-provider", types.RunTrace{}, false},
		{
			"provider-without-batch",
			types.RunTrace{Config: types.RunConfig{Provider: types.ProviderConfig{Type: "anthropic"}}},
			false,
		},
		{
			"batch-disabled",
			types.RunTrace{Config: types.RunConfig{Provider: types.ProviderConfig{
				Batch: &types.BatchProviderConfig{Enabled: false},
			}}},
			false,
		},
		{
			"batch-enabled",
			types.RunTrace{Config: types.RunConfig{Provider: types.ProviderConfig{
				Batch: &types.BatchProviderConfig{Enabled: true},
			}}},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBatchRun(tc.t); got != tc.want {
				t.Errorf("isBatchRun(%s) = %v, want %v", tc.name, got, tc.want)
			}
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

	if m.Count != 1 {
		t.Errorf("Count = %d, want 1", m.Count)
	}
	if !approxEqual(m.P50Duration, 750, 0.001) {
		t.Errorf("P50Duration = %v, want 750 (single-element branch)", m.P50Duration)
	}
	if !approxEqual(m.P95Duration, 750, 0.001) {
		t.Errorf("P95Duration = %v, want 750 (single-element branch)", m.P95Duration)
	}
}

func TestClose(t *testing.T) {
	dir := t.TempDir()
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
