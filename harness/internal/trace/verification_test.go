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
			must := require.New(t)
			e := build(t)
			recorder, ok := e.(VerificationRecorder)
			must.True(ok, "%T does not implement VerificationRecorder", e)

			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			for _, r := range want {
				recorder.RecordVerification(r)
			}
			got, err := e.Finish(context.Background(), "verification_error")
			must.NoError(err)
			must.Equal(want, got.VerificationResults)

			e.Start("run-2", &types.RunConfig{RunID: "run-2"})
			got, err = e.Finish(context.Background(), "success")
			must.NoError(err)
			must.Empty(got.VerificationResults, "after restart")
		})
	}
}

func TestJSONLTraceEmitter_RunFinishedCarriesVerificationResults(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	e := NewJSONLTraceEmitter(&buf, false)
	e.Start("run-1", &types.RunConfig{RunID: "run-1"})
	e.RecordVerification(types.VerificationResult{
		Attempt: 1,
		Passed:  true,
		Details: map[string]any{"command": "go test ./..."},
	})
	_, err := e.Finish(context.Background(), "success")
	must.NoError(err)

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	var ev Event
	err = json.Unmarshal([]byte(lines[len(lines)-1]), &ev)
	must.NoError(err, "unmarshal run_finished line")
	must.Equal(EventKindRunFinished, ev.Kind)
	must.NotNil(ev.Trace)

	got := ev.Trace.VerificationResults
	must.Len(got, 1)
	must.Equal(1, got[0].Attempt)
	must.True(got[0].Passed)
	must.Equal("go test ./...", got[0].Details["command"])
}

func TestGCSTraceEmitter_UploadedObjectCarriesVerificationResults(t *testing.T) {
	must := require.New(t)
	srv := newGCSCaptureServer()
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	emitter, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "b",
		CredentialSource: &staticBearerSource{token: "t"},
		EndpointBaseURL:  httpSrv.URL,
	})
	must.NoError(err)
	emitter.Start("run-gcs", &types.RunConfig{RunID: "run-gcs"})
	emitter.RecordVerification(types.VerificationResult{Attempt: 1, Passed: false, Feedback: "lint failed"})
	_, err = emitter.Finish(context.Background(), "verification_failed")
	must.NoError(err)

	var uploaded types.RunTrace
	body := strings.TrimRight(string(srv.last().Body), "\n")
	err = json.Unmarshal([]byte(body), &uploaded)
	must.NoError(err, "unmarshal uploaded body: %q", body)
	must.Len(uploaded.VerificationResults, 1)
	must.Equal("lint failed", uploaded.VerificationResults[0].Feedback)
}

func TestNestedJSONLEmitter_VerificationNotForwardedToParent(t *testing.T) {
	must := require.New(t)
	parent := NewJSONLTraceEmitter(&bytes.Buffer{}, false)
	parent.Start("parent-run", &types.RunConfig{RunID: "parent-run"})

	child := NewNestedJSONLEmitter(parent, "parent-run", context.Background())
	child.Start("child-run", &types.RunConfig{RunID: "child-run"})
	child.RecordVerification(types.VerificationResult{Attempt: 1, Passed: false})
	childTrace, err := child.Finish(context.Background(), "verification_failed")
	must.NoError(err, "child Finish")
	must.Len(childTrace.VerificationResults, 1)

	got, err := parent.Finish(context.Background(), "success")
	must.NoError(err, "parent Finish")
	must.Empty(got.VerificationResults)
}
