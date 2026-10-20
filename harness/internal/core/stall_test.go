package core

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
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
	must := require.New(t)
	s := &stallDetector{}
	call := okCall("read_file", `{"path":"main.go"}`, "package main")

	for i := 1; i <= stallRepeats; i++ {
		out := call.record(s)
		must.Empty(out, "call %d should not end the run before a nudge", i)
	}
	text, ok := s.takeNudge()
	must.True(ok, "no nudge after three identical calls")
	must.Equal(fmt.Sprintf(stallNudgeText, stallRepeats), text)
	_, again := s.takeNudge()
	must.False(again, "takeNudge returned the same nudge twice")

	for i := 1; i < stallRepeats; i++ {
		out := call.record(s)
		must.Empty(out, "post-nudge call %d should wait for a full threshold", i)
	}
	must.Equal("stalled", call.record(s), "post-nudge call %d", stallRepeats)
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
			must := require.New(t)
			threshold := len(tc.pattern) * stallRepeats
			nudgedAt, outcome, endedAt := runStallBatches(t, &stallDetector{}, repeatCalls(tc.pattern, 2*stallRepeats+1))
			must.Equal([]int{threshold}, nudgedAt, "want exactly one nudge, at call %d", threshold)
			must.Equal("stalled", outcome)
			must.Equal(2*threshold, endedAt)
		})
	}
}

func TestStallDetector_CycleNudgeNamesPatternLength(t *testing.T) {
	must := require.New(t)
	s := &stallDetector{}
	pattern := []stallCall{okCall("a", `{}`, "1"), okCall("b", `{}`, "2")}
	for _, c := range repeatCalls(pattern, stallRepeats) {
		c.record(s)
	}
	text, ok := s.takeNudge()
	must.True(ok, "no nudge after the pattern repeated %d times", stallRepeats)
	must.Contains(text, "The last 6 tool calls")
}

func TestStallDetector_AlternationBrokenAfterNudgeDoesNotStall(t *testing.T) {
	must := require.New(t)
	a := okCall("read_file", `{"path":"a.go"}`, "A")
	b := okCall("read_file", `{"path":"b.go"}`, "B")
	calls := repeatCalls([]stallCall{a, b}, stallRepeats)
	for i := 0; i < 20; i++ {
		calls = append(calls, okCall("read_file", fmt.Sprintf(`{"path":"f%d.go"}`, i), "new"))
	}

	nudgedAt, outcome, _ := runStallBatches(t, &stallDetector{}, calls)
	must.Len(nudgedAt, 1, "want one nudge, got nudges at calls %v", nudgedAt)
	must.Empty(outcome, "want no outcome once the model changes approach")
}

func TestStallDetector_ChangingOutputIsNotAStall(t *testing.T) {
	must := require.New(t)
	s := &stallDetector{}
	for i := 0; i < 3*stallWindowSize; i++ {
		call := okCall("run_command", `{"command":"tail build.log"}`, fmt.Sprintf("progress %d%%", i))
		out := call.record(s)
		must.Empty(out, "call %d: poll whose output changes", i+1)
		_, ok := s.takeNudge()
		must.False(ok, "call %d: nudge for a poll whose output changes", i+1)
	}
}

func TestStallDetector_KeyOrderAndWhitespaceDoNotDefeatDetection(t *testing.T) {
	must := require.New(t)
	s := &stallDetector{}
	inputs := []string{`{"a":1,"b":2}`, `{"b":2,"a":1}`, "{ \"a\" : 1,\n \"b\" : 2 }"}
	for _, in := range inputs {
		out := okCall("tool", in, "same").record(s)
		must.Empty(out, "input %s", in)
	}
	_, ok := s.takeNudge()
	must.True(ok, "inputs that differ only in key order or whitespace must count as the same call")
}

func TestStallDetector_SuccessFlagIsPartOfTheSignature(t *testing.T) {
	must := require.New(t)
	s := &stallDetector{}
	in := `{"path":"main.go"}`
	stallCall{name: "read_file", input: in, output: "x", success: true}.record(s)
	stallCall{name: "read_file", input: in, output: "x", success: false}.record(s)
	stallCall{name: "read_file", input: in, output: "x", success: true}.record(s)
	_, ok := s.takeNudge()
	must.False(ok, "nudge for calls whose success differs")
}

