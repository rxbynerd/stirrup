package main

import (
	"bytes"
	"encoding/xml"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval"
)

// --- writeJUnit / convert subcommand tests ---
//
// cmdRun / cmdConvert call log.Fatal on error and consume os.Args, so we
// drive the JUnit-side behaviour through the writeJUnit helper plus the
// loadResult / writeJSON pair that cmdConvert composes. The helper is
// the only place where file I/O for JUnit lives, so testing it gives us
// the same coverage as a binary-shelling test without forking.

// junitDoc mirrors enough of the JUnit XML to assert structural facts
// without leaking reporter package internals into this file.
type junitDoc struct {
	XMLName    xml.Name `xml:"testsuites"`
	TestSuites []struct {
		Name      string `xml:"name,attr"`
		Tests     int    `xml:"tests,attr"`
		Failures  int    `xml:"failures,attr"`
		Errors    int    `xml:"errors,attr"`
		TestCases []struct {
			Name    string `xml:"name,attr"`
			Failure *struct {
				Type    string `xml:"type,attr"`
				Message string `xml:"message,attr"`
			} `xml:"failure"`
			Error *struct {
				Type    string `xml:"type,attr"`
				Message string `xml:"message,attr"`
			} `xml:"error"`
		} `xml:"testcase"`
	} `xml:"testsuite"`
}

const junitXMLHeader = `<?xml version="1.0" encoding="UTF-8"?>`

// requireJUnitHeader asserts that data begins with the XML declaration the
// reporter emits.
func requireJUnitHeader(t *testing.T, data []byte) {
	t.Helper()
	must := require.New(t)
	got := string(data[:min(len(data), len(junitXMLHeader))])
	must.Equal(junitXMLHeader, got, "XML header")
}

func sampleResult() eval.SuiteResult {
	started := time.Date(2026, 5, 9, 9, 0, 0, 0, time.UTC)
	return eval.SuiteResult{
		SuiteID:     "convert-suite",
		RunID:       "run-1",
		StartedAt:   started,
		CompletedAt: started.Add(3 * time.Second),
		PassRate:    0.5,
		Tasks: []eval.TaskResult{
			{TaskID: "ok", Outcome: "pass", DurationMs: 100},
			{TaskID: "bad", Outcome: "fail", DurationMs: 250,
				JudgeVerdict: eval.JudgeVerdict{Reason: "criterion not met"}},
		},
	}
}

// TestWriteJUnit_CreatesFile validates the helper that backs both --junit
// on `eval run` and --to-junit on `eval convert`: a file is created at the
// requested path with well-formed JUnit XML inside.
func TestWriteJUnit_CreatesFile(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "junit.xml")

	must.NoError(writeJUnit(path, sampleResult()))

	data, err := os.ReadFile(path)
	must.NoError(err)
	requireJUnitHeader(t, data)

	var doc junitDoc
	must.NoError(xml.Unmarshal(data, &doc), "output:\n%s", data)
	must.Len(doc.TestSuites, 1)
	suite := doc.TestSuites[0]
	must.Equal("convert-suite", suite.Name)
	must.Equal(2, suite.Tests)
	must.Equal(1, suite.Failures)
	must.Equal(0, suite.Errors)

	// The mode is fixed at 0o644 regardless of the process umask so that
	// CI upload steps can always read the artifact.
	stat, err := os.Stat(path)
	must.NoError(err)
	perm := stat.Mode().Perm()
	must.Equal(os.FileMode(0o644), perm, "file mode %o", perm)
}

// TestWriteJUnit_ErrorsOnMissingDirectory confirms errors propagate when the
// target directory does not exist. cmdRun / cmdConvert surface this through
// log.Fatalf, but we want unit-level coverage that an error is returned.
func TestWriteJUnit_ErrorsOnMissingDirectory(t *testing.T) {
	must := require.New(t)
	err := writeJUnit("/nonexistent/dir/junit.xml", sampleResult())
	must.Error(err)
}

