package reporter

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/eval"
)

// parseJUnit decodes the JUnit XML produced by WriteJUnit into the same
// mirror structs used for emission. Tests use this to assert structural
// invariants without re-implementing an XML parser.
func parseJUnit(t *testing.T, b []byte) xmlTestSuites {
	t.Helper()
	var got xmlTestSuites
	if err := xml.Unmarshal(b, &got); err != nil {
		t.Fatalf("parsing emitted XML: %v\n--- output ---\n%s", err, string(b))
	}
	return got
}

// runWriteJUnit is a small helper to keep tests focused on assertions.
func runWriteJUnit(t *testing.T, result eval.SuiteResult) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteJUnit(&buf, result); err != nil {
		t.Fatalf("WriteJUnit: %v", err)
	}
	return buf.Bytes()
}

func TestWriteJUnit_HeaderAndRoot(t *testing.T) {
	result := eval.SuiteResult{SuiteID: "s1"}
	out := runWriteJUnit(t, result)

	if !bytes.HasPrefix(out, []byte(xml.Header)) {
		preview := out
		if len(preview) > 64 {
			preview = preview[:64]
		}
		t.Fatalf("output should start with XML header %q, got %q", xml.Header, string(preview))
	}

	doc := parseJUnit(t, out)
	if len(doc.TestSuites) != 1 {
		t.Fatalf("want exactly 1 <testsuite>, got %d", len(doc.TestSuites))
	}
}

func TestWriteJUnit_CountsAndAttributes(t *testing.T) {
	started := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	completed := started.Add(2500 * time.Millisecond)

	result := eval.SuiteResult{
		SuiteID:     "demo",
		StartedAt:   started,
		CompletedAt: completed,
		Tasks: []eval.TaskResult{
			{TaskID: "p1", Outcome: "pass", DurationMs: 1234},
			{TaskID: "p2", Outcome: "pass", DurationMs: 100},
			{TaskID: "f1", Outcome: "fail", DurationMs: 500,
				JudgeVerdict: eval.JudgeVerdict{Reason: "expected 200, got 500"}},
			{TaskID: "e1", Outcome: "error", DurationMs: 50, Error: "harness crashed"},
		},
	}

	out := runWriteJUnit(t, result)
	doc := parseJUnit(t, out)
	suite := doc.TestSuites[0]

	if suite.Name != "demo" {
		t.Errorf("name = %q, want %q", suite.Name, "demo")
	}
	if suite.Tests != 4 {
		t.Errorf("tests = %d, want 4", suite.Tests)
	}
	if suite.Failures != 1 {
		t.Errorf("failures = %d, want 1", suite.Failures)
	}
	if suite.Errors != 1 {
		t.Errorf("errors = %d, want 1", suite.Errors)
	}
	if suite.Time != "2.500" {
		t.Errorf("time = %q, want %q (3 d.p.)", suite.Time, "2.500")
	}
	if suite.Timestamp != "2026-05-09T12:00:00Z" {
		t.Errorf("timestamp = %q, want RFC3339 UTC", suite.Timestamp)
	}
	if len(suite.TestCases) != 4 {
		t.Fatalf("testcases = %d, want 4", len(suite.TestCases))
	}
}

func TestWriteJUnit_TestCaseTimeIsSeconds(t *testing.T) {
	result := eval.SuiteResult{
		SuiteID: "s",
		Tasks: []eval.TaskResult{
			{TaskID: "t", Outcome: "pass", DurationMs: 1500},
		},
	}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	tc := doc.TestSuites[0].TestCases[0]
	if tc.Time != "1.500" {
		t.Errorf("time = %q, want %q", tc.Time, "1.500")
	}
	if tc.Classname != "s" {
		t.Errorf("classname = %q, want suiteId %q", tc.Classname, "s")
	}
}

func TestWriteJUnit_PassHasNoChildren(t *testing.T) {
	result := eval.SuiteResult{
		SuiteID: "s",
		Tasks: []eval.TaskResult{
			{TaskID: "p", Outcome: "pass", DurationMs: 10},
		},
	}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	tc := doc.TestSuites[0].TestCases[0]
	if tc.Failure != nil {
		t.Errorf("pass case should have no <failure>, got %+v", tc.Failure)
	}
	if tc.Error != nil {
		t.Errorf("pass case should have no <error>, got %+v", tc.Error)
	}
}

