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
