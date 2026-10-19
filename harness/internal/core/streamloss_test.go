package core

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

func ignoreEmitFailure(string, error) {}

// countingTool replaces test_tool with a handler that counts invocations.
func countingTool(loop *AgenticLoop) *atomic.Int32 {
	calls := &atomic.Int32{}
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:        "test_tool",
		Description: "A test tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			calls.Add(1)
			return "tool result", nil
		},
	})
	loop.Tools = registry
	return calls
}

// blockingProvider blocks every Stream until its context ends, then
// reports the context error, and counts the calls it received.
type blockingProvider struct {
	calls   atomic.Int32
	entered chan struct{}
	once    sync.Once
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{entered: make(chan struct{})}
}

func (p *blockingProvider) Stream(ctx context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	p.calls.Add(1)
	p.once.Do(func() { close(p.entered) })
	ch := make(chan types.StreamEvent, 1)
	go func() {
		defer close(ch)
		<-ctx.Done()
		ch <- types.StreamEvent{Type: "error", Error: ctx.Err()}
	}()
	return ch, nil
}

func TestRun_ControlStreamLostDuringProviderCallCancelsRun(t *testing.T) {
	prov := newBlockingProvider()
	tr := &cancellableTransport{}
	loop := buildTestLoop(nil)
	loop.Provider = prov
	loop.Transport = tr
	git := &ctxProbeGit{}
	loop.Git = git
	handoff := &fakeHandoff{}
	loop.Handoff = handoff
	toolCalls := countingTool(loop)

	go func() {
		<-prov.entered
		loop.ControlStreamLost(transport.ErrPeerEndedStream)
	}()

	start := time.Now()
	rt, err := loop.Run(context.Background(), exportConfig(nil))
	require.NoError(t, err)
	require.Equal(t, "control_stream_lost", rt.Outcome)
	assert.Equal(t, int32(1), prov.calls.Load(), "no model request may follow the loss")
	assert.Zero(t, toolCalls.Load(), "no tool may run after the loss")
	assert.Contains(t, rt.Error, "control stream lost", "RunTrace.Error should describe the loss")
	assert.Contains(t, rt.Error, transport.ErrPeerEndedStream.Error(), "RunTrace.Error should include the loss cause")
	assertDoneStopReason(t, tr, "control_stream_lost")
	assertLiveDetached(t, "git finalise", git.observed(), gitFinaliseBudget, start)
	calls, obs := handoff.observed()
	require.Equal(t, 1, calls, "workspace handoff calls")
	assertLiveDetached(t, "workspace handoff", obs, handoffBudget, start)
}

func TestRun_ControlStreamLostBeforeInFlightResponseSkipsItsTools(t *testing.T) {
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptCallTestTool, scriptEndTurn}}
	loop, tr := buildUserInputTestLoop(prov)
	toolCalls := countingTool(loop)
	prov.onCall = func(i int) {
		if i == 0 {
			loop.ControlStreamLost(errors.New("rpc error: code = Unavailable desc = connection refused"))
		}
	}

	rt, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.Equal(t, "control_stream_lost", rt.Outcome)
	assert.Zero(t, toolCalls.Load(), "no tool from the in-flight response may run")
	assert.Len(t, prov.params(), 1, "only the in-flight response may reach the provider")
	assertDoneStopReason(t, tr, "control_stream_lost")
}

func TestRun_ControlStreamLostDuringToolCallCancelsTheTool(t *testing.T) {
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptCallTestTool, scriptEndTurn}}
	loop, tr := buildUserInputTestLoop(prov)
	var sawCancel atomic.Bool
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:        "test_tool",
		Description: "A test tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			loop.ControlStreamLost(transport.ErrPeerEndedStream)
			select {
			case <-ctx.Done():
				sawCancel.Store(true)
			case <-time.After(10 * time.Second):
			}
			return "tool result", nil
		},
	})
	loop.Tools = registry

	rt, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	assert.True(t, sawCancel.Load(), "the loss must cancel the tool handler's context")
	require.Equal(t, "control_stream_lost", rt.Outcome)
	assert.Len(t, prov.params(), 1, "no model request may follow the loss")
	assertDoneStopReason(t, tr, "control_stream_lost")
}

func TestRun_ControlStreamLostBeforeStartCancelsRunWithoutProviderCall(t *testing.T) {
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}}
	loop, _ := buildUserInputTestLoop(prov)
	loop.ControlStreamLost(transport.ErrPeerEndedStream)

	rt, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.Equal(t, "control_stream_lost", rt.Outcome)
	require.Empty(t, prov.params(), "the provider must not be called after a pre-run loss")
}

// ctxHonouringGit fails Setup once its context has ended, as a strategy
// running git through exec.CommandContext does.
type ctxHonouringGit struct{}

func (ctxHonouringGit) Setup(ctx context.Context, _, _ string) error { return ctx.Err() }
func (ctxHonouringGit) Checkpoint(context.Context, string) error     { return nil }
func (ctxHonouringGit) Finalise(context.Context, string) (*types.GitHandoff, error) {
	return nil, nil
}

// ctxHonouringPrompt fails Build once its context has ended.
type ctxHonouringPrompt struct{}

func (ctxHonouringPrompt) Build(ctx context.Context, _ prompt.PromptContext) (string, error) {
	return "system", ctx.Err()
}