func TestWriteJUnit_FailEmitsFailure(t *testing.T) {
	result := eval.SuiteResult{
		SuiteID: "s",
		Tasks: []eval.TaskResult{
			{
				TaskID:  "t",
				Outcome: "fail",
				JudgeVerdict: eval.JudgeVerdict{
					Reason: "judge said no",
					Details: []eval.JudgeDetail{
						{Type: "test-command", Reason: "exit code 1"},
						{Type: "file-exists", Reason: "missing /tmp/expected"},
					},
				},
			},
		},
	}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	tc := doc.TestSuites[0].TestCases[0]
	if tc.Failure == nil {
		t.Fatal("fail case should have <failure>")
	}
	if tc.Error != nil {
		t.Error("fail case should not have <error>")
	}
	if tc.Failure.Type != "EvalFailure" {
		t.Errorf("failure type = %q, want %q", tc.Failure.Type, "EvalFailure")
	}
	if tc.Failure.Message != "judge said no" {
		t.Errorf("failure message = %q, want judge reason", tc.Failure.Message)
	}
	body := tc.Failure.Body
	if !strings.Contains(body, "judge said no") {
		t.Errorf("failure body should contain reason; got %q", body)
	}
	if !strings.Contains(body, "test-command: exit code 1") {
		t.Errorf("failure body should contain detail line %q; got %q",
			"test-command: exit code 1", body)
	}
	if !strings.Contains(body, "file-exists: missing /tmp/expected") {
		t.Errorf("failure body should contain second detail; got %q", body)
	}
}

// TestWriteJUnit_FailMessageFallback pins that when the judge verdict has
// no top-level Reason but carries sub-judge Details, the <failure
// message=...> attribute is populated from the first detail.
func TestWriteJUnit_FailMessageFallback(t *testing.T) {
	result := eval.SuiteResult{
		SuiteID: "s",
		Tasks: []eval.TaskResult{
			{
				TaskID:  "composite-no-reason",
				Outcome: "fail",
				JudgeVerdict: eval.JudgeVerdict{
					Reason: "", // intentionally empty
					Details: []eval.JudgeDetail{
						{Type: "test-command", Reason: "exit code 1"},
						{Type: "file-exists", Reason: "missing /tmp/x"},
					},
				},
			},
		},
	}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	tc := doc.TestSuites[0].TestCases[0]
	if tc.Failure == nil {
		t.Fatal("fail case should have <failure>")
	}
	if tc.Failure.Message != "exit code 1" {
		t.Errorf("failure message = %q, want first detail reason %q", tc.Failure.Message, "exit code 1")
	}
}

// TestWriteJUnit_UnknownOutcome pins that an outcome value not in
// {"pass", "fail", "error"} surfaces as <error type="UnknownOutcome"> and
// increments the suite Errors count.
func TestWriteJUnit_UnknownOutcome(t *testing.T) {
	result := eval.SuiteResult{
		SuiteID: "s",
		Tasks: []eval.TaskResult{
			{TaskID: "skipped-task", Outcome: "skipped"},
		},
	}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	suite := doc.TestSuites[0]
	if suite.Errors != 1 {
		t.Errorf("suite errors = %d, want 1", suite.Errors)
	}
	tc := suite.TestCases[0]
	if tc.Error == nil {
		t.Fatal("unknown outcome case should have <error>")
	}
	if tc.Error.Type != "UnknownOutcome" {
		t.Errorf("error type = %q, want %q", tc.Error.Type, "UnknownOutcome")
	}
	if !strings.Contains(tc.Error.Message, "skipped") {
		t.Errorf("error message = %q, want it to mention the unknown outcome value", tc.Error.Message)
	}
}

func TestWriteJUnit_ErrorEmitsError(t *testing.T) {
	result := eval.SuiteResult{
		SuiteID: "s",
		Tasks: []eval.TaskResult{
			{TaskID: "boom", Outcome: "error", Error: "exec: harness binary missing"},
		},
	}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	tc := doc.TestSuites[0].TestCases[0]
	if tc.Error == nil {
		t.Fatal("error case should have <error>")
	}
	if tc.Failure != nil {
		t.Error("error case should not have <failure>")
	}
	if tc.Error.Type != "HarnessError" {
		t.Errorf("error type = %q, want %q", tc.Error.Type, "HarnessError")
	}
	if tc.Error.Message != "exec: harness binary missing" {
		t.Errorf("error message = %q, want task error", tc.Error.Message)
	}
}

