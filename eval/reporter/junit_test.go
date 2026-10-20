package reporter

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval"
)

// parseJUnit decodes the JUnit XML produced by WriteJUnit into the same
// mirror structs used for emission. Tests use this to assert structural
// invariants without re-implementing an XML parser.
func parseJUnit(t *testing.T, b []byte) xmlTestSuites {
	t.Helper()
	must := require.New(t)
	var got xmlTestSuites
	err := xml.Unmarshal(b, &got)
	must.NoError(err, "parsing emitted XML\n--- output ---\n%s", string(b))
	return got
}

// runWriteJUnit is a small helper to keep tests focused on assertions.
func runWriteJUnit(t *testing.T, result eval.SuiteResult) []byte {
	t.Helper()
	must := require.New(t)
	var buf bytes.Buffer
	must.NoError(WriteJUnit(&buf, result))
	return buf.Bytes()
}

func TestWriteJUnit_HeaderAndRoot(t *testing.T) {
	must := require.New(t)
	result := eval.SuiteResult{SuiteID: "s1"}
	out := runWriteJUnit(t, result)

	preview := out
	if len(preview) > 64 {
		preview = preview[:64]
	}
	must.True(bytes.HasPrefix(out, []byte(xml.Header)),
		"output should start with XML header %q, got %q", xml.Header, string(preview))

	doc := parseJUnit(t, out)
	must.Len(doc.TestSuites, 1, "want exactly one <testsuite>")
}

func TestWriteJUnit_CountsAndAttributes(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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

	check.Equal("demo", suite.Name)
	check.Equal(4, suite.Tests)
	check.Equal(1, suite.Failures)
	check.Equal(1, suite.Errors)
	check.Equal("2.500", suite.Time, "time uses three decimal places")
	check.Equal("2026-05-09T12:00:00Z", suite.Timestamp, "timestamp is RFC 3339 UTC")
	must.Len(suite.TestCases, 4)
}

func TestWriteJUnit_TestCaseTimeIsSeconds(t *testing.T) {
	check := assert.New(t)
	result := eval.SuiteResult{
		SuiteID: "s",
		Tasks: []eval.TaskResult{
			{TaskID: "t", Outcome: "pass", DurationMs: 1500},
		},
	}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	tc := doc.TestSuites[0].TestCases[0]
	check.Equal("1.500", tc.Time)
	check.Equal("s", tc.Classname, "classname is the suite ID")
}

func TestWriteJUnit_PassHasNoChildren(t *testing.T) {
	check := assert.New(t)
	result := eval.SuiteResult{
		SuiteID: "s",
		Tasks: []eval.TaskResult{
			{TaskID: "p", Outcome: "pass", DurationMs: 10},
		},
	}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	tc := doc.TestSuites[0].TestCases[0]
	check.Nil(tc.Failure, "a passing case has no <failure>")
	check.Nil(tc.Error, "a passing case has no <error>")
}

func TestWriteJUnit_FailEmitsFailure(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NotNil(tc.Failure, "a failing case has a <failure>")
	check.Nil(tc.Error, "a failing case has no <error>")
	check.Equal("EvalFailure", tc.Failure.Type)
	check.Equal("judge said no", tc.Failure.Message)
	check.Contains(tc.Failure.Body, "judge said no")
	check.Contains(tc.Failure.Body, "test-command: exit code 1")
	check.Contains(tc.Failure.Body, "file-exists: missing /tmp/expected")
}

func TestWriteJUnit_FailRendersSkippedDetails(t *testing.T) {
	must := require.New(t)
	result := eval.SuiteResult{
		SuiteID: "s",
		Tasks: []eval.TaskResult{
			{
				TaskID:  "t",
				Outcome: "fail",
				JudgeVerdict: eval.JudgeVerdict{
					Status: "fail",
					Reason: "sub-judge 1 of 2 (file-exists) failed (require all); 1 skipped",
					Details: []eval.JudgeDetail{
						{Type: "file-exists", Status: "fail", Reason: "missing paths: a.txt"},
						{Type: "diff-review", Status: eval.JudgeStatusSkipped, Reason: "not evaluated: sub-judge 1 of 2 had already decided"},
					},
				},
			},
		},
	}
	tc := parseJUnit(t, runWriteJUnit(t, result)).TestSuites[0].TestCases[0]
	must.NotNil(tc.Failure, "a failing case has a <failure>")
	want := "file-exists: missing paths: a.txt\n" +
		"diff-review: skipped (not evaluated: sub-judge 1 of 2 had already decided)"
	must.True(strings.HasSuffix(tc.Failure.Body, want),
		"failure body = %q, want it to end with %q", tc.Failure.Body, want)
}

