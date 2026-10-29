package core

import (
	"context"
	"reflect"
	"slices"

	"github.com/rxbynerd/stirrup/types"
)

// thinkingBinding is the request input besides the history before it that
// a signed thinking block is bound to: Anthropic rejects a block replayed
// under a different system prompt or tool list.
type thinkingBinding struct {
	system string
	tools  []types.ToolDefinition
}

func (b *thinkingBinding) equal(other *thinkingBinding) bool {
	return b.system == other.system && slices.EqualFunc(b.tools, other.tools, func(x, y types.ToolDefinition) bool {
		return reflect.DeepEqual(x, y)
	})
}

// bindRequestThinking returns the history to send under systemPrompt and
// toolDefs. When either differs from the request the history was last sent
// with, in this run or in the run a follow-up continues, its thinking
// blocks are stripped, since they could no longer be replayed. The
// request's binding becomes the one later requests compare against.
func (l *AgenticLoop) bindRequestThinking(ctx context.Context, systemPrompt string, toolDefs []types.ToolDefinition, messages []types.Message) []types.Message {
	binding := &thinkingBinding{system: systemPrompt, tools: slices.Clone(toolDefs)}
	previous := l.requestBinding
	l.requestBinding = binding
	if previous == nil || previous.equal(binding) || !hasThinkingBlocks(messages) {
		return messages
	}
	l.Logger.InfoContext(ctx, "thinking blocks dropped from the history: the system prompt or tools differ from the request they were last sent with")
	return types.StripThinkingBlocks(messages)
}

func hasThinkingBlocks(messages []types.Message) bool {
	return slices.ContainsFunc(messages, func(m types.Message) bool {
		return slices.ContainsFunc(m.Content, types.IsThinkingBlock)
	})
}
