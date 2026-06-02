package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool/builtins"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// SessionState is the lifecycle state of a sub-agent session (issue #71).
//
//   - running    — the sub-agent has been dispatched; no result yet.
//   - done       — the sub-agent finished with a non-error outcome.
//   - error      — the sub-agent finished with an error outcome (child
//     failure, upstream control-plane error, transport disconnect, or
//     timeout). The accompanying SubAgentResult.Outcome distinguishes them.
//   - terminated — the session was explicitly terminated by the agent
//     before it finished.
//   - not_found  — returned for a lookup against an unknown session id; it
//     is never stored on a live session.
type SessionState string

const (
	SessionRunning    SessionState = "running"
	SessionDone       SessionState = "done"
	SessionError      SessionState = "error"
	SessionTerminated SessionState = "terminated"
	SessionNotFound   SessionState = "not_found"
)

// SessionStatus is the snapshot returned by Status/Wait and serialised to
// the model by the session tools. Result is populated once the session
// reaches a terminal state (done/error/terminated).
type SessionStatus struct {
	SessionID string          `json:"sessionId"`
	State     SessionState    `json:"state"`
	Result    *SubAgentResult `json:"result,omitempty"`
}

// errorOutcomes are the SubAgentResult.Outcome values that map a terminal
// session to SessionError rather than SessionDone. The transport failure
// outcomes mirror dispatchAsyncToolCall's error taxonomy so a session and
// a blocking async tool surface the same vocabulary.
var errorOutcomes = map[string]bool{
	"error":                true,
	"upstream_error":       true,
	"transport_disconnect": true,
	"timeout":              true,
	"cancelled":            true,
}

// subAgentExcludedTools is the set of tool IDs withheld from a spawned
// sub-agent's registry. spawn_agent prevents unbounded recursion; the
// session tools prevent a sub-agent from opening its own detached
// sessions (which would escape the parent's concurrency cap and lifetime
// management). Kept as one list so the recursion guard cannot drift
// between the blocking and detached spawn paths.
var subAgentExcludedTools = []string{
	"spawn_agent",
	"start_session",
	"check_session",
	"wait_session",
	"terminate_session",
}

// session is one entry in the SessionManager's registry. state/result are
// guarded by mu; done is closed exactly once when the session reaches a
// terminal state, unblocking Wait. cancel stops the backend's work (local
// backend only).
type session struct {
	id   string
	seq  int
	mu   sync.Mutex
	done chan struct{}

	state  SessionState
	result *SubAgentResult

	cancel context.CancelFunc
}

func (s *session) status() SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SessionStatus{SessionID: s.id, State: s.state, Result: s.result}
}

func (s *session) isTerminal() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state != SessionRunning
}

// complete transitions a running session to its terminal state. A no-op if
// the session is already terminal (e.g. terminated while a late backend
// result was in flight), so the backend goroutine never panics on a double
// close.
func (s *session) complete(result *SubAgentResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != SessionRunning {
		return
	}
	s.result = result
	if result != nil && errorOutcomes[result.Outcome] {
		s.state = SessionError
	} else {
		s.state = SessionDone
	}
	close(s.done)
}

// markTerminated transitions a running session to terminated. Returns true
// if it made the transition (false if already terminal). The backend's
// terminate signal is sent by the caller only on a true return.
func (s *session) markTerminated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != SessionRunning {
		return false
	}
	s.state = SessionTerminated
	s.result = &SubAgentResult{Outcome: "terminated"}
	close(s.done)
	return true
}

// sessionBackend produces a sub-agent result into a session and stops its
// work on terminate. The transport backend dispatches over the wire and
// correlates the response; the local backend runs SpawnSubAgent in a
// goroutine.
type sessionBackend interface {
	// start kicks off the sub-agent for sess, arranging for sess.complete
	// to be called when it finishes. It must not block on the result. The
	// returned id becomes the session's public handle.
	start(sess *session, cfg SubAgentConfig) (id string, err error)
	// terminate signals the backend to stop sess's in-flight work.
	terminate(sess *session)
}

// SessionManager owns the registry of sub-agent sessions for one run and
// brokers the start/wait/status/terminate lifecycle behind whichever
// backend the run's spawner selects. It backs both the start-and-detach
// session tools (#71) and the blocking transport spawn_agent (#54), which
// is Start+Wait fused.
type SessionManager struct {
	backend       sessionBackend
	maxConcurrent int
	defaultWait   time.Duration
	logger        *slog.Logger

	bgCtx  context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	sessions map[string]*session
	seq      int
}

