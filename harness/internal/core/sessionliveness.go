package core

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// SessionLiveness owns the heartbeat of a session that outlives its runs
// and reports the phase each heartbeat carries. A loop with one sends no
// heartbeat of its own: it reports its phases here instead.
type SessionLiveness interface {
	// SetPhase records the session's phase. A change sends a heartbeat
	// at once, restarts the interval and resumes a paused heartbeat.
	SetPhase(phase types.SessionPhase)
	// SetInterval sets the gap between heartbeats; a non-positive value
	// is ignored.
	SetInterval(d time.Duration)
	// Pause stops heartbeats until the next SetPhase, so an event sent
	// after Pause returns is not followed by a heartbeat until then.
	Pause()
}

// SessionHeartbeat is the SessionLiveness of a `stirrup job` session.
// Every heartbeat is sent under one lock, so a phase change is never
// followed by a heartbeat that still carries the previous phase.
type SessionHeartbeat struct {
	tp     transport.Transport
	logger *slog.Logger

	mu       sync.Mutex
	phase    types.SessionPhase
	interval time.Duration
	paused   bool
	stopped  bool
	failed   bool

	restart chan struct{}
	stop    chan struct{}
	exited  chan struct{}
}

var _ SessionLiveness = (*SessionHeartbeat)(nil)

// StartSessionHeartbeat sends a heartbeat carrying phase and then one
// every interval until Stop. A nil logger selects slog.Default.
func StartSessionHeartbeat(tp transport.Transport, interval time.Duration, phase types.SessionPhase, logger *slog.Logger) *SessionHeartbeat {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = types.DefaultHeartbeatSeconds * time.Second
	}
	h := &SessionHeartbeat{
		tp:       tp,
		logger:   logger,
		phase:    phase,
		interval: interval,
		restart:  make(chan struct{}, 1),
		stop:     make(chan struct{}),
		exited:   make(chan struct{}),
	}
	h.mu.Lock()
	h.emitLocked()
	h.mu.Unlock()
	go h.tick()
	return h
}

func (h *SessionHeartbeat) tick() {
	defer close(h.exited)
	for {
		h.mu.Lock()
		interval := h.interval
		h.mu.Unlock()
		timer := time.NewTimer(interval)
		select {
		case <-h.stop:
			timer.Stop()
			return
		case <-h.restart:
			timer.Stop()
		case <-timer.C:
			h.mu.Lock()
			if !h.paused && !h.stopped {
				h.emitLocked()
			}
			h.mu.Unlock()
		}
	}
}

// SetPhase implements SessionLiveness.
func (h *SessionHeartbeat) SetPhase(phase types.SessionPhase) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped || (phase == h.phase && !h.paused) {
		return
	}
	h.phase = phase
	h.paused = false
	h.emitLocked()
	h.signalRestart()
}

// SetInterval implements SessionLiveness.
func (h *SessionHeartbeat) SetInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if d == h.interval {
		return
	}
	h.interval = d
	h.signalRestart()
}

// Pause implements SessionLiveness.
func (h *SessionHeartbeat) Pause() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.paused = true
}

// Stop ends the heartbeat and waits for its goroutine, so no heartbeat
// follows an event sent after Stop returns. It is safe to call more than
// once.
func (h *SessionHeartbeat) Stop() {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		<-h.exited
		return
	}
	h.stopped = true
	h.mu.Unlock()
	close(h.stop)
	<-h.exited
}

func (h *SessionHeartbeat) signalRestart() {
	select {
	case h.restart <- struct{}{}:
	default:
	}
}

// setSessionPhase reports phase and step to the session's liveness. A
// loop without one, and every sub-agent loop, reports nothing.
func (l *AgenticLoop) setSessionPhase(phase, step string) {
	if l.Liveness == nil || l.ParentRunID != "" {
		return
	}
	l.Liveness.SetPhase(types.SessionPhase{Phase: phase, Step: step})
}

// startRunLiveness starts the run's liveness signal and returns the func
// that ends it just before "done": the run's own heartbeat, or, under a
// session liveness, a pause that the session's next phase ends.
func (l *AgenticLoop) startRunLiveness(ctx context.Context) func() {
	if l.Liveness == nil || l.ParentRunID != "" {
		return l.startHeartbeat(ctx, l.heartbeatInterval())
	}
	l.setSessionPhase(types.SessionPhaseConstructing, types.SessionStepRunSetup)
	return l.Liveness.Pause
}

// emitLocked sends one heartbeat; only the first failure is logged.
func (h *SessionHeartbeat) emitLocked() {
	phase := h.phase
	err := h.tp.Emit(types.HarnessEvent{Type: "heartbeat", SessionPhase: &phase})
	if err != nil && !h.failed {
		h.failed = true
		h.logger.Warn("transport emit failed", "event", "heartbeat", "error", err)
	}
}
