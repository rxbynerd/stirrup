package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestEmitters_RecordVerificationReachesRunTrace(t *testing.T) {
	want := []types.VerificationResult{
		{Attempt: 1, Passed: false, Feedback: "tests failed"},
		{Attempt: 2, Passed: false, Error: "exec failed"},
	}
	for name, build := range stopDetailsEmitterBuilders(t) {
		t.Run(name, func(t *testing.T) {
			e := build(t)
			recorder, ok := e.(VerificationRecorder)
			require.True(t, ok, "%T does not implement VerificationRecorder", e)

			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			for _, r := range want {
				recorder.RecordVerification(r)
			}
			got, err := e.Finish(context.Background(), "verification_error")
			require.NoError(t, err)
			require.Equal(t, want, got.VerificationResults)

			e.Start("run-2", &types.RunConfig{RunID: "run-2"})
			got, err = e.Finish(context.Background(), "success")
			require.NoError(t, err)
			require.Empty(t, got.VerificationResults, "after restart")
		})
	}
}

func TestJSONLTraceEmitter_RunFinishedCarriesVerificationResults(t *testing.T) {
	var buf bytes.Buffer
	e := NewJSONLTraceEmitter(&buf, false)
	e.Start("run-1", &types.RunConfig{RunID: "run-1"})
	e.RecordVerification(types.VerificationResult{
		Attempt: 1,
		Passed:  true,
		Details: map[string]any{"command": "go test ./..."},
	})
	_, err := e.Finish(context.Background(), "success")
	require.NoError(t, err)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	var ev Event
	err = json.Unmarshal([]byte(lines[len(lines)-1]), &ev)
	require.NoError(t, err, "unmarshal run_finished line")
	require.Equal(t, EventKindRunFinished, ev.Kind)
	require.NotNil(t, ev.Trace)

	got := ev.Trace.VerificationResults
	require.Len(t, got, 1)
	require.Equal(t, 1, got[0].Attempt)
	require.True(t, got[0].Passed)
	require.Equal(t, "go test ./...", got[0].Details["command"])
}

func TestGCSTraceEmitter_UploadedObjectCarriesVerificationResults(t *testing.T) {
	srv := newGCSCaptureServer()
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	emitter, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "b",
		CredentialSource: &staticBearerSource{token: "t"},
		EndpointBaseURL:  httpSrv.URL,
	})
	require.NoError(t, err)
	emitter.Start("run-gcs", &types.RunConfig{RunID: "run-gcs"})
	emitter.RecordVerification(types.VerificationResult{Attempt: 1, Passed: false, Feedback: "lint failed"})
	_, err = emitter.Finish(context.Background(), "verification_failed")
	require.NoError(t, err)

	var uploaded types.RunTrace
	body := strings.TrimRight(string(srv.last().Body), "\n")
	err = json.Unmarshal([]byte(body), &uploaded)
	require.NoError(t, err, "unmarshal uploaded body: %q", body)
	require.Len(t, uploaded.VerificationResults, 1)
	require.Equal(t, "lint failed", uploaded.VerificationResults[0].Feedback)
}

func TestNestedJSONLEmitter_VerificationNotForwardedToParent(t *testing.T) {
	parent := NewJSONLTraceEmitter(&bytes.Buffer{}, false)
	parent.Start("parent-run", &types.RunConfig{RunID: "parent-run"})

	child := NewNestedJSONLEmitter(parent, "parent-run", context.Background())
	child.Start("child-run", &types.RunConfig{RunID: "child-run"})
	child.RecordVerification(types.VerificationResult{Attempt: 1, Passed: false})
	childTrace, err := child.Finish(context.Background(), "verification_failed")
	require.NoError(t, err, "child Finish")
	require.Len(t, childTrace.VerificationResults, 1)

	got, err := parent.Finish(context.Background(), "success")
	require.NoError(t, err, "parent Finish")
	require.Empty(t, got.VerificationResults)
}