// SessionManagerOptions configures NewSessionManager.
type SessionManagerOptions struct {
	// UseTransport selects the transport backend (spawner=="transport").
	// When false the local in-process backend is used.
	UseTransport bool

	// Transport is the live control-plane transport. Required (and must
	// not be a NullTransport) when UseTransport is true.
	Transport transport.Transport

	// Parent and ParentConfig are required by the local backend to run
	// SpawnSubAgent. The transport backend ignores Parent.
	Parent       *AgenticLoop
	ParentConfig *types.RunConfig

	// MaxConcurrent caps simultaneously-running detached sessions.
	MaxConcurrent int

	// DefaultWait is the per-wait timeout applied when a caller passes a
	// non-positive timeout.
	DefaultWait time.Duration

	// MaxLifetime bounds how long the transport backend waits for a single
	// session's response before resolving it as a timeout. Sessions are
	// also torn down when the manager is Closed (run end).
	MaxLifetime time.Duration

	Logger *slog.Logger
}

// NewSessionManager constructs a SessionManager. It returns an error when
// the transport backend is requested without a transport that can deliver
// control-plane responses — the config-time analogue of
// dispatchAsyncToolCall's fast-fail, surfaced here so the misconfiguration
// is caught at factory construction rather than on the first spawn.
func NewSessionManager(opts SessionManagerOptions) (*SessionManager, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	defaultWait := opts.DefaultWait
	if defaultWait <= 0 {
		defaultWait = DefaultAsyncToolTimeout
	}
	maxConcurrent := opts.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = types.DefaultMaxConcurrentSessions
	}

	bgCtx, cancel := context.WithCancel(context.Background())

	m := &SessionManager{
		maxConcurrent: maxConcurrent,
		defaultWait:   defaultWait,
		logger:        logger,
		bgCtx:         bgCtx,
		cancel:        cancel,
		sessions:      make(map[string]*session),
	}

	if opts.UseTransport {
		if opts.Transport == nil || transport.IsNull(opts.Transport) {
			cancel()
			return nil, errors.New("session manager: transport spawner requires a live control-plane transport, but the loop has none")
		}
		maxLifetime := opts.MaxLifetime
		if maxLifetime <= 0 {
			maxLifetime = time.Hour
		}
		correlator := transport.NewCorrelator("session")
		correlator.AttachTo(opts.Transport, extractAsyncToolResult)
		m.backend = &transportSessionBackend{
			transport:   opts.Transport,
			correlator:  correlator,
			bgCtx:       bgCtx,
			maxLifetime: maxLifetime,
			complete:    completeFromAsyncResult,
		}
	} else {
		m.backend = &localSessionBackend{
			parent:       opts.Parent,
			parentConfig: opts.ParentConfig,
			bgCtx:        bgCtx,
			nextID:       m.nextLocalID,
		}
	}

	return m, nil
}

// Close cancels every live session and releases the manager's background
// context. Idempotent. Wired into the loop's owned closers so sessions do
// not outlive the run.
func (m *SessionManager) Close() error {
	m.cancel()
	m.mu.Lock()
	live := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		live = append(live, s)
	}
	m.mu.Unlock()
	for _, s := range live {
		if s.markTerminated() {
			m.backend.terminate(s)
		}
	}
	return nil
}

// Start dispatches a new detached session and returns its handle. It
// enforces the live-session cap. The session persists in the registry
// until the manager is Closed, so a later Status/Wait can observe its
// result.
func (m *SessionManager) Start(cfg SubAgentConfig) (string, error) {
	if cfg.Prompt == "" {
		return "", errors.New("session prompt must not be empty")
	}
	if err := m.reserveSlot(); err != nil {
		return "", err
	}

	sess := &session{done: make(chan struct{}), state: SessionRunning}
	id, err := m.backend.start(sess, cfg)
	if err != nil {
		return "", err
	}
	sess.id = id

	m.mu.Lock()
	m.seq++
	sess.seq = m.seq
	m.sessions[id] = sess
	m.mu.Unlock()
	return id, nil
}

// reserveSlot fails when the number of running sessions has reached the
// configured cap. Terminal sessions (done/error/terminated) do not count
// against the cap — they are kept only for later inspection.
func (m *SessionManager) reserveSlot() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	running := 0
	for _, s := range m.sessions {
		if !s.isTerminal() {
			running++
		}
	}
	if running >= m.maxConcurrent {
		return fmt.Errorf("session limit reached: %d concurrent sessions already running (max %d)", running, m.maxConcurrent)
	}
	return nil
}

