package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// doneReactingTransport fires a control event synchronously from
// inside the "done" Emit, which is the exact window between the run's
// terminal event and Run returning.
type doneReactingTransport struct {
	cancellableTransport
	onDone func()
	once   sync.Once
}

func (t *doneReactingTransport) Emit(event types.HarnessEvent) error {
	err := t.cancellableTransport.Emit(event)
	if event.Type == "done" && t.onDone != nil {
		t.once.Do(t.onDone)
	}
	return err
}

// TestLoop_CancelAfterDoneEndsSession pins that a cancel delivered
// after the run's "done" — the control plane's cue to send it — still
// ends the session: the follow-up window never opens.
func TestLoop_CancelAfterDoneEndsSession(t *testing.T) {
	must := require.New(t)
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}}
	tr := &doneReactingTransport{}
	loop := buildTestLoop(&mockProvider{})
	loop.Provider = prov
	loop.Transport = tr
	loop.ensureControlRouting()
	tr.onDone = func() { tr.FireControl(types.ControlEvent{Type: "cancel"}) }
	config := buildTestConfig()

	rt, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("success", rt.Outcome, "the run had already finished when cancel arrived")

	start := time.Now()
	RunFollowUpLoop(context.Background(), loop, config, 30, FollowUpOptions{})
	elapsed := time.Since(start)
	must.LessOrEqual(elapsed, time.Second, "follow-up window stayed open after a post-done cancel")
}

// TestLoop_CancelBeforeRunCancelsItPromptly pins that a cancel arriving
// between loop construction and Run does not let the run proceed.
func TestLoop_CancelBeforeRunCancelsItPromptly(t *testing.T) {
	must := require.New(t)
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}}
	loop, tr := buildUserInputTestLoop(prov)
	tr.FireControl(types.ControlEvent{Type: "cancel"})

	rt, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err)
	must.Equal("cancelled", rt.Outcome)
	must.Empty(prov.params(), "provider was called after a pre-run cancel")
}

// blockingWarningTransport parks every "warning" Emit until released,
// standing in for a write mutex held during token streaming or a
// stalled flow-control window.
type blockingWarningTransport struct {
	cancellableTransport
	release chan struct{}
}

func (t *blockingWarningTransport) Emit(event types.HarnessEvent) error {
	if event.Type == "warning" {
		<-t.release
	}
	return t.cancellableTransport.Emit(event)
}

// TestLoop_RejectionWarningNeverBlocksControlDispatch pins that a
// rejected user_response does not stall the control handler on the
// transport's write path: a cancel sent while the warning cannot be
// emitted still lands, and the warning follows once the transport
// frees up.
func TestLoop_RejectionWarningNeverBlocksControlDispatch(t *testing.T) {
	must := require.New(t)
	tr := &blockingWarningTransport{release: make(chan struct{})}
	loop := buildTestLoop(&mockProvider{})
	loop.Transport = tr
	loop.ensureControlRouting()
	t.Cleanup(func() { _ = loop.Close() })

	tr.FireControl(userResponse("   ", "r-empty"))

	dispatched := make(chan struct{})
	go func() {
		tr.FireControl(types.ControlEvent{Type: "cancel"})
		close(dispatched)
	}()
	select {
	case <-dispatched:
	case <-time.After(2 * time.Second):
		must.FailNow("cancel dispatch blocked behind a pending warning emit")
	}
	must.True(loop.sessionCancelled.Load(), "cancel was dispatched but not applied")

	close(tr.release)
	w := awaitWarnings(t, &tr.cancellableTransport, 1)
	must.Len(w, 1, "the deferred rejection should be the only warning")
	must.Equal("r-empty", w[0].RequestID)
}

// TestRunFollowUpLoop_FlushesQueuedInputOnExit pins that input still
// queued when the follow-up window closes is rejected with a warning
// per event rather than dropped, on the no-window path and on the
// grace-expiry path.
func TestRunFollowUpLoop_FlushesQueuedInputOnExit(t *testing.T) {
	t.Run("no window configured", func(t *testing.T) {
		must := require.New(t)
		loop, tr := buildUserInputTestLoop(&recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}})
		tr.FireControl(userResponse("late one", "r-late-1"))
		tr.FireControl(userResponse("late two", "r-late-2"))

		RunFollowUpLoop(context.Background(), loop, buildTestConfig(), 0, FollowUpOptions{})

		w := warningEvents(tr)
		must.Len(w, 2, "want one warning per queued event")
		must.Equal("r-late-1", w[0].RequestID, "warnings should follow arrival order")
		must.Equal("r-late-2", w[1].RequestID)
		must.Contains(w[0].Message, "no follow-up window", "the warning should state the reason")
	})

	t.Run("grace expired with input queued", func(t *testing.T) {
		must := require.New(t)
		loop, tr := buildUserInputTestLoop(&recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}})
		// Fill the queue without a notify token so the loop waits on the
		// grace timer rather than starting a run.
		loop.userInput.push(queuedUserInput{RequestID: "r-stale", Text: "stale"})
		<-loop.userInput.notify

		RunFollowUpLoop(context.Background(), loop, buildTestConfig(), 1, FollowUpOptions{})

		w := warningEvents(tr)
		must.Len(w, 1, "want the stale event rejected on exit")
		must.Equal("r-stale", w[0].RequestID)
	})
}

// TestRunFollowUpLoop_CancelDuringFollowUpEndsSession pins that a
// cancel while a follow-up run is active cancels that run and closes
// the window instead of waiting out the grace period.
func TestRunFollowUpLoop_CancelDuringFollowUpEndsSession(t *testing.T) {
	must := require.New(t)
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}}
	loop, tr := buildUserInputTestLoop(prov)
	prov.onCall = func(i int) {
		if i == 0 {
			tr.FireControl(types.ControlEvent{Type: "cancel"})
		}
	}
	config := buildTestConfig()

	var outcome string
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunFollowUpLoop(context.Background(), loop, config, 30, FollowUpOptions{
			OnRunComplete: func(_ *types.RunConfig, rt *types.RunTrace, _ error) { outcome = rt.Outcome },
		})
	}()
	tr.FireControl(userResponse("start", "r1"))

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		must.FailNow("follow-up window stayed open after a cancel during the follow-up run")
	}
	must.Equal("cancelled", outcome, "follow-up outcome")
}
