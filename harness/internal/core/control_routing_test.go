package core

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

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
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}}
	tr := &doneReactingTransport{}
	loop := buildTestLoop(&mockProvider{})
	loop.Provider = prov
	loop.Transport = tr
	loop.ensureControlRouting()
	tr.onDone = func() { tr.FireControl(types.ControlEvent{Type: "cancel"}) }
	config := buildTestConfig()

	rt, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "success" {
		t.Fatalf("outcome = %q, want success (the run had already finished when cancel arrived)", rt.Outcome)
	}

	start := time.Now()
	RunFollowUpLoop(context.Background(), loop, config, 30, FollowUpOptions{})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("follow-up window stayed open %v after a post-done cancel", elapsed)
	}
}

// TestLoop_CancelBeforeRunCancelsItPromptly pins that a cancel arriving
// between loop construction and Run does not let the run proceed.
func TestLoop_CancelBeforeRunCancelsItPromptly(t *testing.T) {
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}}
	loop, tr := buildUserInputTestLoop(prov)
	tr.FireControl(types.ControlEvent{Type: "cancel"})

	rt, err := loop.Run(context.Background(), buildTestConfig())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rt.Outcome != "cancelled" {
		t.Fatalf("outcome = %q, want cancelled", rt.Outcome)
	}
	if n := len(prov.params()); n != 0 {
		t.Errorf("provider called %d times after a pre-run cancel, want 0", n)
	}
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
		t.Fatal("cancel dispatch blocked behind a pending warning emit")
	}
	if !loop.sessionCancelled.Load() {
		t.Fatal("cancel was dispatched but not applied")
	}

	close(tr.release)
	w := awaitWarnings(t, &tr.cancellableTransport, 1)
	if len(w) != 1 || w[0].RequestID != "r-empty" {
		t.Fatalf("warnings after release = %+v, want the deferred rejection", w)
	}
}

// TestRunFollowUpLoop_FlushesQueuedInputOnExit pins that input still
// queued when the follow-up window closes is rejected with a warning
// per event rather than dropped, on the no-window path and on the
// grace-expiry path.
func TestRunFollowUpLoop_FlushesQueuedInputOnExit(t *testing.T) {
	t.Run("no window configured", func(t *testing.T) {
		loop, tr := buildUserInputTestLoop(&recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}})
		tr.FireControl(userResponse("late one", "r-late-1"))
		tr.FireControl(userResponse("late two", "r-late-2"))

		RunFollowUpLoop(context.Background(), loop, buildTestConfig(), 0, FollowUpOptions{})

		w := warningEvents(tr)
		if len(w) != 2 || w[0].RequestID != "r-late-1" || w[1].RequestID != "r-late-2" {
			t.Fatalf("warnings = %+v, want one per queued event in arrival order", w)
		}
		if !strings.Contains(w[0].Message, "no follow-up window") {
			t.Errorf("warning message = %q, want the reason", w[0].Message)
		}
	})

	t.Run("grace expired with input queued", func(t *testing.T) {
		loop, tr := buildUserInputTestLoop(&recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}})
		// Fill the queue without a notify token so the loop waits on the
		// grace timer rather than starting a run.
		loop.userInput.push(queuedUserInput{RequestID: "r-stale", Text: "stale"})
		<-loop.userInput.notify

		RunFollowUpLoop(context.Background(), loop, buildTestConfig(), 1, FollowUpOptions{})

		w := warningEvents(tr)
		if len(w) != 1 || w[0].RequestID != "r-stale" {
			t.Fatalf("warnings = %+v, want the stale event rejected on exit", w)
		}
	})
}

// TestRunFollowUpLoop_CancelDuringFollowUpEndsSession pins that a
// cancel while a follow-up run is active cancels that run and closes
// the window instead of waiting out the grace period.
func TestRunFollowUpLoop_CancelDuringFollowUpEndsSession(t *testing.T) {
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
		t.Fatal("follow-up window stayed open after a cancel during the follow-up run")
	}
	if outcome != "cancelled" {
		t.Errorf("follow-up outcome = %q, want cancelled", outcome)
	}
}
