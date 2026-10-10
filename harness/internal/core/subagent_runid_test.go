package core

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/types"
	tracereader "github.com/rxbynerd/stirrup/types/trace"
)

func TestSubAgentRunID(t *testing.T) {
	if got := subAgentRunID("run-p", 3); got != "run-p/sub-3" {
		t.Errorf("subAgentRunID(run-p, 3) = %q, want run-p/sub-3", got)
	}
	if got := subAgentRunID("", 1); got != "sub-1" {
		t.Errorf("subAgentRunID(\"\", 1) = %q, want sub-1", got)
	}
}

// TestSpawnSubAgent_ChildRunIDsCountSpawnsPerRun pins that each spawn
// within a run gets the next "<parent>/sub-<n>" ID, and that a new run
// on the same loop numbers its spawns from 1 again.
func TestSpawnSubAgent_ChildRunIDsCountSpawnsPerRun(t *testing.T) {
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
		if _, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{Prompt: "subtask"}); err != nil {
			t.Fatalf("SpawnSubAgent: %v", err)
		}
	}
	spawn()
	spawn()
	if _, err := parentLoop.Run(context.Background(), parentConfig); err != nil {
		t.Fatalf("Run: %v", err)
	}
	spawn()

	turns, _ := parentEmitter.snapshot()
	var childIDs []string
	for _, turn := range turns {
		if turn.ParentRunID != "" {
			childIDs = append(childIDs, turn.RunID)
		}
	}
	want := []string{"test-run-1/sub-1", "test-run-1/sub-2", "test-run-1/sub-1"}
	if !slices.Equal(childIDs, want) {
		t.Errorf("child run IDs = %v, want %v", childIDs, want)
	}
}

// TestSpawnSubAgent_JSONLTraceGroupsChildren runs two sub-agents
// against a parent JSONL emitter and reads the file back: each child's
// turn_record lines carry its own run ID and the parent's, and the
// reader groups them into separate sub-runs with their outcomes.
func TestSpawnSubAgent_JSONLTraceGroupsChildren(t *testing.T) {
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
		if _, err := SpawnSubAgent(context.Background(), parentLoop, parentConfig, SubAgentConfig{Prompt: "subtask"}); err != nil {
			t.Fatalf("SpawnSubAgent: %v", err)
		}
	}
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	rec, err := tracereader.NewReader(bytes.NewReader(buf.Bytes())).ReadRecording()
	if err != nil {
		t.Fatalf("ReadRecording: %v", err)
	}
	if len(rec.Turns) != 0 {
		t.Errorf("parent Turns = %d, want 0: child turns leaked into the parent history", len(rec.Turns))
	}
	if len(rec.SubRuns) != 2 {
		t.Fatalf("SubRuns = %d, want 2", len(rec.SubRuns))
	}
	for i, sub := range rec.SubRuns {
		wantID := subAgentRunID(parentConfig.RunID, int64(i+1))
		if sub.RunID != wantID || sub.ParentRunID != parentConfig.RunID {
			t.Errorf("SubRuns[%d] ids = (%q, %q), want (%q, %q)", i, sub.RunID, sub.ParentRunID, wantID, parentConfig.RunID)
		}
		if sub.Config.RunID != wantID || sub.Config.Prompt != "subtask" {
			t.Errorf("SubRuns[%d].Config = (%q, %q), want the child's config", i, sub.Config.RunID, sub.Config.Prompt)
		}
		if sub.Config.Provider.APIKeyRef == "secret://TEST" {
			t.Errorf("SubRuns[%d].Config carries the unredacted key reference", i)
		}
		if len(sub.Turns) != 1 {
			t.Fatalf("SubRuns[%d].Turns = %d, want 1", i, len(sub.Turns))
		}
		if turn := sub.Turns[0]; turn.RunID != wantID || turn.ParentRunID != parentConfig.RunID {
			t.Errorf("SubRuns[%d] turn tags = (%q, %q)", i, turn.RunID, turn.ParentRunID)
		}
		if sub.Summary == nil || sub.Summary.Outcome != "success" || sub.Summary.Turns != 1 {
			t.Errorf("SubRuns[%d].Summary = %+v, want a one-turn success", i, sub.Summary)
		}
	}
}
