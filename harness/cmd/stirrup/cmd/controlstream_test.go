package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/harness/internal/core"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// losingControlPlane assigns a task, records what the harness sends, and
// ends the RPC from its side once end is closed.
type losingControlPlane struct {
	pb.UnimplementedHarnessServiceServer
	task *pb.RunConfig
	end  chan struct{}

	mu       sync.Mutex
	received []*pb.HarnessEvent
}

func (s *losingControlPlane) RunTask(stream pb.HarnessService_RunTaskServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	if err := stream.Send(&pb.ControlEvent{Type: "task_assignment", Task: s.task}); err != nil {
		return err
	}
	go func() {
		for {
			ev, err := stream.Recv()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.received = append(s.received, ev)
			s.mu.Unlock()
		}
	}()
	select {
	case <-s.end:
		return nil
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
}

// serveStoppableControlPlane is serveControlPlane returning the server so
// a test can stop it to fail the connection hard.
func serveStoppableControlPlane(t *testing.T, srv pb.HarnessServiceServer) *grpc.Server {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	pb.RegisterHarnessServiceServer(grpcServer, srv)
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)
	t.Setenv("CONTROL_PLANE_ADDR", lis.Addr().String())
	return grpcServer
}

// blockingToolCallProvider holds its first request open until the
// harness cancels it. Were the request to outlive the cancel, it would
// answer with a read_file call, so a harness that ignored the loss would
// run the tool and make a second request.
type blockingToolCallProvider struct {
	*httptest.Server
	requests  atomic.Int32
	cancelled atomic.Bool
	reached   chan struct{}
}

func startBlockingToolCallProvider(t *testing.T) *blockingToolCallProvider {
	t.Helper()
	p := &blockingToolCallProvider{reached: make(chan struct{})}
	var once sync.Once
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := p.requests.Add(1)
		// The server notices a client disconnect only once the body
		// has been read to EOF.
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			once.Do(func() { close(p.reached) })
			select {
			case <-r.Context().Done():
				p.cancelled.Store(true)
				return
			case <-time.After(10 * time.Second):
			}
			_, _ = fmt.Fprint(w, "data: "+
				`{"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"}}]},"finish_reason":null}]}`+
				"\n\ndata: "+
				`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+
				"\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = fmt.Fprint(w, "data: "+
			`{"id":"x","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":null}]}`+
			"\n\ndata: "+
			`{"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+
			"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(p.Close)
	return p
}

func streamLossJobConfig(t *testing.T, providerURL string) *pb.RunConfig {
	t.Helper()
	task := followUpJobConfig(t, providerURL, 30, 0)
	task.RunId = "job-stream-loss"
	task.MaxTurns = 3
	task.Tools = &pb.ToolsConfig{BuiltIn: []string{"read_file"}}
	task.Executor.WorkspaceExportTo = "gs://bucket/runs/job-stream-loss/workspace.tar.gz"
	return task
}

// assertJobCancelledOnLoss runs the shared assertions for a control
// stream lost while the provider call was blocked.
func assertJobCancelledOnLoss(t *testing.T, provider *blockingToolCallProvider, sink *stubResultSink, exporter *recordingExporter, errCh <-chan error) {
	t.Helper()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("runJob returned nil, want the loss to fail the process")
		}
		if !strings.Contains(err.Error(), "control_stream_lost") {
			t.Errorf("runJob error = %v, want it to name the control_stream_lost outcome", err)
		}
	case <-time.After(terminalWaitBudget):
		t.Fatal("runJob did not return after the control stream was lost")
	}

	if got := provider.requests.Load(); got != 1 {
		t.Errorf("provider requests = %d, want 1: no model request may follow the loss", got)
	}
	if !provider.cancelled.Load() {
		t.Error("the in-flight provider request was not cancelled")
	}
	if len(sink.calls) != 1 {
		t.Fatalf("result sink received %d results, want 1", len(sink.calls))
	}
	result := sink.calls[0]
	if result.Outcome != "control_stream_lost" {
		t.Errorf("RunResult.Outcome = %q, want control_stream_lost", result.Outcome)
	}
	if !strings.Contains(result.Error, "control stream lost") {
		t.Errorf("RunResult.Error = %q, want the loss named", result.Error)
	}
	if got, want := exporter.destinations(), []string{"gs://bucket/runs/job-stream-loss/workspace.tar.gz"}; !equalStrings(got, want) {
		t.Errorf("export destinations = %v, want %v: the workspace export runs after a loss", got, want)
	}
}

