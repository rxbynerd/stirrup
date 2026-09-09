package core

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/types"
)

// maxQueuedUserInput bounds the FIFO of user_response control events
// awaiting injection. A control plane that outruns the model's turn
// cadence by this much is not conversing with it; the overflow is
// reported rather than absorbed so the sender can back off.
const maxQueuedUserInput = 16

// queuedUserInput is one accepted user_response awaiting injection.
type queuedUserInput struct {
	RequestID string
	Text      string
}

// userInputQueue is the bounded FIFO shared by Run (drained at every
// turn boundary of the active run) and RunFollowUpLoop (popped one at a
// time to start the next run when no run is active).
type userInputQueue struct {
	mu    sync.Mutex
	items []queuedUserInput
	// notify carries at most one pending signal so a waiter learns that
	// the queue became non-empty without the pusher ever blocking.
	notify chan struct{}
}

func newUserInputQueue() *userInputQueue {
	return &userInputQueue{notify: make(chan struct{}, 1)}
}

// push appends in, rejecting it (returning false) when the queue is
// full. The newest event is the one dropped so the sender that just
// sent it learns immediately, from the warning echoing its requestId,
// that it was not accepted.
func (q *userInputQueue) push(in queuedUserInput) bool {
	q.mu.Lock()
	if len(q.items) >= maxQueuedUserInput {
		q.mu.Unlock()
		return false
	}
	q.items = append(q.items, in)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return true
}

// drain removes and returns every queued item in arrival order.
func (q *userInputQueue) drain() []queuedUserInput {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.items
	q.items = nil
	return items
}

// pop removes and returns the oldest item; ok is false when empty.
func (q *userInputQueue) pop() (in queuedUserInput, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return queuedUserInput{}, false
	}
	in = q.items[0]
	q.items = q.items[1:]
	return in, true
}

// clear discards every queued item and reports how many were dropped.
func (q *userInputQueue) clear() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.items)
	q.items = nil
	return n
}

// pending reports the number of queued items.
func (q *userInputQueue) pending() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// maxPendingRejections bounds the warnings waiting for the emitter
// goroutine. Rejections are rare (an over-full queue or empty input),
// so the bound only matters against a control plane flooding the
// stream; beyond it the log line is the record.
const maxPendingRejections = 64

// ensureControlRouting registers the loop's single control-event
// handler with the transport exactly once and starts the goroutine
// that emits rejection warnings on the handler's behalf. Every Run on
// the loop and RunFollowUpLoop share the handler, so an event is
// routed by the loop's state (active run or idle) rather than by which
// handler happened to be registered when it arrived. Safe to call
// repeatedly.
func (l *AgenticLoop) ensureControlRouting() {
	l.controlOnce.Do(func() {
		l.userInput = newUserInputQueue()
		l.idleCancel = make(chan struct{}, 1)
		l.rejections = make(chan types.HarnessEvent, maxPendingRejections)
		l.controlStop = make(chan struct{})
		go l.emitRejections()
		l.Transport.OnControl(l.routeControl)
	})
}

// stopControlRouting ends the rejection emitter; pending warnings are
// dropped since the transport is closing with them.
func (l *AgenticLoop) stopControlRouting() {
	if l.controlStop == nil {
		return
	}
	l.controlStopOnce.Do(func() { close(l.controlStop) })
}

// emitRejections is the only goroutine that emits on behalf of the
// control handler. Transport handlers run on the transport's read
// goroutine and must not call Emit: Emit takes the write mutex the
// loop holds while streaming tokens, and under full-duplex flow
// control a blocked read goroutine can wedge the whole stream — with
// cancel undeliverable.
func (l *AgenticLoop) emitRejections() {
	for {
		select {
		case ev := <-l.rejections:
			l.emitWarning(ev)
		case <-l.controlStop:
			return
		}
	}
}

func (l *AgenticLoop) emitWarning(ev types.HarnessEvent) {
	if err := l.Transport.Emit(ev); err != nil {
		l.Logger.Warn("transport emit failed", "event", "warning", "error", err)
	}
}

// routeControl dispatches one control event. "cancel" ends the
// session: queued input is rejected (cancel always wins over it), the
// active run — if any — is cancelled, and the idle consumer is woken so
// no further run starts; the cancellation is sticky for the loop's
// lifetime. Every other control type has its own consumer
// (permission, async tool, sandbox-token correlators) and is ignored
// here. Runs on the transport's read goroutine: nothing here blocks or
// emits.
func (l *AgenticLoop) routeControl(event types.ControlEvent) {
	switch event.Type {
	case "cancel":
		l.sessionCancelled.Store(true)
		for _, in := range l.userInput.drain() {
			l.rejectUserInput(in.RequestID, "discarded by cancel")
		}
		l.controlMu.Lock()
		cancel := l.cancelActive
		l.controlMu.Unlock()
		if cancel != nil {
			cancel(ErrCancelledByControlPlane)
		}
		select {
		case l.idleCancel <- struct{}{}:
		default:
		}
	case "user_response":
		l.enqueueUserInput(event)
	}
}

