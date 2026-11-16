package core

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

const testLivenessInterval = 20 * time.Millisecond

// stallFor is long enough for several testLivenessInterval ticks.
const stallFor = 10 * testLivenessInterval

// heartbeatPhases returns the phase of every heartbeat recorded so far,
// with "" for a heartbeat that carries none.
func (t *recordingTransport) heartbeatPhases() []types.SessionPhase {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []types.SessionPhase
	for _, e := range t.events {
		if e.Type != "heartbeat" {
			continue
		}
		if e.SessionPhase == nil {
			out = append(out, types.SessionPhase{})
			continue
		}
		out = append(out, *e.SessionPhase)
	}
	return out
}

func (t *recordingTransport) snapshot() []types.HarnessEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]types.HarnessEvent(nil), t.events...)
}

func countPhase(phases []types.SessionPhase, want types.SessionPhase) int {
	n := 0
	for _, p := range phases {
		if p == want {
			n++
		}
	}
	return n
}

func TestSessionHeartbeat_SendsAtStartAndEveryInterval(t *testing.T) {
	must := require.New(t)
	rec := &recordingTransport{}
	awaiting := types.SessionPhase{Phase: types.SessionPhaseAwaitingAssignment}

	h := StartSessionHeartbeat(rec, testLivenessInterval, awaiting, nil)
	defer h.Stop()

	must.Equal([]types.SessionPhase{awaiting}, rec.heartbeatPhases(), "a heartbeat is sent at start")
	must.Eventually(func() bool { return countPhase(rec.heartbeatPhases(), awaiting) >= 4 },
		5*time.Second, time.Millisecond, "heartbeats did not keep arriving at the interval")
}

func TestSessionHeartbeat_PhaseChangeSendsAtOnce(t *testing.T) {
	must := require.New(t)
	rec := &recordingTransport{}
	h := StartSessionHeartbeat(rec, time.Hour, types.SessionPhase{Phase: types.SessionPhaseAwaitingAssignment}, nil)
	defer h.Stop()

	constructing := types.SessionPhase{Phase: types.SessionPhaseConstructing, Step: types.SessionStepSandboxCreate}
	h.SetPhase(constructing)
	h.SetPhase(constructing)

	must.Equal([]types.SessionPhase{
		{Phase: types.SessionPhaseAwaitingAssignment},
		constructing,
	}, rec.heartbeatPhases(), "a repeated phase sends nothing")
}

func TestSessionHeartbeat_PauseHoldsHeartbeatsUntilNextPhase(t *testing.T) {
	must := require.New(t)
	rec := &recordingTransport{}
	postRun := types.SessionPhase{Phase: types.SessionPhasePostRun}
	h := StartSessionHeartbeat(rec, testLivenessInterval, postRun, nil)
	defer h.Stop()

	h.Pause()
	paused := len(rec.heartbeatPhases())
	time.Sleep(stallFor)
	must.Len(rec.heartbeatPhases(), paused, "a paused heartbeat sends nothing")

	h.SetPhase(postRun)
	must.Len(rec.heartbeatPhases(), paused+1, "the next SetPhase resumes with a heartbeat, even for the same phase")
	must.Eventually(func() bool { return len(rec.heartbeatPhases()) >= paused+3 },
		5*time.Second, time.Millisecond, "ticks did not resume after the pause")
}

func TestSessionHeartbeat_StopEndsHeartbeats(t *testing.T) {
	must := require.New(t)
	rec := &recordingTransport{}
	h := StartSessionHeartbeat(rec, time.Millisecond, types.SessionPhase{Phase: types.SessionPhaseRunning}, nil)
	must.Eventually(func() bool { return len(rec.heartbeatPhases()) >= 3 }, 5*time.Second, time.Millisecond)

	h.Stop()
	h.Stop()
	stopped := len(rec.heartbeatPhases())
	h.SetPhase(types.SessionPhase{Phase: types.SessionPhaseIdleFollowUp})
	time.Sleep(20 * time.Millisecond)
	must.Len(rec.heartbeatPhases(), stopped, "nothing is sent after Stop returns")
}

func TestSessionHeartbeat_SetIntervalChangesCadence(t *testing.T) {
	must := require.New(t)
	rec := &recordingTransport{}
	h := StartSessionHeartbeat(rec, time.Hour, types.SessionPhase{Phase: types.SessionPhaseConstructing}, nil)
	defer h.Stop()

	h.SetInterval(0)
	time.Sleep(stallFor)
	must.Len(rec.heartbeatPhases(), 1, "a non-positive interval is ignored")

	h.SetInterval(testLivenessInterval)
	must.Eventually(func() bool { return len(rec.heartbeatPhases()) >= 3 },
		5*time.Second, time.Millisecond, "the shorter interval did not take effect")
}

