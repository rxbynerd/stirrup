package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
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

// sessionRetentionFactor multiplies MaxConcurrent to bound the total
// number of sessions retained in the registry (running plus terminal).
// Terminal sessions are kept for later inspection but evicted oldest-first
// once the registry reaches MaxConcurrent * sessionRetentionFactor, so a
// run that starts and forgets many sessions cannot grow it without bound.
const sessionRetentionFactor = 8

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
// terminal state, unblocking Wait. id and seq are set once at construction
// (before the backend starts, so a terminate racing the start sees them)
// and never mutated, so they are read without the lock. terminal is an
// atomic mirror of "state != running" that the manager's eviction sweep
// reads without taking mu (avoiding an mu->s.mu lock-order inversion).
// onTerminal fires exactly once when the session first leaves the running
// state; the manager uses it to release the concurrency slot. cancel stops
// the backend's work (local backend only).
type session struct {
	id  string
	seq int
	mu  sync.Mutex

	done     chan struct{}
	terminal atomic.Bool

	state  SessionState
	result *SubAgentResult

	onTerminal func()
	cancel     context.CancelFunc
}

func (s *session) status() SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SessionStatus{SessionID: s.id, State: s.state, Result: s.result}
}

func (s *session) isTerminal() bool {
	return s.terminal.Load()
}

// complete transitions a running session to its terminal state. A no-op if
// the session is already terminal (e.g. terminated while a late backend
// result was in flight), so the backend goroutine never panics on a double
// close. onTerminal is invoked after the lock is released to keep the
// s.mu -> m.mu ordering one-directional.
func (s *session) complete(result *SubAgentResult) {
	s.mu.Lock()
	if s.state != SessionRunning {
		s.mu.Unlock()
		return
	}
	s.result = result
	if result != nil && errorOutcomes[result.Outcome] {
		s.state = SessionError
	} else {
		s.state = SessionDone
	}
	s.terminal.Store(true)
	close(s.done)
	onTerminal := s.onTerminal
	s.mu.Unlock()

	if onTerminal != nil {
		onTerminal()
	}
}

// markTerminated transitions a running session to terminated. Returns true
// if it made the transition (false if already terminal). The backend's
// terminate signal is sent by the caller only on a true return. onTerminal
// fires (once) after the lock is released, as in complete.
func (s *session) markTerminated() bool {
	s.mu.Lock()
	if s.state != SessionRunning {
		s.mu.Unlock()
		return false
	}
	s.state = SessionTerminated
	s.result = &SubAgentResult{Outcome: "terminated"}
	s.terminal.Store(true)
	close(s.done)
	onTerminal := s.onTerminal
	s.mu.Unlock()

	if onTerminal != nil {
		onTerminal()
	}
	return true
}

