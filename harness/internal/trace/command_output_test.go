package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
	typestrace "github.com/rxbynerd/stirrup/types/trace"
)

func TestJSONLCommandOutputRecordAndArchiveLink(t *testing.T) {
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf)
	emitter.Start("run-1", &types.RunConfig{})
	record := types.CommandOutputRecord{
		ArchiveID: "archive-1", RunID: "run-1", Turn: 1, ToolUseID: "tool-1", CaptureComplete: true,
		Stdout: types.CommandOutputStreamRecord{RawBytes: 99, ScrubbedBytes: 80, RawSHA256: "raw", ScrubbedSHA256: "scrubbed", ArchiveMember: "commands/tool-1/stdout.txt"},
	}
	emitter.RecordCommandOutput(record)
	emitter.RecordCommandOutputArchive("/tmp/run-1.command-output.tar.gz")
	if _, err := emitter.Finish(context.Background(), "success"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines=%d\n%s", len(lines), buf.String())
	}
	var event Event
	if err := json.Unmarshal([]byte(lines[1]), &event); err != nil {
		t.Fatal(err)
	}
	if event.Kind != EventKindCommandOutputRecord || event.CommandOutput == nil || event.CommandOutput.Stdout.RawBytes != 99 {
		t.Fatalf("event=%+v", event)
	}
	if strings.Contains(lines[1], "stdout.txt content") {
		t.Fatal("trace embedded command stream content")
	}
	recording, err := typestrace.NewReader(strings.NewReader(buf.String())).ReadRecording()
	if err != nil {
		t.Fatal(err)
	}
	if len(recording.CommandOutputs) != 1 || recording.FinalOutcome.CommandOutputArchive == "" {
		t.Fatalf("recording=%+v", recording)
	}
}
