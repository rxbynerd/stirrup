// Package provider — replay.go implements a deterministic ProviderAdapter
// that replays recorded model outputs for eval testing without API calls.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// replayServeOrder numbers Stream calls across every replay provider, so
// the one that served a run's latest turn can be told apart.
var replayServeOrder atomic.Int64

// ReplayProvider replays recorded TurnRecords as streaming model responses.
// Each call to Stream returns the next turn's ModelOutput as stream events.
// Thread-safe: the turn counter uses atomic operations.
type ReplayProvider struct {
	turns   []types.TurnRecord
	counter atomic.Int64
	// lastServed is the index of the turn Stream most recently served,
	// or -1 before the first; ForSubRun reads the recorded run that turn
	// belongs to.
	lastServed atomic.Int64
	// servedAt orders this provider's latest Stream call among every
	// replay provider's; zero before the first.
	servedAt atomic.Int64
	// subRuns holds each recorded sub-agent by its recorded run ID.
	subRuns map[string]types.SubRunRecording
	// exhausted, when set, is the error Stream returns once every turn
	// has been served, in place of the generic exhaustion error.
	exhausted error
}

// NewReplayProvider creates a ReplayProvider from a sequence of recorded
// turns. Turns of every top-level run (empty ParentRunID) replay in
// order, so a recording spanning follow-up runs replays whole; a
// sub-agent's turns are dropped so its output is never served to the
// parent's requests. A sequence holding only sub-agent turns replays the
// first turn's run.
func NewReplayProvider(turns []types.TurnRecord) *ReplayProvider {
	rp := &ReplayProvider{turns: replayableTurns(turns)}
	rp.lastServed.Store(-1)
	return rp
}

// NewReplayProviderFromRecording creates a ReplayProvider that serves the
// recording's top-level turns and keeps each sub-run's turns for
// ForSubRun.
func NewReplayProviderFromRecording(rec *types.RunRecording) *ReplayProvider {
	rp := NewReplayProvider(rec.Turns)
	if len(rec.SubRuns) > 0 {
		rp.subRuns = make(map[string]types.SubRunRecording, len(rec.SubRuns))
		for _, sub := range rec.SubRuns {
			rp.subRuns[sub.RunID] = sub
		}
	}
	return rp
}

// ForSubRun returns a provider serving the recorded turns of the sub-agent
// childRunID, spawned under parentRunID with prompt. The child is matched
// by its run ID relative to its parent ("sub-2" in "run-1/sub-2") under
// the recorded run of the turn served last, which is the one that spawned
// it. A recording without that sub-run, or whose sub-run was spawned with
// another prompt, yields an error and a provider that fails with it.
func (rp *ReplayProvider) ForSubRun(parentRunID, childRunID, prompt string) (*ReplayProvider, error) {
	relative := strings.TrimPrefix(childRunID, parentRunID+"/")
	recordedParent := ""
	if idx := rp.lastServed.Load(); idx >= 0 && int(idx) < len(rp.turns) {
		recordedParent = rp.turns[idx].RunID
	}
	recordedChild := relative
	if recordedParent != "" {
		recordedChild = recordedParent + "/" + relative
	}
	sub, ok := rp.subRuns[recordedChild]
	child := &ReplayProvider{turns: sub.Turns}
	child.lastServed.Store(-1)
	var err error
	switch {
	case !ok:
		err = fmt.Errorf("replay provider: no recorded turns for sub-agent %q (recorded run %q)", childRunID, recordedChild)
	case !samePrompt(sub.Config.Prompt, prompt):
		err = fmt.Errorf("replay provider: sub-agent %q was spawned with a different prompt from recorded run %q", childRunID, recordedChild)
	default:
		child.exhausted = fmt.Errorf("replay provider: all %d recorded turns of sub-agent %q exhausted", len(sub.Turns), childRunID)
		return child, nil
	}
	child.exhausted = err
	return child, err
}

// samePrompt reports whether a live sub-agent prompt matches the recorded
// one. The recording holds the prompt scrubbed, and an empty recorded
// prompt (a sub-run with no subrun_started config) matches any.
func samePrompt(recorded, live string) bool {
	return recorded == "" || recorded == live || recorded == security.Scrub(live)
}

func replayableTurns(turns []types.TurnRecord) []types.TurnRecord {
	if len(turns) == 0 {
		return turns
	}
	keep := func(turn types.TurnRecord) bool { return turn.ParentRunID == "" }
	if !slices.ContainsFunc(turns, keep) {
		runID := turns[0].RunID
		keep = func(turn types.TurnRecord) bool { return turn.RunID == runID }
	}
	kept := make([]types.TurnRecord, 0, len(turns))
	for _, turn := range turns {
		if keep(turn) {
			kept = append(kept, turn)
		}
	}
	return kept
}

// ServedAt orders the provider's latest Stream call among every replay
// provider's: a larger value served more recently, and zero never served.
func (rp *ReplayProvider) ServedAt() int64 { return rp.servedAt.Load() }

// Len reports how many turns the provider can serve.
func (rp *ReplayProvider) Len() int { return len(rp.turns) }

// Stream replays the next recorded turn's model output as a channel of
// StreamEvents. Returns an error if all turns have been exhausted.
func (rp *ReplayProvider) Stream(ctx context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	idx := int(rp.counter.Add(1) - 1)
	if idx >= len(rp.turns) {
		if rp.exhausted != nil {
			return nil, rp.exhausted
		}
		return nil, fmt.Errorf("replay provider: all %d turns exhausted (requested turn %d)", len(rp.turns), idx)
	}
	rp.lastServed.Store(int64(idx))
	rp.servedAt.Store(replayServeOrder.Add(1))

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
				// Recordings drop the signature, so this is empty for a
				// persisted recording; forwarding it keeps the replayed
				// event identical to the recorded block.
				ch <- types.StreamEvent{
					Type:             "tool_call",
					ID:               block.ID,
					Name:             block.Name,
					Input:            inputMap,
					ThoughtSignature: block.ThoughtSignature,
				}

			case "tool_search":
				inputMap, err := rawMessageToMap(block.Input)
				if err != nil {
					ch <- types.StreamEvent{
						Type:  "error",
						Error: fmt.Errorf("replay provider: invalid tool search input JSON for %q: %w", block.Name, err),
					}
					return
				}
				ch <- types.StreamEvent{
					Type:  "tool_search",
					ID:    block.ID,
					Name:  block.Name,
					Input: inputMap,
				}

			case "tool_search_result":
				ch <- types.StreamEvent{
					Type:           "tool_search_result",
					ID:             block.ToolUseID,
					ToolReferences: block.ToolReferences,
					IsError:        block.IsError,
					Name:           block.Name,
					Text:           block.Content,
				}

			case "thinking", "redacted_thinking":
				// Recordings drop the signature, so a replayed block is
				// unsigned and the Anthropic adapter will not resend it;
				// forwarding it keeps the replayed turn's block sequence
				// identical to the recorded one.
				estimatedTokens += len(block.Text) / 4
				ch <- types.StreamEvent{
					Type:             block.Type,
					Text:             block.Text,
					ThoughtSignature: block.ThoughtSignature,
				}
			}
		}

		stopReason := turn.StopReason
		if stopReason == "" {
			stopReason = "end_turn"
			if hasToolUse {
				stopReason = "tool_use"
			}
		}
		if estimatedTokens == 0 {
			estimatedTokens = 1
		}

		// Known gap: ReplayFields is NOT forwarded here, unlike
		// ThoughtSignature above; see docs/provider-quirks.md.
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
