package core

import (
	"context"
	"fmt"

	"github.com/rxbynerd/stirrup/types"
)

// carriedHistory is what a top-level run keeps for a follow-up run that
// continues its history (followUpHistory "continue").
type carriedHistory struct {
	messages []types.Message
	// unscreened marks a trailing tool_result or verifier-feedback
	// message that arrived after the run's last pre-turn screening.
	unscreened bool
	// rewritten carries historyRewritten: the history holds thinking
	// blocks produced against a rewritten history, so none may be
	// replayed.
	rewritten bool
	// withheld is the outcome of a run whose history is not carried
	// because screening stopped content in it.
	withheld string
	// toolCalls counts the tool_use blocks in messages.
	toolCalls int
	// cacheKey is the provider cache key the history was produced under.
	cacheKey string
}

func (c *carriedHistory) rewrote() bool {
	return c != nil && c.rewritten
}

// retainHistory keeps the run's committed history for a follow-up that
// continues it. A run ended by screening keeps none: its history holds
// the content the Rule-of-Two monitor or the guard stopped.
func (l *AgenticLoop) retainHistory(config *types.RunConfig, outcome string, messages []types.Message) {
	l.carried = nil
	if l.ParentRunID != "" || !config.ContinuesFollowUpHistory() {
		return
	}
	switch outcome {
	case "rule_of_two_violation", "guardrail_blocked":
		l.carried = &carriedHistory{withheld: outcome}
		return
	}
	committed := committedHistory(messages)
	l.carried = &carriedHistory{
		messages:   committed,
		unscreened: len(committed) > l.screenedLen && len(arrivedUntrustedChunks(committed)) > 0,
		rewritten:  l.historyRewritten,
		toolCalls:  countToolUses(committed),
		cacheKey:   l.cacheKey,
	}
}

// continuesHistory reports whether a run starts from the carried
// messages rather than from its prompt alone.
func (c *carriedHistory) continuesHistory() bool {
	return c != nil && len(c.messages) > 0
}

// beginRunState sets the per-run state a run's inner loops share: the
// tool-call count escalation starts from and the provider cache key. A run
// continuing carried history inherits both; any other run counts zero and
// keys on its own run ID.
func (l *AgenticLoop) beginRunState(config *types.RunConfig, carried *carriedHistory) {
	l.carriedToolCalls = 0
	l.cacheKey = providerCacheKey(config.RunID)
	if !carried.continuesHistory() {
		return
	}
	l.carriedToolCalls = carried.toolCalls
	if carried.cacheKey != "" {
		l.cacheKey = carried.cacheKey
	}
}

func countToolUses(messages []types.Message) int {
	n := 0
	for _, m := range messages {
		for _, b := range m.Content {
			if b.Type == "tool_use" {
				n++
			}
		}
	}
	return n
}

// takeCarriedHistory hands the history the previous run kept to a
// follow-up run, and discards it for any other run.
func (l *AgenticLoop) takeCarriedHistory(followUp bool) *carriedHistory {
	carried := l.carried
	l.carried = nil
	if !followUp {
		return nil
	}
	return carried
}

// startingHistory returns the history a run starts from and whether
// screening it ended the run. A run continuing carried history starts
// from that history with its prompt appended under the merge rule
// mid-run input follows; a carried tail that arrived after the previous
// run's last screening is screened first. Every other run starts from
// its prompt alone.
func (l *AgenticLoop) startingHistory(ctx context.Context, config *types.RunConfig, carried *carriedHistory) ([]types.Message, bool) {
	l.screenedLen = 0
	if carried != nil && carried.withheld != "" {
		l.warnHistoryWithheld(ctx, carried.withheld)
	}
	if carried == nil || len(carried.messages) == 0 {
		messages := buildMessages(config.Prompt)
		l.screenedLen = len(messages)
		return messages, false
	}
	l.Logger.InfoContext(ctx, "follow-up continues the previous run's history", "messages", len(carried.messages))
	messages := carried.messages
	if carried.unscreened {
		// Screening rewrites the tail in place; the previous run's turn
		// records may still share its blocks.
		last := &messages[len(messages)-1]
		last.Content = append([]types.ContentBlock(nil), last.Content...)
		if l.screenArrivedContent(ctx, config, 0, messages) != "" {
			return appendUserText(messages, config.Prompt, false), true
		}
	}
	messages = appendUserText(messages, config.Prompt, false)
	l.screenedLen = len(messages)
	return messages, false
}

// warnHistoryWithheld reports a follow-up that starts from its prompt
// alone despite followUpHistory "continue".
func (l *AgenticLoop) warnHistoryWithheld(ctx context.Context, previousOutcome string) {
	l.Logger.WarnContext(ctx, "follow-up history not continued", "previousOutcome", previousOutcome)
	if err := l.Transport.Emit(types.HarnessEvent{
		Type:    "warning",
		Message: fmt.Sprintf("followUpHistory continue: the previous run ended %q, so this follow-up starts from its prompt alone", previousOutcome),
	}); err != nil {
		l.logEmitFailure("warning", err)
	}
}

// committedHistory returns a copy of the longest prefix of messages that
// ends on committed turns: an assistant message whose tool_use blocks
// are not all answered by tool_result blocks in the message after it is
// dropped, together with everything after it, until the last assistant
// message is answered.
func committedHistory(messages []types.Message) []types.Message {
	end := len(messages)
	for {
		i := lastAssistantIndex(messages[:end])
		if i < 0 || toolUseAnswered(messages[:end], i) {
			break
		}
		end = i
	}
	if end == 0 {
		return nil
	}
	return append([]types.Message(nil), messages[:end]...)
}

func lastAssistantIndex(messages []types.Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			return i
		}
	}
	return -1
}

// toolUseAnswered reports whether every tool_use block of messages[i]
// has a tool_result in the user message that follows it.
func toolUseAnswered(messages []types.Message, i int) bool {
	pending := map[string]bool{}
	for _, b := range messages[i].Content {
		if b.Type == "tool_use" {
			pending[b.ID] = true
		}
	}
	if len(pending) == 0 {
		return true
	}
	if i+1 >= len(messages) || messages[i+1].Role != "user" {
		return false
	}
	for _, b := range messages[i+1].Content {
		if b.Type == "tool_result" {
			delete(pending, b.ToolUseID)
		}
	}
	return len(pending) == 0
}
