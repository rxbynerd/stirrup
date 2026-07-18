package core

import (
	"encoding/json"
	"testing"
)

func TestStallDetector_RepeatedCalls(t *testing.T) {
	s := &stallDetector{}
	input := json.RawMessage(`{"path":"main.go"}`)

	if outcome := s.recordToolCall("read_file", input, true); outcome != "" {
		t.Fatalf("call 1: unexpected outcome %q", outcome)
	}
	if outcome := s.recordToolCall("read_file", input, true); outcome != "" {
		t.Fatalf("call 2: unexpected outcome %q", outcome)
	}

	if outcome := s.recordToolCall("read_file", input, true); outcome != "stalled" {
		t.Fatalf("call 3: expected 'stalled', got %q", outcome)
	}
}

func TestStallDetector_DifferentCallsResetRepeatCount(t *testing.T) {
	s := &stallDetector{}
	inputA := json.RawMessage(`{"path":"a.go"}`)
	inputB := json.RawMessage(`{"path":"b.go"}`)

	s.recordToolCall("read_file", inputA, true)
	s.recordToolCall("read_file", inputA, true)

	if outcome := s.recordToolCall("read_file", inputB, true); outcome != "" {
		t.Fatalf("expected no outcome after different call, got %q", outcome)
	}

	if outcome := s.recordToolCall("read_file", inputB, true); outcome != "" {
		t.Fatalf("expected no outcome, got %q", outcome)
	}

	if outcome := s.recordToolCall("read_file", inputB, true); outcome != "stalled" {
		t.Fatalf("expected 'stalled', got %q", outcome)
	}
}

func TestStallDetector_ConsecutiveFailures(t *testing.T) {
	s := &stallDetector{}

	for i := 0; i < maxConsecutiveFailures-1; i++ {
		input := json.RawMessage(`{"i":` + string(rune('0'+i)) + `}`)
		if outcome := s.recordToolCall("tool_"+string(rune('a'+i)), input, false); outcome != "" {
			t.Fatalf("call %d: unexpected outcome %q", i+1, outcome)
		}
	}

	input := json.RawMessage(`{"final":true}`)
	if outcome := s.recordToolCall("tool_final", input, false); outcome != "tool_failures" {
		t.Fatalf("expected 'tool_failures', got %q", outcome)
	}
}

func TestStallDetector_SuccessResetsFailures(t *testing.T) {
	s := &stallDetector{}

	for i := 0; i < maxConsecutiveFailures-1; i++ {
		input := json.RawMessage(`{"i":` + string(rune('0'+i)) + `}`)
		s.recordToolCall("tool_"+string(rune('a'+i)), input, false)
	}

	s.recordToolCall("good_tool", json.RawMessage(`{}`), true)

	if outcome := s.recordToolCall("bad_tool", json.RawMessage(`{}`), false); outcome != "" {
		t.Fatalf("expected no outcome after reset, got %q", outcome)
	}
}

func TestStallDetector_MixedCalls(t *testing.T) {
	s := &stallDetector{}
	input := json.RawMessage(`{"path":"main.go"}`)

	s.recordToolCall("read_file", input, true)
	s.recordToolCall("read_file", input, false)

	if outcome := s.recordToolCall("read_file", input, true); outcome != "stalled" {
		t.Fatalf("expected 'stalled', got %q", outcome)
	}
}

func TestStallDetector_SameNameDifferentInput(t *testing.T) {
	s := &stallDetector{}

	s.recordToolCall("read_file", json.RawMessage(`{"path":"a.go"}`), true)
	s.recordToolCall("read_file", json.RawMessage(`{"path":"b.go"}`), true)
	if outcome := s.recordToolCall("read_file", json.RawMessage(`{"path":"c.go"}`), true); outcome != "" {
		t.Fatalf("expected no stall for different inputs, got %q", outcome)
	}
}
