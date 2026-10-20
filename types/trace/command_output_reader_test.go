package trace

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestSynthesizeLegacyCommandOutputs_AddsLegacyRecordWithoutDuplicatingNative(t *testing.T) {
	must := require.New(t)
	recording := &types.RunRecording{RunID: "run-1", Turns: []types.TurnRecord{{Turn: 1, ToolCalls: []types.ToolCallRecord{{ID: "tool-1", Name: "run_command", Output: "legacy output", Success: true}}}}}

	synthesizeLegacyCommandOutputs(recording)

	must.Len(recording.CommandOutputs, 1, "outputs=%+v", recording.CommandOutputs)
	must.True(recording.CommandOutputs[0].LegacySingleRepresentation, "outputs=%+v", recording.CommandOutputs)
	must.Equal("run-1", recording.CommandOutputs[0].RunID, "record=%+v", recording.CommandOutputs[0])
	must.Equal(int64(len("legacy output")), recording.CommandOutputs[0].Stdout.RawBytes, "record=%+v", recording.CommandOutputs[0])

	recording.CommandOutputs = []types.CommandOutputRecord{{RunID: "run-1", ToolUseID: "tool-1", CaptureComplete: true}}

	synthesizeLegacyCommandOutputs(recording)

	must.Len(recording.CommandOutputs, 1, "native record duplicated: %+v", recording.CommandOutputs)
}
