package context

import (
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/rxbynerd/stirrup/types"
)

const (
	// fitMarginPercent is the share of the room left after the response
	// reserve that a prepared request must leave unused, absorbing the
	// error of the characters/4 token estimate.
	fitMarginPercent = 10

	// truncationSlackChars is added to the characters each truncated block
	// must give up, covering the per-field rounding of token estimates that
	// divide each field's length by 4 separately.
	truncationSlackChars = 8
)

// FitLimit returns the largest estimated size, in tokens, of a prepared
// request (messages, system prompt and tool definitions) that fits the
// budget: the room left after the response reserve, less a safety margin
// of fitMarginPercent. It is zero or negative when nothing fits.
func FitLimit(budget TokenBudget) int {
	available := budget.MaxTokens - budget.ReserveForResponse
	if available <= 0 {
		return available
	}
	return available - available*fitMarginPercent/100
}

// TruncateToolResults returns a view of messages in which tool_result
// content is cut until the content removed accounts for at least excess
// tokens at four characters per token. Results larger than the offload
// fallback's kept size are cut first, oldest first; smaller results are cut
// only when that is not enough. Each cut keeps a head and a tail of the
// result around a marker and drops the block's structured envelope, which
// mirrors the full output. Block types, IDs and message order are kept, so
// every result stays paired with its call. messages and its content slices
// are never modified; the second result is the number of tool results cut.
func TruncateToolResults(messages []types.Message, excess int) ([]types.Message, int) {
	if excess <= 0 {
		return messages, 0
	}
	t := &resultTruncator{messages: messages, need: excess * 4}
	for _, minSize := range []int{2*truncationKeepChars + 1, 0} {
		t.pass(minSize)
	}
	if t.view == nil {
		return messages, 0
	}
	return t.view, t.cut
}

type resultTruncator struct {
	messages []types.Message
	view     []types.Message
	copied   map[int]bool
	cut      int
	need     int
}

// pass cuts tool results of at least minSize bytes, oldest first, until
// need is met.
func (t *resultTruncator) pass(minSize int) {
	for i := range t.messages {
		for j := range t.messages[i].Content {
			if t.need <= 0 {
				return
			}
			block := t.current(i).Content[j]
			size := len(block.Content) + len(block.Structured)
			if block.Type != "tool_result" || size < minSize {
				continue
			}
			fromContent := t.need + truncationSlackChars - len(block.Structured)
			keep := (len(block.Content) - fromContent - len(truncationMarker(len(block.Content)))) / 2
			truncated := truncateHeadTail(block.Content, max(keep, 0))
			saved := size - len(truncated)
			if saved <= truncationSlackChars {
				continue
			}
			content := t.writable(i)
			content[j].Content = truncated
			content[j].Structured = nil
			content[j].Kind = ""
			t.need -= saved - truncationSlackChars
			t.cut++
		}
	}
}

func (t *resultTruncator) current(i int) types.Message {
	if t.view != nil {
		return t.view[i]
	}
	return t.messages[i]
}

// writable returns message i's content in the view, copying the view and
// the message's content slice on first write.
func (t *resultTruncator) writable(i int) []types.ContentBlock {
	if t.view == nil {
		t.view = make([]types.Message, len(t.messages))
		copy(t.view, t.messages)
		t.copied = map[int]bool{}
	}
	if !t.copied[i] {
		t.view[i].Content = slices.Clone(t.messages[i].Content)
		t.copied[i] = true
	}
	return t.view[i].Content
}

// truncateHeadTail keeps up to keep bytes at each end of content, snapped
// to rune boundaries, with a marker naming the bytes removed in between.
// content is returned unchanged when the cut would not make it shorter.
func truncateHeadTail(content string, keep int) string {
	if len(content) <= 2*keep+len(truncationMarker(len(content))) {
		return content
	}
	head := keep
	for head > 0 && !utf8.RuneStart(content[head]) {
		head--
	}
	tail := len(content) - keep
	for tail < len(content) && !utf8.RuneStart(content[tail]) {
		tail++
	}
	return content[:head] + truncationMarker(tail-head) + content[tail:]
}

func truncationMarker(removed int) string {
	return fmt.Sprintf("\n[...truncated %d bytes...]\n", removed)
}