// Status returns a non-blocking snapshot of a session. An unknown id
// yields a not_found status rather than an error so the model gets a
// structured answer it can reason about.
func (m *SessionManager) Status(id string) SessionStatus {
	sess := m.get(id)
	if sess == nil {
		return SessionStatus{SessionID: id, State: SessionNotFound}
	}
	return sess.status()
}

// Wait blocks until the session reaches a terminal state, the timeout
// elapses, or ctx is cancelled, then returns the current snapshot. A
// timeout is not an error: the returned status simply still reads
// "running", and the caller may wait again. An unknown id yields
// not_found.
func (m *SessionManager) Wait(ctx context.Context, id string, timeout time.Duration) SessionStatus {
	sess := m.get(id)
	if sess == nil {
		return SessionStatus{SessionID: id, State: SessionNotFound}
	}
	if timeout <= 0 {
		timeout = m.defaultWait
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-sess.done:
	case <-timer.C:
	case <-ctx.Done():
	}
	return sess.status()
}

// Terminate stops a session's in-flight work and marks it terminated.
// Unknown ids return an error so the model learns the handle was invalid.
func (m *SessionManager) Terminate(id string) error {
	sess := m.get(id)
	if sess == nil {
		return fmt.Errorf("unknown session %q", id)
	}
	if sess.markTerminated() {
		m.backend.terminate(sess)
	}
	return nil
}

// SpawnAndWait is the blocking transport spawn_agent path (#54): Start +
// Wait fused, with the session removed from the registry on return so a
// one-shot blocking spawn does not linger as a detached session or count
// against the concurrency cap beyond its own lifetime.
func (m *SessionManager) SpawnAndWait(ctx context.Context, cfg SubAgentConfig, timeout time.Duration) (*SubAgentResult, error) {
	id, err := m.Start(cfg)
	if err != nil {
		// A start failure (emit/transport) is surfaced as a result, not a
		// Go error, so spawn_agent behaves like its in-process sibling,
		// which encodes failure in the result rather than failing the tool.
		return &SubAgentResult{Outcome: "transport_disconnect", Output: err.Error()}, nil
	}
	defer m.forget(id)

	if timeout <= 0 {
		timeout = m.defaultWait
	}
	st := m.Wait(ctx, id, timeout)
	switch st.State {
	case SessionDone, SessionError, SessionTerminated:
		if st.Result != nil {
			return st.Result, nil
		}
		return &SubAgentResult{Outcome: string(st.State)}, nil
	default:
		// Still running after the wait timed out.
		return &SubAgentResult{
			Outcome: "timeout",
			Output:  fmt.Sprintf("spawn_agent timed out after %s waiting for the sub-agent result", timeout),
		}, nil
	}
}

func (m *SessionManager) get(id string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

func (m *SessionManager) forget(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

func (m *SessionManager) nextLocalID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	return fmt.Sprintf("session-%d", m.seq)
}

// --- transport backend -----------------------------------------------------

type transportSessionBackend struct {
	transport   transport.Transport
	correlator  *transport.Correlator
	bgCtx       context.Context
	maxLifetime time.Duration
	complete    func(payload any) *SubAgentResult
}

func (b *transportSessionBackend) start(sess *session, cfg SubAgentConfig) (string, error) {
	input, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("marshal sub-agent config: %w", err)
	}

	id, err := b.correlator.StartPending(func(requestID string) error {
		return b.transport.Emit(types.HarnessEvent{
			Type:      "tool_result_request",
			RequestID: requestID,
			ToolName:  "spawn_agent",
			Input:     input,
		})
	})
	if err != nil {
		return "", err
	}

	// Await the control-plane response on the manager's background context
	// so the session survives the turn that spawned it. The entry is
	// Forgotten once resolved — the durable result lives on the session.
	go func() {
		defer b.correlator.Forget(id)
		payload, awaitErr := b.correlator.AwaitID(b.bgCtx, id, b.maxLifetime)
		if awaitErr != nil {
			sess.complete(sessionResultFromAwaitErr(awaitErr))
			return
		}
		sess.complete(b.complete(payload))
	}()
	return id, nil
}

