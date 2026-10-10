package context

import (
	"context"

	"github.com/rxbynerd/stirrup/types"
)

const (
	// DefaultClearKeep is the number of most recent tool results the
	// clear-tool-results strategy keeps when Keep is unset.
	DefaultClearKeep = 3

	// defaultClearAtLeastPercent is the share of the trigger one clearing
	// batch must reclaim when ClearAtLeast is unset.
	defaultClearAtLeastPercent = 25

	// ClearedToolResultPlaceholder replaces the content of every cleared
	// tool result. It must stay byte-identical across turns: any change
	// invalidates the provider's cached prefix.
	ClearedToolResultPlaceholder = "[Tool result cleared by the harness to save context. Run the tool again if you need this output.]"
)

// ClearToolResultsConfig configures a ClearToolResultsStrategy.
type ClearToolResultsConfig struct {
	// Keep is the number of most recent tool results never cleared.
	Keep int
	// ClearAtLeast is the minimum estimated tokens one batch reclaims;
	// zero or less selects defaultClearAtLeastPercent of the trigger.
	ClearAtLeast int
	// ExcludeTools names tools whose results are never cleared.
	ExcludeTools []string
	// InternalName maps the tool name recorded in a tool_use block to the
	// internal tool ID, so ExcludeTools matches tools presented under an
	// alias. Nil matches the recorded name only.
	InternalName func(string) string
}

// ClearToolResultsStrategy replaces the content of old tool results with a
// fixed placeholder in occasional batches. It remembers every result it
// has cleared and re-applies the same placeholder on each call, so between
// batches the prepared history is the previous one plus the new messages,
// which keeps the provider's prompt cache valid. A batch runs only when
// the request exceeds FitLimit, and clears the oldest eligible results
// until at least the configured minimum is reclaimed and the request fits.
// It never writes to the workspace.
type ClearToolResultsStrategy struct {
	keep         int
	clearAtLeast int
	excluded     map[string]bool
	internalName func(string) string

	// cleared holds the results cleared from the history whose task hashes
	// to taskHash.
	cleared        map[resultRef]bool
	taskHash       string
	warnedFallback bool

	lastCompaction *CompactionEvent
}

// resultRef identifies a tool_result block by its position in the
// append-only history and the call it answers.
type resultRef struct {
	message, block int
	toolUseID      string
}

// clearCandidate is a tool result a batch may clear and the estimated
// tokens clearing it reclaims.
type clearCandidate struct {
	ref     resultRef
	reclaim int
}

// NewClearToolResultsStrategy creates a ClearToolResultsStrategy. A
// negative Keep is treated as zero.
func NewClearToolResultsStrategy(cfg ClearToolResultsConfig) *ClearToolResultsStrategy {
	excluded := make(map[string]bool, len(cfg.ExcludeTools))
	for _, name := range cfg.ExcludeTools {
		excluded[name] = true
	}
	return &ClearToolResultsStrategy{
		keep:         max(cfg.Keep, 0),
		clearAtLeast: cfg.ClearAtLeast,
		excluded:     excluded,
		internalName: cfg.InternalName,
		cleared:      map[resultRef]bool{},
	}
}

// Prepare implements ContextStrategy. It returns messages with every
// previously cleared result replaced by the placeholder, and runs a new
// clearing batch when that view still exceeds FitLimit. When the eligible
// results cannot reclaim the batch minimum, or clearing all of them still
// does not fit, it drops the oldest exchanges as the sliding window does.
func (s *ClearToolResultsStrategy) Prepare(_ context.Context, messages []types.Message, budget TokenBudget) ([]types.Message, error) {
	s.lastCompaction = nil
	if len(messages) == 0 {
		return messages, nil
	}
	s.forgetOtherHistory(messages)

	hv := &historyView{messages: messages}
	tokens := budget.CurrentTokens
	for ref := range s.cleared {
		tokens -= clearResult(hv, ref)
	}

	trigger := FitLimit(budget)
	if trigger > 0 && tokens <= trigger {
		return hv.result(), nil
	}

	var batch []clearCandidate
	reclaimed := 0
	if trigger > 0 {
		minimum := s.batchMinimum(trigger)
		batch, reclaimed = s.selectBatch(hv.result(), max(minimum, tokens-trigger), minimum)
	}
	for _, c := range batch {
		clearResult(hv, c.ref)
		s.cleared[c.ref] = true
	}
	view := hv.result()
	tokensAfter := tokens - reclaimed

	event := &CompactionEvent{
		Strategy:           "clear-tool-results",
		MessagesBefore:     len(messages),
		TokensBefore:       tokens,
		ToolResultsCleared: len(batch),
	}
	if trigger > 0 && tokensAfter <= trigger {
		event.MessagesAfter = len(view)
		event.TokensAfter = tokensAfter
		s.lastCompaction = event
		return view, nil
	}

	window := budget
	window.CurrentTokens = tokensAfter
	result := slidingWindowFallback(view, window)
	if len(batch) == 0 && len(result) == len(view) {
		return view, nil
	}
	event.MessagesAfter = len(result)
	event.TokensAfter = tokensAfter - estimateHistory(view) + estimateHistory(result)
	if len(result) < len(view) && !s.warnedFallback {
		s.warnedFallback = true
		event.Warnings = []string{
			"clear-tool-results: clearing tool results cannot keep the history within the context budget; dropping the oldest exchanges, which invalidates the prompt cache on every turn that drops one",
		}
	}
	s.lastCompaction = event
	return result, nil
}

