package context

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/rxbynerd/stirrup/types"
)

// minRecentMessages is the minimum number of recent messages to preserve
// verbatim when summarising. This keeps enough context for coherent
// continuation while the older history gets compressed into a summary.
const minRecentMessages = 6

// SummaryProvider is the minimal interface needed to generate summaries.
// It mirrors the Stream method of ProviderAdapter but is defined locally
// to avoid a circular import between the context and provider packages.
type SummaryProvider interface {
	Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error)
}

// SummariseStrategy compresses old conversation turns into a summary
// using an LLM call, preserving recent messages verbatim. If the summary
// call fails, it falls back to sliding-window truncation.
type SummariseStrategy struct {
	provider SummaryProvider
	model    string

	// Cached summary state to avoid re-summarising unchanged history.
	cachedHash    string
	cachedSummary string
}

// NewSummariseStrategy creates a SummariseStrategy that will use the given
// provider and model to generate conversation summaries.
func NewSummariseStrategy(provider SummaryProvider, model string) *SummariseStrategy {
	return &SummariseStrategy{
		provider: provider,
		model:    model,
	}
}

// Prepare implements ContextStrategy. If messages fit within the token budget,
// they are returned as-is. Otherwise, older messages are summarised into a
// single synthetic message, and recent messages are preserved verbatim.
func (s *SummariseStrategy) Prepare(ctx context.Context, messages []types.Message, budget TokenBudget) ([]types.Message, error) {
	available := budget.MaxTokens - budget.ReserveForResponse
	if available <= 0 {
		// Degenerate case: no room. Keep the minimum recent messages.
		return keepRecent(messages, minRecentMessages), nil
	}

	if budget.CurrentTokens <= available {
		return messages, nil
	}

	// Split into old messages (to summarise) and recent (to keep verbatim).
	splitIdx := splitPoint(messages, minRecentMessages)
	if splitIdx <= 0 {
		// Not enough messages to summarise; return what we have.
		return messages, nil
	}

	old := messages[:splitIdx]
	recent := messages[splitIdx:]

	// Check if we already have a cached summary for these old messages.
	hash := hashMessages(old)
	if hash == s.cachedHash && s.cachedSummary != "" {
		return prependSummary(s.cachedSummary, recent), nil
	}

	// Generate a summary via the provider.
	summary, err := s.generateSummary(ctx, old)
	if err != nil {
		// Fallback: sliding window behavior (drop oldest, keep recent).
		return slidingWindowFallback(messages, budget), nil
	}

	// Cache the result.
	s.cachedHash = hash
	s.cachedSummary = summary

	return prependSummary(summary, recent), nil
}

// splitPoint determines where to split messages. We keep at least
// minRecent messages at the end, returning the index that divides
// old messages from recent messages.
func splitPoint(messages []types.Message, minRecent int) int {
	if len(messages) <= minRecent {
		return 0
	}
	return len(messages) - minRecent
}

// keepRecent returns the last n messages, or all messages if there are fewer than n.
func keepRecent(messages []types.Message, n int) []types.Message {
	if len(messages) <= n {
		return messages
	}
	return messages[len(messages)-n:]
}

// prependSummary constructs a new message slice with the summary as the
// first message (role: user), followed by the recent messages.
func prependSummary(summary string, recent []types.Message) []types.Message {
	summaryMsg := types.Message{
		Role: "user",
		Content: []types.ContentBlock{
			{
				Type: "text",
				Text: fmt.Sprintf("<conversation_summary>\n%s\n</conversation_summary>", summary),
			},
		},
	}

	result := make([]types.Message, 0, 1+len(recent))
	result = append(result, summaryMsg)
	result = append(result, recent...)
	return result
}

// generateSummary calls the provider to produce a concise summary of the
// given messages. It collects all streamed text deltas into a single string.
func (s *SummariseStrategy) generateSummary(ctx context.Context, messages []types.Message) (string, error) {
	prompt := buildSummaryMessages(messages)

	ch, err := s.provider.Stream(ctx, types.StreamParams{
		Model:       s.model,
		System:      "You are a precise summariser. Produce a concise summary of the conversation so far, preserving key decisions, file paths, code changes, tool results, and any other details that would be needed to continue the conversation coherently. Do not include preamble.",
		Messages:    prompt,
		MaxTokens:   2048,
		Temperature: 0.0,
	})
	if err != nil {
		return "", fmt.Errorf("start summary stream: %w", err)
	}

	var sb strings.Builder
	for ev := range ch {
		switch ev.Type {
		case "text_delta":
			sb.WriteString(ev.Text)
		case "error":
			if ev.Error != nil {
				return "", fmt.Errorf("summary stream error: %w", ev.Error)
			}
		}
	}

	result := strings.TrimSpace(sb.String())
	if result == "" {
		return "", fmt.Errorf("provider returned empty summary")
	}
	return result, nil
}

// buildSummaryMessages formats the conversation history into a single user
// message asking for a summary.
func buildSummaryMessages(messages []types.Message) []types.Message {
	var sb strings.Builder
	sb.WriteString("Summarise the following conversation history. Preserve all important details including file paths, decisions made, code changes, tool calls and their results, and errors encountered.\n\n")

	for _, msg := range messages {
		sb.WriteString(fmt.Sprintf("[%s]: ", msg.Role))
		for _, block := range msg.Content {
			switch block.Type {
			case "text":
				sb.WriteString(block.Text)
			case "tool_use":
				sb.WriteString(fmt.Sprintf("<tool_use name=%q id=%q />", block.Name, block.ID))
			case "tool_result":
				// Truncate very long tool results to keep the summary prompt manageable.
				content := block.Content
				if len(content) > 2000 {
					content = content[:2000] + "... [truncated]"
				}
				sb.WriteString(fmt.Sprintf("<tool_result id=%q>%s</tool_result>", block.ToolUseID, content))
			}
		}
		sb.WriteString("\n")
	}

	return []types.Message{
		{
			Role: "user",
			Content: []types.ContentBlock{
				{Type: "text", Text: sb.String()},
			},
		},
	}
}

// hashMessages produces a deterministic hash of message contents for cache
// invalidation. We hash roles and text content, which is sufficient to
// detect changes without being expensive.
func hashMessages(messages []types.Message) string {
	h := sha256.New()
	for _, msg := range messages {
		h.Write([]byte(msg.Role))
		for _, block := range msg.Content {
			h.Write([]byte(block.Type))
			h.Write([]byte(block.Text))
			h.Write([]byte(block.Content))
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// slidingWindowFallback replicates the sliding window behavior as a
// degraded fallback when summarisation fails.
func slidingWindowFallback(messages []types.Message, budget TokenBudget) []types.Message {
	sw := NewSlidingWindowStrategy()
	// Use context.Background() since this is a fallback path; the original
	// context may already be done.
	result, _ := sw.Prepare(context.Background(), messages, budget)
	return result
}