func TestWriteJUnit_EmptySuite(t *testing.T) {
	result := eval.SuiteResult{SuiteID: "empty"}
	out := runWriteJUnit(t, result)
	doc := parseJUnit(t, out)
	suite := doc.TestSuites[0]
	if suite.Tests != 0 || suite.Failures != 0 || suite.Errors != 0 {
		t.Errorf("empty suite counts: tests=%d failures=%d errors=%d, want 0/0/0",
			suite.Tests, suite.Failures, suite.Errors)
	}
	if len(suite.TestCases) != 0 {
		t.Errorf("empty suite should have no testcases, got %d", len(suite.TestCases))
	}
	if suite.Name != "empty" {
		t.Errorf("name = %q, want %q", suite.Name, "empty")
	}
}

// TestWriteJUnit_XMLEscaping pins that we delegate escaping to encoding/xml
// rather than concatenating strings. Task IDs containing XML metacharacters
// must round-trip unchanged through Unmarshal.
func TestWriteJUnit_XMLEscaping(t *testing.T) {
	weird := `<>&"'`
	result := eval.SuiteResult{
		SuiteID: weird,
		Tasks: []eval.TaskResult{
			{
				TaskID:  weird,
				Outcome: "fail",
				JudgeVerdict: eval.JudgeVerdict{
					Reason: "needs <escape> & \"quotes\"",
				},
			},
		},
	}
	out := runWriteJUnit(t, result)
	if strings.Contains(string(out), `name="<>&"'"`) {
		t.Fatalf("attribute quoting was not escaped:\n%s", string(out))
	}
	doc := parseJUnit(t, out)
	tc := doc.TestSuites[0].TestCases[0]
	if tc.Name != weird {
		t.Errorf("task name round-trip mismatch: got %q, want %q", tc.Name, weird)
	}
	if doc.TestSuites[0].Name != weird {
		t.Errorf("suite name round-trip mismatch: got %q, want %q", doc.TestSuites[0].Name, weird)
	}
	if tc.Failure == nil || !strings.Contains(tc.Failure.Body, `needs <escape> & "quotes"`) {
		t.Errorf("failure body round-trip mismatch: %+v", tc.Failure)
	}
}

// TestWriteJUnit_ZeroTimestampFallback pins that with StartedAt/CompletedAt
// both zero, the suite's Time attribute is the sum of per-task DurationMs
// converted to seconds, and Timestamp is empty rather than the Go zero-time
// string.
func TestWriteJUnit_ZeroTimestampFallback(t *testing.T) {
	result := eval.SuiteResult{
		SuiteID: "fallback",
		Tasks: []eval.TaskResult{
			{TaskID: "t1", Outcome: "pass", DurationMs: 1200},
			{TaskID: "t2", Outcome: "pass", DurationMs: 300},
		},
	}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	suite := doc.TestSuites[0]
	if suite.Time != "1.500" {
		t.Errorf("time = %q, want %q (sum of DurationMs / 1000)", suite.Time, "1.500")
	}
	if suite.Timestamp != "" {
		t.Errorf("timestamp = %q, want empty when StartedAt is zero", suite.Timestamp)
	}
}

// TestWriteJUnit_BackwardTimestampFallback pins that when CompletedAt
// precedes StartedAt, a negative wall-clock duration is not emitted; the
// DurationMs sum is used instead.
func TestWriteJUnit_BackwardTimestampFallback(t *testing.T) {
	started := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	result := eval.SuiteResult{
		SuiteID:     "skewed",
		StartedAt:   started,
		CompletedAt: started.Add(-time.Second), // 1s before StartedAt
		Tasks: []eval.TaskResult{
			{TaskID: "t1", Outcome: "pass", DurationMs: 2000},
		},
	}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	suite := doc.TestSuites[0]
	if suite.Time != "2.000" {
		t.Errorf("time = %q, want %q (DurationMs sum, not negative wall-clock)", suite.Time, "2.000")
	}
}

