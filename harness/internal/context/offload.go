package context

import (
	"context"
	"fmt"
	"strings"

	"github.com/rxbynerd/stirrup/types"
)

// FileWriter is the minimal interface needed to write offloaded content to
// the workspace. The executor.Executor satisfies this via its WriteFile method.
type FileWriter interface {
	WriteFile(ctx context.Context, path string, content string) error
}

const (
	// offloadSizeThreshold is the minimum character count for a tool_result
	// content block to be eligible for offloading.
	offloadSizeThreshold = 2000

	// recentPreservedMessages is the number of recent messages to keep in
	// full regardless of size. 6 messages = 3 assistant+user turn pairs.
	recentPreservedMessages = 6

	// truncationKeepChars is the number of characters to keep at the start
	// and end of a content block when truncation is used as a fallback.
	truncationKeepChars = 500
)

// OffloadToFileStrategy writes large tool_result content blocks to workspace
// files and replaces them with a short reference. Only content blocks that
// exceed offloadSizeThreshold in older messages (outside the most recent
// recentPreservedMessages) are offloaded. If writing a file fails, the
// content is truncated in place as a fallback.
type OffloadToFileStrategy struct {
	writer         FileWriter
	lastCompaction *CompactionEvent
}

// NewOffloadToFileStrategy creates an OffloadToFileStrategy that writes
// offloaded content using the given FileWriter.
func NewOffloadToFileStrategy(writer FileWriter) *OffloadToFileStrategy {
	return &OffloadToFileStrategy{writer: writer}
}

// Prepare implements ContextStrategy. If messages fit within the token budget,
// they are returned unchanged. Otherwise, large tool_result blocks in older
// messages are offloaded to files, reducing the in-context token count.
func (o *OffloadToFileStrategy) Prepare(ctx context.Context, messages []types.Message, budget TokenBudget) ([]types.Message, error) {
	o.lastCompaction = nil

	if len(messages) == 0 {
		return messages, nil
	}

	available := budget.MaxTokens - budget.ReserveForResponse
	if available <= 0 {
		// Degenerate case: no room. Keep recent messages only.
		result := keepRecent(messages, recentPreservedMessages)
		o.lastCompaction = &CompactionEvent{
			Strategy:       "offload-to-file",
			MessagesBefore: len(messages),
			MessagesAfter:  len(result),
			TokensBefore:   budget.CurrentTokens,
		}
		return result, nil
	}

	if budget.CurrentTokens <= available {
		return messages, nil
	}

	// Determine the boundary between offload-eligible and recent messages.
	recentStart := len(messages) - recentPreservedMessages
	if recentStart < 0 {
		recentStart = 0
	}

	// Deep-copy the offload-eligible prefix so we don't mutate the caller's slice.
	result := make([]types.Message, len(messages))
	copy(result, messages)

	for i := 0; i < recentStart; i++ {
		msg := result[i]
		modified := false
		newContent := make([]types.ContentBlock, len(msg.Content))
		copy(newContent, msg.Content)

		for j, block := range newContent {
			if block.Type != "tool_result" {
				continue
			}
			if len(block.Content) < offloadSizeThreshold {
				continue
			}

			filePath := offloadFilePath(i, block.ToolUseID)
			err := o.writer.WriteFile(ctx, filePath, block.Content)
			if err != nil {
				// Fallback: truncate in place.
				newContent[j].Content = truncateContent(block.Content)
			} else {
				newContent[j].Content = fmt.Sprintf(
					"[Full output offloaded to %s — read this file if you need the details]",
					filePath,
				)
			}
			modified = true
		}

		if modified {
			result[i] = types.Message{
				Role:    msg.Role,
				Content: newContent,
			}
		}
	}

	o.lastCompaction = &CompactionEvent{
		Strategy:       "offload-to-file",
		MessagesBefore: len(messages),
		MessagesAfter:  len(result),
		TokensBefore:   budget.CurrentTokens,
	}
	return result, nil
}

// LastCompaction returns details of the most recent compaction, or nil if
// no compaction was needed.
func (o *OffloadToFileStrategy) LastCompaction() *CompactionEvent {
	return o.lastCompaction
}

// offloadFilePath returns the workspace-relative path for an offloaded content
// block, scoped by turn index and tool use ID.
func offloadFilePath(turnIndex int, toolUseID string) string {
	return fmt.Sprintf(".stirrup/context/turn-%d-%s.txt", turnIndex, toolUseID)
}

// truncateContent keeps the first and last truncationKeepChars characters of
// content with a truncation marker in between. Used as a fallback when file
// writing fails.
func truncateContent(content string) string {
	if len(content) <= truncationKeepChars*2 {
		return content
	}
	var sb strings.Builder
	sb.WriteString(content[:truncationKeepChars])
	sb.WriteString("\n[...truncated...]\n")
	sb.WriteString(content[len(content)-truncationKeepChars:])
	return sb.String()
}