func TestStallDetector_CallsBeforeNudgeTakenDoNotCount(t *testing.T) {
	must := require.New(t)
	s := &stallDetector{}
	call := okCall("read_file", `{"path":"main.go"}`, "same")
	for i := 0; i < 3*stallRepeats; i++ {
		out := call.record(s)
		must.Empty(out, "call %d in the nudging batch; the model has not seen the nudge yet", i+1)
	}
	_, ok := s.takeNudge()
	must.True(ok, "no nudge from the batch")

	for i := 1; i < stallRepeats; i++ {
		out := call.record(s)
		must.Empty(out, "post-nudge call %d", i)
	}
	must.Equal("stalled", call.record(s), "post-nudge call %d", stallRepeats)
}

func TestStallDetector_WindowIsBounded(t *testing.T) {
	must := require.New(t)
	s := &stallDetector{}
	for i := 0; i < 5*stallWindowSize; i++ {
		okCall("read_file", fmt.Sprintf(`{"i":%d}`, i), "x").record(s)
	}
	must.Len(s.window, stallWindowSize)
}

func TestStallDetector_ConsecutiveFailuresEndRun(t *testing.T) {
	must := require.New(t)
	s := &stallDetector{}

	for i := 0; i < maxConsecutiveFailures-1; i++ {
		c := stallCall{name: fmt.Sprintf("tool_%d", i), input: fmt.Sprintf(`{"i":%d}`, i), output: "boom"}
		out := c.record(s)
		must.Empty(out, "call %d", i+1)
	}

	out := (stallCall{name: "tool_final", input: `{"final":true}`, output: "boom"}).record(s)
	must.Equal("tool_failures", out)
}

func TestStallDetector_SuccessResetsFailures(t *testing.T) {
	must := require.New(t)
	s := &stallDetector{}

	for i := 0; i < maxConsecutiveFailures-1; i++ {
		stallCall{name: fmt.Sprintf("tool_%d", i), input: fmt.Sprintf(`{"i":%d}`, i), output: "boom"}.record(s)
	}

	okCall("good_tool", `{}`, "ok").record(s)

	out := (stallCall{name: "bad_tool", input: `{}`, output: "boom"}).record(s)
	must.Empty(out, "expected no outcome after the success reset")
}

func TestStallDetector_FailureLimitNotDeferredByNudge(t *testing.T) {
	must := require.New(t)
	s := &stallDetector{}
	failing := stallCall{name: "run_command", input: `{"command":"make"}`, output: "exit 2"}
	for i := 1; i < maxConsecutiveFailures; i++ {
		out := failing.record(s)
		must.Empty(out, "call %d", i)
	}
	out := failing.record(s)
	must.Equal("tool_failures", out, "call %d with a nudge pending", maxConsecutiveFailures)
}

func TestStallDetector_SameNameDifferentInputNoNudge(t *testing.T) {
	must := require.New(t)
	s := &stallDetector{}

	okCall("read_file", `{"path":"a.go"}`, "x").record(s)
	okCall("read_file", `{"path":"b.go"}`, "x").record(s)
	okCall("read_file", `{"path":"c.go"}`, "x").record(s)
	_, ok := s.takeNudge()
	must.False(ok, "nudge for calls with different inputs")
}

func spilledRendering(refID, tail string) string {
	return "[command output spilled to compliance archive]\n" +
		"exit_code=1 timed_out=false timeout_seconds=30 capture_complete=true archive_id=run-1\n" +
		"stdout raw_bytes=40000 scrubbed_bytes=40000 raw_sha256=aa scrubbed_sha256=aa\n" +
		"stdout_ref=stirrup://command-output/" + refID + "/stdout\n" +
		"stdout_tail:\n" + tail + "\n" +
		"stderr raw_bytes=0 scrubbed_bytes=0 raw_sha256=bb scrubbed_sha256=bb\n" +
		"stderr_ref=stirrup://command-output/" + refID + "/stderr\n" +
		"stderr_tail:\n"
}

func TestStallSignature_IgnoresPerCallCommandOutputRefs(t *testing.T) {
	must := require.New(t)
	input := json.RawMessage(`{"command":"go test ./..."}`)
	first := stallSignatureOf("run_command", input, spilledRendering("0123456789abcdef", "FAIL"), false)
	second := stallSignatureOf("run_command", input, spilledRendering("fedcba9876543210", "FAIL"), false)
	must.Equal(first, second, "spilled renderings that differ only in their references")

	changed := stallSignatureOf("run_command", input, spilledRendering("fedcba9876543210", "ok"), false)
	must.NotEqual(first, changed, "spilled renderings with different output")
}

func TestCanonicalToolInput_EqualityRules(t *testing.T) {
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
			must := require.New(t)
			ca := string(canonicalToolInput(json.RawMessage(tc.a)))
			cb := string(canonicalToolInput(json.RawMessage(tc.b)))
			if tc.same {
				must.Equal(ca, cb)
			} else {
				must.NotEqual(ca, cb)
			}
		})
	}
}
