// Package trace — events.go defines the on-disk JSONL event shape the
// streaming JSONLTraceEmitter writes and the reader in
// types/trace/reader.go consumes. See docs/trace-inspection.md for the
// wire schema and backward-compatibility notes.
package trace

import (
	"encoding/json"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// EventKind is the discriminator that prefixes every streaming event.
type EventKind string

const (
	// EventKindRunStarted is the first line of a streaming trace file.
	EventKindRunStarted EventKind = "run_started"
	// EventKindTurnRecord captures one agentic-loop turn's full I/O.
	EventKindTurnRecord EventKind = "turn_record"
	// EventKindToolCallRecord captures one tool invocation's raw input
	// and output. Emitted in addition to the inline ToolCalls inside the
	// enclosing turn_record so per-call consumers (e.g. live UIs, future
	// gRPC fan-out) see each call as soon as it lands.
	EventKindToolCallRecord EventKind = "tool_call_record"
	// EventKindRunFinished is the last line of a streaming trace file.
	// Carries the canonical RunTrace summary; backward-compatible with
	// pre-streaming single-blob traces.
	EventKindRunFinished EventKind = "run_finished"
	// EventKindHookRecord captures one lifecycle hook's result, emitted
	// as each PreRun/PostRun hook completes (mirrors tool_call_record).
	EventKindHookRecord EventKind = "hook_record"
)

// CurrentSchemaVersion is the streaming-trace schema version emitted in
// run_started events. Bump on backward-incompatible event-shape changes.
const CurrentSchemaVersion = "1"

// Event is the wire-shape of one line in a streaming JSONL trace file.
// Fields are populated according to Kind; reading code must dispatch
// on Kind before consulting the payload.
//
// Marshal/Unmarshal use json.RawMessage rather than concrete payload
// pointers so a future kind addition does not break older readers: an
// unrecognised Kind round-trips as opaque bytes.
type Event struct {
	Kind          EventKind        `json:"kind"`
	SchemaVersion string           `json:"schemaVersion,omitempty"`
	RunID         string           `json:"runId,omitempty"`
	StartedAt     *time.Time       `json:"startedAt,omitempty"`
	CompletedAt   *time.Time       `json:"completedAt,omitempty"`
	Config        *types.RunConfig `json:"config,omitempty"`

	// Turn / ToolCall payload — populated for turn_record and
	// tool_call_record respectively.
	Turn        int                    `json:"turn,omitempty"`
	ModelInput  *types.ModelInput      `json:"modelInput,omitempty"`
	ModelOutput []types.ContentBlock   `json:"modelOutput,omitempty"`
	ToolCalls   []types.ToolCallRecord `json:"toolCalls,omitempty"`
	ToolCall    *types.ToolCallRecord  `json:"toolCall,omitempty"`

	// Hook payload — populated for hook_record events.
	Hook *types.HookExecution `json:"hook,omitempty"`

	// Trace summary — populated for run_finished events. Embedding the
	// full RunTrace here keeps the backward-compat reader's job trivial:
	// a kindless legacy line and a run_finished event both decode the
	// RunTrace payload from the same position.
	Trace *types.RunTrace `json:"trace,omitempty"`
}

// MarshalLine renders the event as a single line of JSON terminated by
// '\n'. Returns an error only if json.Marshal does (no event field is
// itself non-marshalable in practice).
func MarshalLine(e Event) ([]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
