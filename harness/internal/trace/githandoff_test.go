package trace

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestEmitters_RecordGitHandoffReachesRunTrace(t *testing.T) {
	want := types.GitHandoff{Branch: "stirrup/run-1", SHA: "bbb", BaseSHA: "aaa", Dirty: true, Error: "dirty"}
	builders := stopDetailsEmitterBuilders(t)
	for _, name := range []string{"jsonl", "otel", "gcs"} {
		t.Run(name, func(t *testing.T) {
			e := builders[name](t)
			recorder, ok := e.(GitHandoffRecorder)
			require.True(t, ok, "%T does not implement GitHandoffRecorder", e)

			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			handoff := want
			recorder.RecordGitHandoff(handoff)
			handoff.SHA = "mutated"
			got, err := e.Finish(context.Background(), "handoff_failed")
			require.NoError(t, err)
			require.NotNil(t, got.Git)
			require.Equal(t, want, *got.Git)

			e.Start("run-2", &types.RunConfig{RunID: "run-2"})
			got, err = e.Finish(context.Background(), "success")
			require.NoError(t, err)
			require.Nil(t, got.Git)
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
			require.True(t, ok, "%T does not implement WorkspaceExportRecorder", e)
			errorRecorder, ok := e.(RunErrorRecorder)
			require.True(t, ok, "%T does not implement RunErrorRecorder", e)

			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			receipt := want
			exportRecorder.RecordWorkspaceExport(receipt)
			receipt.URI = "mutated"
			errorRecorder.RecordRunError("workspace export: denied")
			got, err := e.Finish(context.Background(), "handoff_failed")
			require.NoError(t, err)
			require.NotNil(t, got.WorkspaceExport)
			require.Equal(t, want, *got.WorkspaceExport)
			require.Equal(t, "workspace export: denied", got.Error)

			e.Start("run-2", &types.RunConfig{RunID: "run-2"})
			got, err = e.Finish(context.Background(), "success")
			require.NoError(t, err)
			require.Nil(t, got.WorkspaceExport)
			require.Empty(t, got.Error)
		})
	}
}

func TestEmitters_RecordStructuredOutputReachesRunTrace(t *testing.T) {
	builders := stopDetailsEmitterBuilders(t)
	for _, name := range []string{"jsonl", "otel", "gcs"} {
		t.Run(name, func(t *testing.T) {
			e := builders[name](t)
			recorder, ok := e.(StructuredOutputRecorder)
			require.True(t, ok, "%T does not implement StructuredOutputRecorder", e)

			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			doc := json.RawMessage(`{"title":"Fix it"}`)
			recorder.RecordStructuredOutput(doc)
			doc[2] = 'X'
			got, err := e.Finish(context.Background(), "success")
			require.NoError(t, err)
			require.JSONEq(t, `{"title":"Fix it"}`, string(got.StructuredOutput))

			e.Start("run-2", &types.RunConfig{RunID: "run-2"})
			got, err = e.Finish(context.Background(), "success")
			require.NoError(t, err)
			require.Nil(t, got.StructuredOutput)
		})
	}
}
