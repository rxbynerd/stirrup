package main

import (
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// makeTraceWithOutcomeMode builds a RunTrace with the fields the mining
// filters consult: outcome, mode, provider type, model, and start time.
func makeTraceWithOutcomeMode(id, outcome, mode, provider, model string, started time.Time) types.RunTrace {
	return types.RunTrace{
		ID:        id,
		Outcome:   outcome,
		StartedAt: started,
		Config: types.RunConfig{
			Prompt: "p-" + id,
			Mode:   mode,
			Provider: types.ProviderConfig{
				Type: provider,
			},
			ModelRouter: types.ModelRouterConfig{
				Model: model,
			},
		},
	}
}

// TestFilterTracesForMining_FailedOnlyDefault pins that only EvalFailed
// traces match unless --include-inconclusive is set; EvalPassed is always
// excluded.
func TestFilterTracesForMining_FailedOnlyDefault(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	traces := []types.RunTrace{
		makeTraceWithOutcomeMode("t1", "success", "execution", "anthropic", "claude", base),
		makeTraceWithOutcomeMode("t2", "error", "execution", "anthropic", "claude", base),
		makeTraceWithOutcomeMode("t3", "max_turns", "execution", "anthropic", "claude", base),
		makeTraceWithOutcomeMode("t4", "tool_failures", "execution", "anthropic", "claude", base),
	}

	// Default: only EvalFailed (error + tool_failures).
	got := filterTracesForMining(traces, types.EvalFailed, false, false)
	gotIDs := traceIDs(got)
	want := []string{"t2", "t4"}
	if !sameSet(gotIDs, want) {
		t.Errorf("default: got %v, want %v", gotIDs, want)
	}

	// With --include-inconclusive: add t3.
	got = filterTracesForMining(traces, types.EvalFailed, true, false)
	gotIDs = traceIDs(got)
	want = []string{"t2", "t3", "t4"}
	if !sameSet(gotIDs, want) {
		t.Errorf("include-inconclusive: got %v, want %v", gotIDs, want)
	}
}

// TestFilterTracesForMining_ExcludesBatchByDefault pins the batch-exclusion
// default.
func TestFilterTracesForMining_ExcludesBatchByDefault(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stream := makeTraceWithOutcomeMode("s1", "error", "execution", "anthropic", "claude", base)
	batch := makeTraceWithOutcomeMode("b1", "error", "execution", "anthropic", "claude", base)
	batch.Config.Provider.Batch = &types.BatchProviderConfig{Enabled: true}

	got := filterTracesForMining([]types.RunTrace{stream, batch}, types.EvalFailed, false, false)
	if len(got) != 1 || got[0].ID != "s1" {
		t.Errorf("default-excludes-batch: got %v, want [s1]", traceIDs(got))
	}
	got = filterTracesForMining([]types.RunTrace{stream, batch}, types.EvalFailed, false, true)
	if len(got) != 2 {
		t.Errorf("include-batch: got %d, want 2", len(got))
	}
}

// TestSampleTraces_Limit pins that with no stratification, limit truncates
// to the first N in input order (StartedAt-desc from QueryTraces).
func TestSampleTraces_Limit(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	traces := []types.RunTrace{
		makeTraceWithOutcomeMode("a", "error", "execution", "anthropic", "claude", base),
		makeTraceWithOutcomeMode("b", "error", "execution", "anthropic", "claude", base),
		makeTraceWithOutcomeMode("c", "error", "execution", "anthropic", "claude", base),
	}
	got := sampleTraces(traces, "", 2)
	if len(got) != 2 {
		t.Fatalf("limit=2: got %d, want 2", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Errorf("limit=2 order: got %v, want [a, b]", traceIDs(got))
	}

	// limit=0 returns all.
	got = sampleTraces(traces, "", 0)
	if len(got) != 3 {
		t.Errorf("limit=0: got %d, want 3", len(got))
	}
}

// TestSampleTraces_StratifyByModel pins that with three failed traces from
// model A and one from model B, --sample-by model --limit 2 picks one from
// each.
func TestSampleTraces_StratifyByModel(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	traces := []types.RunTrace{
		makeTraceWithOutcomeMode("a1", "error", "execution", "anthropic", "claude-3-5", base),
		makeTraceWithOutcomeMode("a2", "error", "execution", "anthropic", "claude-3-5", base),
		makeTraceWithOutcomeMode("a3", "error", "execution", "anthropic", "claude-3-5", base),
		makeTraceWithOutcomeMode("b1", "error", "execution", "anthropic", "claude-opus-4", base),
	}
	got := sampleTraces(traces, "model", 2)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	models := map[string]int{}
	for _, tr := range got {
		models[tr.Config.ModelRouter.Model]++
	}
	if models["claude-3-5"] != 1 || models["claude-opus-4"] != 1 {
		t.Errorf("stratified counts = %v, want one per model", models)
	}
}

// TestBuildMinedTask_HydratedDescriptionIncludesContext pins that when a
// recording is available, the mined task's Description includes the
// failing-turn excerpt (last assistant message + failing tool name).
func TestBuildMinedTask_HydratedDescriptionIncludesContext(t *testing.T) {
	trace := makeTraceWithOutcomeMode("h1", "tool_failures", "execution", "anthropic", "claude", time.Now())
	rec := types.RunRecording{
		RunID:  "h1",
		Config: trace.Config,
		Turns: []types.TurnRecord{
			{
				Turn: 1,
				ModelOutput: []types.ContentBlock{
					{Type: "text", Text: "I'll run the failing command now."},
				},
				ToolCalls: []types.ToolCallRecord{
					{
						Name:    "run_command",
						Output:  "exit 1: permission denied",
						Success: false,
					},
				},
			},
		},
		FinalOutcome: trace,
	}
	task := buildMinedTask(trace, rec, true)
	if !strings.Contains(task.Description, "I'll run the failing command now.") {
		t.Errorf("description missing assistant excerpt: %q", task.Description)
	}
	if !strings.Contains(task.Description, "run_command") {
		t.Errorf("description missing failing tool name: %q", task.Description)
	}
}

// TestBuildMinedTask_ThinTraceFallback pins the no-recording path: the task
// is still emitted, the description flags thin-trace status, and the
// prompt comes from the trace's RunConfig.
func TestBuildMinedTask_ThinTraceFallback(t *testing.T) {
	trace := makeTraceWithOutcomeMode("t1", "error", "execution", "anthropic", "claude", time.Now())
	task := buildMinedTask(trace, types.RunRecording{}, false)
	if task.Prompt != "p-t1" {
		t.Errorf("Prompt = %q, want p-t1", task.Prompt)
	}
	if !strings.Contains(task.Description, "thin trace") {
		t.Errorf("description missing thin-trace marker: %q", task.Description)
	}
}

// TestSampleTraces_StratifyByOutcome pins that with failed + inconclusive
// traces, sampling by outcome at a small limit pulls one from each
// non-empty stratum before doubling up.
func TestSampleTraces_StratifyByOutcome(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	traces := []types.RunTrace{
		makeTraceWithOutcomeMode("e1", "error", "execution", "anthropic", "claude", base),
		makeTraceWithOutcomeMode("e2", "error", "execution", "anthropic", "claude", base),
		makeTraceWithOutcomeMode("e3", "error", "execution", "anthropic", "claude", base),
		makeTraceWithOutcomeMode("i1", "max_turns", "execution", "anthropic", "claude", base),
	}
	got := sampleTraces(traces, "outcome", 2)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	outcomes := map[types.EvalOutcome]int{}
	for _, tr := range got {
		outcomes[types.EvalOutcomeFor(tr)]++
	}
	if outcomes[types.EvalFailed] != 1 || outcomes[types.EvalInconclusive] != 1 {
		t.Errorf("stratified outcomes = %v, want one each", outcomes)
	}
}

// --- helpers ---

func traceIDs(traces []types.RunTrace) []string {
	ids := make([]string, len(traces))
	for i, t := range traces {
		ids[i] = t.ID
	}
	return ids
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	a := map[string]struct{}{}
	for _, s := range got {
		a[s] = struct{}{}
	}
	for _, s := range want {
		if _, ok := a[s]; !ok {
			return false
		}
	}
	return true
}
