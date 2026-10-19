package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
	typestrace "github.com/rxbynerd/stirrup/types/trace"
)

func TestJSONLTraceEmitter_RecordsCommandOutputAndArchiveLink(t *testing.T) {
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)
	emitter.Start("run-1", &types.RunConfig{})
	record := types.CommandOutputRecord{
		ArchiveID: "archive-1", RunID: "run-1", Turn: 1, ToolUseID: "tool-1", CaptureComplete: true,
		Stdout: types.CommandOutputStreamRecord{RawBytes: 99, ScrubbedBytes: 80, RawSHA256: "raw", ScrubbedSHA256: "scrubbed", ArchiveMember: "commands/tool-1/stdout.txt"},
	}
	emitter.RecordCommandOutput(record)
	emitter.RecordCommandOutputArchive("/tmp/run-1.command-output.tar.gz")
	_, err := emitter.Finish(context.Background(), "success")
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 3, "trace:\n%s", buf.String())

	var event Event
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &event))
	require.Equal(t, EventKindCommandOutputRecord, event.Kind)
	require.NotNil(t, event.CommandOutput, "event=%+v", event)
	require.EqualValues(t, 99, event.CommandOutput.Stdout.RawBytes)
	require.NotContains(t, lines[1], "stdout.txt content", "trace must not embed command stream content")

	recording, err := typestrace.NewReader(strings.NewReader(buf.String())).ReadRecording()
	require.NoError(t, err)
	require.Len(t, recording.CommandOutputs, 1, "recording=%+v", recording)
	require.NotEmpty(t, recording.FinalOutcome.CommandOutputArchive)
}
