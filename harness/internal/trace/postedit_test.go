package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"

	"github.com/rxbynerd/stirrup/types"
)

func TestJSONLTraceEmitter_RecordPostEditCheck_StreamsAccumulatesAndScrubs(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.Len(trace.PostEditChecks, 2)
	first := trace.PostEditChecks[0]
	must.Equal("toolu_1", first.ToolUseID)
	must.Equal(1, first.ExitCode)
	must.Equal("gofmt -l {path}", first.Command)

	var streamed []Event
	for _, ev := range readEvents(t, buf.Bytes()) {
		if ev.Kind == EventKindPostEditCheck {
			streamed = append(streamed, ev)
		}
	}
	must.Len(streamed, 2)
	must.NotNil(streamed[1].PostEditCheck)
	must.True(streamed[1].PostEditCheck.TimedOut)
	must.NotContains(buf.String(), "sk-ant-api03-leak", "secret reached the persisted trace")
}

func TestGCSTraceEmitter_RecordPostEditCheck_AccumulatesAndScrubs(t *testing.T) {
	must := require.New(t)
	srv := newGCSCaptureServer()
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	emitter, err := NewGCSTraceEmitter(context.Background(), GCSTraceEmitterOptions{
		Bucket:           "my-bucket",
		CredentialSource: &staticBearerSource{token: "test-token"},
		EndpointBaseURL:  httpSrv.URL,
	})
	must.NoError(err)
	emitter.Start("run-gcs-post-edit", nil)
	emitter.RecordPostEditCheck(types.PostEditCheckRecord{
		ToolUseID: "toolu_1", Tool: "edit_file", Path: "a.go", Command: "gofmt -l {path}", ExitCode: 1,
	})
	emitter.RecordPostEditCheck(types.PostEditCheckRecord{
		Tool: "write_file", Path: "sk-ant-api03-leak.go", Index: 1, Command: "check",
		TimedOut: true, Error: "timed out holding sk-ant-api03-leak",
	})

	tr, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)
	must.Len(tr.PostEditChecks, 2)
	must.Equal("toolu_1", tr.PostEditChecks[0].ToolUseID)
	must.True(tr.PostEditChecks[1].TimedOut)

	body := string(srv.last().Body)
	must.NotContains(body, "sk-ant-api03-leak", "secret reached the uploaded trace")
	var uploaded types.RunTrace
	must.NoError(json.Unmarshal([]byte(strings.TrimSpace(body)), &uploaded))
	must.Len(uploaded.PostEditChecks, 2)
	must.Equal(1, uploaded.PostEditChecks[0].ExitCode)
}

func TestJSONLTraceEmitter_NoPostEditChecks_OmitsField(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	emitter := NewJSONLTraceEmitter(&buf, false)
	emitter.Start("run-none", nil)
	trace, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)
	must.Nil(trace.PostEditChecks)
	must.NotContains(buf.String(), "postEditChecks")
}

func TestOTelTraceEmitter_RecordPostEditCheck_EmitsSpanAndAggregates(t *testing.T) {
	must := require.New(t)
	emitter, exporter := newTestOTelEmitter()
	emitter.Start("run-otel-post-edit", &types.RunConfig{RunID: "run-otel-post-edit"})

	emitter.RecordPostEditCheck(types.PostEditCheckRecord{Tool: "edit_file", Path: "a.go", Command: "go vet", ExitCode: 2, DurationMs: 900})
	emitter.RecordPostEditCheck(types.PostEditCheckRecord{Tool: "edit_file", Path: "b.go", Command: "go vet"})

	trace, err := emitter.Finish(context.Background(), "success")
	must.NoError(err)
	must.Len(trace.PostEditChecks, 2)

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
		must.Equal("go vet", attrs["post_edit_check.command"], "span attributes: %v", attrs)

		want := time.Duration(attrs["post_edit_check.duration_ms"].(int64)) * time.Millisecond
		must.Equal(want, s.EndTime.Sub(s.StartTime), "span should last the check duration")

		failed := attrs["post_edit_check.exit_code"] != int64(0)
		must.Equal(failed, s.Status.Code == codes.Error,
			"span status %v does not match exit code %v", s.Status.Code, attrs["post_edit_check.exit_code"])
	}
	must.Equal(2, spans)
}

func TestNestedJSONLEmitter_ForwardsPostEditCheckToParent(t *testing.T) {
	must := require.New(t)
	var buf bytes.Buffer
	parent := NewJSONLTraceEmitter(&buf, false)
	parent.Start("parent-run-1", nil)
	child := NewNestedJSONLEmitter(parent, "parent-run-1", context.Background())
	child.Start("child-run-1", nil)

	child.RecordPostEditCheck(types.PostEditCheckRecord{Tool: "edit_file", Path: "a.go", Command: "true"})

	trace, err := parent.Finish(context.Background(), "success")
	must.NoError(err)
	must.Len(trace.PostEditChecks, 1)
	must.Equal("a.go", trace.PostEditChecks[0].Path)
}
