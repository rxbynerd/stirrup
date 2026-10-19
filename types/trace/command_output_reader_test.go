package trace

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestSynthesizeLegacyCommandOutputs_AddsLegacyRecordWithoutDuplicatingNative(t *testing.T) {
	recording := &types.RunRecording{RunID: "run-1", Turns: []types.TurnRecord{{Turn: 1, ToolCalls: []types.ToolCallRecord{{ID: "tool-1", Name: "run_command", Output: "legacy output", Success: true}}}}}

	synthesizeLegacyCommandOutputs(recording)

	require.Len(t, recording.CommandOutputs, 1, "outputs=%+v", recording.CommandOutputs)
	require.True(t, recording.CommandOutputs[0].LegacySingleRepresentation, "outputs=%+v", recording.CommandOutputs)
	require.Equal(t, "run-1", recording.CommandOutputs[0].RunID, "record=%+v", recording.CommandOutputs[0])
	require.Equal(t, int64(len("legacy output")), recording.CommandOutputs[0].Stdout.RawBytes, "record=%+v", recording.CommandOutputs[0])

	recording.CommandOutputs = []types.CommandOutputRecord{{RunID: "run-1", ToolUseID: "tool-1", CaptureComplete: true}}

	synthesizeLegacyCommandOutputs(recording)

	require.Len(t, recording.CommandOutputs, 1, "native record duplicated: %+v", recording.CommandOutputs)
}
