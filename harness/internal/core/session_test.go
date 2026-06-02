package core

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// fakeSessionBackend drives the SessionManager state machine without a real
// sub-agent. onStart, when set, is invoked synchronously during start so a
// test can complete (or not) the session deterministically.
type fakeSessionBackend struct {
	idCounter  int
	startErr   error
	onStart    func(sess *session)
	started    []*session
	terminated []*session
}

func (b *fakeSessionBackend) start(sess *session, _ SubAgentConfig) (string, error) {
	if b.startErr != nil {
		return "", b.startErr
	}
	b.idCounter++
	id := "session-" + strconv.Itoa(b.idCounter)
	b.started = append(b.started, sess)
	if b.onStart != nil {
		b.onStart(sess)
	}
	return id, nil
}

func (b *fakeSessionBackend) terminate(sess *session) {
	b.terminated = append(b.terminated, sess)
}

func newTestManager(b sessionBackend, maxConcurrent int) *SessionManager {
	bg, cancel := context.WithCancel(context.Background())
	return &SessionManager{
		backend:       b,
		maxConcurrent: maxConcurrent,
		defaultWait:   50 * time.Millisecond,
		logger:        slog.Default(),
		bgCtx:         bg,
		cancel:        cancel,
		sessions:      make(map[string]*session),
	}
}

