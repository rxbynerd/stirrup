package reporter

import (
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/rxbynerd/stirrup/eval"
)

// JUnit XML output is documented in docs/eval.md.

type xmlTestSuites struct {
	XMLName    xml.Name       `xml:"testsuites"`
	TestSuites []xmlTestSuite `xml:"testsuite"`
}

type xmlTestSuite struct {
	XMLName   xml.Name      `xml:"testsuite"`
	Name      string        `xml:"name,attr"`
	Tests     int           `xml:"tests,attr"`
	Failures  int           `xml:"failures,attr"`
	Errors    int           `xml:"errors,attr"`
	Time      string        `xml:"time,attr"`
	Timestamp string        `xml:"timestamp,attr"`
	TestCases []xmlTestCase `xml:"testcase"`
}

type xmlTestCase struct {
	XMLName   xml.Name    `xml:"testcase"`
	Name      string      `xml:"name,attr"`
	Classname string      `xml:"classname,attr"`
	Time      string      `xml:"time,attr"`
	Failure   *xmlFailure `xml:"failure,omitempty"`
	Error     *xmlError   `xml:"error,omitempty"`
	SystemOut string      `xml:"system-out,omitempty"`
}

type xmlFailure struct {
	XMLName xml.Name `xml:"failure"`
	Type    string   `xml:"type,attr"`
	Message string   `xml:"message,attr"`
	Body    string   `xml:",chardata"`
}

type xmlError struct {
	XMLName xml.Name `xml:"error"`
	Type    string   `xml:"type,attr"`
	Message string   `xml:"message,attr"`
	Body    string   `xml:",chardata"`
}

// WriteJUnit encodes result as JUnit XML to w. The output begins with a
// standard XML declaration (`<?xml version="1.0" encoding="UTF-8"?>`)
// followed by an indented <testsuites> tree containing exactly one
// <testsuite> per SuiteResult.
func WriteJUnit(w io.Writer, result eval.SuiteResult) error {
	suite := buildTestSuite(result)
	doc := xmlTestSuites{TestSuites: []xmlTestSuite{suite}}

	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}

	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}

	_, err := io.WriteString(w, "\n")
	return err
}

// buildTestSuite converts a SuiteResult into the XML mirror struct.
// Outcome is a closed set {"pass", "fail", "error"}; an unknown value is
// promoted to an error so totals stay consistent rather than silently
// inflating Tests without bumping Failures or Errors.
func buildTestSuite(result eval.SuiteResult) xmlTestSuite {
	failures := 0
	errors := 0
	cases := make([]xmlTestCase, 0, len(result.Tasks))
	for _, t := range result.Tasks {
		switch t.Outcome {
		case "fail":
			failures++
		case "error":
			errors++
		case "pass":

		default:

			errors++
		}
		cases = append(cases, buildTestCase(result.SuiteID, t))
	}

	// Prefer wall-clock CompletedAt-StartedAt; fall back to the sum of
	// per-task durations when unset (e.g. dry-run results) so the field
	// is never negative or nonsensical.
	var suiteSeconds float64
	if !result.CompletedAt.IsZero() && !result.StartedAt.IsZero() && result.CompletedAt.After(result.StartedAt) {
		suiteSeconds = result.CompletedAt.Sub(result.StartedAt).Seconds()
	} else {
		var ms int64
		for _, t := range result.Tasks {
			ms += t.DurationMs
		}
		suiteSeconds = float64(ms) / 1000.0
	}

	timestamp := ""
	if !result.StartedAt.IsZero() {
		timestamp = result.StartedAt.UTC().Format(time.RFC3339)
	}

	return xmlTestSuite{
		Name:      result.SuiteID,
		Tests:     len(result.Tasks),
		Failures:  failures,
		Errors:    errors,
		Time:      formatSeconds(suiteSeconds),
		Timestamp: timestamp,
		TestCases: cases,
	}
}