// LastCompaction returns details of the most recent clearing batch or
// fallback, or nil when Prepare only re-applied earlier batches.
func (s *ClearToolResultsStrategy) LastCompaction() *CompactionEvent {
	return s.lastCompaction
}

// forgetOtherHistory drops the cleared set when messages starts with a
// different task, as a follow-up run reusing the strategy does.
func (s *ClearToolResultsStrategy) forgetOtherHistory(messages []types.Message) {
	hash := hashMessages(messages[:1])
	if hash == s.taskHash {
		return
	}
	s.taskHash = hash
	s.cleared = map[resultRef]bool{}
	s.warnedFallback = false
}

// batchMinimum returns the estimated tokens one batch must reclaim.
func (s *ClearToolResultsStrategy) batchMinimum(trigger int) int {
	if s.clearAtLeast > 0 {
		return s.clearAtLeast
	}
	return max(trigger*defaultClearAtLeastPercent/100, 1)
}

// selectBatch returns the oldest eligible results whose clearing reclaims
// at least need estimated tokens, or all of them when that is not enough.
// It returns no batch when every eligible result together reclaims less
// than minimum.
func (s *ClearToolResultsStrategy) selectBatch(view []types.Message, need, minimum int) ([]clearCandidate, int) {
	candidates := s.candidates(view)
	total := 0
	for _, c := range candidates {
		total += c.reclaim
	}
	if total < minimum {
		return nil, 0
	}
	reclaimed := 0
	for i, c := range candidates {
		reclaimed += c.reclaim
		if reclaimed >= need {
			return candidates[:i+1], reclaimed
		}
	}
	return candidates, reclaimed
}

// candidates returns the results a batch may clear, oldest first. The
// Keep most recent results, the results in a trailing user message (which
// the model has not yet seen), results already cleared and results of
// excluded tools are never candidates.
func (s *ClearToolResultsStrategy) candidates(view []types.Message) []clearCandidate {
	var all []clearCandidate
	for i, msg := range view {
		for j, block := range msg.Content {
			if block.Type != "tool_result" {
				continue
			}
			all = append(all, clearCandidate{
				ref:     resultRef{message: i, block: j, toolUseID: block.ToolUseID},
				reclaim: clearReclaim(block),
			})
		}
	}
	all = all[:max(len(all)-s.keep, 0)]

	last := len(view) - 1
	eligible := all[:0]
	for _, c := range all {
		if c.reclaim <= 0 || s.cleared[c.ref] || s.isExcluded(view, c.ref) {
			continue
		}
		if c.ref.message == last && view[last].Role == "user" {
			continue
		}
		eligible = append(eligible, c)
	}
	return eligible
}

// isExcluded reports whether the result answers a call, in the assistant
// message before it, to a tool named in ExcludeTools.
func (s *ClearToolResultsStrategy) isExcluded(view []types.Message, ref resultRef) bool {
	if len(s.excluded) == 0 || ref.message == 0 {
		return false
	}
	for _, block := range view[ref.message-1].Content {
		if block.Type != "tool_use" || block.ID != ref.toolUseID {
			continue
		}
		if s.excluded[block.Name] {
			return true
		}
		return s.internalName != nil && s.excluded[s.internalName(block.Name)]
	}
	return false
}

// clearReclaim returns the estimated tokens saved by clearing block.
func clearReclaim(block types.ContentBlock) int {
	return estimateBlockTokens(block) - estimateBlockTokens(clearedBlock(block))
}

// clearedBlock returns block with its content replaced by the placeholder
// and its structured envelope, which mirrors the content, dropped.
func clearedBlock(block types.ContentBlock) types.ContentBlock {
	block.Content = ClearedToolResultPlaceholder
	block.Structured = nil
	block.Kind = ""
	return block
}

// clearResult replaces the content of the result at ref with the
// placeholder in hv and returns the estimated tokens reclaimed. It changes
// nothing, and returns zero, when ref no longer names a tool result
// answering the same call.
func clearResult(hv *historyView, ref resultRef) int {
	if ref.message >= len(hv.messages) || ref.block >= len(hv.messages[ref.message].Content) {
		return 0
	}
	block := hv.messages[ref.message].Content[ref.block]
	if block.Type != "tool_result" || block.ToolUseID != ref.toolUseID {
		return 0
	}
	hv.writable(ref.message)[ref.block] = clearedBlock(block)
	return clearReclaim(block)
}