func TestRunJob_ControlPlaneEndingRPCDuringProviderCallCancelsRun(t *testing.T) {
	useTempMarkerPaths(t)
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	t.Setenv("CONTROL_PLANE_SESSION_ID", "")
	sink := &stubResultSink{}
	installStubResultSink(t, sink)
	exporter := &recordingExporter{}
	installRecordingExporter(t, exporter)
	provider := startBlockingToolCallProvider(t)

	srv := &losingControlPlane{task: streamLossJobConfig(t, provider.URL), end: make(chan struct{})}
	serveStoppableControlPlane(t, srv)

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	select {
	case <-provider.reached:
	case <-time.After(eventWaitBudget):
		t.Fatal("the run never reached the provider")
	}
	close(srv.end)

	assertJobCancelledOnLoss(t, provider, sink, exporter, errCh)
}

func TestRunJob_ConnectionFailureDuringProviderCallCancelsRun(t *testing.T) {
	useTempMarkerPaths(t)
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	t.Setenv("CONTROL_PLANE_SESSION_ID", "")
	sink := &stubResultSink{}
	installStubResultSink(t, sink)
	exporter := &recordingExporter{}
	installRecordingExporter(t, exporter)
	provider := startBlockingToolCallProvider(t)

	srv := &losingControlPlane{task: streamLossJobConfig(t, provider.URL), end: make(chan struct{})}
	grpcServer := serveStoppableControlPlane(t, srv)

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	select {
	case <-provider.reached:
	case <-time.After(eventWaitBudget):
		t.Fatal("the run never reached the provider")
	}
	grpcServer.Stop()

	assertJobCancelledOnLoss(t, provider, sink, exporter, errCh)
}

func TestRunJob_StreamLostDuringConstructionStartsNoRun(t *testing.T) {
	useTempMarkerPaths(t)
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	t.Setenv("CONTROL_PLANE_SESSION_ID", "")
	sink := &stubResultSink{}
	installStubResultSink(t, sink)
	provider, requests := startOpenAIStub(t)

	srv := &losingControlPlane{task: followUpJobConfig(t, provider.URL, 30, 0), end: make(chan struct{})}
	serveStoppableControlPlane(t, srv)

	prevBuild := buildJobLoop
	t.Cleanup(func() { buildJobLoop = prevBuild })
	buildJobLoop = func(ctx context.Context, config *types.RunConfig, tp transport.Transport, opts ...core.LoopOption) (*core.AgenticLoop, error) {
		close(srv.end)
		select {
		case <-ctx.Done():
		case <-time.After(eventWaitBudget):
			t.Error("construction context was not cancelled by the stream loss")
		}
		return prevBuild(ctx, config, tp, opts...)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	select {
	case err := <-errCh:
		if !errors.Is(err, core.ErrControlStreamLost) || !strings.HasPrefix(err.Error(), "building harness: ") {
			t.Errorf("runJob error = %v, want a construction-time control stream loss", err)
		}
	case <-time.After(terminalWaitBudget):
		t.Fatal("runJob did not return after the control stream ended")
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("provider requests = %d, want 0: no run may start once the stream is gone", got)
	}
	if len(sink.calls) != 0 {
		t.Errorf("result sink received %d results, want 0: no run started", len(sink.calls))
	}
}

func TestRunJob_StreamEndedAfterDoneClosesFollowUpWindow(t *testing.T) {
	useTempMarkerPaths(t)
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	t.Setenv("CONTROL_PLANE_SESSION_ID", "")
	provider, requests := startOpenAIStub(t)

	const graceSecs = 30
	srv := newRecordingControlPlane(followUpJobConfig(t, provider.URL, 30, graceSecs))
	startControlPlane(t, srv)

	start := time.Now()
	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	select {
	case <-srv.doneCh:
	case <-time.After(terminalWaitBudget):
		t.Fatal("control plane never received done")
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("runJob error = %v, want nil: the run succeeded before the stream ended", err)
		}
	case <-time.After(graceSecs * time.Second / 2):
		t.Fatalf("runJob still waiting %v after the control stream ended", time.Since(start))
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("provider requests = %d, want 1", got)
	}
}

// fakeLiveness is a Transport with Liveness whose stream end the test
// controls.
type fakeLiveness struct {
	recordingTransport
	done chan struct{}
	err  error
}

