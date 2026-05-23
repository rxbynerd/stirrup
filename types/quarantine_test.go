package types

import (
	"strings"
	"testing"
)

// TestClassifyForQuarantine_Empty pins that an empty recording set
// produces no flags. A no-data miner output is allowed through
// without a quarantine challenge.
func TestClassifyForQuarantine_Empty(t *testing.T) {
	flags := ClassifyForQuarantine(nil)
	if len(flags) != 0 {
		t.Errorf("nil recordings flags = %v, want empty", flags)
	}
	flags = ClassifyForQuarantine([]RunRecording{})
	if len(flags) != 0 {
		t.Errorf("empty recordings flags = %v, want empty", flags)
	}
}

// TestClassifyForQuarantine_SmallPayload pins that recordings well
// under the default 256 KiB threshold do not trigger
// QuarantineLargePayload. A modest conversation should ingest
// without lighting up the flag.
func TestClassifyForQuarantine_SmallPayload(t *testing.T) {
	recordings := []RunRecording{
		{
			RunID: "r1",
			Turns: []TurnRecord{
				{
					Turn:        1,
					ModelOutput: []ContentBlock{{Type: "text", Text: "hello world"}},
				},
			},
		},
	}
	flags := ClassifyForQuarantine(recordings)
	if len(flags) != 0 {
		t.Errorf("small-payload recording flags = %v, want empty", flags)
	}
}

// TestClassifyForQuarantine_LargeToolOutput pins that a tool call
// whose output exceeds the threshold individually trips
// QuarantineLargePayload. The threshold is conservative enough that
// ordinary conversation turns stay under it; a single multi-hundred-
// kilobyte tool output is the signal we want to flag.
func TestClassifyForQuarantine_LargeToolOutput(t *testing.T) {
	big := strings.Repeat("x", DefaultLargePayloadBytes+1)
	recordings := []RunRecording{
		{
			RunID: "r1",
			Turns: []TurnRecord{
				{
					Turn: 1,
					ToolCalls: []ToolCallRecord{
						{Name: "read_file", Output: big},
					},
				},
			},
		},
	}
	flags := ClassifyForQuarantine(recordings)
	if len(flags) != 1 || flags[0] != QuarantineLargePayload {
		t.Errorf("large-payload flags = %v, want [%s]", flags, QuarantineLargePayload)
	}
}

// TestClassifyForQuarantine_LargeTurnAggregate pins that a turn
// whose aggregate content (model output + tool I/O) exceeds the
// threshold trips the flag even when no single payload is big on
// its own. Catches the "many medium-sized tool outputs" case.
func TestClassifyForQuarantine_LargeTurnAggregate(t *testing.T) {
	half := strings.Repeat("y", DefaultLargePayloadBytes/2)
	recordings := []RunRecording{
		{
			RunID: "r1",
			Turns: []TurnRecord{
				{
					Turn:        1,
					ModelOutput: []ContentBlock{{Type: "text", Text: half}},
					ToolCalls: []ToolCallRecord{
						{Name: "a", Output: half},
						{Name: "b", Output: half},
					},
				},
			},
		},
	}
	flags := ClassifyForQuarantine(recordings)
	if len(flags) != 1 || flags[0] != QuarantineLargePayload {
		t.Errorf("aggregate-large flags = %v, want [%s]", flags, QuarantineLargePayload)
	}
}
