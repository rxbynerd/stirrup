package core

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// fakeControlPlane answers every tool_result_request on the RunTask stream
// with the ControlEvent that respond returns, and records the requests it
// saw. A nil respond leaves the request unanswered.
type fakeControlPlane struct {
	pb.UnimplementedHarnessServiceServer

	respond func(req *pb.HarnessEvent) *pb.ControlEvent

	mu       sync.Mutex
	requests []*pb.HarnessEvent
}

func (s *fakeControlPlane) RunTask(stream pb.HarnessService_RunTaskServer) error {
	for {
		ev, err := stream.Recv()
		if err != nil {
			return nil
		}
		if ev.Type != "tool_result_request" {
			continue
		}
		s.mu.Lock()
		s.requests = append(s.requests, ev)
		s.mu.Unlock()
		if s.respond == nil {
			continue
		}
		if resp := s.respond(ev); resp != nil {
			if err := stream.Send(resp); err != nil {
				return err
			}
		}
	}
}

func (s *fakeControlPlane) seen() []*pb.HarnessEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*pb.HarnessEvent, len(s.requests))
	copy(out, s.requests)
	return out
}

func controlPlaneToolsConfig() types.ToolsConfig {
	return types.ToolsConfig{
		BuiltIn: []string{"web_fetch"},
		ControlPlane: []types.ControlPlaneToolConfig{
			{
				Name:        "search_memory",
				Description: "Search long-term memory.",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"integer"}},"required":["query"]}`),
			},
			{
				Name:           "save_memory",
				Description:    "Persist a memory.",
				InputSchema:    json.RawMessage(`{"type":"object","properties":{"content":{"type":"string"}},"required":["content"]}`),
				TimeoutSeconds: 1,
			},
		},
	}
}

// startControlPlaneLoop wires a real GRPCTransport over bufconn to cp and
// builds a loop whose registry comes from buildToolRegistry, so the test
// covers registration, schema validation, dispatch, and the wire.
func startControlPlaneLoop(t *testing.T, cp *fakeControlPlane) *AgenticLoop {
	t.Helper()
	must := require.New(t)

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	t.Cleanup(srv.Stop)
	pb.RegisterHarnessServiceServer(srv, cp)
	go func() { _ = srv.Serve(lis) }()

	tr, err := transport.NewGRPCTransport(context.Background(), "passthrough:///bufconn",
		transport.WithDialOptions(
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		),
	)
	must.NoError(err, "NewGRPCTransport")
	t.Cleanup(func() { _ = tr.Close() })

	registry := buildToolRegistry(executor.NewNoneExecutor(), edit.NewWholeFileStrategy(), controlPlaneToolsConfig(), nil)
	loop := buildAsyncTestLoop(t, tr, asyncEchoTool())
	loop.Tools = registry
	return loop
}

func TestControlPlaneTool_RoundTripOverGRPC(t *testing.T) {
	must := require.New(t)
	cp := &fakeControlPlane{
		respond: func(req *pb.HarnessEvent) *pb.ControlEvent {
			return &pb.ControlEvent{
				Type:      "tool_result_response",
				RequestId: req.RequestId,
				Content:   `{"memories":[{"content":"remembered"}]}`,
			}
		},
	}
	loop := startControlPlaneLoop(t, cp)

	defs := loop.Tools.List()
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	for _, want := range []string{"web_fetch", "search_memory", "save_memory"} {
		must.Contains(names, want)
	}

	output, success := loop.dispatchToolCall(context.Background(), types.ToolCall{
		ID:    "tc_cp_1",
		Name:  "search_memory",
		Input: json.RawMessage(`{"query":"lunch","limit":3}`),
	})
	must.True(success, "expected success, got failure: %q", output)
	must.Equal(`{"memories":[{"content":"remembered"}]}`, output, "content not delivered verbatim") //nolint:testifylint // byte-exact comparison is the point

	reqs := cp.seen()
	must.Len(reqs, 1)
	req := reqs[0]
	must.Equal("search_memory", req.ToolName)
	must.Equal("tc_cp_1", req.ToolUseId)
	must.NotEmpty(req.RequestId)

	var input map[string]any
	must.NoError(json.Unmarshal(req.Input, &input), "request input is not JSON: %s", req.Input)
	must.Equal("lunch", input["query"])
	must.InDelta(float64(3), input["limit"], 0)
}

func TestControlPlaneTool_IsErrorSurfacesAsToolFailure(t *testing.T) {
	must := require.New(t)
	cp := &fakeControlPlane{
		respond: func(req *pb.HarnessEvent) *pb.ControlEvent {
			return &pb.ControlEvent{
				Type:      "tool_result_response",
				RequestId: req.RequestId,
				Content:   "billet unavailable",
				IsError:   &pb.OptionalBool{Value: true},
			}
		},
	}
	loop := startControlPlaneLoop(t, cp)

	output, success := loop.dispatchToolCall(context.Background(), types.ToolCall{
		ID:    "tc_cp_2",
		Name:  "search_memory",
		Input: json.RawMessage(`{"query":"x"}`),
	})
	must.False(success, "expected failure, got success: %q", output)
	must.Contains(output, "upstream_error")
	must.Contains(output, "billet unavailable")
}

func TestControlPlaneTool_InputValidatedBeforeRequest(t *testing.T) {
	must := require.New(t)
	cp := &fakeControlPlane{}
	loop := startControlPlaneLoop(t, cp)

	output, success := loop.dispatchToolCall(context.Background(), types.ToolCall{
		ID:    "tc_cp_3",
		Name:  "search_memory",
		Input: json.RawMessage(`{"limit":"three"}`),
	})
	must.False(success, "expected schema rejection, got success: %q", output)
	must.Contains(output, "Invalid input")

	time.Sleep(50 * time.Millisecond)
	must.Empty(cp.seen(), "control plane should not see a request for invalid input")
}

func TestControlPlaneTool_TimeoutSecondsBoundsTheWait(t *testing.T) {
	must := require.New(t)
	cp := &fakeControlPlane{} // never answers
	loop := startControlPlaneLoop(t, cp)

	start := time.Now()
	output, success := loop.dispatchToolCall(context.Background(), types.ToolCall{
		ID:    "tc_cp_4",
		Name:  "save_memory",
		Input: json.RawMessage(`{"content":"note"}`),
	})
	elapsed := time.Since(start)

	must.False(success, "expected timeout failure, got success: %q", output)
	must.Contains(output, "timeout")
	must.GreaterOrEqual(elapsed, time.Second, "wait ended before the configured 1s timeout")
	must.LessOrEqual(elapsed, 10*time.Second, "wait ran past the bound, so the 60s default may apply")
	must.Len(cp.seen(), 1)
}
