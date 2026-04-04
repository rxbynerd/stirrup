package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// JSONLTraceEmitter writes run traces as newline-delimited JSON to an
// io.Writer (typically a file).
type JSONLTraceEmitter struct {
	writer    io.Writer
	closer    io.Closer
	mu        sync.Mutex
	runID     string
	config    *types.RunConfig
	startedAt time.Time
	turns     []types.TurnTrace
	toolCalls []types.ToolCallTrace
}

// NewJSONLTraceEmitter creates a trace emitter that writes to w.
func NewJSONLTraceEmitter(w io.Writer) *JSONLTraceEmitter {
	emitter := &JSONLTraceEmitter{writer: w}
	if closer, ok := w.(io.Closer); ok {
		emitter.closer = closer
	}
	return emitter
}

// Start initialises the trace with run metadata.
func (e *JSONLTraceEmitter) Start(runID string, config *types.RunConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.runID = runID
	e.config = config
	e.startedAt = time.Now()
	e.turns = nil
	e.toolCalls = nil
}

// RecordTurn appends a turn trace.
func (e *JSONLTraceEmitter) RecordTurn(turn types.TurnTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.turns = append(e.turns, turn)
}

// RecordToolCall appends a tool call trace.
func (e *JSONLTraceEmitter) RecordToolCall(call types.ToolCallTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.toolCalls = append(e.toolCalls, call)
}

// Finish builds the final RunTrace, redacts the config, writes it as a
// JSON line to the backing writer, and returns the trace.
func (e *JSONLTraceEmitter) Finish(_ context.Context, outcome string) (*types.RunTrace, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now()

	// Aggregate token usage across turns.
	var totalTokens types.TokenUsage
	for _, turn := range e.turns {
		totalTokens.Input += turn.Tokens.Input
		totalTokens.Output += turn.Tokens.Output
	}

	// Convert tool call traces to summaries.
	summaries := make([]types.ToolCallSummary, len(e.toolCalls))
	for i, tc := range e.toolCalls {
		summaries[i] = types.ToolCallSummary(tc)
	}

	// Redact sensitive config fields.
	var redactedConfig types.RunConfig
	if e.config != nil {
		redactedConfig = e.config.Redact()
	}

	trace := &types.RunTrace{
		ID:          e.runID,
		Config:      redactedConfig,
		StartedAt:   e.startedAt,
		CompletedAt: now,
		Turns:       len(e.turns),
		TokenUsage:  totalTokens,
		ToolCalls:   summaries,
		Outcome:     outcome,
	}

	data, err := json.Marshal(trace)
	if err != nil {
		return nil, fmt.Errorf("marshal trace: %w", err)
	}

	data = append(data, '\n')
	if _, err := e.writer.Write(data); err != nil {
		return nil, fmt.Errorf("write trace: %w", err)
	}

	return trace, nil
}

// Close releases the backing writer when it owns a closable resource such as a file.
func (e *JSONLTraceEmitter) Close() error {
	if e.closer == nil {
		return nil
	}
	return e.closer.Close()
}