func TestWriteJUnit_ProducesParseableXML(t *testing.T) {
	result := eval.SuiteResult{
		SuiteID:     "smoke",
		StartedAt:   time.Now().UTC(),
		CompletedAt: time.Now().UTC().Add(time.Second),
		Tasks: []eval.TaskResult{
			{TaskID: "ok", Outcome: "pass", DurationMs: 10},
			{TaskID: "bad", Outcome: "fail", DurationMs: 20,
				JudgeVerdict: eval.JudgeVerdict{Reason: "no"}},
			{TaskID: "boom", Outcome: "error", DurationMs: 30, Error: "x"},
		},
	}
	out := runWriteJUnit(t, result)

	// EOF terminates the loop on success; any other error is a
	// parser-detected malformation and must fail the test.
	dec := xml.NewDecoder(bytes.NewReader(out))
	for {
		_, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("malformed XML token: %v\n--- output ---\n%s", err, out)
		}
	}
}

func singleRunJUnitFixture() eval.SuiteResult {
	started := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	return eval.SuiteResult{
		SuiteID:     "golden",
		StartedAt:   started,
		CompletedAt: started.Add(1500 * time.Millisecond),
		Tasks: []eval.TaskResult{
			{TaskID: "ok", Outcome: "pass", DurationMs: 100, PassFraction: 1},
			{TaskID: "bad", Outcome: "fail", DurationMs: 200, JudgeVerdict: eval.JudgeVerdict{
				Reason:  "1 of 2 sub-judges failed",
				Details: []eval.JudgeDetail{{Type: "file-exists", Reason: "missing out.txt"}},
			}},
			{TaskID: "boom", Outcome: "error", DurationMs: 300, Error: "harness exited 7"},
		},
	}
}

const singleRunJUnitGolden = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites>
  <testsuite name="golden" tests="3" failures="1" errors="1" time="1.500" timestamp="2026-05-09T12:00:00Z">
    <testcase name="ok" classname="golden" time="0.100"></testcase>
    <testcase name="bad" classname="golden" time="0.200">
      <failure type="EvalFailure" message="1 of 2 sub-judges failed">1 of 2 sub-judges failed&#xA;&#xA;file-exists: missing out.txt</failure>
    </testcase>
    <testcase name="boom" classname="golden" time="0.300">
      <error type="HarnessError" message="harness exited 7">harness exited 7</error>
    </testcase>
  </testsuite>
