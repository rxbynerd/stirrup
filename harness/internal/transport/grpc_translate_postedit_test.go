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
	original := &pb.RunConfig{
		Tools: &pb.ToolsConfig{
			PostEdit: []*pb.PostEditCheck{
				{Glob: "*.go", Command: "gofmt -l {path}", TimeoutSeconds: 30},
				{Glob: "cmd/*.go", Command: "go vet ./cmd/..."},
			},
		},
	}
	raw, err := proto.Marshal(original)
	require.NoError(t, err)
	var decoded pb.RunConfig
	require.NoError(t, proto.Unmarshal(raw, &decoded))

	got := runConfigFromProto(&decoded).Tools.PostEdit
	want := []types.PostEditCheckConfig{
		{Glob: "*.go", Command: "gofmt -l {path}", TimeoutSeconds: 30},
		{Glob: "cmd/*.go", Command: "go vet ./cmd/..."},
	}
	require.Equal(t, want, got)

	absent := runConfigFromProto(&pb.RunConfig{Tools: &pb.ToolsConfig{}}).Tools.PostEdit
	require.Nil(t, absent, "absent post_edit should stay nil")
}

// The proto JSON mirror of RunConfig must accept the Go-side JSON shape
// for tools.postEdit unchanged.
func TestRunConfigFromProto_PostEditJSONMirrorMatchesGoShape(t *testing.T) {
	goSide := types.ToolsConfig{PostEdit: []types.PostEditCheckConfig{
		{Glob: "*.go", Command: "gofmt -l {path}", TimeoutSeconds: 45},
	}}
	raw, err := json.Marshal(map[string]any{"postEdit": goSide.PostEdit})
	require.NoError(t, err)
	var pc pb.ToolsConfig
	require.NoError(t, protojson.Unmarshal(raw, &pc), "proto JSON mirror rejected Go JSON %s", raw)
	require.Equal(t, goSide.PostEdit, toolsConfigFromProto(&pc).PostEdit)
}

func TestRunTraceToProto_PostEditCheckSummary(t *testing.T) {
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
	require.NoError(t, err)
	var decoded pb.RunTrace
	require.NoError(t, proto.Unmarshal(raw, &decoded))

	s := decoded.GetPostEditChecks()
	require.EqualValues(t, 5, s.GetRuns(), "runs")
	require.EqualValues(t, 2, s.GetFailures(), "failures")
	require.EqualValues(t, 1, s.GetTimeouts(), "timeouts")
	require.EqualValues(t, 1, s.GetErrors(), "errors")

	pt := runTraceToProto(&types.RunTrace{Outcome: "success"})
	require.Nil(t, pt.PostEditChecks, "summary should be absent when no check ran")
}