func TestRun_ControlStreamLostBeforeSetupStepsReportsTheLoss(t *testing.T) {
	for name, configure := range map[string]func(*AgenticLoop){
		"git setup":    func(l *AgenticLoop) { l.Git = ctxHonouringGit{} },
		"prompt build": func(l *AgenticLoop) { l.Prompt = ctxHonouringPrompt{} },
	} {
		t.Run(name, func(t *testing.T) {
			prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}}
			loop, tr := buildUserInputTestLoop(prov)
			configure(loop)
			loop.ControlStreamLost(transport.ErrPeerEndedStream)

			rt, _ := loop.Run(context.Background(), buildTestConfig())
			require.NotNil(t, rt, "Run returned no trace")
			require.Equal(t, "control_stream_lost", rt.Outcome, "run error: %q", rt.Error)
			assertDoneStopReason(t, tr, "control_stream_lost")
		})
	}
}

func TestRun_ControlPlaneCancelOutranksALaterStreamLoss(t *testing.T) {
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}}
	loop, tr := buildUserInputTestLoop(prov)
	tr.FireControl(types.ControlEvent{Type: "cancel"})
	loop.ControlStreamLost(transport.ErrPeerEndedStream)

	rt, err := loop.Run(context.Background(), buildTestConfig())
	require.NoError(t, err)
	require.Equal(t, "cancelled", rt.Outcome, "the control plane asked first")
}

func TestControlStreamLost_ClosesIdleFollowUpWindow(t *testing.T) {
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}}
	loop, tr := buildUserInputTestLoop(prov)
	config := buildTestConfig()

	rt, err := loop.Run(context.Background(), config)
	require.NoError(t, err)
	require.Equal(t, "success", rt.Outcome)

	followUps := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunFollowUpLoop(context.Background(), loop, config, 30, FollowUpOptions{
			OnRunComplete: func(*types.RunConfig, *types.RunTrace, error) { followUps++ },
		})
	}()
	loop.ControlStreamLost(transport.ErrPeerEndedStream)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		require.Fail(t, "follow-up window stayed open after the control stream ended")
	}
	assert.Zero(t, followUps, "no follow-up run may start after the loss")
	assert.Len(t, prov.params(), 1, "only the primary run may reach the provider")
	var dones int
	for _, ev := range tr.Events() {
		if ev.Type == "done" {
			dones++
		}
	}
	assert.Equal(t, 1, dones, "an idle loss emits no extra done")
}

func TestControlStreamLost_DuringFollowUpRunCancelsRun(t *testing.T) {
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}}
	loop, tr := buildUserInputTestLoop(prov)
	prov.onCall = func(i int) {
		if i == 0 {
			loop.ControlStreamLost(transport.ErrPeerEndedStream)
		}
	}

	var outcome string
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunFollowUpLoop(context.Background(), loop, buildTestConfig(), 30, FollowUpOptions{
			OnRunComplete: func(_ *types.RunConfig, rt *types.RunTrace, _ error) { outcome = rt.Outcome },
		})
	}()
	tr.FireControl(userResponse("start", "r1"))

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		require.Fail(t, "follow-up window stayed open after a loss during the follow-up run")
	}
	require.Equal(t, "control_stream_lost", outcome, "follow-up run outcome")
}

// levelCounter is a slog.Handler that counts records by level and
// message.
type levelCounter struct {
	mu     sync.Mutex
	counts map[slog.Level]int
	msg    string
}

func newLevelCounter(msg string) *levelCounter {
	return &levelCounter{counts: map[slog.Level]int{}, msg: msg}
}

func (h *levelCounter) Enabled(context.Context, slog.Level) bool { return true }
func (h *levelCounter) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *levelCounter) WithGroup(string) slog.Handler            { return h }

func (h *levelCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Message != h.msg {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counts[r.Level]++
	return nil
}

func (h *levelCounter) count(level slog.Level) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[level]
}

func TestLogEmitFailure_OneWarningAfterStreamLoss(t *testing.T) {
	loop, _ := buildUserInputTestLoop(&recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}})
	logs := newLevelCounter("transport emit failed")
	loop.Logger = slog.New(logs)
	emitErr := errors.New("send harness event: EOF")

	loop.logEmitFailure("text_delta", emitErr)
	loop.logEmitFailure("text_delta", emitErr)
	require.Equal(t, 2, logs.count(slog.LevelWarn), "a live stream reports every failure")

	loop.ControlStreamLost(transport.ErrPeerEndedStream)
	for range 5 {
		loop.logEmitFailure("tool_result", emitErr)
	}
	assert.Equal(t, 3, logs.count(slog.LevelWarn), "only the first failure after the loss warns")
	assert.Equal(t, 4, logs.count(slog.LevelDebug), "the four failures after the first log at debug")
}

// failingTransport rejects every Emit.
type failingTransport struct {
	cancellableTransport
	attempts atomic.Int32
}

func (t *failingTransport) Emit(types.HarnessEvent) error {
	t.attempts.Add(1)
	return errors.New("send harness event: EOF")
}

func TestStartHeartbeat_LogsOnlyTheFirstFailure(t *testing.T) {
	tr := &failingTransport{}
	loop := buildTestLoop(nil)
	loop.Transport = tr
	logs := newLevelCounter("transport emit failed")
	loop.Logger = slog.New(logs)

	stop := loop.startHeartbeat(context.Background(), time.Millisecond)
	t.Cleanup(stop)
	require.Eventually(t, func() bool { return tr.attempts.Load() >= 5 }, 5*time.Second, time.Millisecond, "heartbeat did not tick")
	stop()

	require.Equal(t, 1, logs.count(slog.LevelWarn)+logs.count(slog.LevelDebug), "heartbeat failure log records")
}