</testsuites>
`

// TestWriteJUnit_SingleRunGolden pins the exact bytes for single-run
// results, which carry no Trials and therefore no system-out.
func TestWriteJUnit_SingleRunGolden(t *testing.T) {
	got := string(runWriteJUnit(t, singleRunJUnitFixture()))
	if got != singleRunJUnitGolden {
		t.Errorf("single-run JUnit changed:\n--- got ---\n%s\n--- want ---\n%s", got, singleRunJUnitGolden)
	}
}

func multiTrialTask(id string, trials ...eval.TrialResult) eval.TaskResult {
	r := eval.TaskResult{TaskID: id, Trials: trials}
	c := r.Counts()
	r.Outcome = c.Outcome()
	r.PassFraction = c.PassFraction()
	picked := false
	for _, tr := range trials {
		r.DurationMs += tr.DurationMs
		if !picked && tr.Outcome == r.Outcome {
			r.JudgeVerdict = tr.JudgeVerdict
			r.Error = tr.Error
			picked = true
		}
	}
	return r
}

func TestWriteJUnit_MultiTrialPassSummarisesTrials(t *testing.T) {
	result := eval.SuiteResult{SuiteID: "s", Trials: 3, Tasks: []eval.TaskResult{
		multiTrialTask("flaky",
			eval.TrialResult{Trial: 1, Outcome: "pass", DurationMs: 1000, Turns: 4, JudgeVerdict: eval.JudgeVerdict{Passed: true, Reason: "command exited 0"}},
			eval.TrialResult{Trial: 2, Outcome: "fail", DurationMs: 2000, Turns: 6, JudgeVerdict: eval.JudgeVerdict{Reason: "command exited 1"}},
			eval.TrialResult{Trial: 3, Outcome: "pass", DurationMs: 1500, Turns: 5, JudgeVerdict: eval.JudgeVerdict{Passed: true, Reason: "command exited 0"}},
		),
	}}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	suite := doc.TestSuites[0]
	if suite.Tests != 1 || suite.Failures != 0 || len(suite.TestCases) != 1 {
		t.Fatalf("want one passing testcase, got tests=%d failures=%d cases=%d", suite.Tests, suite.Failures, len(suite.TestCases))
	}
	tc := suite.TestCases[0]
	if tc.Failure != nil || tc.Error != nil {
		t.Errorf("majority-pass task should have no failure/error: %+v %+v", tc.Failure, tc.Error)
	}
	if tc.Time != "4.500" {
		t.Errorf("time = %q, want the summed trial durations 4.500", tc.Time)
	}
	for _, want := range []string{
		"pass fraction 0.667 (2/3 trials passed, 1 failed, 0 errored)",
		"trial 1: pass (1.000s, 4 turns)",
		"trial 2: fail (2.000s, 6 turns): command exited 1",
		"trial 3: pass (1.500s, 5 turns)",
	} {
		if !strings.Contains(tc.SystemOut, want) {
			t.Errorf("system-out missing %q:\n%s", want, tc.SystemOut)
		}
	}
}

func TestWriteJUnit_MultiTrialFailListsTrialReasons(t *testing.T) {
	result := eval.SuiteResult{SuiteID: "s", Trials: 3, Tasks: []eval.TaskResult{
		multiTrialTask("broken",
			eval.TrialResult{Trial: 1, Outcome: "fail", JudgeVerdict: eval.JudgeVerdict{Reason: "missing out.txt"}},
			eval.TrialResult{Trial: 2, Outcome: "pass", JudgeVerdict: eval.JudgeVerdict{Passed: true, Reason: "ok"}},
			eval.TrialResult{Trial: 3, Outcome: "fail", JudgeVerdict: eval.JudgeVerdict{Details: []eval.JudgeDetail{{Type: "test-command", Reason: "exit code 2"}}}},
		),
	}}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	tc := doc.TestSuites[0].TestCases[0]
	if doc.TestSuites[0].Failures != 1 || tc.Failure == nil {
		t.Fatalf("want one failure, got %+v", doc.TestSuites[0])
	}
	if tc.Failure.Message != "1/3 trials passed: missing out.txt" {
		t.Errorf("failure message = %q", tc.Failure.Message)
	}
	for _, want := range []string{"Non-passing trials:", "trial 1 (fail): missing out.txt", "trial 3 (fail): exit code 2"} {
		if !strings.Contains(tc.Failure.Body, want) {
			t.Errorf("failure body missing %q:\n%s", want, tc.Failure.Body)
		}
	}
	if strings.Contains(tc.Failure.Body, "trial 2") {
		t.Errorf("failure body should list only non-passing trials:\n%s", tc.Failure.Body)
	}
}

func TestWriteJUnit_MultiTrialErrorListsTrialErrors(t *testing.T) {
	result := eval.SuiteResult{SuiteID: "s", Trials: 3, Tasks: []eval.TaskResult{
		multiTrialTask("infra",
			eval.TrialResult{Trial: 1, Outcome: "error", Error: "harness exited 7"},
			eval.TrialResult{Trial: 2, Outcome: "error", Error: "context deadline exceeded"},
			eval.TrialResult{Trial: 3, Outcome: "pass", JudgeVerdict: eval.JudgeVerdict{Passed: true}},
		),
	}}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	tc := doc.TestSuites[0].TestCases[0]
	if doc.TestSuites[0].Errors != 1 || tc.Error == nil {
		t.Fatalf("want one error, got %+v", doc.TestSuites[0])
	}
	if tc.Error.Message != "1/3 trials passed: harness exited 7" {
		t.Errorf("error message = %q", tc.Error.Message)
	}
	for _, want := range []string{"trial 1 (error): harness exited 7", "trial 2 (error): context deadline exceeded"} {
		if !strings.Contains(tc.Error.Body, want) {
			t.Errorf("error body missing %q:\n%s", want, tc.Error.Body)
		}
	}
}