// recordingLiveness records what a loop reports to its SessionLiveness.
type recordingLiveness struct {
	mu     sync.Mutex
	calls  []string
	phases []types.SessionPhase
}

func (r *recordingLiveness) SetPhase(phase types.SessionPhase) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "phase")
	r.phases = append(r.phases, phase)
}

func (r *recordingLiveness) SetInterval(time.Duration) {}

func (r *recordingLiveness) Pause() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "pause")
}

func hookedTestConfig() *types.RunConfig {
	config := buildTestConfig()
	config.Hooks = &types.HooksConfig{
		PreRun:  []types.HookConfig{{Name: "pre", Command: "true"}},
		PostRun: []types.HookConfig{{Name: "post", Command: "true"}},
	}
	return config
}

func TestLoopRun_SessionLivenessReportsEachPhase(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(simpleSuccessProvider())
	rec := &recordingTransport{}
	loop.Transport = rec
	loop.Hooks = &fakeHookRunner{}
	liveness := &recordingLiveness{}
	loop.Liveness = liveness

	_, err := loop.Run(context.Background(), hookedTestConfig())
	must.NoError(err)

	must.Equal([]types.SessionPhase{
		{Phase: types.SessionPhaseConstructing, Step: types.SessionStepRunSetup},
		{Phase: types.SessionPhaseConstructing, Step: types.SessionStepPreRunHooks},
		{Phase: types.SessionPhaseConstructing, Step: types.SessionStepRunSetup},
		{Phase: types.SessionPhaseRunning},
		{Phase: types.SessionPhasePostRun},
		{Phase: types.SessionPhasePostRun, Step: types.SessionStepPostRunHooks},
		{Phase: types.SessionPhasePostRun},
	}, liveness.phases)
	must.Equal("pause", liveness.calls[len(liveness.calls)-1], "the loop pauses the session heartbeat before done")
	must.Zero(rec.heartbeatCount(), "a loop with a session liveness sends no heartbeat of its own")
}

func TestLoopRun_SessionLivenessSkipsHookStepsWithoutHooks(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(simpleSuccessProvider())
	loop.Transport = &recordingTransport{}
	liveness := &recordingLiveness{}
	loop.Liveness = liveness

	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)

	must.Equal([]types.SessionPhase{
		{Phase: types.SessionPhaseConstructing, Step: types.SessionStepRunSetup},
		{Phase: types.SessionPhaseRunning},
		{Phase: types.SessionPhasePostRun},
	}, liveness.phases)
}

func TestLoopRun_SessionLivenessReportsPostRunAfterSetupFailure(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(simpleSuccessProvider())
	loop.Transport = &recordingTransport{}
	loop.Hooks = &fakeHookRunner{preErr: errSentinelPreHookFailure}
	liveness := &recordingLiveness{}
	loop.Liveness = liveness

	_, err := loop.Run(context.Background(), hookedTestConfig())
	must.Error(err)

	must.NotContains(liveness.phases, types.SessionPhase{Phase: types.SessionPhaseRunning}, "a run that fails setup never runs")
	must.Equal(types.SessionPhase{Phase: types.SessionPhasePostRun}, liveness.phases[len(liveness.phases)-1])
	must.Equal("pause", liveness.calls[len(liveness.calls)-1])
}

func TestLoopRun_SubAgentLoopIgnoresSessionLiveness(t *testing.T) {
	must := require.New(t)
	loop := buildTestLoop(simpleSuccessProvider())
	rec := &recordingTransport{}
	loop.Transport = rec
	loop.HeartbeatInterval = time.Millisecond
	loop.ParentRunID = "parent-run"
	loop.Provider = &fireAndCloseProvider{onStream: func() { time.Sleep(stallFor) }}
	liveness := &recordingLiveness{}
	loop.Liveness = liveness

	config := buildTestConfig()
	config.MaxTurns = 1
	_, _ = loop.Run(context.Background(), config)

	must.Empty(liveness.phases, "a sub-agent reports no session phase")
	must.Positive(rec.heartbeatCount(), "a sub-agent keeps its own run heartbeat")
}

// blockUntilDoneProvider holds every stream open until its context ends.
type blockUntilDoneProvider struct{}

