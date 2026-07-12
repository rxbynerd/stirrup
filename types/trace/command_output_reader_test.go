package trace

import (
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func TestSynthesizeLegacyCommandOutputs(t *testing.T) {
	recording := &types.RunRecording{RunID: "run-1", Turns: []types.TurnRecord{{Turn: 1, ToolCalls: []types.ToolCallRecord{{ID: "tool-1", Name: "run_command", Output: "legacy output", Success: true}}}}}
	synthesizeLegacyCommandOutputs(recording)
	if len(recording.CommandOutputs) != 1 || !recording.CommandOutputs[0].LegacySingleRepresentation {
		t.Fatalf("outputs=%+v", recording.CommandOutputs)
	}
	if recording.CommandOutputs[0].RunID != "run-1" || recording.CommandOutputs[0].Stdout.RawBytes != int64(len("legacy output")) {
		t.Fatalf("record=%+v", recording.CommandOutputs[0])
	}
	recording.CommandOutputs = []types.CommandOutputRecord{{RunID: "run-1", ToolUseID: "tool-1", CaptureComplete: true}}
	synthesizeLegacyCommandOutputs(recording)
	if len(recording.CommandOutputs) != 1 {
		t.Fatalf("native record duplicated: %+v", recording.CommandOutputs)
	}
}
