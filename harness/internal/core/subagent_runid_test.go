package core

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/types"
	tracereader "github.com/rxbynerd/stirrup/types/trace"
)

func TestSubAgentRunID_FormatsParentAndIndex(t *testing.T) {
	tests := []struct {
		name     string
		parentID string
		index    int64
		want     string
	}{
		{name: "with parent run", parentID: "run-p", index: 3, want: "run-p/sub-3"},
		{name: "without parent run", parentID: "", index: 1, want: "sub-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, subAgentRunID(tc.parentID, tc.index))
		})
	}
}

// TestSpawnSubAgent_ChildRunIDsCountSpawnsPerRun pins that each spawn
// within a run gets the next "<parent>/sub-<n>" ID, and that a new run
// on the same loop numbers its spawns from 1 again.
func TestSpawnSubAgent_ChildRunIDsCountSpawnsPerRun(t *testing.T) {
	must := require.New(t)
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "done"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	parentEmitter := &recordingTraceEmitter{}
	parentLoop := buildSubAgentTestLoop(prov)
	parentLoop.Trace = parentEmitter
	parentConfig := buildTestConfig()

	spawn := func() {
		t.Helper()
		_, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{Prompt: "subtask"})
		must.NoError(err, "SpawnSubAgent")
	}
	spawn()
	spawn()
	_, err := parentLoop.Run(context.Background(), parentConfig)
	must.NoError(err, "Run")
	spawn()

	turns, _ := parentEmitter.snapshot()
	var childIDs []string
	for _, turn := range turns {
		if turn.ParentRunID != "" {
			childIDs = append(childIDs, turn.RunID)
		}
	}
	want := []string{"test-run-1/sub-1", "test-run-1/sub-2", "test-run-1/sub-1"}
	must.Equal(want, childIDs)
}

// TestSpawnSubAgent_JSONLTraceGroupsChildren runs two sub-agents
// against a parent JSONL emitter and reads the file back: each child's
// turn_record lines carry its own run ID and the parent's, and the
// reader groups them into separate sub-runs with their outcomes.
func TestSpawnSubAgent_JSONLTraceGroupsChildren(t *testing.T) {
	must := require.New(t)
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "child reply"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}
	var buf bytes.Buffer
	emitter := trace.NewJSONLTraceEmitter(&buf, false)
	parentLoop := buildSubAgentTestLoop(prov)
	parentLoop.Trace = emitter
	parentConfig := buildTestConfig()
	emitter.Start(parentConfig.RunID, parentConfig)

	for range 2 {
		_, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{Prompt: "subtask"})
		must.NoError(err, "SpawnSubAgent")
	}
	_, err := emitter.Finish(context.Background(), "success")
	must.NoError(err, "Finish")

	rec, err := tracereader.NewReader(bytes.NewReader(buf.Bytes())).ReadRecording()
	must.NoError(err, "ReadRecording")
	must.Empty(rec.Turns, "child turns leaked into the parent history")
	must.Len(rec.SubRuns, 2)
	for i, sub := range rec.SubRuns {
		t.Run(fmt.Sprintf("sub-run %d", i+1), func(t *testing.T) {
			must := require.New(t)
			wantID := subAgentRunID(parentConfig.RunID, int64(i+1))
			must.Equal(wantID, sub.RunID, "sub-run run ID")
			must.Equal(parentConfig.RunID, sub.ParentRunID, "sub-run parent run ID")
			must.Equal(wantID, sub.Config.RunID, "child config run ID")
			must.Equal("subtask", sub.Config.Prompt, "child config prompt")
			must.NotEqual("secret://TEST", sub.Config.Provider.APIKeyRef, "child config carries the unredacted key reference")

			must.Len(sub.Turns, 1)
			turn := sub.Turns[0]
			must.Equal(wantID, turn.RunID, "turn run ID")
			must.Equal(parentConfig.RunID, turn.ParentRunID, "turn parent run ID")

			must.NotNil(sub.Summary)
			must.Equal("success", sub.Summary.Outcome)
			must.Equal(1, sub.Summary.Turns)
		})
	}
}
