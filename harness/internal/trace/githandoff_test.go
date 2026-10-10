package trace

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

func TestEmitters_RecordGitHandoffReachesRunTrace(t *testing.T) {
	want := types.GitHandoff{Branch: "stirrup/run-1", SHA: "bbb", BaseSHA: "aaa", Dirty: true, Error: "dirty"}
	builders := stopDetailsEmitterBuilders(t)
	for _, name := range []string{"jsonl", "otel", "gcs"} {
		t.Run(name, func(t *testing.T) {
			e := builders[name](t)
			recorder, ok := e.(GitHandoffRecorder)
			if !ok {
				t.Fatalf("%T does not implement GitHandoffRecorder", e)
			}

			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			handoff := want
			recorder.RecordGitHandoff(handoff)
			handoff.SHA = "mutated"
			got, err := e.Finish(context.Background(), "handoff_failed")
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if got.Git == nil || *got.Git != want {
				t.Errorf("Git = %+v, want %+v", got.Git, want)
			}

			e.Start("run-2", &types.RunConfig{RunID: "run-2"})
			got, err = e.Finish(context.Background(), "success")
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if got.Git != nil {
				t.Errorf("Git after restart = %+v, want nil", got.Git)
			}
		})
	}
}

func TestEmitters_RecordWorkspaceExportAndRunErrorReachRunTrace(t *testing.T) {
	want := types.ExportReceipt{URI: "gs://b/ws.tar.gz", Status: types.ExportStatusFailed, Required: true, Error: "denied"}
	builders := stopDetailsEmitterBuilders(t)
	for _, name := range []string{"jsonl", "otel", "gcs"} {
		t.Run(name, func(t *testing.T) {
			e := builders[name](t)
			exportRecorder, ok := e.(WorkspaceExportRecorder)
			if !ok {
				t.Fatalf("%T does not implement WorkspaceExportRecorder", e)
			}
			errorRecorder, ok := e.(RunErrorRecorder)
			if !ok {
				t.Fatalf("%T does not implement RunErrorRecorder", e)
			}

			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			receipt := want
			exportRecorder.RecordWorkspaceExport(receipt)
			receipt.URI = "mutated"
			errorRecorder.RecordRunError("workspace export: denied")
			got, err := e.Finish(context.Background(), "handoff_failed")
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if got.WorkspaceExport == nil || *got.WorkspaceExport != want {
				t.Errorf("WorkspaceExport = %+v, want %+v", got.WorkspaceExport, want)
			}
			if got.Error != "workspace export: denied" {
				t.Errorf("Error = %q, want the recorded run error", got.Error)
			}

			e.Start("run-2", &types.RunConfig{RunID: "run-2"})
			got, err = e.Finish(context.Background(), "success")
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if got.WorkspaceExport != nil || got.Error != "" {
				t.Errorf("after restart WorkspaceExport = %+v, Error = %q, want both unset", got.WorkspaceExport, got.Error)
			}
		})
	}
}

func TestEmitters_RecordStructuredOutputReachesRunTrace(t *testing.T) {
	builders := stopDetailsEmitterBuilders(t)
	for _, name := range []string{"jsonl", "otel", "gcs"} {
		t.Run(name, func(t *testing.T) {
			e := builders[name](t)
			recorder, ok := e.(StructuredOutputRecorder)
			if !ok {
				t.Fatalf("%T does not implement StructuredOutputRecorder", e)
			}

			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			doc := json.RawMessage(`{"title":"Fix it"}`)
			recorder.RecordStructuredOutput(doc)
			doc[2] = 'X'
			got, err := e.Finish(context.Background(), "success")
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if string(got.StructuredOutput) != `{"title":"Fix it"}` {
				t.Errorf("StructuredOutput = %s, want the recorded document", got.StructuredOutput)
			}

			e.Start("run-2", &types.RunConfig{RunID: "run-2"})
			got, err = e.Finish(context.Background(), "success")
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if got.StructuredOutput != nil {
				t.Errorf("after restart StructuredOutput = %s, want unset", got.StructuredOutput)
			}
		})
	}
}
