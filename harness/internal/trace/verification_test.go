package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

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
			if !ok {
				t.Fatalf("%T does not implement VerificationRecorder", e)
			}

			e.Start("run-1", &types.RunConfig{RunID: "run-1"})
			for _, r := range want {
				recorder.RecordVerification(r)
			}
			got, err := e.Finish(context.Background(), "verification_error")
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if !reflect.DeepEqual(got.VerificationResults, want) {
				t.Errorf("VerificationResults = %+v, want %+v", got.VerificationResults, want)
			}

			e.Start("run-2", &types.RunConfig{RunID: "run-2"})
			got, err = e.Finish(context.Background(), "success")
			if err != nil {
				t.Fatalf("Finish: %v", err)
			}
			if len(got.VerificationResults) != 0 {
				t.Errorf("VerificationResults after restart = %+v, want empty", got.VerificationResults)
			}
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
	if _, err := e.Finish(context.Background(), "success"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	var ev Event
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &ev); err != nil {
		t.Fatalf("unmarshal run_finished: %v", err)
	}
	if ev.Kind != EventKindRunFinished || ev.Trace == nil {
		t.Fatalf("last line kind=%q trace=%v, want run_finished with a trace", ev.Kind, ev.Trace)
	}
	got := ev.Trace.VerificationResults
	if len(got) != 1 || got[0].Attempt != 1 || !got[0].Passed || got[0].Details["command"] != "go test ./..." {
		t.Errorf("persisted VerificationResults = %+v", got)
	}
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
	if err != nil {
		t.Fatalf("NewGCSTraceEmitter: %v", err)
	}
	emitter.Start("run-gcs", &types.RunConfig{RunID: "run-gcs"})
	emitter.RecordVerification(types.VerificationResult{Attempt: 1, Passed: false, Feedback: "lint failed"})
	if _, err := emitter.Finish(context.Background(), "verification_failed"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	var uploaded types.RunTrace
	body := strings.TrimRight(string(srv.last().Body), "\n")
	if err := json.Unmarshal([]byte(body), &uploaded); err != nil {
		t.Fatalf("unmarshal uploaded body: %v\nbody=%q", err, body)
	}
	if len(uploaded.VerificationResults) != 1 || uploaded.VerificationResults[0].Feedback != "lint failed" {
		t.Errorf("uploaded VerificationResults = %+v", uploaded.VerificationResults)
	}
}

func TestNestedJSONLEmitter_VerificationNotForwardedToParent(t *testing.T) {
	parent := NewJSONLTraceEmitter(&bytes.Buffer{}, false)
	parent.Start("parent-run", &types.RunConfig{RunID: "parent-run"})

	child := NewNestedJSONLEmitter(parent, "parent-run", context.Background())
	child.Start("child-run", &types.RunConfig{RunID: "child-run"})
	child.RecordVerification(types.VerificationResult{Attempt: 1, Passed: false})
	childTrace, err := child.Finish(context.Background(), "verification_failed")
	if err != nil {
		t.Fatalf("child Finish: %v", err)
	}
	if len(childTrace.VerificationResults) != 1 {
		t.Errorf("child VerificationResults = %+v, want one entry", childTrace.VerificationResults)
	}

	got, err := parent.Finish(context.Background(), "success")
	if err != nil {
		t.Fatalf("parent Finish: %v", err)
	}
	if len(got.VerificationResults) != 0 {
		t.Errorf("parent VerificationResults = %+v, want empty", got.VerificationResults)
	}
}