func (blockUntilDoneProvider) Stream(ctx context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestLoopRun_SessionHeartbeatCoversEveryStalledPhase stalls the preRun
// hooks, a model turn and a postRun hook that outlives the run's
// deadline, and requires several heartbeats tagged with each.
func TestLoopRun_SessionHeartbeatCoversEveryStalledPhase(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	loop := buildTestLoop(nil)
	rec := &recordingTransport{}
	loop.Transport = rec
	deadline := 3 * stallFor
	loop.Provider = blockUntilDoneProvider{}
	var postCtxErr error
	loop.Hooks = &fakeHookRunner{
		onPre: func() { time.Sleep(stallFor) },
		onPost: func(ctx context.Context) {
			time.Sleep(stallFor)
			postCtxErr = ctx.Err()
		},
	}
	h := StartSessionHeartbeat(rec, testLivenessInterval, types.SessionPhase{Phase: types.SessionPhaseConstructing}, nil)
	defer h.Stop()
	loop.Liveness = h

	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	runTrace, err := loop.Run(ctx, hookedTestConfig())
	must.NoError(err)
	must.Equal("timeout", runTrace.Outcome, "the postRun hook runs after the run's deadline")
	must.NoError(postCtxErr, "the postRun hook runs on a detached context")

	phases := rec.heartbeatPhases()
	for _, want := range []types.SessionPhase{
		{Phase: types.SessionPhaseConstructing, Step: types.SessionStepPreRunHooks},
		{Phase: types.SessionPhaseRunning},
		{Phase: types.SessionPhasePostRun, Step: types.SessionStepPostRunHooks},
	} {
		check.GreaterOrEqual(countPhase(phases, want), 3, "heartbeats during %+v; got %v", want, phases)
	}
	events := rec.snapshot()
	must.Equal("done", events[len(events)-1].Type, "no heartbeat follows done while the session is paused")
}

func (r *recordingLiveness) snapshot() []types.SessionPhase {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]types.SessionPhase(nil), r.phases...)
}

func TestRunFollowUpLoop_SessionLivenessReportsIdleWindowAndFollowUpRun(t *testing.T) {
	must := require.New(t)
	loop, tr := buildFollowUpTestLoop(t)
	liveness := &recordingLiveness{}
	loop.Liveness = liveness
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ran := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunFollowUpLoop(ctx, loop, buildTestConfig(), 30, FollowUpOptions{
			OnRunComplete: func(*types.RunConfig, *types.RunTrace, error) { ran <- struct{}{} },
		})
	}()
	must.Eventually(func() bool { return len(liveness.snapshot()) > 0 }, 5*time.Second, time.Millisecond)
	tr.FireControl(types.ControlEvent{Type: "user_response", UserResponse: "again"})
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		must.FailNow("follow-up run did not complete")
	}
	must.Eventually(func() bool { return len(liveness.snapshot()) >= 5 }, 5*time.Second, time.Millisecond)
	cancel()
	<-done

	idle := types.SessionPhase{Phase: types.SessionPhaseIdleFollowUp}
	must.Equal([]types.SessionPhase{
		idle,
		{Phase: types.SessionPhaseConstructing, Step: types.SessionStepRunSetup},
		{Phase: types.SessionPhaseRunning},
		{Phase: types.SessionPhasePostRun},
		idle,
	}, liveness.snapshot(), "the window reopens after the follow-up run")
}

func TestRunFollowUpLoop_SessionLivenessReportsNothingWithoutWindow(t *testing.T) {
	must := require.New(t)
	loop, _ := buildFollowUpTestLoop(t)
	liveness := &recordingLiveness{}
	loop.Liveness = liveness

	RunFollowUpLoop(context.Background(), loop, buildTestConfig(), 0, FollowUpOptions{})

	must.Empty(liveness.snapshot(), "no idle phase is reported when no window opens")
}

// TestRunFollowUpLoop_SessionHeartbeatCoversStalledIdleWindow requires
// heartbeats through an idle follow-up window and none after it closes.
func TestRunFollowUpLoop_SessionHeartbeatCoversStalledIdleWindow(t *testing.T) {
	must := require.New(t)
	loop, _ := buildFollowUpTestLoop(t)
	rec := &recordingTransport{}
	loop.Transport = rec
	h := StartSessionHeartbeat(rec, testLivenessInterval, types.SessionPhase{Phase: types.SessionPhasePostRun}, nil)
	h.Pause()
	loop.Liveness = h

	RunFollowUpLoop(context.Background(), loop, buildTestConfig(), 1, FollowUpOptions{})
	h.Stop()

	idle := types.SessionPhase{Phase: types.SessionPhaseIdleFollowUp}
	must.GreaterOrEqual(countPhase(rec.heartbeatPhases(), idle), 3, "heartbeats during the idle window")
}

func TestSessionHeartbeat_LogsOnlyTheFirstFailure(t *testing.T) {
	must := require.New(t)
	tr := &failingTransport{}
	logs := newLevelCounter("transport emit failed")

	h := StartSessionHeartbeat(tr, time.Millisecond, types.SessionPhase{Phase: types.SessionPhaseRunning}, slog.New(logs))
	must.Eventually(func() bool { return tr.attempts.Load() >= 5 }, 5*time.Second, time.Millisecond, "heartbeat did not tick")
	h.Stop()

	must.Equal(1, logs.count(slog.LevelWarn), "heartbeat failure log records")
}