// TestConvert_JSONToJUnitPreservesOutcomes simulates `eval run --output dir`
// followed by `eval convert --from result.json --to-junit junit.xml`: JSON →
// in-memory SuiteResult → JUnit XML must preserve task counts and outcomes.
func TestConvert_JSONToJUnitPreservesOutcomes(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "result.json")
	xmlPath := filepath.Join(dir, "junit.xml")

	original := sampleResult()
	original.Tasks = append(original.Tasks, eval.TaskResult{
		TaskID: "boom", Outcome: "error", DurationMs: 10, Error: "harness exec failed",
	})

	must.NoError(writeJSON(jsonPath, original))

	loaded, err := loadResult(jsonPath)
	must.NoError(err)

	must.NoError(writeJUnit(xmlPath, loaded))

	data, err := os.ReadFile(xmlPath)
	must.NoError(err)
	var doc junitDoc
	must.NoError(xml.Unmarshal(data, &doc))
	must.Len(doc.TestSuites, 1)
	suite := doc.TestSuites[0]
	must.Equal(3, suite.Tests)
	must.Equal(1, suite.Failures)
	must.Equal(1, suite.Errors)
	must.Len(suite.TestCases, 3)

	names := make([]string, 0, len(suite.TestCases))
	for _, tc := range suite.TestCases {
		names = append(names, tc.Name)
		switch tc.Name {
		case "ok":
			must.Nil(tc.Failure, "pass case has a failure child")
			must.Nil(tc.Error, "pass case has an error child")
		case "bad":
			must.NotNil(tc.Failure, "fail case has no failure child")
			must.Equal("EvalFailure", tc.Failure.Type)
		case "boom":
			must.NotNil(tc.Error, "error case has no error child")
			must.Equal("HarnessError", tc.Error.Type)
			must.Equal("harness exec failed", tc.Error.Message)
		}
	}
	must.ElementsMatch([]string{"ok", "bad", "boom"}, names)
}

// TestConvert_LoadResultRejectsBadJSON pins that cmdConvert's loadResult
// step surfaces a useful error rather than crashing on malformed input.
// The CLI itself uses log.Fatalf; we exercise the helper directly.
func TestConvert_LoadResultRejectsBadJSON(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "broken.json")
	must.NoError(os.WriteFile(path, []byte("{not json"), 0o644))

	_, err := loadResult(path)
	must.ErrorContains(err, "parsing result JSON")
}

// TestConvert_LoadResultMissingFile covers the os.ReadFile failure
// branch of loadResult. The error shape differs from the bad-JSON
// branch — it surfaces *PathError directly rather than the
// "parsing result JSON" wrapper — and a refactor that accidentally
// wrapped both branches the same way would lose information about
// "file not found" vs "file present but corrupt".
func TestConvert_LoadResultMissingFile(t *testing.T) {
	must := require.New(t)
	_, err := loadResult("/nonexistent/path/result.json")
	must.Error(err)
	must.NotContains(err.Error(), "parsing result JSON",
		"missing-file path must not be wrapped as a parse error")
}

// TestRun_ConvertSubcommandWritesJUnit drives the `convert` arm of run()'s
// switch end to end: a result.json written with writeJSON is converted to
// JUnit XML at the requested path.
//
// The required-flag guards in cmdConvert call log.Fatal, which exits the
// test process, so they are not covered here.
func TestRun_ConvertSubcommandWritesJUnit(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "result.json")
	xmlPath := filepath.Join(dir, "junit.xml")

	must.NoError(writeJSON(jsonPath, sampleResult()))

	var stdout bytes.Buffer
	code := run([]string{"convert", "--from", jsonPath, "--to-junit", xmlPath}, &stdout)
	must.Equal(0, code, "run convert exit code")

	data, err := os.ReadFile(xmlPath)
	must.NoError(err)
	requireJUnitHeader(t, data)
}

// TestCmdRun_JUnitFlag drives the `run` arm of run()'s switch with
// --junit against a fake harness and asserts the JUnit XML is
// created. The assertions are deliberately coarse: this is a wiring test
// for the `*junitPath != ""` guard and the writeJUnit call inside cmdRun;
// per-suite content shape is covered by reporter tests.
func TestCmdRun_JUnitFlag(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "results")
	xmlPath := filepath.Join(dir, "junit.xml")

	harnessPath := writeFakeHarness(t, `#!/bin/sh
shift
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$TRACE" ]; then
  echo '{"id":"run-1","turns":1,"cost":0.0,"outcome":"success"}' > "$TRACE"
fi
`)

	// The fixture lives under testdata/ so the test does not depend on the
	// HCL grammar. Update it alongside any grammar change.
	suitePath, err := filepath.Abs(filepath.Join("testdata", "minimal_suite.hcl"))
	must.NoError(err)

	var stdout bytes.Buffer
	code := run([]string{
		"run",
		"--suite", suitePath,
		"--harness", harnessPath,
		"--output", outputDir,
		"--junit", xmlPath,
	}, &stdout)
	must.Equal(0, code, "run exit code")

	data, err := os.ReadFile(xmlPath)
	must.NoError(err)
	requireJUnitHeader(t, data)
}