// setActiveRun records the cancel function of the run in flight so a
// control-plane cancel reaches it; nil marks the loop idle.
func (l *AgenticLoop) setActiveRun(cancel context.CancelCauseFunc) {
	l.controlMu.Lock()
	defer l.controlMu.Unlock()
	l.cancelActive = cancel
}

// enqueueUserInput accepts a user_response into the queue or rejects
// it observably: a rejection is never silent, since the control plane
// otherwise cannot distinguish "queued for the next turn" from "lost".
func (l *AgenticLoop) enqueueUserInput(event types.ControlEvent) {
	text := strings.TrimSpace(event.UserResponse)
	if text == "" {
		l.rejectUserInput(event.RequestID, "empty user_response")
		return
	}
	if l.sessionCancelled.Load() {
		l.rejectUserInput(event.RequestID, "session cancelled")
		return
	}
	if !l.userInput.push(queuedUserInput{RequestID: event.RequestID, Text: text}) {
		l.rejectUserInput(event.RequestID, fmt.Sprintf("user input queue full (%d pending)", maxQueuedUserInput))
	}
}

// rejectUserInput reports a dropped user_response in the log and, via
// the emitter goroutine, as a "warning" carrying the event's requestId.
// Safe to call from the control handler.
func (l *AgenticLoop) rejectUserInput(requestID, reason string) {
	l.Logger.Warn("user_response dropped", "requestId", requestID, "reason", reason)
	ev := userInputDroppedEvent(requestID, reason)
	select {
	case l.rejections <- ev:
	default:
		l.Logger.Warn("rejection warning not emitted: emitter backlog full", "requestId", requestID)
	}
}

func userInputDroppedEvent(requestID, reason string) types.HarnessEvent {
	return types.HarnessEvent{
		Type:      "warning",
		RequestID: requestID,
		Message:   "user_response dropped: " + reason,
	}
}

// flushQueuedUserInput rejects everything still queued, emitting each
// warning synchronously. For the loop's own goroutines (never the
// control handler), at points after which no consumer will drain the
// queue — so nothing is dropped silently.
func (l *AgenticLoop) flushQueuedUserInput(reason string) {
	if l.userInput == nil {
		return
	}
	for _, in := range l.userInput.drain() {
		l.Logger.Warn("user_response dropped", "requestId", in.RequestID, "reason", reason)
		l.emitWarning(userInputDroppedEvent(in.RequestID, reason))
	}
}

// absorbQueuedUserInput drains the queue into the message history and
// screens what it injected. It returns the number of inputs injected
// and, when screening blocks the run, the terminal outcome. Operator
// input is classified like the initial prompt: the Rule-of-Two monitor
// observes it (and aborts on a latch transition when so configured)
// but never rewrites it, and the pre-turn guard's deny ends the run
// rather than scrubbing the operator's words.
func (l *AgenticLoop) absorbQueuedUserInput(ctx context.Context, config *types.RunConfig, turn int, messages *[]types.Message) (int, string) {
	if l.userInput == nil {
		return 0, ""
	}
	inputs := l.userInput.drain()
	if len(inputs) == 0 {
		return 0, ""
	}
	texts := make([]string, len(inputs))
	for i, in := range inputs {
		texts[i] = in.Text
	}
	*messages = injectUserInput(*messages, texts)
	l.Logger.Info("user input injected", "turn", turn, "count", len(inputs))

	if det := l.observeSensitive(ctx, config, "user_response", turn, texts); det.Transition && l.ruleOfTwoAction() == "abort" {
		l.recordRuleOfTwoAction(ctx, "abort")
		return len(inputs), "rule_of_two_violation"
	}
	in := guard.Input{
		Phase:   guard.PhasePreTurn,
		Content: batchUntrustedChunks(texts),
		Source:  fmt.Sprintf("user_response:n=%d", len(texts)),
		Mode:    config.Mode,
		RunID:   config.RunID,
	}
	allow, decision, _ := l.guardCheck(ctx, in, guardFailOpen(config))
	l.ratchetRuleOfTwo(ctx, config, decision, turn)
	if !allow {
		return len(inputs), "guardrail_blocked"
	}
	return len(inputs), ""
}

// injectUserInput appends texts to the history as one paragraph-separated
// text block. It goes onto the trailing user message when there is one —
// merged into the text block that ends it (the turn-0 prompt), or after
// the tool_result blocks, which the Anthropic wire requires to come
// first — so providers that demand strict role alternation never see
// two consecutive user messages; otherwise it becomes a new user
// message after the assistant turn. A single block, rather than one per
// input, keeps adapters that join a message's text parts without a
// separator from gluing inputs together.
func injectUserInput(messages []types.Message, texts []string) []types.Message {
	joined := strings.Join(texts, "\n\n")
	if n := len(messages); n > 0 && messages[n-1].Role == "user" {
		last := messages[n-1]
		content := append([]types.ContentBlock(nil), last.Content...)
		if m := len(content); m > 0 && content[m-1].Type == "text" {
			content[m-1].Text += "\n\n" + joined
		} else {
			content = append(content, types.ContentBlock{Type: "text", Text: joined})
		}
		last.Content = content
		messages[n-1] = last
		return messages
	}
	return append(messages, types.Message{
		Role:    "user",
		Content: []types.ContentBlock{{Type: "text", Text: joined}},
	})
}