func (b *transportSessionBackend) terminate(sess *session) {
	// Best-effort: tell the control plane to stop the sub-agent job, then
	// drop the correlator entry so the awaiting goroutine unwinds.
	if err := b.transport.Emit(types.HarnessEvent{Type: "session_terminate", RequestID: sess.id}); err != nil {
		// The transport is gone; the awaiting goroutine will resolve via
		// the background context. Nothing more to do.
		_ = err
	}
	b.correlator.Forget(sess.id)
}

// completeFromAsyncResult translates a correlator payload (always an
// asyncToolResult from extractAsyncToolResult) into a SubAgentResult. On a
// successful response the content is the control plane's serialised
// SubAgentResult JSON; if it does not parse, the raw content is surfaced
// as the output so nothing is silently lost.
func completeFromAsyncResult(payload any) *SubAgentResult {
	res, ok := payload.(asyncToolResult)
	if !ok {
		return &SubAgentResult{Outcome: "error", Output: fmt.Sprintf("unexpected session payload type %T", payload)}
	}
	if res.isError {
		return &SubAgentResult{Outcome: "upstream_error", Output: security.Scrub(res.content)}
	}
	var sr SubAgentResult
	if err := json.Unmarshal([]byte(res.content), &sr); err != nil || sr.Outcome == "" {
		return &SubAgentResult{Outcome: "done", Output: res.content}
	}
	return &sr
}

// sessionResultFromAwaitErr maps a correlator await error onto a distinct
// SubAgentResult.Outcome, mirroring dispatchAsyncToolCall so the failure
// vocabulary is identical across the blocking and detached paths.
func sessionResultFromAwaitErr(err error) *SubAgentResult {
	switch {
	case strings.Contains(err.Error(), "emit failed"):
		return &SubAgentResult{Outcome: "transport_disconnect", Output: err.Error()}
	case errors.Is(err, context.DeadlineExceeded):
		return &SubAgentResult{Outcome: "timeout", Output: err.Error()}
	case errors.Is(err, context.Canceled):
		return &SubAgentResult{Outcome: "cancelled", Output: err.Error()}
	default:
		return &SubAgentResult{Outcome: "timeout", Output: err.Error()}
	}
}

// --- local backend ---------------------------------------------------------

type localSessionBackend struct {
	parent       *AgenticLoop
	parentConfig *types.RunConfig
	bgCtx        context.Context
	nextID       func() string
}

func (b *localSessionBackend) start(sess *session, cfg SubAgentConfig) (string, error) {
	id := b.nextID()
	// Derive from the manager's background context, not the spawning tool
	// call's ctx, so a detached session keeps running after the call that
	// started it returns. cancel is stored for Terminate.
	childCtx, cancel := context.WithCancel(b.bgCtx)
	sess.cancel = cancel

	go func() {
		defer cancel()
		result, err := SpawnSubAgent(childCtx, b.parent, b.parentConfig, cfg)
		if err != nil {
			sess.complete(&SubAgentResult{Outcome: "error", Output: err.Error()})
			return
		}
		sess.complete(result)
	}()
	return id, nil
}

func (b *localSessionBackend) terminate(sess *session) {
	if sess.cancel != nil {
		sess.cancel()
	}
}

// --- builtins.SessionController adapter ------------------------------------

// sessionController adapts *SessionManager to builtins.SessionController,
// marshalling typed SessionStatus snapshots to JSON at the package boundary
// so the builtins package stays free of a core import.
type sessionController struct{ mgr *SessionManager }

var _ builtins.SessionController = sessionController{}

func newSessionController(mgr *SessionManager) sessionController {
	return sessionController{mgr: mgr}
}

func (c sessionController) Start(_ context.Context, prompt, mode string, maxTurns int) (string, error) {
	return c.mgr.Start(SubAgentConfig{Prompt: prompt, Mode: mode, MaxTurns: maxTurns})
}

func (c sessionController) Status(sessionID string) (json.RawMessage, error) {
	return json.Marshal(c.mgr.Status(sessionID))
}

func (c sessionController) Wait(ctx context.Context, sessionID string, timeoutSeconds int) (json.RawMessage, error) {
	return json.Marshal(c.mgr.Wait(ctx, sessionID, time.Duration(timeoutSeconds)*time.Second))
}

func (c sessionController) Terminate(sessionID string) (json.RawMessage, error) {
	if err := c.mgr.Terminate(sessionID); err != nil {
		return nil, err
	}
	return json.Marshal(c.mgr.Status(sessionID))
}