func TestWriteJUnit_FailRendersShadowVerdicts(t *testing.T) {
	must := require.New(t)
	result := eval.SuiteResult{
		SuiteID: "s",
		Tasks: []eval.TaskResult{
			{
				TaskID:  "t",
				Outcome: "fail",
				JudgeVerdict: eval.JudgeVerdict{
					Status: "fail",
					Reason: "sub-judge 1 of 2 (file-exists) failed (require all); 1 shadow recorded",
					Details: []eval.JudgeDetail{
						{Type: "file-exists", Status: "fail", Reason: "missing paths: a.txt"},
						{Type: "diff-review", Status: eval.JudgeStatusShadow, ShadowVerdict: "pass", Reason: "decision model: p(pass)=0.91 confidence=0.82"},
					},
				},
			},
		},
	}
	tc := parseJUnit(t, runWriteJUnit(t, result)).TestSuites[0].TestCases[0]
	must.NotNil(tc.Failure, "a failing case has a <failure>")
	want := "diff-review: shadow pass (decision model: p(pass)=0.91 confidence=0.82)"
	must.True(strings.HasSuffix(tc.Failure.Body, want),
		"failure body = %q, want it to end with %q", tc.Failure.Body, want)
}

func TestWriteJUnit_PassRendersShadowVerdictsInSystemOut(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	shadowed := eval.TaskResult{
		TaskID:  "shadowed",
		Outcome: "pass",
		JudgeVerdict: eval.JudgeVerdict{
			Passed: true,
			Status: "pass",
			Reason: "all 1 sub-judge passed; 1 shadow recorded",
			Details: []eval.JudgeDetail{
				{Type: "file-exists", Passed: true, Status: "pass", Reason: "all paths exist"},
				{Type: "diff-review", Status: eval.JudgeStatusShadow, ShadowVerdict: "fail", Reason: "decision model: p(pass)=0.12 confidence=0.70"},
			},
		},
	}
	plain := eval.TaskResult{
		TaskID:  "plain",
		Outcome: "pass",
		JudgeVerdict: eval.JudgeVerdict{Passed: true, Status: "pass", Reason: "all 1 sub-judge passed", Details: []eval.JudgeDetail{
			{Type: "file-exists", Passed: true, Status: "pass", Reason: "all paths exist"},
		}},
	}
	cases := parseJUnit(t, runWriteJUnit(t, eval.SuiteResult{SuiteID: "s", Tasks: []eval.TaskResult{shadowed, plain}})).TestSuites[0].TestCases

	must.Nil(cases[0].Failure, "a passing task has no <failure>")
	must.Nil(cases[0].Error, "a passing task has no <error>")
	want := "diff-review: shadow fail (decision model: p(pass)=0.12 confidence=0.70)"
	check.True(strings.HasPrefix(cases[0].SystemOut, shadowed.JudgeVerdict.Reason),
		"system-out = %q, want it to start with the reason %q", cases[0].SystemOut, shadowed.JudgeVerdict.Reason)
	check.True(strings.HasSuffix(cases[0].SystemOut, want),
		"system-out = %q, want it to end with the shadow verdict %q", cases[0].SystemOut, want)
	check.Empty(cases[1].SystemOut, "a passing task without shadows has no system-out")
}

func TestWriteJUnit_ErrorCompositeEmitsDetails(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	reason := "sub-judge 2 of 3 (diff-review) errored (require all); 1 skipped: provider returned HTTP 503"
	result := eval.SuiteResult{
		SuiteID: "s",
		Tasks: []eval.TaskResult{
			{
				TaskID:  "t",
				Outcome: "error",
				Error:   reason,
				JudgeVerdict: eval.JudgeVerdict{
					Status: "error",
					Reason: reason,
					Details: []eval.JudgeDetail{
						{Type: "file-exists", Status: "pass", Passed: true, Reason: "all paths exist"},
						{Type: "diff-review", Status: "error", Reason: "provider returned HTTP 503"},
						{Type: "test-command", Status: eval.JudgeStatusSkipped, Reason: "not evaluated: sub-judge 2 of 3 had already decided"},
					},
				},
			},
		},
	}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	suite := doc.TestSuites[0]
	check.Equal(1, suite.Errors)
	check.Equal(0, suite.Failures)

	tc := suite.TestCases[0]
	must.NotNil(tc.Error, "want an <error>")
	must.Nil(tc.Failure, "want no <failure>")
	check.Equal(reason, tc.Error.Message)
	want := reason + "\n\n" +
		"file-exists: all paths exist\n" +
		"diff-review: provider returned HTTP 503\n" +
		"test-command: skipped (not evaluated: sub-judge 2 of 3 had already decided)"
	check.Equal(want, tc.Error.Body)
}

