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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	must := require.New(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(err)
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

// waitCancelled reports whether the server observed the client abandon
// the first request within eventWaitBudget. The handler notices the
// disconnect asynchronously, possibly after runJob has returned.
func (p *blockingToolCallProvider) waitCancelled() bool {
	deadline := time.Now().Add(eventWaitBudget)
	for !p.cancelled.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return p.cancelled.Load()
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
	must := require.New(t)
	check := assert.New(t)
	select {
	case err := <-errCh:
		must.Error(err, "runJob returned nil, want the loss to fail the process")
		must.ErrorContains(err, "control_stream_lost", "runJob should name the control_stream_lost outcome")
	case <-time.After(terminalWaitBudget):
		must.FailNow("runJob did not return after the control stream was lost")
	}

	check.Equal(int32(1), provider.requests.Load(), "no model request may follow the loss")
	check.True(provider.waitCancelled(), "the in-flight provider request was not cancelled")
	must.Len(sink.calls, 1, "the result sink should receive exactly one result")
	result := sink.calls[0]
	check.Equal("control_stream_lost", result.Outcome)
	check.Contains(result.Error, "control stream lost", "RunResult.Error should name the loss")
	check.Equal([]string{"gs://bucket/runs/job-stream-loss/workspace.tar.gz"}, exporter.destinations(),
		"the workspace export runs after a loss")
}

func TestRunJob_ControlPlaneEndingRPCDuringProviderCallCancelsRun(t *testing.T) {
	must := require.New(t)
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
		must.FailNow("the run never reached the provider")
	}
	close(srv.end)

	assertJobCancelledOnLoss(t, provider, sink, exporter, errCh)
}

func TestRunJob_ConnectionFailureDuringProviderCallCancelsRun(t *testing.T) {
	must := require.New(t)
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
		must.FailNow("the run never reached the provider")
	}
	grpcServer.Stop()

	assertJobCancelledOnLoss(t, provider, sink, exporter, errCh)
}

func TestRunJob_StreamLostDuringConstructionStartsNoRun(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
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
			check.Fail("construction context was not cancelled by the stream loss")
		}
		return prevBuild(ctx, config, tp, opts...)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- runJob(jobCmd, nil) }()

	select {
	case err := <-errCh:
		must.ErrorIs(err, core.ErrControlStreamLost, "runJob should report the loss during construction")
		must.True(strings.HasPrefix(err.Error(), "building harness: "), "the error should come from building the harness")
	case <-time.After(terminalWaitBudget):
		must.FailNow("runJob did not return after the control stream ended")
	}
	check.Equal(int32(0), requests.Load(), "no run may start once the stream is gone")
	check.Empty(sink.calls, "no result is recorded when no run started")
}

func TestRunJob_StreamEndedAfterDoneClosesFollowUpWindow(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
		must.FailNow("control plane never received done")
	}
	select {
	case err := <-errCh:
		must.NoError(err, "the run succeeded before the stream ended")
	case <-time.After(graceSecs * time.Second / 2):
		must.FailNowf("runJob did not return after the control stream ended", "waited %v", time.Since(start))
	}
	check.Equal(int32(1), requests.Load())
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

func TestWatchControlStream_ReportsLossCause(t *testing.T) {
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
			must := require.New(t)
			tp := &fakeLiveness{done: make(chan struct{}), err: tc.err}
			got := make(chan error, 1)
			stop := watchControlStream(tp, func(err error) { got <- err })
			if tc.end {
				close(tp.done)
			}
			if tc.wantLoss != nil {
				select {
				case err := <-got:
					must.ErrorIs(err, tc.wantLoss)
				case <-time.After(5 * time.Second):
					must.FailNow("onLoss was not called")
				}
			}
			stop()
			stop()
			if tc.wantLoss == nil {
				select {
				case err := <-got:
					must.Failf("onLoss called, want no call", "onLoss(%v)", err)
				default:
				}
			}
		})
	}
}

func TestWatchControlStream_TransportWithoutLivenessIsNotWatched(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	stop := watchControlStream(&recordingTransport{}, func(error) {
		check.Fail("onLoss called for a transport without liveness")
	})
	stop()
	must.NoError(controlStreamErr(&recordingTransport{}), "a transport without liveness reports no stream error")
}

func TestPostRunPolicy_FollowUpStreamLossFailsTheProcess(t *testing.T) {
	must := require.New(t)
	policy := &postRunPolicy{emit: func(_ context.Context, _ *types.RunConfig, _ *types.RunTrace) {}}
	rt := finaliseTestTrace("run-2")
	rt.Outcome = "control_stream_lost"
	rt.Error = "control stream lost: control plane ended the stream"
	policy.finaliseFollowUp(&types.RunConfig{RunID: "run-2"}, rt, nil)

	err := policy.followUpErr()
	must.ErrorContains(err, "run-2", "the error should name the follow-up run")
	must.ErrorContains(err, "control stream lost", "the error should name the loss")
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
	check := assert.New(t)
	must := require.New(t)
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
		must.FailNow("the run never reached the provider")
	}
	close(srv.end)

	select {
	case err := <-errCh:
		must.ErrorContains(err, "control_stream_lost", "runWithConfig should return the control_stream_lost outcome")
	case <-time.After(terminalWaitBudget):
		must.FailNow("runWithConfig did not return after the control stream was lost")
	}
	check.Equal(int32(1), provider.requests.Load())
	must.Len(sink.calls, 1, "the run should record exactly one result")
	check.Equal("control_stream_lost", sink.calls[0].Outcome)
}
