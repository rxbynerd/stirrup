package transport

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/types"
)

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