func TestSessionManager_StartStatusComplete(t *testing.T) {
	fb := &fakeSessionBackend{}
	m := newTestManager(fb, 4)

	id, err := m.Start(SubAgentConfig{Prompt: "do a thing"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st := m.Status(id); st.State != SessionRunning {
		t.Fatalf("state after Start = %q, want running", st.State)
	}

	// Complete via the captured session, as a backend would.
	fb.started[0].complete(&SubAgentResult{Outcome: "completed", Output: "ok", Turns: 3})

	st := m.Status(id)
	if st.State != SessionDone {
		t.Fatalf("state after complete = %q, want done", st.State)
	}
	if st.Result == nil || st.Result.Output != "ok" || st.Result.Turns != 3 {
		t.Fatalf("result = %#v, want {completed ok 3}", st.Result)
	}

	// Wait returns the terminal snapshot without blocking.
	if w := m.Wait(context.Background(), id, time.Second); w.State != SessionDone {
		t.Fatalf("Wait state = %q, want done", w.State)
	}
}

func TestSessionManager_StartEmptyPrompt(t *testing.T) {
	m := newTestManager(&fakeSessionBackend{}, 4)
	if _, err := m.Start(SubAgentConfig{Prompt: ""}); err == nil {
		t.Fatal("Start with empty prompt: err = nil, want error")
	}
}

func TestSessionManager_ConcurrencyCap(t *testing.T) {
	fb := &fakeSessionBackend{}
	m := newTestManager(fb, 1)

	id1, err := m.Start(SubAgentConfig{Prompt: "first"})
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if _, err := m.Start(SubAgentConfig{Prompt: "second"}); err == nil {
		t.Fatal("second Start: err = nil, want session-limit error")
	}

	// Completing the first frees the slot.
	fb.started[0].complete(&SubAgentResult{Outcome: "completed"})
	if st := m.Status(id1); st.State != SessionDone {
		t.Fatalf("id1 state = %q, want done", st.State)
	}
	if _, err := m.Start(SubAgentConfig{Prompt: "third"}); err != nil {
		t.Fatalf("third Start after slot freed: %v", err)
	}
}

func TestSessionManager_Terminate(t *testing.T) {
	fb := &fakeSessionBackend{}
	m := newTestManager(fb, 4)

	id, _ := m.Start(SubAgentConfig{Prompt: "long task"})
	if err := m.Terminate(id); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	st := m.Status(id)
	if st.State != SessionTerminated {
		t.Fatalf("state after Terminate = %q, want terminated", st.State)
	}
	if len(fb.terminated) != 1 {
		t.Fatalf("backend.terminate calls = %d, want 1", len(fb.terminated))
	}
	// Terminating again (already terminal) does not re-signal the backend.
	_ = m.Terminate(id)
	if len(fb.terminated) != 1 {
		t.Fatalf("backend.terminate calls after second Terminate = %d, want 1", len(fb.terminated))
	}
}

func TestSessionManager_TerminateUnknown(t *testing.T) {
	m := newTestManager(&fakeSessionBackend{}, 4)
	if err := m.Terminate("session-404"); err == nil {
		t.Fatal("Terminate unknown: err = nil, want error")
	}
}

func TestSessionManager_StatusAndWaitNotFound(t *testing.T) {
	m := newTestManager(&fakeSessionBackend{}, 4)
	if st := m.Status("nope"); st.State != SessionNotFound {
		t.Fatalf("Status unknown = %q, want not_found", st.State)
	}
	if st := m.Wait(context.Background(), "nope", 10*time.Millisecond); st.State != SessionNotFound {
		t.Fatalf("Wait unknown = %q, want not_found", st.State)
	}
}

func TestSessionManager_SpawnAndWait_Happy(t *testing.T) {
	fb := &fakeSessionBackend{onStart: func(s *session) {
		s.complete(&SubAgentResult{Outcome: "completed", Output: "result text", Turns: 2})
	}}
	m := newTestManager(fb, 4)

	res, err := m.SpawnAndWait(context.Background(), SubAgentConfig{Prompt: "go"}, time.Second)
	if err != nil {
		t.Fatalf("SpawnAndWait: %v", err)
	}
	if res.Outcome != "completed" || res.Output != "result text" {
		t.Fatalf("result = %#v, want {completed result text}", res)
	}
	// The one-shot spawn is forgotten, leaving no lingering session.
	if len(m.sessions) != 0 {
		t.Fatalf("sessions after SpawnAndWait = %d, want 0", len(m.sessions))
	}
}

func TestSessionManager_SpawnAndWait_Timeout(t *testing.T) {
	// Backend never completes the session.
	fb := &fakeSessionBackend{onStart: func(*session) {}}
	m := newTestManager(fb, 4)

	res, err := m.SpawnAndWait(context.Background(), SubAgentConfig{Prompt: "go"}, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("SpawnAndWait: %v", err)
	}
	if res.Outcome != "timeout" {
		t.Fatalf("outcome = %q, want timeout", res.Outcome)
	}
	if len(m.sessions) != 0 {
		t.Fatalf("sessions after timed-out SpawnAndWait = %d, want 0 (forgotten)", len(m.sessions))
	}
}

func TestSessionManager_SpawnAndWait_StartFailure(t *testing.T) {
	fb := &fakeSessionBackend{startErr: errors.New("correlator: emit failed: boom")}
	m := newTestManager(fb, 4)
	res, err := m.SpawnAndWait(context.Background(), SubAgentConfig{Prompt: "go"}, time.Second)
	if err != nil {
		t.Fatalf("SpawnAndWait: %v", err)
	}
	if res.Outcome != "transport_disconnect" {
		t.Fatalf("outcome = %q, want transport_disconnect", res.Outcome)
	}
}

// --- transport backend (real backend over a fake transport) ----------------

func newTransportManager(t *testing.T, tr transport.Transport) *SessionManager {
	t.Helper()
	m, err := NewSessionManager(SessionManagerOptions{
		UseTransport: true,
		Transport:    tr,
		MaxConcurrent: 4,
		DefaultWait:   time.Second,
		MaxLifetime:   2 * time.Second,
		Logger:        slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	return m
}

func TestSessionManager_Transport_RoundTrip(t *testing.T) {
	tr := newAsyncTestTransport()
	m := newTransportManager(t, tr)
	defer func() { _ = m.Close() }()

	id, err := m.Start(SubAgentConfig{Prompt: "investigate", Mode: "research", MaxTurns: 5})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The start dispatched a tool_result_request shaped like a spawn.
	ev := tr.Events()
	if len(ev) != 1 || ev[0].Type != "tool_result_request" || ev[0].ToolName != "spawn_agent" {
		t.Fatalf("emitted events = %#v, want one tool_result_request for spawn_agent", ev)
	}
	var sent SubAgentConfig
	if err := json.Unmarshal(ev[0].Input, &sent); err != nil || sent.Prompt != "investigate" || sent.Mode != "research" {
		t.Fatalf("emitted input = %s, want the SubAgentConfig", string(ev[0].Input))
	}
	if st := m.Status(id); st.State != SessionRunning {
		t.Fatalf("state before response = %q, want running", st.State)
	}

	// Control plane echoes a serialised SubAgentResult.
	body, _ := json.Marshal(SubAgentResult{Outcome: "completed", Output: "findings", Turns: 4})
	tr.FireControl(types.ControlEvent{Type: "tool_result_response", RequestID: id, Content: string(body)})

	st := m.Wait(context.Background(), id, time.Second)
	if st.State != SessionDone {
		t.Fatalf("state after response = %q, want done", st.State)
	}
	if st.Result == nil || st.Result.Output != "findings" || st.Result.Turns != 4 {
		t.Fatalf("result = %#v, want {completed findings 4}", st.Result)
	}
}

func TestSessionManager_Transport_UpstreamError(t *testing.T) {
	tr := newAsyncTestTransport()
	m := newTransportManager(t, tr)
	defer func() { _ = m.Close() }()

	id, _ := m.Start(SubAgentConfig{Prompt: "x"})
	isErr := true
	tr.FireControl(types.ControlEvent{Type: "tool_result_response", RequestID: id, Content: "kaboom", IsError: &isErr})

	st := m.Wait(context.Background(), id, time.Second)
	if st.State != SessionError {
		t.Fatalf("state = %q, want error", st.State)
	}
	if st.Result == nil || st.Result.Outcome != "upstream_error" {
		t.Fatalf("result = %#v, want upstream_error", st.Result)
	}
}

func TestSessionManager_Transport_Terminate(t *testing.T) {
	tr := newAsyncTestTransport()
	m := newTransportManager(t, tr)
	defer func() { _ = m.Close() }()

	id, _ := m.Start(SubAgentConfig{Prompt: "x"})
	if err := m.Terminate(id); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if st := m.Status(id); st.State != SessionTerminated {
		t.Fatalf("state = %q, want terminated", st.State)
	}
	if rid := tr.lastRequestID("session_terminate"); rid != id {
		t.Fatalf("session_terminate requestID = %q, want %q", rid, id)
	}
}

func TestSessionManager_Transport_EmitFailure(t *testing.T) {
	tr := newAsyncTestTransport()
	tr.emitErr = errors.New("stream closed")
	m := newTransportManager(t, tr)
	defer func() { _ = m.Close() }()

	if _, err := m.Start(SubAgentConfig{Prompt: "x"}); err == nil {
		t.Fatal("Start with failing transport: err = nil, want error")
	}
	res, err := m.SpawnAndWait(context.Background(), SubAgentConfig{Prompt: "x"}, time.Second)
	if err != nil {
		t.Fatalf("SpawnAndWait: %v", err)
	}
	if res.Outcome != "transport_disconnect" {
		t.Fatalf("outcome = %q, want transport_disconnect", res.Outcome)
	}
}

func TestNewSessionManager_TransportOnNull(t *testing.T) {
	_, err := NewSessionManager(SessionManagerOptions{
		UseTransport: true,
		Transport:    &transport.NullTransport{},
	})
	if err == nil {
		t.Fatal("NewSessionManager(transport, NullTransport): err = nil, want misconfiguration error")
	}
}

// TestSubAgentExcludedTools_RecursionGuard pins that a spawned sub-agent's
// tool registry withholds spawn_agent AND every session tool, so a
// sub-agent can neither recurse via spawn_agent nor open its own detached
// sessions outside the parent's concurrency cap and lifetime management.
func TestSubAgentExcludedTools_RecursionGuard(t *testing.T) {
	parent := tool.NewRegistry()
	keep := &tool.Tool{Name: "read_file", Handler: func(context.Context, json.RawMessage) (string, error) { return "", nil }}
	parent.Register(keep)
	for _, name := range subAgentExcludedTools {
		parent.Register(&tool.Tool{Name: name, Handler: func(context.Context, json.RawMessage) (string, error) { return "", nil }})
	}

	child := filterToolRegistry(parent, subAgentExcludedTools...)
	if child.Resolve("read_file") == nil {
		t.Error("read_file should survive the sub-agent filter")
	}
	for _, name := range subAgentExcludedTools {
		if child.Resolve(name) != nil {
			t.Errorf("excluded tool %q leaked into the sub-agent registry", name)
		}
	}
}