// sessionBackend produces a sub-agent result into a session and stops its
// work on terminate. The transport backend dispatches over the wire and
// correlates the response; the local backend runs SpawnSubAgent in a
// goroutine.
type sessionBackend interface {
	// start kicks off the sub-agent for sess under the manager-allocated
	// id, arranging for sess.complete to be called when it finishes. It
	// must not block on the result.
	start(sess *session, cfg SubAgentConfig, id string) error
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
	maxTotal      int
	defaultWait   time.Duration
	logger        *slog.Logger

	bgCtx  context.Context
	cancel context.CancelFunc

	mu sync.Mutex
	// running is the count of sessions still in the running state. It is
	// the authoritative concurrency gauge — reserved under mu in Start
	// before the backend dispatches (so concurrent Starts cannot both pass
	// the cap) and released exactly once per session via onTerminal.
	running  int
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
		// Retain up to a small multiple of the live cap so terminal
		// sessions stay inspectable, but evict oldest-first past the
		// ceiling so a long run that starts (and forgets) many sessions
		// cannot grow the registry without bound.
		maxTotal:    maxConcurrent * sessionRetentionFactor,
		defaultWait: defaultWait,
		logger:      logger,
		bgCtx:       bgCtx,
		cancel:      cancel,
		sessions:    make(map[string]*session),
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
// reserves a concurrency slot atomically (so concurrent Starts cannot both
// pass a full cap), allocates an unguessable id, dispatches the backend,
// and registers the session. The session persists in the registry until it
// is evicted or the manager is Closed, so a later Status/Wait can observe
// its result.
func (m *SessionManager) Start(cfg SubAgentConfig) (string, error) {
	if cfg.Prompt == "" {
		return "", errors.New("session prompt must not be empty")
	}
	if err := m.reserveSlot(); err != nil {
		return "", err
	}

	id := newSessionID()
	// onTerminal releases the slot exactly once, when the session first
	// leaves the running state (via complete or markTerminated).
	sess := &session{id: id, done: make(chan struct{}), state: SessionRunning, onTerminal: m.releaseSlot}

	if err := m.backend.start(sess, cfg, id); err != nil {
		// Roll back the reserved slot: the session never started, so its
		// onTerminal will never fire.
		m.releaseSlot()
		return "", err
	}

	m.mu.Lock()
	m.seq++
	sess.seq = m.seq
	m.evictLocked()
	m.sessions[id] = sess
	m.mu.Unlock()
	return id, nil
}

// reserveSlot claims a running-session slot, failing when the cap is
// already reached. Terminal sessions do not hold a slot (it is released in
// onTerminal), so the cap bounds concurrency, not total retention.
func (m *SessionManager) reserveSlot() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running >= m.maxConcurrent {
		return fmt.Errorf("session limit reached: %d concurrent sessions already running (max %d)", m.running, m.maxConcurrent)
	}
	m.running++
	return nil
}

// releaseSlot returns a running-session slot to the pool. Called exactly
// once per reserved slot: from onTerminal when a session reaches a terminal
// state, or directly from Start to roll back a backend that failed to
// dispatch.
func (m *SessionManager) releaseSlot() {
	m.mu.Lock()
	if m.running > 0 {
		m.running--
	}
	m.mu.Unlock()
}

// evictLocked drops oldest-first terminal sessions while the registry is at
// or above its retention ceiling. Caller holds m.mu. It reads each
// session's terminal flag atomically rather than taking s.mu, so it cannot
// invert the s.mu -> m.mu order that onTerminal relies on. Running sessions
// are never evicted; since running is capped below maxTotal there is always
// a terminal victim when the ceiling is hit.
func (m *SessionManager) evictLocked() {
	for len(m.sessions) >= m.maxTotal {
		victimID := ""
		victimSeq := int(^uint(0) >> 1) // max int
		for id, s := range m.sessions {
			if s.isTerminal() && s.seq < victimSeq {
				victimID, victimSeq = id, s.seq
			}
		}
		if victimID == "" {
			return
		}
		delete(m.sessions, victimID)
	}
}

// newSessionID returns an unguessable session handle. The random suffix
// stops a prompt-injected model from iterating session-N ids to interfere
// with sibling sessions it did not start. crypto/rand.Read never fails on
// supported platforms; a read error degrades to a still-unique but
// lower-entropy id rather than aborting the spawn.
func newSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "session-" + hex.EncodeToString(b[:])
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
		// Still running after the wait timed out. A blocking spawn that
		// times out has no later wait to collect the result, so terminate
		// the session to stop the backend's work — otherwise the
		// control-plane job (or local goroutine) would run on, orphaned,
		// until its own lifetime cap.
		_ = m.Terminate(id)
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

// transport backend.

type transportSessionBackend struct {
	transport   transport.Transport
	correlator  *transport.Correlator
	bgCtx       context.Context
	maxLifetime time.Duration
	complete    func(payload any) *SubAgentResult
}