// buildTestCase converts a TaskResult into an XML <testcase>. A task run
// over several trials stays one testcase: system-out summarises every
// trial, and a failure or error message carries the pass count and each
// non-passing trial's reason.
func buildTestCase(suiteID string, t eval.TaskResult) xmlTestCase {
	tc := xmlTestCase{
		Name:      t.TaskID,
		Classname: suiteID,
		Time:      formatSeconds(float64(t.DurationMs) / 1000.0),
	}

	multiTrial := len(t.Trials) > 1
	if multiTrial {
		tc.SystemOut = trialSummary(t)
	}

	switch t.Outcome {
	case "fail":
		// Fall back to the first sub-judge Reason when the verdict has no
		// top-level Reason, so CI renderers never show an empty headline.
		msg := t.JudgeVerdict.Reason
		if msg == "" && len(t.JudgeVerdict.Details) > 0 {
			msg = t.JudgeVerdict.Details[0].Reason
		}
		body := failureBody(t.JudgeVerdict)
		if multiTrial {
			msg = trialHeadline(t, msg)
			body += "\n\n" + trialReasons(t)
		}
		tc.Failure = &xmlFailure{
			Type:    "EvalFailure",
			Message: msg,
			Body:    body,
		}
	case "error":
		msg, body := t.Error, t.Error
		if multiTrial {
			msg = trialHeadline(t, msg)
			body += "\n\n" + trialReasons(t)
		}
		tc.Error = &xmlError{
			Type:    "HarnessError",
			Message: msg,
			Body:    body,
		}
	case "pass":

	default:
		// Surface as <error> so operators can grep for "UnknownOutcome".
		msg := fmt.Sprintf("unknown task outcome %q", t.Outcome)
		tc.Error = &xmlError{
			Type:    "UnknownOutcome",
			Message: msg,
			Body:    msg,
		}
	}

	return tc
}

// failureBody assembles the <failure> body text. The judge verdict reason
// comes first; when sub-judge details are present, a blank line separates
// them and each detail is rendered as `Type: Reason`.
func failureBody(v eval.JudgeVerdict) string {
	if len(v.Details) == 0 {
		return v.Reason
	}
	var b strings.Builder
	if v.Reason != "" {
		b.WriteString(v.Reason)
		b.WriteString("\n\n")
	}
	for i, d := range v.Details {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s: %s", d.Type, d.Reason)
	}
	return b.String()
}

func trialHeadline(t eval.TaskResult, msg string) string {
	c := t.Counts()
	return fmt.Sprintf("%d/%d trials passed: %s", c.Pass, c.Total(), msg)
}

func trialSummary(t eval.TaskResult) string {
	c := t.Counts()
	var b strings.Builder
	fmt.Fprintf(&b, "pass fraction %.3f (%d/%d trials passed, %d failed, %d errored)",
		c.PassFraction(), c.Pass, c.Total(), c.Fail, c.Error)
	for _, tr := range t.Trials {
		fmt.Fprintf(&b, "\ntrial %d: %s (%ss, %d turns)", tr.Trial, tr.Outcome,
			formatSeconds(float64(tr.DurationMs)/1000.0), tr.Turns)
		if tr.Outcome != "pass" {
			fmt.Fprintf(&b, ": %s", trialReason(tr))
		}
	}
	return b.String()
}

func trialReasons(t eval.TaskResult) string {
	var b strings.Builder
	b.WriteString("Non-passing trials:")
	for _, tr := range t.Trials {
		if tr.Outcome == "pass" {
			continue
		}
		fmt.Fprintf(&b, "\ntrial %d (%s): %s", tr.Trial, tr.Outcome, trialReason(tr))
	}
	return b.String()
}

func trialReason(tr eval.TrialResult) string {
	if tr.Outcome == "fail" {
		if tr.JudgeVerdict.Reason != "" {
			return tr.JudgeVerdict.Reason
		}
		if len(tr.JudgeVerdict.Details) > 0 {
			return tr.JudgeVerdict.Details[0].Reason
		}
	}
	if tr.Error != "" {
		return tr.Error
	}
	return tr.JudgeVerdict.Reason
}

// formatSeconds renders a duration in seconds with three decimal places —
// the precision JUnit consumers (GitHub Actions, mikepenz/action-junit-report,
// etc.) expect. strconv.FormatFloat is preferred over fmt.Sprintf to avoid
// the format-string allocation on the hot path.
func formatSeconds(s float64) string {
	return strconv.FormatFloat(s, 'f', 3, 64)
}
