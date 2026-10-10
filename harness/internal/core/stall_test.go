package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type stallCall struct {
	name    string
	input   string
	output  string
	success bool
}

func okCall(name, input, output string) stallCall {
	return stallCall{name: name, input: input, output: output, success: true}
}

func (c stallCall) record(s *stallDetector) string {
	return s.recordToolCall(c.name, json.RawMessage(c.input), c.output, c.success)
}

// runStallBatches feeds each call as its own batch, taking any nudge
// after it as the loop does, and returns the 1-based call numbers that
// produced a nudge and the outcome that ended the sequence.
func runStallBatches(t *testing.T, s *stallDetector, calls []stallCall) (nudgedAt []int, outcome string, endedAt int) {
	t.Helper()
	for i, c := range calls {
		if out := c.record(s); out != "" {
			return nudgedAt, out, i + 1
		}
		if _, ok := s.takeNudge(); ok {
			nudgedAt = append(nudgedAt, i+1)
		}
	}
	return nudgedAt, "", 0
}

func repeatCalls(pattern []stallCall, times int) []stallCall {
	var out []stallCall
	for i := 0; i < times; i++ {
		out = append(out, pattern...)
	}
	return out
}

func TestStallDetector_IdenticalCallsNudgeBeforeStall(t *testing.T) {
	s := &stallDetector{}
	call := okCall("read_file", `{"path":"main.go"}`, "package main")

	for i := 1; i <= stallRepeats; i++ {
		if out := call.record(s); out != "" {
			t.Fatalf("call %d: outcome %q, want none before a nudge", i, out)
		}
	}
	text, ok := s.takeNudge()
	if !ok {
		t.Fatal("no nudge after three identical calls")
	}
	if want := fmt.Sprintf(stallNudgeText, stallRepeats); text != want {
		t.Errorf("nudge text = %q, want %q", text, want)
	}
	if _, again := s.takeNudge(); again {
		t.Error("takeNudge returned the same nudge twice")
	}

	for i := 1; i < stallRepeats; i++ {
		if out := call.record(s); out != "" {
			t.Fatalf("post-nudge call %d: outcome %q, want a full threshold before stalling", i, out)
		}
	}
	if out := call.record(s); out != "stalled" {
		t.Fatalf("post-nudge call %d: outcome %q, want stalled", stallRepeats, out)
	}
}

func TestStallDetector_CyclesNudgeOnceThenStall(t *testing.T) {
	a := okCall("read_file", `{"path":"a.go"}`, "A")
	b := okCall("grep", `{"pattern":"x"}`, "B")
	c := okCall("list_directory", `{"path":"."}`, "C")
	d := okCall("run_command", `{"command":"go test"}`, "FAIL")

	cases := []struct {
		name    string
		pattern []stallCall
	}{
		{"length 2", []stallCall{a, b}},
		{"length 3", []stallCall{a, b, c}},
		{"length 4", []stallCall{a, b, c, d}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			threshold := len(tc.pattern) * stallRepeats
			nudgedAt, outcome, endedAt := runStallBatches(t, &stallDetector{}, repeatCalls(tc.pattern, 2*stallRepeats+1))
			if fmt.Sprint(nudgedAt) != fmt.Sprint([]int{threshold}) {
				t.Errorf("nudges at calls %v, want exactly one at call %d", nudgedAt, threshold)
			}
			if outcome != "stalled" || endedAt != 2*threshold {
				t.Errorf("ended with %q at call %d, want stalled at call %d", outcome, endedAt, 2*threshold)
			}
		})
	}
}

func TestStallDetector_CycleNudgeNamesPatternLength(t *testing.T) {
	s := &stallDetector{}
	pattern := []stallCall{okCall("a", `{}`, "1"), okCall("b", `{}`, "2")}
	for _, c := range repeatCalls(pattern, stallRepeats) {
		c.record(s)
	}
	text, ok := s.takeNudge()
	if !ok || !strings.Contains(text, "The last 6 tool calls") {
		t.Errorf("nudge = %q, %v; want one naming the last 6 tool calls", text, ok)
	}
}

func TestStallDetector_AlternationBrokenAfterNudgeDoesNotStall(t *testing.T) {
	a := okCall("read_file", `{"path":"a.go"}`, "A")
	b := okCall("read_file", `{"path":"b.go"}`, "B")
	calls := repeatCalls([]stallCall{a, b}, stallRepeats)
	for i := 0; i < 20; i++ {
		calls = append(calls, okCall("read_file", fmt.Sprintf(`{"path":"f%d.go"}`, i), "new"))
	}

	nudgedAt, outcome, _ := runStallBatches(t, &stallDetector{}, calls)
	if len(nudgedAt) != 1 || outcome != "" {
		t.Errorf("nudges %v, outcome %q; want one nudge and no outcome once the model changes approach", nudgedAt, outcome)
	}
}

func TestStallDetector_ChangingOutputIsNotAStall(t *testing.T) {
	s := &stallDetector{}
	for i := 0; i < 3*stallWindowSize; i++ {
		call := okCall("run_command", `{"command":"tail build.log"}`, fmt.Sprintf("progress %d%%", i))
		if out := call.record(s); out != "" {
			t.Fatalf("call %d: outcome %q, want none for a poll whose output changes", i+1, out)
		}
		if _, ok := s.takeNudge(); ok {
			t.Fatalf("call %d: nudge for a poll whose output changes", i+1)
		}
	}
}