func (b *transportSessionBackend) start(sess *session, cfg SubAgentConfig, id string) error {
	input, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal sub-agent config: %w", err)
	}

	if err := b.correlator.StartPendingWithID(id, func(requestID string) error {
		return b.transport.Emit(types.HarnessEvent{
			Type:      "tool_result_request",
			RequestID: requestID,
			ToolName:  "spawn_agent",
			Input:     input,
		})
	}); err != nil {
		return err
	}

	// Await the control-plane response on the manager's background context
	// so the session survives the turn that spawned it. Forget on every
	// exit so a terminated/timed-out session's correlator entry is dropped
	// promptly (Forget also unblocks a parked AwaitID); the durable result
	// lives on the session, not the correlator.
	go func() {
		defer b.correlator.Forget(id)
		payload, awaitErr := b.correlator.AwaitID(b.bgCtx, id, b.maxLifetime)
		if awaitErr != nil {
			sess.complete(sessionResultFromAwaitErr(awaitErr))
			return
		}
		sess.complete(b.complete(payload))
	}()
	return nil
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
// asyncToolResult from extractAsyncToolResult) into a SubAgentResult.
//
// The control plane is partially trusted: it could embed secret-shaped
// strings in a response. Every content path is scrubbed before it becomes
// the SubAgentResult.Output, because that value flows into the model
// context and the JSONL trace before the outbound transport scrub can act —
// matching dispatchAsyncToolCall's point-of-entry scrub on the error path.
// On a successful response the content is the control plane's serialised
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
		return &SubAgentResult{Outcome: "done", Output: security.Scrub(res.content)}
	}
	sr.Output = security.Scrub(sr.Output)
	return &sr
}

// sessionResultFromAwaitErr maps a correlator await error onto a distinct
// SubAgentResult.Outcome, mirroring dispatchAsyncToolCall so the failure
// vocabulary is identical across the blocking and detached paths. The
// default arm treats any unrecognised correlator failure as a timeout —
// the conservative choice for the model, which then knows the result did
// not arrive.
func sessionResultFromAwaitErr(err error) *SubAgentResult {
	switch {
	case errors.Is(err, transport.ErrEmitFailed):
		return &SubAgentResult{Outcome: "transport_disconnect", Output: err.Error()}
	case errors.Is(err, context.DeadlineExceeded):
		return &SubAgentResult{Outcome: "timeout", Output: err.Error()}
	case errors.Is(err, context.Canceled):
		return &SubAgentResult{Outcome: "cancelled", Output: err.Error()}
	default:
		return &SubAgentResult{Outcome: "timeout", Output: err.Error()}
	}
}

// local backend.

type localSessionBackend struct {
	parent       *AgenticLoop
	parentConfig *types.RunConfig
	bgCtx        context.Context
}

// start runs the sub-agent in a background goroutine. SpawnSubAgent reuses
// the parent loop's Provider, Executor, Edit and Tools while giving the
// child its own message history, context strategy, transport, and (for
// Cedar) a cloned permission policy. Those reused components are the same
// ones the parallel async-tool dispatch path (#184) already drives
// concurrently via multiple in-flight spawn_agent calls, so concurrent use
// here is on the same footing — detached sessions only widen the window,
// they do not introduce a new sharing pattern. cancel is stored for
// Terminate / manager Close.
func (b *localSessionBackend) start(sess *session, cfg SubAgentConfig, _ string) error {
	// Derive from the manager's background context, not the spawning tool
	// call's ctx, so a detached session keeps running after the call that
	// started it returns.
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
	return nil
}

func (b *localSessionBackend) terminate(sess *session) {
	if sess.cancel != nil {
		sess.cancel()
	}
}

// builtins.SessionController adapter.

// sessionController adapts *SessionManager to builtins.SessionController,
// marshalling typed SessionStatus snapshots to JSON at the package boundary
// so the builtins package stays free of a core import.
type sessionController struct{ mgr *SessionManager }

var _ builtins.SessionController = sessionController{}

func newSessionController(mgr *SessionManager) sessionController {
	return sessionController{mgr: mgr}
}

// Start ignores the tool-call context deliberately: a detached session
// must outlive the call that started it, so the underlying work derives
// from the manager's background context (cancelled on run end), not from
// the per-call ctx. Wait, by contrast, threads ctx through so a blocking
// wait unblocks on run cancellation.
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