// TestWriteJUnit_FailMessageFallback pins that when the judge verdict has
// no top-level Reason but carries sub-judge Details, the <failure
// message=...> attribute is populated from the first detail.
func TestWriteJUnit_FailMessageFallback(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NotNil(tc.Failure, "a failing case has a <failure>")
	check.Equal("exit code 1", tc.Failure.Message, "message falls back to the first detail reason")
}

// TestWriteJUnit_UnknownOutcome pins that an outcome value not in
// {"pass", "fail", "error"} surfaces as <error type="UnknownOutcome"> and
// increments the suite Errors count.
func TestWriteJUnit_UnknownOutcome(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	result := eval.SuiteResult{
		SuiteID: "s",
		Tasks: []eval.TaskResult{
			{TaskID: "skipped-task", Outcome: "skipped"},
		},
	}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	suite := doc.TestSuites[0]
	check.Equal(1, suite.Errors)

	tc := suite.TestCases[0]
	must.NotNil(tc.Error, "an unknown outcome has an <error>")
	check.Equal("UnknownOutcome", tc.Error.Type)
	check.Contains(tc.Error.Message, "skipped", "the message names the unknown outcome")
}

func TestWriteJUnit_ErrorEmitsError(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	result := eval.SuiteResult{
		SuiteID: "s",
		Tasks: []eval.TaskResult{
			{TaskID: "boom", Outcome: "error", Error: "exec: harness binary missing"},
		},
	}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	tc := doc.TestSuites[0].TestCases[0]
	must.NotNil(tc.Error, "an error case has an <error>")
	check.Nil(tc.Failure, "an error case has no <failure>")
	check.Equal("HarnessError", tc.Error.Type)
	check.Equal("exec: harness binary missing", tc.Error.Message)
}

func TestWriteJUnit_EmptySuite(t *testing.T) {
	check := assert.New(t)
	result := eval.SuiteResult{SuiteID: "empty"}
	out := runWriteJUnit(t, result)
	doc := parseJUnit(t, out)
	suite := doc.TestSuites[0]
	check.Zero(suite.Tests)
	check.Zero(suite.Failures)
	check.Zero(suite.Errors)
	check.Empty(suite.TestCases)
	check.Equal("empty", suite.Name)
}

// TestWriteJUnit_XMLEscaping pins that we delegate escaping to encoding/xml
// rather than concatenating strings. Task IDs containing XML metacharacters
// must round-trip unchanged through Unmarshal.
func TestWriteJUnit_XMLEscaping(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	metachars := `<>&"'`
	result := eval.SuiteResult{
		SuiteID: metachars,
		Tasks: []eval.TaskResult{
			{
				TaskID:  metachars,
				Outcome: "fail",
				JudgeVerdict: eval.JudgeVerdict{
					Reason: "needs <escape> & \"quotes\"",
				},
			},
		},
	}
	out := runWriteJUnit(t, result)
	must.NotContains(string(out), `name="<>&"'"`, "attribute quoting must be escaped")

	doc := parseJUnit(t, out)
	tc := doc.TestSuites[0].TestCases[0]
	check.Equal(metachars, tc.Name, "task name round-trips")
	check.Equal(metachars, doc.TestSuites[0].Name, "suite name round-trips")
	must.NotNil(tc.Failure, "a failing case has a <failure>")
	check.Contains(tc.Failure.Body, `needs <escape> & "quotes"`)
}

// TestWriteJUnit_ZeroTimestampFallback pins that with StartedAt/CompletedAt
// both zero, the suite's Time attribute is the sum of per-task DurationMs
// converted to seconds, and Timestamp is empty rather than the Go zero-time
// string.
func TestWriteJUnit_ZeroTimestampFallback(t *testing.T) {
	check := assert.New(t)
	result := eval.SuiteResult{
		SuiteID: "fallback",
		Tasks: []eval.TaskResult{
			{TaskID: "t1", Outcome: "pass", DurationMs: 1200},
			{TaskID: "t2", Outcome: "pass", DurationMs: 300},
		},
	}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	suite := doc.TestSuites[0]
	check.Equal("1.500", suite.Time, "time is the sum of DurationMs in seconds")
	check.Empty(suite.Timestamp, "timestamp is empty when StartedAt is zero")
}

// TestWriteJUnit_BackwardTimestampFallback pins that when CompletedAt
// precedes StartedAt, a negative wall-clock duration is not emitted; the
// DurationMs sum is used instead.
func TestWriteJUnit_BackwardTimestampFallback(t *testing.T) {
	must := require.New(t)
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
	must.Equal("2.000", suite.Time, "time is the DurationMs sum, not the negative wall-clock span")
}

