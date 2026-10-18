package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/security"
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
		l.logEmitFailure("warning", err)
	}
}

// routeControl dispatches one control event. "cancel" ends the session
// (see cancelSession). Every other control type has its own consumer
// (permission, async tool, sandbox-token correlators) and is ignored
// here. Runs on the transport's read goroutine: nothing here blocks or
// emits.
func (l *AgenticLoop) routeControl(event types.ControlEvent) {
	switch event.Type {
	case "cancel":
		l.cancelSession(ErrCancelledByControlPlane, "discarded by cancel")
	case "user_response":
		l.enqueueUserInput(event)
	}
}

// cancelSession ends the session: queued input is rejected with
// dropReason (cancellation always wins over it), the active run — if
// any — is cancelled with cause, and the idle consumer is woken so no
// further run starts. The cancellation is sticky for the loop's
// lifetime and the first cause is kept for a run that starts later.
// Never blocks or emits, so it is safe on the transport's read
// goroutine. Requires ensureControlRouting.
func (l *AgenticLoop) cancelSession(cause error, dropReason string) {
	l.controlMu.Lock()
	if l.sessionCause == nil {
		l.sessionCause = cause
	}
	l.sessionCancelled.Store(true)
	cancel := l.cancelActive
	l.controlMu.Unlock()
	for _, in := range l.userInput.drain() {
		l.rejectUserInput(in.RequestID, dropReason)
	}
	if cancel != nil {
		cancel(cause)
	}
	select {
	case l.idleCancel <- struct{}{}:
	default:
	}
}

// beginRun records cancel as the active run's and returns the cause the
// run must be cancelled with when the session already ended, nil
// otherwise. It shares controlMu with cancelSession, so a concurrent
// cancel either finds the run registered or is found by it.
func (l *AgenticLoop) beginRun(cancel context.CancelCauseFunc) error {
	l.controlMu.Lock()
	defer l.controlMu.Unlock()
	l.cancelActive = cancel
	if !l.sessionCancelled.Load() {
		return nil
	}
	if l.sessionCause == nil {
		return ErrCancelledByControlPlane
	}
	return l.sessionCause
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
// Accepted text gets the dynamic-context treatment — markup stripped,
// capped at security.MaxOperatorTextBytes — since both arrive over the
// same channel at the same trust tier; an alteration is reported as a
// security event and a "warning" echoing the requestId.
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
	sanitized, reasons := security.SanitizeOperatorText(text)
	if len(reasons) > 0 {
		l.reportUserInputSanitized(event.RequestID, len(text), len(sanitized), reasons)
		sanitized = strings.TrimSpace(sanitized)
		if sanitized == "" {
			l.rejectUserInput(event.RequestID, "empty after sanitisation")
			return
		}
	}
	if !l.userInput.push(queuedUserInput{RequestID: event.RequestID, Text: sanitized}) {
		l.rejectUserInput(event.RequestID, fmt.Sprintf("user input queue full (%d pending)", maxQueuedUserInput))
	}
}

// reportUserInputSanitized records an altered user_response in the
// security log and, via the emitter goroutine, as a "warning" the
// control plane can correlate. Safe to call from the control handler.
func (l *AgenticLoop) reportUserInputSanitized(requestID string, originalLen, sanitizedLen int, reasons []string) {
	l.Logger.Warn("user_response sanitized", "requestId", requestID, "reasons", reasons)
	if l.Security != nil {
		l.Security.UserResponseSanitized(requestID, security.DynamicContextSanitizationEvent{
			Key: "user_response", OriginalLength: originalLen, SanitizedLength: sanitizedLen, Reasons: reasons,
		})
	}
	ev := types.HarnessEvent{
		Type:      "warning",
		RequestID: requestID,
		Message:   "user_response sanitized: " + strings.Join(reasons, ", "),
	}
	select {
	case l.rejections <- ev:
	default:
		l.Logger.Warn("sanitisation warning not emitted: emitter backlog full", "requestId", requestID)
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
	l.Logger.InfoContext(ctx, "user input injected", "turn", turn, "count", len(inputs))

	if det := l.observeSensitive(ctx, config, "user_response", turn, texts); det.Transition && l.ruleOfTwoAction() == "abort" {
		l.recordRuleOfTwoAction(ctx, "abort")
		return len(inputs), "rule_of_two_violation"
	}
	// The guard gets the scrubbed copy; the model keeps the operator's
	// words, which, like the prompt, are never rewritten or fenced.
	scrubbed, spans := scrubForGuard(append([]string(nil), texts...))
	in := guard.Input{
		Phase:          guard.PhasePreTurn,
		Content:        batchUntrustedChunks(scrubbed),
		Source:         fmt.Sprintf("user_response:n=%d", len(texts)),
		Mode:           config.Mode,
		RunID:          config.RunID,
		IgnoreMinChunk: true,
	}
	if spans > 0 {
		in.Metadata = map[string]string{guard.MetadataRedactedSpans: strconv.Itoa(spans)}
	}
	allow, decision, _ := l.guardCheck(ctx, in, guardFailOpen(config))
	l.ratchetRuleOfTwo(ctx, config, decision, turn)
	if !allow {
		return len(inputs), "guardrail_blocked"
	}
	// Reported once screening passes, so input that ends the run never
	// appears on the transcript.
	for _, in := range inputs {
		l.emitUserMessage(types.UserMessageSourceUserResponse, in.Text, in.RequestID)
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
// separator from gluing inputs together. A harness-injected trailing
// message (verifier feedback, an escalation nudge) loses its Synthetic
// mark on merge: it now carries the operator's words, which consumers
// that drop synthetic turns (compaction, the LLM judge) must keep. Its
// Origin is kept, so merged verifier feedback is still classified as
// untrusted, together with the operator's words.
func injectUserInput(messages []types.Message, texts []string) []types.Message {
	return appendUserText(messages, strings.Join(texts, "\n\n"), false)
}

// injectSyntheticUserText appends harness-authored text with the same
// merge rule as injectUserInput. A new message is marked Synthetic; a
// merge keeps the trailing message's mark, because that message also
// carries content the harness did not author.
func injectSyntheticUserText(messages []types.Message, text string) []types.Message {
	return appendUserText(messages, text, true)
}

func appendUserText(messages []types.Message, text string, synthetic bool) []types.Message {
	return appendUserBlock(messages, types.ContentBlock{Type: "text", Text: text}, synthetic)
}

// appendUserBlock appends a text block under the appendUserText merge
// rule. Text joins a trailing text block of the same Origin; otherwise it
// becomes its own block, opening with the paragraph break adapters that
// join text parts without a separator would not add.
func appendUserBlock(messages []types.Message, block types.ContentBlock, synthetic bool) []types.Message {
	if n := len(messages); n > 0 && messages[n-1].Role == "user" {
		last := messages[n-1]
		content := append([]types.ContentBlock(nil), last.Content...)
		m := len(content)
		switch {
		case m > 0 && content[m-1].Type == "text" && content[m-1].Origin == block.Origin:
			content[m-1].Text += "\n\n" + block.Text
		case m > 0 && content[m-1].Type == "text":
			block.Text = "\n\n" + block.Text
			content = append(content, block)
		default:
			content = append(content, block)
		}
		last.Content = content
		if !synthetic {
			last.Synthetic = false
		}
		// A fresh slice: the previous turn's request (and its turn
		// record) may still alias the old backing array.
		out := make([]types.Message, n)
		copy(out, messages)
		out[n-1] = last
		return out
	}
	return append(messages, types.Message{
		Role:      "user",
		Synthetic: synthetic,
		Content:   []types.ContentBlock{block},
	})
}