func TestStallDetector_KeyOrderAndWhitespaceDoNotDefeatDetection(t *testing.T) {
	s := &stallDetector{}
	inputs := []string{`{"a":1,"b":2}`, `{"b":2,"a":1}`, "{ \"a\" : 1,\n \"b\" : 2 }"}
	for _, in := range inputs {
		if out := okCall("tool", in, "same").record(s); out != "" {
			t.Fatalf("input %s: outcome %q", in, out)
		}
	}
	if _, ok := s.takeNudge(); !ok {
		t.Error("no nudge: inputs that differ only in key order or whitespace must count as the same call")
	}
}

func TestStallDetector_SuccessFlagIsPartOfTheSignature(t *testing.T) {
	s := &stallDetector{}
	in := `{"path":"main.go"}`
	stallCall{name: "read_file", input: in, output: "x", success: true}.record(s)
	stallCall{name: "read_file", input: in, output: "x", success: false}.record(s)
	stallCall{name: "read_file", input: in, output: "x", success: true}.record(s)
	if _, ok := s.takeNudge(); ok {
		t.Error("nudge for calls whose success differs")
	}
}

func TestStallDetector_CallsBeforeNudgeIsTakenDoNotCount(t *testing.T) {
	s := &stallDetector{}
	call := okCall("read_file", `{"path":"main.go"}`, "same")
	for i := 0; i < 3*stallRepeats; i++ {
		if out := call.record(s); out != "" {
			t.Fatalf("call %d in the nudging batch: outcome %q; the model has not seen the nudge yet", i+1, out)
		}
	}
	if _, ok := s.takeNudge(); !ok {
		t.Fatal("no nudge from the batch")
	}
	for i := 1; i <= stallRepeats; i++ {
		out := call.record(s)
		if i < stallRepeats && out != "" {
			t.Fatalf("post-nudge call %d: outcome %q", i, out)
		}
		if i == stallRepeats && out != "stalled" {
			t.Fatalf("post-nudge call %d: outcome %q, want stalled", i, out)
		}
	}
}

func TestStallDetector_WindowIsBounded(t *testing.T) {
	s := &stallDetector{}
	for i := 0; i < 5*stallWindowSize; i++ {
		okCall("read_file", fmt.Sprintf(`{"i":%d}`, i), "x").record(s)
	}
	if len(s.window) != stallWindowSize {
		t.Errorf("window holds %d signatures, want %d", len(s.window), stallWindowSize)
	}
}

func TestStallDetector_ConsecutiveFailures(t *testing.T) {
	s := &stallDetector{}

	for i := 0; i < maxConsecutiveFailures-1; i++ {
		c := stallCall{name: fmt.Sprintf("tool_%d", i), input: fmt.Sprintf(`{"i":%d}`, i), output: "boom"}
		if out := c.record(s); out != "" {
			t.Fatalf("call %d: unexpected outcome %q", i+1, out)
		}
	}

	if out := (stallCall{name: "tool_final", input: `{"final":true}`, output: "boom"}).record(s); out != "tool_failures" {
		t.Fatalf("expected 'tool_failures', got %q", out)
	}
}

func TestStallDetector_SuccessResetsFailures(t *testing.T) {
	s := &stallDetector{}

	for i := 0; i < maxConsecutiveFailures-1; i++ {
		stallCall{name: fmt.Sprintf("tool_%d", i), input: fmt.Sprintf(`{"i":%d}`, i), output: "boom"}.record(s)
	}

	okCall("good_tool", `{}`, "ok").record(s)

	if out := (stallCall{name: "bad_tool", input: `{}`, output: "boom"}).record(s); out != "" {
		t.Fatalf("expected no outcome after reset, got %q", out)
	}
}

func TestStallDetector_FailureLimitIsNotDeferredByANudge(t *testing.T) {
	s := &stallDetector{}
	failing := stallCall{name: "run_command", input: `{"command":"make"}`, output: "exit 2"}
	for i := 1; i < maxConsecutiveFailures; i++ {
		if out := failing.record(s); out != "" {
			t.Fatalf("call %d: outcome %q", i, out)
		}
	}
	if out := failing.record(s); out != "tool_failures" {
		t.Errorf("call %d: outcome %q, want tool_failures even with a nudge pending", maxConsecutiveFailures, out)
	}
}

func TestStallDetector_SameNameDifferentInput(t *testing.T) {
	s := &stallDetector{}

	okCall("read_file", `{"path":"a.go"}`, "x").record(s)
	okCall("read_file", `{"path":"b.go"}`, "x").record(s)
	okCall("read_file", `{"path":"c.go"}`, "x").record(s)
	if _, ok := s.takeNudge(); ok {
		t.Error("nudge for calls with different inputs")
	}
}

func TestCanonicalToolInput(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		same bool
	}{
		{"key order", `{"a":1,"b":2}`, `{"b":2,"a":1}`, true},
		{"nested key order", `{"o":{"x":[1,{"q":1,"p":2}]}}`, `{"o":{"x":[1,{"p":2,"q":1}]}}`, true},
		{"whitespace", `{"a":1}`, " {\n\t\"a\" : 1 } ", true},
		{"number spelling kept", `{"n":1}`, `{"n":1.0}`, false},
		{"large integers kept", `{"n":9007199254740993}`, `{"n":9007199254740992}`, false},
		{"array order matters", `[1,2]`, `[2,1]`, false},
		{"invalid JSON compared raw", `{"a":1`, `{"a":1`, true},
		{"invalid JSON never equals a valid form", `{"a":1} trailing`, `{"a":1}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ca := string(canonicalToolInput(json.RawMessage(tc.a)))
			cb := string(canonicalToolInput(json.RawMessage(tc.b)))
			if (ca == cb) != tc.same {
				t.Errorf("canonical(%s) = %s, canonical(%s) = %s; same = %v, want %v", tc.a, ca, tc.b, cb, ca == cb, tc.same)
			}
		})
	}
}
