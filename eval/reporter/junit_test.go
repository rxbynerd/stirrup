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

// TestWriteJUnit_FailMessageFallback pins the I3 message-synthesis
// behaviour: when the judge verdict has no top-level Reason but
// carries sub-judge Details, the <failure message=...> attribute
// must be populated from the first detail rather than left blank.
// CI UIs (mikepenz/action-junit-report, Jenkins) render this
// attribute as the failure headline.
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

// TestWriteJUnit_UnknownOutcome pins the closed-set default branch:
// an outcome value not in {"pass", "fail", "error"} must surface as
// <error type="UnknownOutcome"> and increment the suite Errors count
// so CI renderers don't silently inflate Tests against a placid 0/0
// failures/errors total.
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
	// Raw output must not contain unescaped attribute-breaking quotes.
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

// TestWriteJUnit_ZeroTimestampFallback pins the dry-run-shaped path
// in buildTestSuite where StartedAt/CompletedAt are both zero (so
// wall-clock subtraction would either be zero or panic-adjacent
// nonsense): the suite's Time attribute must be the sum of the
// per-task DurationMs values converted to seconds, and Timestamp
// must be empty rather than the Go zero-time string.
func TestWriteJUnit_ZeroTimestampFallback(t *testing.T) {
	result := eval.SuiteResult{
		SuiteID: "fallback",
		// StartedAt and CompletedAt are intentionally zero.
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

// TestWriteJUnit_BackwardTimestampFallback pins the second branch of
// the fallback: when CompletedAt precedes StartedAt (clock skew, or a
// recording deserialised with mismatched fields), we must not emit a
// negative wall-clock duration. The DurationMs sum is used instead.
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
	// A coarse smoke test: feed mixed outcomes through WriteJUnit and confirm
	// the output is well-formed by re-parsing into a generic map.
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

	// Round-trip via a token-walking decoder to ensure no malformed
	// regions. EOF terminates the loop on success; any other error is
	// a parser-detected malformation and must fail the test — the
	// previous "break on any error" formulation made this assertion a
	// no-op for the very class of bug it claimed to catch.
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
