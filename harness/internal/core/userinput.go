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

// ensureControlRouting registers the loop's single control-event
// handler with the transport exactly once. Every Run on the loop and
// RunFollowUpLoop share it, so an event is routed by the loop's state
// (active run or idle) rather than by which handler happened to be
// registered when it arrived. Safe to call repeatedly.
func (l *AgenticLoop) ensureControlRouting() {
	l.controlOnce.Do(func() {
		l.userInput = newUserInputQueue()
		l.idleCancel = make(chan struct{}, 1)
		l.Transport.OnControl(l.routeControl)
	})
}

// routeControl dispatches one control event. "cancel" always wins over
// queued input: the queue is discarded, then the active run is
// cancelled or, with no run active, the idle consumer is told to end
// the session. Every other control type has its own consumer
// (permission, async tool, sandbox-token correlators) and is ignored
// here.
func (l *AgenticLoop) routeControl(event types.ControlEvent) {
	switch event.Type {
	case "cancel":
		if n := l.userInput.clear(); n > 0 {
			l.Logger.Info("queued user input discarded by cancel", "count", n)
		}
		l.controlMu.Lock()
		cancel := l.cancelActive
		l.controlMu.Unlock()
		if cancel != nil {
			cancel(ErrCancelledByControlPlane)
			return
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
	if strings.TrimSpace(event.UserResponse) == "" {
		l.rejectUserInput(event.RequestID, "empty user_response")
		return
	}
	if !l.userInput.push(queuedUserInput{RequestID: event.RequestID, Text: event.UserResponse}) {
		l.rejectUserInput(event.RequestID, fmt.Sprintf("user input queue full (%d pending)", maxQueuedUserInput))
	}
}

// rejectUserInput reports a dropped user_response on the transport as
// a "warning" carrying the event's requestId, and in the log.
func (l *AgenticLoop) rejectUserInput(requestID, reason string) {
	l.Logger.Warn("user_response dropped", "requestId", requestID, "reason", reason)
	if err := l.Transport.Emit(types.HarnessEvent{
		Type:      "warning",
		RequestID: requestID,
		Message:   "user_response dropped: " + reason,
	}); err != nil {
		l.Logger.Warn("transport emit failed", "event", "warning", "error", err)
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
