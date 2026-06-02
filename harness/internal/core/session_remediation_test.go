package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// blockingBackend never completes its sessions and shares no state, so the
// concurrency-cap hammer test can drive it under -race.
type blockingBackend struct{}

func (blockingBackend) start(_ *session, _ SubAgentConfig, _ string) error { return nil }
func (blockingBackend) terminate(_ *session)                               {}

// --- P0-B: concurrency cap under contention --------------------------------

func TestSessionManager_ConcurrencyCap_Concurrent(t *testing.T) {
	const limit = 3
	m := newTestManager(blockingBackend{}, limit)

	const goroutines = limit + 12
	var ok, failed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release all at once to maximise contention
			if _, err := m.Start(SubAgentConfig{Prompt: "x"}); err != nil {
				failed.Add(1)
			} else {
				ok.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if ok.Load() != limit {
		t.Fatalf("successful Starts = %d, want exactly %d (cap)", ok.Load(), limit)
	}
	if failed.Load() != goroutines-limit {
		t.Fatalf("failed Starts = %d, want %d", failed.Load(), goroutines-limit)
	}
}

// --- P0-A / P1-E: completeFromAsyncResult ----------------------------------

func TestCompleteFromAsyncResult_ScrubsSuccessPath(t *testing.T) {
	// A control plane returning a SubAgentResult whose Output embeds a
	// secret-shaped string must be scrubbed before it becomes the result.
	body, _ := json.Marshal(SubAgentResult{Outcome: "completed", Output: "leak: sk-ant-abc123_DEF-456"})
	got := completeFromAsyncResult(asyncToolResult{content: string(body)})
	if got.Outcome != "completed" {
		t.Fatalf("outcome = %q, want completed", got.Outcome)
	}
	if want := security.Scrub("leak: sk-ant-abc123_DEF-456"); got.Output != want {
		t.Fatalf("output = %q, want scrubbed %q", got.Output, want)
	}
	if got.Output == "leak: sk-ant-abc123_DEF-456" {
		t.Fatal("success-path output was not scrubbed")
	}
}

func TestCompleteFromAsyncResult_ScrubsRawFallback(t *testing.T) {
	// Non-SubAgentResult JSON falls back to surfacing the raw content, which
	// must also be scrubbed.
	got := completeFromAsyncResult(asyncToolResult{content: "plain sk-ant-abc123_DEF-456 text"})
	if got.Outcome != "done" {
		t.Fatalf("outcome = %q, want done", got.Outcome)
	}
	if got.Output == "plain sk-ant-abc123_DEF-456 text" {
		t.Fatal("fallback output was not scrubbed")
	}
}

func TestCompleteFromAsyncResult_UpstreamErrorScrubbed(t *testing.T) {
	got := completeFromAsyncResult(asyncToolResult{content: "boom sk-ant-abc123_DEF-456", isError: true})
	if got.Outcome != "upstream_error" {
		t.Fatalf("outcome = %q, want upstream_error", got.Outcome)
	}
	if got.Output == "boom sk-ant-abc123_DEF-456" {
		t.Fatal("upstream error output was not scrubbed")
	}
}

func TestCompleteFromAsyncResult_WrongType(t *testing.T) {
	got := completeFromAsyncResult("not an asyncToolResult")
	if got.Outcome != "error" {
		t.Fatalf("outcome = %q, want error", got.Outcome)
	}
}

func TestCompleteFromAsyncResult_MalformedJSON(t *testing.T) {
	got := completeFromAsyncResult(asyncToolResult{content: "{not valid json"})
	if got.Outcome != "done" {
		t.Fatalf("outcome = %q, want done (raw fallback)", got.Outcome)
	}
	if got.Output != "{not valid json" {
		t.Fatalf("output = %q, want raw content", got.Output)
	}
}

// --- P1-A: failure-outcome taxonomy ----------------------------------------

func TestSessionResultFromAwaitErr_Outcomes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"emit failure", fmt.Errorf("%w: boom", transport.ErrEmitFailed), "transport_disconnect"},
		{"deadline", fmt.Errorf("wrap: %w", context.DeadlineExceeded), "timeout"},
		{"canceled", fmt.Errorf("wrap: %w", context.Canceled), "cancelled"},
		{"unknown", errors.New("correlator: unknown request id"), "timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionResultFromAwaitErr(tc.err); got.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q", got.Outcome, tc.want)
			}
		})
	}
}

