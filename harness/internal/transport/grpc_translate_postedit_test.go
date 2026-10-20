package transport

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

func TestRunConfigFromProto_PostEditPreserved(t *testing.T) {
	must := require.New(t)
	original := &pb.RunConfig{
		Tools: &pb.ToolsConfig{
			PostEdit: []*pb.PostEditCheck{
				{Glob: "*.go", Command: "gofmt -l {path}", TimeoutSeconds: 30},
				{Glob: "cmd/*.go", Command: "go vet ./cmd/..."},
			},
		},
	}
	raw, err := proto.Marshal(original)
	must.NoError(err)
	var decoded pb.RunConfig
	must.NoError(proto.Unmarshal(raw, &decoded))

	got := runConfigFromProto(&decoded).Tools.PostEdit
	want := []types.PostEditCheckConfig{
		{Glob: "*.go", Command: "gofmt -l {path}", TimeoutSeconds: 30},
		{Glob: "cmd/*.go", Command: "go vet ./cmd/..."},
	}
	must.Equal(want, got)

	absent := runConfigFromProto(&pb.RunConfig{Tools: &pb.ToolsConfig{}}).Tools.PostEdit
	must.Nil(absent, "absent post_edit should stay nil")
}

// The proto JSON mirror of RunConfig must accept the Go-side JSON shape
// for tools.postEdit unchanged.
func TestRunConfigFromProto_PostEditJSONMirrorMatchesGoShape(t *testing.T) {
	must := require.New(t)
	goSide := types.ToolsConfig{PostEdit: []types.PostEditCheckConfig{
		{Glob: "*.go", Command: "gofmt -l {path}", TimeoutSeconds: 45},
	}}
	raw, err := json.Marshal(map[string]any{"postEdit": goSide.PostEdit})
	must.NoError(err)
	var pc pb.ToolsConfig
	must.NoError(protojson.Unmarshal(raw, &pc), "proto JSON mirror rejected Go JSON %s", raw)
	must.Equal(goSide.PostEdit, toolsConfigFromProto(&pc).PostEdit)
}

func TestRunTraceToProto_PostEditCheckSummary(t *testing.T) {
	must := require.New(t)
	tr := &types.RunTrace{
		ID:      "run-checks",
		Outcome: "success",
		PostEditChecks: []types.PostEditCheckRecord{
			{Tool: "edit_file", Path: "a.go"},
			{Tool: "edit_file", Path: "a.go", ExitCode: 1},
			{Tool: "edit_file", Path: "b.go", ExitCode: 2},
			{Tool: "write_file", Path: "c.go", TimedOut: true, Error: "command timed out"},
			{Tool: "write_file", Path: "d\n.go", Error: "path rejected: contains a control character"},
		},
	}
	raw, err := proto.Marshal(runTraceToProto(tr))
	must.NoError(err)
	var decoded pb.RunTrace
	must.NoError(proto.Unmarshal(raw, &decoded))

	s := decoded.GetPostEditChecks()
	must.EqualValues(5, s.GetRuns(), "runs")
	must.EqualValues(2, s.GetFailures(), "failures")
	must.EqualValues(1, s.GetTimeouts(), "timeouts")
	must.EqualValues(1, s.GetErrors(), "errors")

	pt := runTraceToProto(&types.RunTrace{Outcome: "success"})
	must.Nil(pt.PostEditChecks, "summary should be absent when no check ran")
}