func TestWriteJUnit_ProducesParseableXML(t *testing.T) {
	must := require.New(t)
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
		must.NoError(err, "malformed XML token\n--- output ---\n%s", out)
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
	must := require.New(t)
	got := string(runWriteJUnit(t, singleRunJUnitFixture()))
	must.Equal(singleRunJUnitGolden, got)
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
	must := require.New(t)
	check := assert.New(t)
	result := eval.SuiteResult{SuiteID: "s", Trials: 3, Tasks: []eval.TaskResult{
		multiTrialTask("flaky",
			eval.TrialResult{Trial: 1, Outcome: "pass", DurationMs: 1000, Turns: 4, JudgeVerdict: eval.JudgeVerdict{Passed: true, Reason: "command exited 0"}},
			eval.TrialResult{Trial: 2, Outcome: "fail", DurationMs: 2000, Turns: 6, JudgeVerdict: eval.JudgeVerdict{Reason: "command exited 1"}},
			eval.TrialResult{Trial: 3, Outcome: "pass", DurationMs: 1500, Turns: 5, JudgeVerdict: eval.JudgeVerdict{Passed: true, Reason: "command exited 0"}},
		),
	}}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	suite := doc.TestSuites[0]
	must.Equal(1, suite.Tests, "want one testcase")
	must.Zero(suite.Failures, "a majority-pass task is not a failure")
	must.Len(suite.TestCases, 1)

	tc := suite.TestCases[0]
	check.Nil(tc.Failure, "a majority-pass task has no <failure>")
	check.Nil(tc.Error, "a majority-pass task has no <error>")
	check.Equal("4.500", tc.Time, "time is the summed trial durations")
	for _, want := range []string{
		"pass fraction 0.667 (2/3 trials passed, 1 failed, 0 errored)",
		"trial 1: pass (1.000s, 4 turns)",
		"trial 2: fail (2.000s, 6 turns): command exited 1",
		"trial 3: pass (1.500s, 5 turns)",
	} {
		check.Contains(tc.SystemOut, want)
	}
}

func TestWriteJUnit_MultiTrialFailListsTrialReasons(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	result := eval.SuiteResult{SuiteID: "s", Trials: 3, Tasks: []eval.TaskResult{
		multiTrialTask("broken",
			eval.TrialResult{Trial: 1, Outcome: "fail", JudgeVerdict: eval.JudgeVerdict{Reason: "missing out.txt"}},
			eval.TrialResult{Trial: 2, Outcome: "pass", JudgeVerdict: eval.JudgeVerdict{Passed: true, Reason: "ok"}},
			eval.TrialResult{Trial: 3, Outcome: "fail", JudgeVerdict: eval.JudgeVerdict{Details: []eval.JudgeDetail{{Type: "test-command", Reason: "exit code 2"}}}},
		),
	}}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	suite := doc.TestSuites[0]
	must.Equal(1, suite.Failures, "want one failure")

	tc := suite.TestCases[0]
	must.NotNil(tc.Failure, "want a <failure>")
	check.Equal("1/3 trials passed: missing out.txt", tc.Failure.Message)
	for _, want := range []string{"Non-passing trials:", "trial 1 (fail): missing out.txt", "trial 3 (fail): exit code 2"} {
		check.Contains(tc.Failure.Body, want)
	}
	check.NotContains(tc.Failure.Body, "trial 2", "the failure body lists only non-passing trials")
}

func TestWriteJUnit_MultiTrialErrorListsTrialErrors(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	result := eval.SuiteResult{SuiteID: "s", Trials: 3, Tasks: []eval.TaskResult{
		multiTrialTask("infra",
			eval.TrialResult{Trial: 1, Outcome: "error", Error: "harness exited 7"},
			eval.TrialResult{Trial: 2, Outcome: "error", Error: "context deadline exceeded"},
			eval.TrialResult{Trial: 3, Outcome: "pass", JudgeVerdict: eval.JudgeVerdict{Passed: true}},
		),
	}}
	doc := parseJUnit(t, runWriteJUnit(t, result))
	suite := doc.TestSuites[0]
	must.Equal(1, suite.Errors, "want one error")

	tc := suite.TestCases[0]
	must.NotNil(tc.Error, "want an <error>")
	check.Equal("1/3 trials passed: harness exited 7", tc.Error.Message)
	for _, want := range []string{"trial 1 (error): harness exited 7", "trial 2 (error): context deadline exceeded"} {
		check.Contains(tc.Error.Body, want)
	}
}
