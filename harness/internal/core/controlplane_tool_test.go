package core

import (
	"context"
	"encoding/json"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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

	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
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
	if err != nil {
		srv.Stop()
		t.Fatalf("NewGRPCTransport: %v", err)
	}
	t.Cleanup(func() {
		_ = tr.Close()
		srv.Stop()
	})

	registry := buildToolRegistry(executor.NewNoneExecutor(), edit.NewWholeFileStrategy(), controlPlaneToolsConfig(), nil)
	loop := buildAsyncTestLoop(t, tr, asyncEchoTool())
	loop.Tools = registry
	return loop
}

func TestControlPlaneTool_RoundTripOverGRPC(t *testing.T) {
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
		if !slices.Contains(names, want) {
			t.Fatalf("registry missing %q: %v", want, names)
		}
	}

	output, success := loop.dispatchToolCall(context.Background(), types.ToolCall{
		ID:    "tc_cp_1",
		Name:  "search_memory",
		Input: json.RawMessage(`{"query":"lunch","limit":3}`),
	})
	if !success {
		t.Fatalf("expected success, got failure: %q", output)
	}
	if output != `{"memories":[{"content":"remembered"}]}` {
		t.Fatalf("content not delivered verbatim: %q", output)
	}

	reqs := cp.seen()
	if len(reqs) != 1 {
		t.Fatalf("control plane saw %d requests, want 1", len(reqs))
	}
	req := reqs[0]
	if req.ToolName != "search_memory" || req.ToolUseId != "tc_cp_1" || req.RequestId == "" {
		t.Fatalf("request fields = name %q, tool_use_id %q, request_id %q", req.ToolName, req.ToolUseId, req.RequestId)
	}
	var input map[string]any
	if err := json.Unmarshal(req.Input, &input); err != nil {
		t.Fatalf("request input is not JSON: %v (%s)", err, req.Input)
	}
	if input["query"] != "lunch" || input["limit"] != float64(3) {
		t.Fatalf("request input = %s", req.Input)
	}
}

func TestControlPlaneTool_IsErrorSurfacesAsToolFailure(t *testing.T) {
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
	if success {
		t.Fatalf("expected failure, got success: %q", output)
	}
	if !strings.Contains(output, "upstream_error") || !strings.Contains(output, "billet unavailable") {
		t.Fatalf("unexpected failure text: %q", output)
	}
}

func TestControlPlaneTool_InputValidatedBeforeRequest(t *testing.T) {
	cp := &fakeControlPlane{}
	loop := startControlPlaneLoop(t, cp)

	output, success := loop.dispatchToolCall(context.Background(), types.ToolCall{
		ID:    "tc_cp_3",
		Name:  "search_memory",
		Input: json.RawMessage(`{"limit":"three"}`),
	})
	if success {
		t.Fatalf("expected schema rejection, got success: %q", output)
	}
	if !strings.Contains(output, "Invalid input") {
		t.Fatalf("unexpected failure text: %q", output)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(cp.seen()); n != 0 {
		t.Fatalf("control plane should not see a request for invalid input, saw %d", n)
	}
}

func TestControlPlaneTool_TimeoutSecondsBoundsTheWait(t *testing.T) {
	cp := &fakeControlPlane{} // never answers
	loop := startControlPlaneLoop(t, cp)

	start := time.Now()
	output, success := loop.dispatchToolCall(context.Background(), types.ToolCall{
		ID:    "tc_cp_4",
		Name:  "save_memory",
		Input: json.RawMessage(`{"content":"note"}`),
	})
	elapsed := time.Since(start)
	if success {
		t.Fatalf("expected timeout failure, got success: %q", output)
	}
	if !strings.Contains(output, "timeout") {
		t.Fatalf("unexpected failure text: %q", output)
	}
	if elapsed < time.Second || elapsed > 10*time.Second {
		t.Fatalf("elapsed %v, want the configured 1s timeout rather than the 60s default", elapsed)
	}
	if n := len(cp.seen()); n != 1 {
		t.Fatalf("control plane saw %d requests, want 1", n)
	}
}
