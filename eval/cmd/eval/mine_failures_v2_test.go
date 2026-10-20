package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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

	t.Run("default keeps only failed traces", func(t *testing.T) {
		must := require.New(t)
		got := filterTracesForMining(traces, types.EvalFailed, false, false)
		must.ElementsMatch([]string{"t2", "t4"}, traceIDs(got))
	})

	t.Run("include-inconclusive also keeps inconclusive traces", func(t *testing.T) {
		must := require.New(t)
		got := filterTracesForMining(traces, types.EvalFailed, true, false)
		must.ElementsMatch([]string{"t2", "t3", "t4"}, traceIDs(got))
	})
}

// TestFilterTracesForMining_ExcludesBatchByDefault pins the batch-exclusion
// default.
func TestFilterTracesForMining_ExcludesBatchByDefault(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stream := makeTraceWithOutcomeMode("s1", "error", "execution", "anthropic", "claude", base)
	batch := makeTraceWithOutcomeMode("b1", "error", "execution", "anthropic", "claude", base)
	batch.Config.Provider.Batch = &types.BatchProviderConfig{Enabled: true}

	t.Run("excludes batch traces by default", func(t *testing.T) {
		must := require.New(t)
		got := filterTracesForMining([]types.RunTrace{stream, batch}, types.EvalFailed, false, false)
		must.Equal([]string{"s1"}, traceIDs(got))
	})

	t.Run("includes batch traces when requested", func(t *testing.T) {
		must := require.New(t)
		got := filterTracesForMining([]types.RunTrace{stream, batch}, types.EvalFailed, false, true)
		must.Len(got, 2)
	})
}

// TestSampleTraces_LimitTakesFirstN pins that with no stratification, limit
// truncates to the first N in input order (StartedAt-desc from QueryTraces).
func TestSampleTraces_LimitTakesFirstN(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	traces := []types.RunTrace{
		makeTraceWithOutcomeMode("a", "error", "execution", "anthropic", "claude", base),
		makeTraceWithOutcomeMode("b", "error", "execution", "anthropic", "claude", base),
		makeTraceWithOutcomeMode("c", "error", "execution", "anthropic", "claude", base),
	}

	t.Run("limit truncates to first n in input order", func(t *testing.T) {
		must := require.New(t)
		got := sampleTraces(traces, "", 2)
		must.Equal([]string{"a", "b"}, traceIDs(got))
	})

	t.Run("zero limit returns all traces", func(t *testing.T) {
		must := require.New(t)
		got := sampleTraces(traces, "", 0)
		must.Len(got, 3)
	})
}

// TestSampleTraces_StratifyByModel pins that with three failed traces from
// model A and one from model B, --sample-by model --limit 2 picks one from
// each.
func TestSampleTraces_StratifyByModel(t *testing.T) {
	must := require.New(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	traces := []types.RunTrace{
		makeTraceWithOutcomeMode("a1", "error", "execution", "anthropic", "claude-3-5", base),
		makeTraceWithOutcomeMode("a2", "error", "execution", "anthropic", "claude-3-5", base),
		makeTraceWithOutcomeMode("a3", "error", "execution", "anthropic", "claude-3-5", base),
		makeTraceWithOutcomeMode("b1", "error", "execution", "anthropic", "claude-opus-4", base),
	}
	got := sampleTraces(traces, "model", 2)
	must.Len(got, 2)

	models := map[string]int{}
	for _, tr := range got {
		models[tr.Config.ModelRouter.Model]++
	}
	must.Equal(map[string]int{"claude-3-5": 1, "claude-opus-4": 1}, models)
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

	t.Run("includes the assistant excerpt", func(t *testing.T) {
		must := require.New(t)
		must.Contains(task.Description, "I'll run the failing command now.")
	})

	t.Run("includes the failing tool name", func(t *testing.T) {
		must := require.New(t)
		must.Contains(task.Description, "run_command")
	})
}

// TestBuildMinedTask_ThinTraceFallback pins the no-recording path: the task
// is still emitted, the description flags thin-trace status, and the
// prompt comes from the trace's RunConfig.
func TestBuildMinedTask_ThinTraceFallback(t *testing.T) {
	trace := makeTraceWithOutcomeMode("t1", "error", "execution", "anthropic", "claude", time.Now())
	task := buildMinedTask(trace, types.RunRecording{}, false)

	t.Run("takes the prompt from the trace config", func(t *testing.T) {
		must := require.New(t)
		must.Equal("p-t1", task.Prompt)
	})

	t.Run("flags the thin trace in the description", func(t *testing.T) {
		must := require.New(t)
		must.Contains(task.Description, "thin trace")
	})
}

// TestSampleTraces_StratifyByOutcome pins that with failed + inconclusive
// traces, sampling by outcome at a small limit pulls one from each
// non-empty stratum before doubling up.
func TestSampleTraces_StratifyByOutcome(t *testing.T) {
	must := require.New(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	traces := []types.RunTrace{
		makeTraceWithOutcomeMode("e1", "error", "execution", "anthropic", "claude", base),
		makeTraceWithOutcomeMode("e2", "error", "execution", "anthropic", "claude", base),
		makeTraceWithOutcomeMode("e3", "error", "execution", "anthropic", "claude", base),
		makeTraceWithOutcomeMode("i1", "max_turns", "execution", "anthropic", "claude", base),
	}
	got := sampleTraces(traces, "outcome", 2)
	must.Len(got, 2)

	outcomes := map[types.EvalOutcome]int{}
	for _, tr := range got {
		outcomes[types.EvalOutcomeFor(tr)]++
	}
	must.Equal(map[types.EvalOutcome]int{types.EvalFailed: 1, types.EvalInconclusive: 1}, outcomes)
}

// --- helpers ---

func traceIDs(traces []types.RunTrace) []string {
	ids := make([]string, len(traces))
	for i, t := range traces {
		ids[i] = t.ID
	}
	return ids
}
