package core

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
	tracereader "github.com/rxbynerd/stirrup/types/trace"
)

const (
	replaySpawnTurn      = `{"kind":"turn_record","runId":"rec-1","turn":1,"modelOutput":[{"type":"tool_use","id":"p1","name":"spawn_agent","input":{"prompt":"Write child.txt.","mode":"execution","max_turns":3}}]}`
	replayParentDoneTurn = `{"kind":"turn_record","runId":"rec-1","turn":2,"modelOutput":[{"type":"text","text":"Parent done."}]}`
)

func replaySpawnConfig(t *testing.T, recording, workspace string) *types.RunConfig {
	t.Helper()
	timeout := 30
	enforce := false
	return &types.RunConfig{
		RunID:            "replay-run",
		Mode:             "execution",
		Prompt:           "Delegate writing child.txt.",
		Provider:         types.ProviderConfig{Type: "replay", ReplayFile: recording},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Model: "replay"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: workspace},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl", FilePath: filepath.Join(t.TempDir(), "trace.jsonl")},
		Tools:            types.ToolsConfig{BuiltIn: []string{"read_file", "edit_file", "spawn_agent"}},
		RuleOfTwo:        &types.RuleOfTwoConfig{Enforce: &enforce},
		MaxTurns:         4,
		Timeout:          &timeout,
	}
}

func runReplaySpawn(t *testing.T, config *types.RunConfig) *types.RunTrace {
	t.Helper()
	must := require.New(t)
	must.NoError(types.ValidateRunConfig(config))
	loop, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
	must.NoError(err)
	defer func() { _ = loop.Close() }()
	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	return runTrace
}

// TestBuildLoop_ReplayProvider_ServesRecordedSubRunToSubAgent pins that a
// sub-agent spawned under a replay serves its own recorded turns, matched
// by its run ID relative to the parent, while the parent goes on to its
// own next turn.
func TestBuildLoop_ReplayProvider_ServesRecordedSubRunToSubAgent(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	workspace := t.TempDir()
	recording := writeReplayRecording(t,
		`{"kind":"run_started","runId":"rec-1"}`,
		replaySpawnTurn,
		`{"kind":"turn_record","runId":"rec-1/sub-1","parentRunId":"rec-1","turn":1,"modelOutput":[{"type":"tool_use","id":"s1","name":"edit_file","input":{"path":"child.txt","operation":"rewrite","content":"from the child\n"}}]}`,
		`{"kind":"turn_record","runId":"rec-1/sub-1","parentRunId":"rec-1","turn":2,"modelOutput":[{"type":"text","text":"Child done."}]}`,
		replayParentDoneTurn,
	)

	runTrace := runReplaySpawn(t, replaySpawnConfig(t, recording, workspace))

	must.Equal("success", runTrace.Outcome)
	check.Equal("Parent done.", runTrace.FinalAssistantText)
	got, err := os.ReadFile(filepath.Join(workspace, "child.txt"))
	must.NoError(err)
	check.Equal("from the child\n", string(got))
	must.Len(runTrace.ToolCalls, 2)
	check.Equal("edit_file", runTrace.ToolCalls[0].Name)
	check.Equal("replay-run/sub-1", runTrace.ToolCalls[0].RunID, "the child ran the recorded sub-run under its own run ID")
	check.True(runTrace.ToolCalls[0].Success)
	check.Equal("spawn_agent", runTrace.ToolCalls[1].Name)
	check.True(runTrace.ToolCalls[1].Success)
}

// TestBuildLoop_ReplayProvider_SubAgentWithoutRecordedTurnsFails pins that
// a spawn the recording holds no turns for fails with an error naming the
// child, rather than drawing the parent's next recorded turn.
func TestBuildLoop_ReplayProvider_SubAgentWithoutRecordedTurnsFails(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	workspace := t.TempDir()
	recording := writeReplayRecording(t,
		`{"kind":"run_started","runId":"rec-1"}`,
		replaySpawnTurn,
		replayParentDoneTurn,
	)
	config := replaySpawnConfig(t, recording, workspace)

	runTrace := runReplaySpawn(t, config)

	must.Equal("success", runTrace.Outcome, "the parent still serves its own next turn")
	check.Equal("Parent done.", runTrace.FinalAssistantText)
	must.Len(runTrace.ToolCalls, 1)
	check.False(runTrace.ToolCalls[0].Success)

	reader, err := tracereader.Open(config.TraceEmitter.FilePath)
	must.NoError(err)
	defer func() { _ = reader.Close() }()
	rec, err := reader.ReadRecording()
	must.NoError(err)
	must.NotEmpty(rec.Turns)
	must.Len(rec.Turns[0].ToolCalls, 1)
	check.Contains(rec.Turns[0].ToolCalls[0].Output, `no recorded turns for sub-agent "replay-run/sub-1"`)
	check.Empty(rec.SubRuns, "the child must not have run")
}

// TestBuildLoop_ReplayProvider_RecordedOutputLimitStopRecovers pins that
// a recorded max_tokens stop replays as one, so the loop takes the
// output-limit recovery path the live run took and serves the recorded
// continuation as the next turn.
func TestBuildLoop_ReplayProvider_RecordedOutputLimitStopRecovers(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	recording := writeReplayRecording(t,
		`{"kind":"turn_record","turn":0,"stopReason":"max_tokens","modelOutput":[{"type":"text","text":"The answer is "}]}`,
		`{"kind":"turn_record","turn":1,"stopReason":"end_turn","modelOutput":[{"type":"text","text":"forty-two."}]}`,
	)
	config := replaySpawnConfig(t, recording, t.TempDir())

	runTrace := runReplaySpawn(t, config)

	must.Equal("success", runTrace.Outcome)
	check.Equal(2, runTrace.Turns)
	check.Equal("The answer is forty-two.", runTrace.FinalAssistantText)

	reader, err := tracereader.Open(config.TraceEmitter.FilePath)
	must.NoError(err)
	defer func() { _ = reader.Close() }()
	rec, err := reader.ReadRecording()
	must.NoError(err)
	must.Len(rec.Turns, 2)
	check.Equal("max_tokens", rec.Turns[0].StopReason, "the stop reason is recorded for the next replay")
	check.True(rec.Turns[0].OutputLimitRecovery)
	check.Equal("end_turn", rec.Turns[1].StopReason)
}
