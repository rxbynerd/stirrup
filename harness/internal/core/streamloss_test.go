package core

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

func ignoreEmitFailure(string, error) {}

// countingTool replaces test_tool with a handler that counts invocations
// and records whether its context was already cancelled.
func countingTool(loop *AgenticLoop) (calls *atomic.Int32, cancelledCalls *atomic.Int32) {
	calls, cancelledCalls = &atomic.Int32{}, &atomic.Int32{}
	registry := tool.NewRegistry()
	registry.Register(&tool.Tool{
		Name:        "test_tool",
		Description: "A test tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			calls.Add(1)
			if ctx.Err() != nil {
				cancelledCalls.Add(1)
			}
			return "tool result", nil
		},
	})
	loop.Tools = registry
	return calls, cancelledCalls
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
	toolCalls, _ := countingTool(loop)

	go func() {
		<-prov.entered
		loop.ControlStreamLost(transport.ErrPeerEndedStream)
	}()

	start := time.Now()
	rt, err := loop.Run(context.Background(), exportConfig(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "control_stream_lost" {
		t.Fatalf("outcome = %q, want control_stream_lost", rt.Outcome)
	}
	if n := prov.calls.Load(); n != 1 {
		t.Errorf("provider calls = %d, want 1: no model request may follow the loss", n)
	}
	if n := toolCalls.Load(); n != 0 {
		t.Errorf("tool ran %d times after the loss, want 0", n)
	}
	if !strings.Contains(rt.Error, "control stream lost") || !strings.Contains(rt.Error, transport.ErrPeerEndedStream.Error()) {
		t.Errorf("RunTrace.Error = %q, want the loss and its cause", rt.Error)
	}
	assertDoneStopReason(t, tr, "control_stream_lost")
	assertLiveDetached(t, "git finalise", git.observed(), gitFinaliseBudget, start)
	calls, obs := handoff.observed()
	if calls != 1 {
		t.Fatalf("workspace handoff calls = %d, want 1", calls)
	}
	assertLiveDetached(t, "workspace handoff", obs, handoffBudget, start)
}

func TestRun_ControlStreamLostBeforeInFlightResponseSkipsItsTools(t *testing.T) {
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptCallTestTool, scriptEndTurn}}
	loop, tr := buildUserInputTestLoop(prov)
	toolCalls, _ := countingTool(loop)
	prov.onCall = func(i int) {
		if i == 0 {
			loop.ControlStreamLost(errors.New("rpc error: code = Unavailable desc = connection refused"))
		}
	}

	rt, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "control_stream_lost" {
		t.Fatalf("outcome = %q, want control_stream_lost", rt.Outcome)
	}
	if n := toolCalls.Load(); n != 0 {
		t.Errorf("tool from the in-flight response ran %d times, want 0", n)
	}
	if n := len(prov.params()); n != 1 {
		t.Errorf("provider calls = %d, want 1", n)
	}
	assertDoneStopReason(t, tr, "control_stream_lost")
}

func TestRun_ControlStreamLostBeforeRunCancelsItWithTheLoss(t *testing.T) {
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}}
	loop, _ := buildUserInputTestLoop(prov)
	loop.ControlStreamLost(transport.ErrPeerEndedStream)

	rt, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "control_stream_lost" {
		t.Fatalf("outcome = %q, want control_stream_lost", rt.Outcome)
	}
	if n := len(prov.params()); n != 0 {
		t.Errorf("provider called %d times after a pre-run loss, want 0", n)
	}
}

func TestRun_ControlPlaneCancelOutranksALaterStreamLoss(t *testing.T) {
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}}
	loop, tr := buildUserInputTestLoop(prov)
	tr.FireControl(types.ControlEvent{Type: "cancel"})
	loop.ControlStreamLost(transport.ErrPeerEndedStream)

	rt, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "cancelled" {
		t.Fatalf("outcome = %q, want cancelled: the control plane asked first", rt.Outcome)
	}
}

func TestControlStreamLost_AfterDoneEndsFollowUpWindow(t *testing.T) {
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}}
	loop, tr := buildUserInputTestLoop(prov)
	config := buildTestConfig()

	rt, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "success" {
		t.Fatalf("outcome = %q, want success", rt.Outcome)
	}

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
		t.Fatal("follow-up window stayed open after the control stream ended")
	}
	if followUps != 0 {
		t.Errorf("follow-up runs = %d, want 0", followUps)
	}
	if n := len(prov.params()); n != 1 {
		t.Errorf("provider calls = %d, want 1 (the primary run only)", n)
	}
	var dones int
	for _, ev := range tr.Events() {
		if ev.Type == "done" {
			dones++
		}
	}
	if dones != 1 {
		t.Errorf("done events = %d, want 1: an idle loss emits no extra done", dones)
	}
}

func TestControlStreamLost_DuringFollowUpRunCancelsIt(t *testing.T) {
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
		t.Fatal("follow-up window stayed open after a loss during the follow-up run")
	}
	if outcome != "control_stream_lost" {
		t.Errorf("follow-up outcome = %q, want control_stream_lost", outcome)
	}
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
	if got := logs.count(slog.LevelWarn); got != 2 {
		t.Fatalf("warnings before any loss = %d, want 2: a live stream reports every failure", got)
	}

	loop.ControlStreamLost(transport.ErrPeerEndedStream)
	for range 5 {
		loop.logEmitFailure("tool_result", emitErr)
	}
	if got := logs.count(slog.LevelWarn); got != 3 {
		t.Errorf("warnings = %d, want 3: only the first failure after the loss warns", got)
	}
	if got := logs.count(slog.LevelDebug); got != 4 {
		t.Errorf("debug records = %d, want 4", got)
	}
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
	deadline := time.Now().Add(5 * time.Second)
	for tr.attempts.Load() < 5 {
		if time.Now().After(deadline) {
			stop()
			t.Fatal("heartbeat did not tick")
		}
		time.Sleep(time.Millisecond)
	}
	stop()

	if got := logs.count(slog.LevelWarn) + logs.count(slog.LevelDebug); got != 1 {
		t.Errorf("heartbeat failure log records = %d, want 1", got)
	}
}
