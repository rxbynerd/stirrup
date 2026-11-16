package cmd

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/harness/internal/core"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// withNewerSchemaFields re-encodes task as a control plane built against a
// newer schema would send it: one extra field at the top level and one
// inside executor, both appended as raw wire bytes.
func withNewerSchemaFields(t *testing.T, task *pb.RunConfig, topLevel, inExecutor protowire.Number) *pb.RunConfig {
	t.Helper()
	must := require.New(t)
	rcDesc := task.ProtoReflect().Descriptor()
	must.Nil(rcDesc.Fields().ByNumber(topLevel), "RunConfig now carries field %d", topLevel)
	must.Nil(task.GetExecutor().ProtoReflect().Descriptor().Fields().ByNumber(inExecutor), "ExecutorConfig now carries field %d", inExecutor)

	executor, err := proto.Marshal(task.GetExecutor())
	must.NoError(err)
	executor = protowire.AppendVarint(protowire.AppendTag(executor, inExecutor, protowire.VarintType), 1)

	rest := proto.Clone(task).(*pb.RunConfig)
	rest.Executor = nil
	wire, err := proto.Marshal(rest)
	must.NoError(err)
	wire = protowire.AppendTag(wire, rcDesc.Fields().ByName("executor").Number(), protowire.BytesType)
	wire = protowire.AppendBytes(wire, executor)
	wire = protowire.AppendVarint(protowire.AppendTag(wire, topLevel, protowire.VarintType), 1)

	var newer pb.RunConfig
	must.NoError(proto.Unmarshal(wire, &newer))
	return &newer
}

func TestRunJob_UnknownAssignedFieldsRejectedBeforeBuild(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	useTempMarkerPaths(t)
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	t.Setenv("CONTROL_PLANE_SESSION_ID", "")
	sink := &stubResultSink{}
	installStubResultSink(t, sink)
	provider, requests := startOpenAIStub(t)

	var builds atomic.Int32
	prevBuild := buildJobLoop
	t.Cleanup(func() { buildJobLoop = prevBuild })
	buildJobLoop = func(ctx context.Context, config *types.RunConfig, tp transport.Transport, opts ...core.LoopOption) (*core.AgenticLoop, error) {
		builds.Add(1)
		return prevBuild(ctx, config, tp, opts...)
	}

	task := withNewerSchemaFields(t, followUpJobConfig(t, provider.URL, 30, 0), 9001, 9002)
	srv := newRecordingControlPlane(task)
	startControlPlane(t, srv)

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()
	select {
	case <-srv.doneCh:
	case <-time.After(terminalWaitBudget):
		must.FailNow("control plane never received done for the rejected assignment")
	}
	select {
	case err := <-errCh:
		must.ErrorContains(err, "task_assignment rejected")
	case <-time.After(terminalWaitBudget):
		must.FailNow("runJob did not return after rejecting the assignment")
	}

	var gotTypes []string
	for _, ev := range srv.events() {
		gotTypes = append(gotTypes, ev.GetType())
	}
	must.Equal([]string{"error", "done"}, gotTypes, "the rejection is the error/done pair and nothing else")
	events := srv.events()
	check.Contains(events[0].GetMessage(), "field 9001 at the top level")
	check.Contains(events[0].GetMessage(), "field 9002 under executor")
	check.Equal("error", events[1].GetStopReason())
	check.Nil(events[1].GetTrace(), "no run started, so done carries no trace")

	check.Equal(int32(0), builds.Load(), "no executor or provider is constructed")
	check.Equal(int32(0), requests.Load(), "no provider request is made")
	check.Empty(sink.calls, "no result is recorded when no run started")
}