// --- P0-C: SessionController adapter ----------------------------------------

func TestSessionController_RoundTrip(t *testing.T) {
	fb := &fakeSessionBackend{}
	m := newTestManager(fb, 4)
	ctrl := newSessionController(m)

	id, err := ctrl.Start(context.Background(), "do work", "research", 5)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	statusJSON, err := ctrl.Status(id)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	var st SessionStatus
	if err := json.Unmarshal(statusJSON, &st); err != nil {
		t.Fatalf("Status JSON: %v", err)
	}
	if st.SessionID != id || st.State != SessionRunning {
		t.Fatalf("status = %#v, want running %q", st, id)
	}

	// Terminate returns the post-terminate snapshot.
	termJSON, err := ctrl.Terminate(id)
	if err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if err := json.Unmarshal(termJSON, &st); err != nil {
		t.Fatalf("Terminate JSON: %v", err)
	}
	if st.State != SessionTerminated {
		t.Fatalf("post-terminate state = %q, want terminated", st.State)
	}

	// Terminate on an unknown id is an error surfaced to the caller.
	if _, err := ctrl.Terminate("session-unknown"); err == nil {
		t.Fatal("Terminate unknown: err = nil, want error")
	}

	// Wait on a terminal session returns immediately with the snapshot.
	waitJSON, err := ctrl.Wait(context.Background(), id, 1)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if err := json.Unmarshal(waitJSON, &st); err != nil {
		t.Fatalf("Wait JSON: %v", err)
	}
	if st.State != SessionTerminated {
		t.Fatalf("wait state = %q, want terminated", st.State)
	}
}

// --- P0-C: local backend ---------------------------------------------------

