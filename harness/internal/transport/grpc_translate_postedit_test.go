package transport

import (
	"encoding/json"
	"reflect"
	"testing"

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
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.RunConfig
	if err := proto.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got := runConfigFromProto(&decoded).Tools.PostEdit
	want := []types.PostEditCheckConfig{
		{Glob: "*.go", Command: "gofmt -l {path}", TimeoutSeconds: 30},
		{Glob: "cmd/*.go", Command: "go vet ./cmd/..."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PostEdit = %+v, want %+v", got, want)
	}

	if absent := runConfigFromProto(&pb.RunConfig{Tools: &pb.ToolsConfig{}}).Tools.PostEdit; absent != nil {
		t.Errorf("absent post_edit should stay nil, got %+v", absent)
	}
}

// The proto JSON mirror of RunConfig must accept the Go-side JSON shape
// for tools.postEdit unchanged.
func TestRunConfigFromProto_PostEditJSONMirrorMatchesGoShape(t *testing.T) {
	goSide := types.ToolsConfig{PostEdit: []types.PostEditCheckConfig{
		{Glob: "*.go", Command: "gofmt -l {path}", TimeoutSeconds: 45},
	}}
	raw, err := json.Marshal(map[string]any{"postEdit": goSide.PostEdit})
	if err != nil {
		t.Fatal(err)
	}
	var pc pb.ToolsConfig
	if err := protojson.Unmarshal(raw, &pc); err != nil {
		t.Fatalf("proto JSON mirror rejected Go JSON %s: %v", raw, err)
	}
	if got := toolsConfigFromProto(&pc).PostEdit; !reflect.DeepEqual(got, goSide.PostEdit) {
		t.Fatalf("round trip = %+v, want %+v", got, goSide.PostEdit)
	}
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
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.RunTrace
	if err := proto.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s := decoded.GetPostEditChecks()
	if s.GetRuns() != 5 || s.GetFailures() != 2 || s.GetTimeouts() != 1 || s.GetErrors() != 1 {
		t.Errorf("summary = %v, want runs 5, failures 2, timeouts 1, errors 1", s)
	}

	if pt := runTraceToProto(&types.RunTrace{Outcome: "success"}); pt.PostEditChecks != nil {
		t.Errorf("summary = %v, want absent when no check ran", pt.PostEditChecks)
	}
}
