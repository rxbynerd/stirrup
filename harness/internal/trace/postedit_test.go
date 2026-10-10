package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/codes"

	"github.com/rxbynerd/stirrup/types"
)

func TestJSONLTraceEmitter_RecordPostEditCheck_StreamsAccumulatesAndScrubs(t *testing.T) {
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)
	emitter.Start("run-post-edit", nil)

	emitter.RecordPostEditCheck(types.PostEditCheckRecord{
		ToolUseID: "toolu_1", Turn: 2, Tool: "edit_file", Path: "a.go", Index: 0,
		Command: "gofmt -l {path}", ExitCode: 1, DurationMs: 12, OutputBytes: 5,
	})
	emitter.RecordPostEditCheck(types.PostEditCheckRecord{
		Tool: "write_file", Path: "sk-ant-api03-leak.go", Index: 1,
		Command: "check", TimedOut: true, Error: "timed out holding sk-ant-api03-leak",
	})

	trace, err := emitter.Finish(context.Background(), "success")
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if len(trace.PostEditChecks) != 2 {
		t.Fatalf("PostEditChecks = %+v", trace.PostEditChecks)
	}
	if got := trace.PostEditChecks[0]; got.ToolUseID != "toolu_1" || got.ExitCode != 1 || got.Command != "gofmt -l {path}" {
		t.Errorf("PostEditChecks[0] = %+v", got)
	}

	var streamed []Event
	for _, ev := range readEvents(t, buf.Bytes()) {
		if ev.Kind == EventKindPostEditCheck {
			streamed = append(streamed, ev)
		}
	}
	if len(streamed) != 2 || streamed[1].PostEditCheck == nil || !streamed[1].PostEditCheck.TimedOut {
		t.Fatalf("post_edit_check events = %+v", streamed)
	}
	if strings.Contains(buf.String(), "sk-ant-api03-leak") {
		t.Errorf("secret reached the persisted trace:\n%s", buf.String())
	}
}

func TestGCSTraceEmitter_RecordPostEditCheck_AccumulatesAndScrubs(t *testing.T) {
	srv := newGCSCaptureServer()
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	emitter, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "my-bucket",
		CredentialSource: &staticBearerSource{token: "test-token"},
		EndpointBaseURL:  httpSrv.URL,
	})
	if err != nil {
		t.Fatalf("NewGCSTraceEmitter: %v", err)
	}
	emitter.Start("run-gcs-post-edit", nil)
	emitter.RecordPostEditCheck(types.PostEditCheckRecord{
		ToolUseID: "toolu_1", Tool: "edit_file", Path: "a.go", Command: "gofmt -l {path}", ExitCode: 1,
	})
	emitter.RecordPostEditCheck(types.PostEditCheckRecord{
		Tool: "write_file", Path: "sk-ant-api03-leak.go", Index: 1, Command: "check",
		TimedOut: true, Error: "timed out holding sk-ant-api03-leak",
	})

	tr, err := emitter.Finish(context.Background(), "success")
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if len(tr.PostEditChecks) != 2 || tr.PostEditChecks[0].ToolUseID != "toolu_1" || !tr.PostEditChecks[1].TimedOut {
		t.Fatalf("returned PostEditChecks = %+v", tr.PostEditChecks)
	}

	body := string(srv.last().Body)
	if strings.Contains(body, "sk-ant-api03-leak") {
		t.Errorf("secret reached the uploaded trace:\n%s", body)
	}
	var uploaded types.RunTrace
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &uploaded); err != nil {
		t.Fatalf("unmarshal uploaded body: %v", err)
	}
	if len(uploaded.PostEditChecks) != 2 || uploaded.PostEditChecks[0].ExitCode != 1 {
		t.Errorf("uploaded PostEditChecks = %+v", uploaded.PostEditChecks)
	}
}

func TestJSONLTraceEmitter_NoPostEditChecks_OmitsField(t *testing.T) {
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)
	emitter.Start("run-none", nil)
	trace, err := emitter.Finish(context.Background(), "success")
	if err != nil {
		t.Fatal(err)
	}
	if trace.PostEditChecks != nil || strings.Contains(buf.String(), "postEditChecks") {
		t.Errorf("unexpected post-edit data: %+v / %s", trace.PostEditChecks, buf.String())
	}
}

func TestOTelTraceEmitter_RecordPostEditCheck_EmitsSpanAndAggregates(t *testing.T) {
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-otel-post-edit", &types.RunConfig{RunID: "run-otel-post-edit"})

	emitter.RecordPostEditCheck(types.PostEditCheckRecord{Tool: "edit_file", Path: "a.go", Command: "go vet", ExitCode: 2, DurationMs: 9})
	emitter.RecordPostEditCheck(types.PostEditCheckRecord{Tool: "edit_file", Path: "b.go", Command: "go vet"})

	trace, err := emitter.Finish(context.Background(), "success")
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.PostEditChecks) != 2 {
		t.Fatalf("PostEditChecks = %+v", trace.PostEditChecks)
	}

	var spans int
	for _, s := range exporter.GetSpans() {
		if s.Name != "post_edit_check" {
			continue
		}
		spans++
		attrs := map[string]any{}
		for _, kv := range s.Attributes {
			attrs[string(kv.Key)] = kv.Value.AsInterface()
		}
		if attrs["post_edit_check.command"] != "go vet" {
			t.Errorf("span attributes = %v", attrs)
		}
		failed := attrs["post_edit_check.exit_code"] != int64(0)
		if failed != (s.Status.Code == codes.Error) {
			t.Errorf("span status %v does not match exit code %v", s.Status.Code, attrs["post_edit_check.exit_code"])
		}
	}
	if spans != 2 {
		t.Errorf("post_edit_check spans = %d, want 2", spans)
	}
}

func TestNestedJSONLEmitter_ForwardsPostEditCheckToParent(t *testing.T) {
	var buf bytes.Buffer
	parent := NewJSONLTraceEmitter(&buf, false)
	parent.Start("parent-run-1", nil)
	child := NewNestedJSONLEmitter(parent, "parent-run-1", context.Background())
	child.Start("child-run-1", nil)

	child.RecordPostEditCheck(types.PostEditCheckRecord{Tool: "edit_file", Path: "a.go", Command: "true"})

	trace, err := parent.Finish(context.Background(), "success")
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.PostEditChecks) != 1 || trace.PostEditChecks[0].Path != "a.go" {
		t.Fatalf("parent PostEditChecks = %+v", trace.PostEditChecks)
	}
}
