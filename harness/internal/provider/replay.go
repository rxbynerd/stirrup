// Package provider — replay.go implements a deterministic ProviderAdapter
// that replays recorded model outputs for eval testing without API calls.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/rxbynerd/stirrup/types"
)

// ReplayProvider replays recorded TurnRecords as streaming model responses.
// Each call to Stream returns the next turn's ModelOutput as stream events.
// Thread-safe: the turn counter uses atomic operations.
type ReplayProvider struct {
	turns   []types.TurnRecord
	counter atomic.Int64
}

// NewReplayProvider creates a ReplayProvider from a sequence of recorded turns.
func NewReplayProvider(turns []types.TurnRecord) *ReplayProvider {
	return &ReplayProvider{turns: turns}
}

// Stream replays the next recorded turn's model output as a channel of
// StreamEvents. Returns an error if all turns have been exhausted.
func (rp *ReplayProvider) Stream(ctx context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	idx := int(rp.counter.Add(1) - 1)
	if idx >= len(rp.turns) {
		return nil, fmt.Errorf("replay provider: all %d turns exhausted (requested turn %d)", len(rp.turns), idx)
	}

	turn := rp.turns[idx]
	ch := make(chan types.StreamEvent, len(turn.ModelOutput)+1)

	go func() {
		defer close(ch)

		hasToolUse := false
		estimatedTokens := 0

		for _, block := range turn.ModelOutput {
			select {
			case <-ctx.Done():
				ch <- types.StreamEvent{Type: "error", Error: ctx.Err()}
				return
			default:
			}

			switch block.Type {
			case "text":
				estimatedTokens += len(block.Text) / 4
				ch <- types.StreamEvent{
					Type: "text_delta",
					Text: block.Text,
				}

			case "tool_use":
				hasToolUse = true
				inputMap, err := rawMessageToMap(block.Input)
				if err != nil {
					ch <- types.StreamEvent{
						Type:  "error",
						Error: fmt.Errorf("replay provider: invalid tool input JSON for %q: %w", block.Name, err),
					}
					return
				}
				// Estimate tokens from the JSON input size.
				estimatedTokens += len(block.Input) / 4
				ch <- types.StreamEvent{
					Type:  "tool_call",
					ID:    block.ID,
					Name:  block.Name,
					Input: inputMap,
				}
			}
		}

		stopReason := "end_turn"
		if hasToolUse {
			stopReason = "tool_use"
		}
		if estimatedTokens == 0 {
			estimatedTokens = 1
		}

		ch <- types.StreamEvent{
			Type:         "message_complete",
			StopReason:   stopReason,
			OutputTokens: estimatedTokens,
		}
	}()

	return ch, nil
}

// rawMessageToMap unmarshals a json.RawMessage into a map[string]any.
func rawMessageToMap(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}