func (f *fakeLiveness) Done() <-chan struct{} { return f.done }
func (f *fakeLiveness) Err() error            { return f.err }

func TestWatchControlStream(t *testing.T) {
	lossCause := errors.New("connection reset")
	tests := []struct {
		name     string
		err      error
		end      bool
		wantLoss error
	}{
		{name: "lost stream reports its cause", err: lossCause, end: true, wantLoss: lossCause},
		{name: "local close is not a loss", err: nil, end: true},
		{name: "live stream reports nothing", err: nil, end: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tp := &fakeLiveness{done: make(chan struct{}), err: tc.err}
			got := make(chan error, 1)
			stop := watchControlStream(tp, func(err error) { got <- err })
			if tc.end {
				close(tp.done)
			}
			if tc.wantLoss != nil {
				select {
				case err := <-got:
					if !errors.Is(err, tc.wantLoss) {
						t.Errorf("onLoss(%v), want %v", err, tc.wantLoss)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("onLoss was not called")
				}
			}
			stop()
			stop()
			select {
			case err := <-got:
				if tc.wantLoss == nil {
					t.Errorf("onLoss(%v) called, want no call", err)
				}
			default:
			}
		})
	}
}

func TestWatchControlStream_TransportWithoutLivenessIsNotWatched(t *testing.T) {
	stop := watchControlStream(&recordingTransport{}, func(error) { t.Error("onLoss called for a transport without liveness") })
	stop()
	if err := controlStreamErr(&recordingTransport{}); err != nil {
		t.Errorf("controlStreamErr() = %v, want nil", err)
	}
}

func TestPostRunPolicy_FollowUpStreamLossFailsTheProcess(t *testing.T) {
	policy := &postRunPolicy{emit: func(_ context.Context, _ *types.RunConfig, _ *types.RunTrace) {}}
	rt := finaliseTestTrace("run-2")
	rt.Outcome = "control_stream_lost"
	rt.Error = "control stream lost: control plane ended the stream"
	policy.finaliseFollowUp(&types.RunConfig{RunID: "run-2"}, rt, nil)

	err := policy.followUpErr()
	if err == nil || !strings.Contains(err.Error(), "run-2") || !strings.Contains(err.Error(), "control stream lost") {
		t.Fatalf("followUpErr() = %v, want the follow-up's loss", err)
	}
}

// silentControlPlane serves a `stirrup harness --transport grpc` stream:
// it sends nothing, drains what the harness sends, and ends the RPC once
// end is closed.
type silentControlPlane struct {
	pb.UnimplementedHarnessServiceServer
	end chan struct{}
}

func (s *silentControlPlane) RunTask(stream pb.HarnessService_RunTaskServer) error {
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				return
			}
		}
	}()
	select {
	case <-s.end:
		return nil
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
}

func TestRunWithConfig_GRPCTransportLossCancelsRun(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	sink := &stubResultSink{}
	installStubResultSink(t, sink)
	provider := startBlockingToolCallProvider(t)

	srv := &silentControlPlane{end: make(chan struct{})}
	serveStoppableControlPlane(t, srv)

	timeout := 30
	enforce := false
	config := &types.RunConfig{
		RunID:            "harness-stream-loss",
		Mode:             "execution",
		Prompt:           "Read the README.",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: provider.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "gpt-4o-mini"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		Transport:        types.TransportConfig{Type: "grpc", Address: os.Getenv("CONTROL_PLANE_ADDR")},
		RuleOfTwo:        &types.RuleOfTwoConfig{Enforce: &enforce},
		Tools:            types.ToolsConfig{BuiltIn: []string{"read_file"}},
		MaxTurns:         3,
		Timeout:          &timeout,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- runWithConfig(config, runOptions{outputMode: "text"}) }()

	select {
	case <-provider.reached:
	case <-time.After(eventWaitBudget):
		t.Fatal("the run never reached the provider")
	}
	close(srv.end)

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "control_stream_lost") {
			t.Errorf("runWithConfig error = %v, want the control_stream_lost outcome", err)
		}
	case <-time.After(terminalWaitBudget):
		t.Fatal("runWithConfig did not return after the control stream was lost")
	}
	if got := provider.requests.Load(); got != 1 {
		t.Errorf("provider requests = %d, want 1", got)
	}
	if len(sink.calls) != 1 || sink.calls[0].Outcome != "control_stream_lost" {
		t.Errorf("result sink calls = %+v, want one control_stream_lost result", sink.calls)
	}
}
