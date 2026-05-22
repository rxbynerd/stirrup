// Package trace — events.go defines the on-disk JSONL event shape the
// streaming JSONLTraceEmitter writes and the reader in
// types/trace/reader.go consumes.
//
// Each line in a streaming trace file is one JSON object with a "kind"
// discriminator. A complete run produces:
//
//	{"kind":"run_started",      "schemaVersion":"1","runId":"...","config":{...redacted...},"startedAt":"..."}
//	{"kind":"turn_record",      "turn":1,"modelInput":{...},"modelOutput":[...],"toolCalls":[...]}
//	{"kind":"tool_call_record", "turn":1,"name":"read_file","input":{...},"output":"..."}
//	...
//	{"kind":"run_finished",     "trace":{...RunTrace summary...},"completedAt":"..."}
//
// turn_record carries the full transcript a sub-agent / replay path needs.
// tool_call_record is emitted in addition to the inline toolCalls inside
// turn_record so a strict event-by-event consumer (e.g. future gRPC fan-out)
// sees each call as soon as it lands rather than waiting for the enclosing
// turn to flush. Both views are scrubbed for known secret patterns by the
// emitter before the line is written.
//
// An interrupted run (SIGKILL, crash, OOM) may end without run_finished;
// the on-disk file is still parseable up to the last completed event.
//
// Backward compatibility: pre-streaming traces emitted a single
// json.Marshal(types.RunTrace) line with no "kind" field. The reader
// (types/trace/reader.go) treats a kindless line as an implicit
// run_finished event with the trace payload embedded.
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