func TestSessionManager_LocalBackend_StartAndComplete(t *testing.T) {
	prov := &mockProvider{events: []types.StreamEvent{
		{Type: "text_delta", Text: "local sub-agent output"},
		{Type: "message_complete", StopReason: "end_turn"},
	}}
	parent := buildSubAgentTestLoop(prov)
	parentConfig := buildTestConfig()

	m, err := NewSessionManager(SessionManagerOptions{
		UseTransport:  false,
		Parent:        parent,
		ParentConfig:  parentConfig,
		MaxConcurrent: 4,
		DefaultWait:   2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	defer func() { _ = m.Close() }()

	id, err := m.Start(SubAgentConfig{Prompt: "do a local subtask"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := m.Wait(context.Background(), id, 2*time.Second)
	if st.State != SessionDone {
		t.Fatalf("state = %q, want done; result=%#v", st.State, st.Result)
	}
	if st.Result == nil || st.Result.Output != "local sub-agent output" {
		t.Fatalf("result = %#v, want output 'local sub-agent output'", st.Result)
	}
}

func TestSessionManager_LocalBackend_Terminate(t *testing.T) {
	// slowProvider blocks until its context is cancelled, so the session
	// stays running until Terminate cancels the child context.
	parent := buildSubAgentTestLoop(&mockProvider{})
	parent.Provider = &slowProvider{}
	parentConfig := buildTestConfig()

	m, err := NewSessionManager(SessionManagerOptions{
		UseTransport:  false,
		Parent:        parent,
		ParentConfig:  parentConfig,
		MaxConcurrent: 4,
		DefaultWait:   time.Second,
	})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	defer func() { _ = m.Close() }()

	id, err := m.Start(SubAgentConfig{Prompt: "long local task"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Terminate(id); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if st := m.Status(id); st.State != SessionTerminated {
		t.Fatalf("state = %q, want terminated", st.State)
	}
}

// --- P1-F: Wait honours context cancellation -------------------------------

func TestSessionManager_Wait_CtxCancelled(t *testing.T) {
	fb := &fakeSessionBackend{onStart: func(*session) {}} // never completes
	m := newTestManager(fb, 4)
	id, _ := m.Start(SubAgentConfig{Prompt: "x"})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	// Long timeout: the only way Wait returns promptly is via ctx.
	st := m.Wait(ctx, id, 10*time.Second)
	if st.State != SessionRunning {
		t.Fatalf("state = %q, want running (cancelled while in flight)", st.State)
	}
}

// --- P1-C: out-of-order transport responses at the manager layer -----------

func TestSessionManager_Transport_OutOfOrder(t *testing.T) {
	tr := newAsyncTestTransport()
	m := newTransportManager(t, tr)
	defer func() { _ = m.Close() }()

	id1, _ := m.Start(SubAgentConfig{Prompt: "one"})
	id2, _ := m.Start(SubAgentConfig{Prompt: "two"})
	if id1 == id2 {
		t.Fatalf("session ids collided: %q", id1)
	}

	b1, _ := json.Marshal(SubAgentResult{Outcome: "completed", Output: "result-one"})
	b2, _ := json.Marshal(SubAgentResult{Outcome: "completed", Output: "result-two"})
	// Respond to the second session first.
	tr.FireControl(types.ControlEvent{Type: "tool_result_response", RequestID: id2, Content: string(b2)})
	tr.FireControl(types.ControlEvent{Type: "tool_result_response", RequestID: id1, Content: string(b1)})

	st1 := m.Wait(context.Background(), id1, time.Second)
	st2 := m.Wait(context.Background(), id2, time.Second)
	if st1.Result == nil || st1.Result.Output != "result-one" {
		t.Fatalf("id1 result = %#v, want result-one", st1.Result)
	}
	if st2.Result == nil || st2.Result.Output != "result-two" {
		t.Fatalf("id2 result = %#v, want result-two", st2.Result)
	}
}

// --- P1-G: terminate when the transport emit fails -------------------------

func TestSessionManager_Transport_TerminateAfterTransportFail(t *testing.T) {
	tr := newAsyncTestTransport()
	m := newTransportManager(t, tr)
	defer func() { _ = m.Close() }()

	id, err := m.Start(SubAgentConfig{Prompt: "x"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The transport dies before terminate is emitted; terminate must still
	// mark the session terminated without surfacing an error.
	tr.emitErr = errors.New("stream closed")
	if err := m.Terminate(id); err != nil {
		t.Fatalf("Terminate after transport fail: %v", err)
	}
	if st := m.Status(id); st.State != SessionTerminated {
		t.Fatalf("state = %q, want terminated", st.State)
	}
}

// --- P0-D: Close tears down live sessions; goroutines unwind ----------------

func TestSessionManager_Close_TerminatesLiveSessions(t *testing.T) {
	fb := &fakeSessionBackend{onStart: func(*session) {}} // never complete
	m := newTestManager(fb, 8)
	id1, _ := m.Start(SubAgentConfig{Prompt: "a"})
	id2, _ := m.Start(SubAgentConfig{Prompt: "b"})

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, id := range []string{id1, id2} {
		if st := m.Status(id); st.State != SessionTerminated {
			t.Fatalf("session %s state after Close = %q, want terminated", id, st.State)
		}
	}
	if fb.startedCount() != 2 {
		t.Fatalf("started = %d, want 2", fb.startedCount())
	}
}

func TestSessionManager_Transport_Close_UnwindsGoroutine(t *testing.T) {
	tr := newAsyncTestTransport()
	m := newTransportManager(t, tr)

	id, err := m.Start(SubAgentConfig{Prompt: "x"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	be := m.backend.(*transportSessionBackend)
	if be.correlator.RetainedCount() != 1 {
		t.Fatalf("RetainedCount before Close = %d, want 1", be.correlator.RetainedCount())
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if st := m.Status(id); st.State != SessionTerminated {
		t.Fatalf("state after Close = %q, want terminated", st.State)
	}
	// The awaiting goroutine forgets its correlator entry as it unwinds.
	waitForCondition(t, time.Second, func() bool { return be.correlator.RetainedCount() == 0 })
}

// --- P1-B: terminal sessions are evicted past the retention ceiling --------

func TestSessionManager_EvictsTerminalSessionsPastCeiling(t *testing.T) {
	// cap=1 -> maxTotal = sessionRetentionFactor. Each session completes
	// immediately (onStart), so only terminal sessions accumulate; the map
	// must never exceed maxTotal.
	fb := &fakeSessionBackend{onStart: func(s *session) {
		s.complete(&SubAgentResult{Outcome: "completed"})
	}}
	m := newTestManager(fb, 1)

	for i := 0; i < sessionRetentionFactor*3; i++ {
		if _, err := m.Start(SubAgentConfig{Prompt: "x"}); err != nil {
			t.Fatalf("Start #%d: %v", i, err)
		}
	}
	m.mu.Lock()
	n := len(m.sessions)
	m.mu.Unlock()
	if n > m.maxTotal {
		t.Fatalf("retained sessions = %d, want <= maxTotal %d", n, m.maxTotal)
	}
}

func waitForCondition(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("condition not met within deadline")
	}
}
