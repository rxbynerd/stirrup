package main

import (
	"bytes"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	dir := t.TempDir()
	path := filepath.Join(dir, "junit.xml")

	if err := writeJUnit(path, sampleResult()); err != nil {
		t.Fatalf("writeJUnit: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading emitted XML: %v", err)
	}

	if !strings.HasPrefix(string(data), `<?xml version="1.0" encoding="UTF-8"?>`) {
		t.Fatalf("missing XML header; first 64 bytes: %q", string(data[:min(len(data), 64)]))
	}

	var doc junitDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshalling emitted XML: %v\n--- output ---\n%s", err, string(data))
	}
	if len(doc.TestSuites) != 1 {
		t.Fatalf("want 1 testsuite, got %d", len(doc.TestSuites))
	}
	suite := doc.TestSuites[0]
	if suite.Name != "convert-suite" || suite.Tests != 2 || suite.Failures != 1 || suite.Errors != 0 {
		t.Errorf("suite attrs unexpected: %+v", suite)
	}

	// File permissions must be a stable 0o644 regardless of the
	// process umask — CI matrix runners ship with a wide range of
	// umasks (e.g. 0077 on hardened images) and a JUnit artifact that
	// the upload step cannot read would silently disappear.
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := stat.Mode().Perm(); perm != 0o644 {
		t.Errorf("file mode = %o, want %o", perm, 0o644)
	}
}

// TestWriteJUnit_BadPath confirms errors propagate when the target
// directory does not exist. cmdRun / cmdConvert surface this through
// log.Fatalf, but we want unit-level coverage that an error is returned.
func TestWriteJUnit_BadPath(t *testing.T) {
	err := writeJUnit("/nonexistent/dir/junit.xml", sampleResult())
	if err == nil {
		t.Fatal("expected error for unwritable path")
	}
}

// TestConvertRoundTrip simulates `eval run --output dir` followed by
// `eval convert --from result.json --to-junit junit.xml`: JSON → in-memory
// SuiteResult → JUnit XML must preserve task counts and outcomes.
func TestConvertRoundTrip(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "result.json")
	xmlPath := filepath.Join(dir, "junit.xml")

	original := sampleResult()
	original.Tasks = append(original.Tasks, eval.TaskResult{
		TaskID: "boom", Outcome: "error", DurationMs: 10, Error: "harness exec failed",
	})

	if err := writeJSON(jsonPath, original); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}

	loaded, err := loadResult(jsonPath)
	if err != nil {
		t.Fatalf("loadResult: %v", err)
	}

	if err := writeJUnit(xmlPath, loaded); err != nil {
		t.Fatalf("writeJUnit: %v", err)
	}

	data, err := os.ReadFile(xmlPath)
	if err != nil {
		t.Fatalf("reading XML: %v", err)
	}
	var doc junitDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parsing XML: %v", err)
	}
	if len(doc.TestSuites) != 1 {
		t.Fatalf("want 1 testsuite, got %d", len(doc.TestSuites))
	}
	suite := doc.TestSuites[0]
	if suite.Tests != 3 || suite.Failures != 1 || suite.Errors != 1 {
		t.Errorf("counts unexpected: tests=%d failures=%d errors=%d (want 3/1/1)",
			suite.Tests, suite.Failures, suite.Errors)
	}
	if len(suite.TestCases) != 3 {
		t.Fatalf("want 3 testcases, got %d", len(suite.TestCases))
	}

	var passCase, failCase, errCase = false, false, false
	for _, tc := range suite.TestCases {
		switch tc.Name {
		case "ok":
			if tc.Failure != nil || tc.Error != nil {
				t.Errorf("pass case has unexpected children: %+v", tc)
			}
			passCase = true
		case "bad":
			if tc.Failure == nil || tc.Failure.Type != "EvalFailure" {
				t.Errorf("fail case missing or wrong <failure>: %+v", tc.Failure)
			}
			failCase = true
		case "boom":
			if tc.Error == nil || tc.Error.Type != "HarnessError" {
				t.Errorf("error case missing or wrong <error>: %+v", tc.Error)
			}
			if tc.Error != nil && tc.Error.Message != "harness exec failed" {
				t.Errorf("error message = %q, want harness error", tc.Error.Message)
			}
			errCase = true
		}
	}
	if !passCase || !failCase || !errCase {
		t.Errorf("missing one of pass/fail/error cases: pass=%v fail=%v err=%v",
			passCase, failCase, errCase)
	}
}

// TestConvert_LoadResultRejectsBadJSON pins that cmdConvert's loadResult
// step surfaces a useful error rather than crashing on malformed input.
// The CLI itself uses log.Fatalf; we exercise the helper directly.
func TestConvert_LoadResultRejectsBadJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadResult(path)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "parsing result JSON") {
		t.Errorf("error = %q, want it to mention parsing", err.Error())
	}
}

// TestConvert_LoadResultMissingFile covers the os.ReadFile failure
// branch of loadResult. The error shape differs from the bad-JSON
// branch — it surfaces *PathError directly rather than the
// "parsing result JSON" wrapper — and a refactor that accidentally
// wrapped both branches the same way would lose information about
// "file not found" vs "file present but corrupt".
func TestConvert_LoadResultMissingFile(t *testing.T) {
	_, err := loadResult("/nonexistent/path/result.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if strings.Contains(err.Error(), "parsing result JSON") {
		t.Errorf("error = %q, missing-file path must not be wrapped as a parse error", err.Error())
	}
}

// TestRun_ConvertDispatch exercises the `convert` arm of run()'s
// switch end-to-end: it writes a result.json with writeJSON, calls
// run([]string{"convert", "--from", ..., "--to-junit", ...}), and
// asserts the JUnit XML lands at the requested path with the
// expected header. Without this test, a refactor that miswired the
// "convert" case (e.g. dispatching to cmdCompare) would have no
// signal — the convert-side helpers (writeJUnit, loadResult) are
// well-covered, but the run() dispatcher itself was untested.
//
// The two required-flag check paths inside cmdConvert
// (`-from is required`, `-to-junit is required`) call log.Fatal,
// which exits the test process. They are not unit-tested here;
// covering them needs subprocess testing or a fatal-interceptor
// shim, neither of which is justified for guard clauses this small.
func TestRun_ConvertDispatch(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "result.json")
	xmlPath := filepath.Join(dir, "junit.xml")

	if err := writeJSON(jsonPath, sampleResult()); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}

	var stdout bytes.Buffer
	code := run([]string{"convert", "--from", jsonPath, "--to-junit", xmlPath}, &stdout)
	if code != 0 {
		t.Fatalf("run convert exit code = %d, want 0", code)
	}

	data, err := os.ReadFile(xmlPath)
	if err != nil {
		t.Fatalf("reading emitted XML: %v", err)
	}
	if !strings.HasPrefix(string(data), `<?xml version="1.0" encoding="UTF-8"?>`) {
		t.Fatalf("missing XML header; first 64 bytes: %q", string(data[:min(len(data), 64)]))
	}
}

// TestCmdRun_JUnitFlag drives the `run` arm of run()'s switch with
// --dry-run + --junit and asserts the JUnit XML is created. Dry-run
// mode short-circuits the harness binary requirement (see
// runner.RunSuite), so this test works on a bare-bones runner and
// has no external dependencies beyond a fixture suite.
//
// The assertions are deliberately coarse: this is a wiring test for
// the `*junitPath != ""` guard, the writeJUnit call inside cmdRun,
// and (post-B2) the warning-not-fatal behaviour. Per-suite content
// shape is covered by reporter tests; per-helper file shape is
// covered by TestWriteJUnit_CreatesFile. Here we only need to know
// the flag value reached writeJUnit.
func TestCmdRun_JUnitFlag(t *testing.T) {
	dir := t.TempDir()
	outputDir := filepath.Join(dir, "results")
	xmlPath := filepath.Join(dir, "junit.xml")

	// The fixture is checked in under testdata/ so the test does not
	// depend on the precise HCL grammar — if the grammar changes,
	// the fixture is updated alongside it.
	suitePath, err := filepath.Abs(filepath.Join("testdata", "minimal_suite.hcl"))
	if err != nil {
		t.Fatalf("resolving fixture: %v", err)
	}

	var stdout bytes.Buffer
	code := run([]string{
		"run",
		"--suite", suitePath,
		"--dry-run",
		"--output", outputDir,
		"--junit", xmlPath,
	}, &stdout)
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0", code)
	}

	data, err := os.ReadFile(xmlPath)
	if err != nil {
		t.Fatalf("reading emitted XML: %v", err)
	}
	if !strings.HasPrefix(string(data), `<?xml version="1.0" encoding="UTF-8"?>`) {
		t.Fatalf("missing XML header; first 64 bytes: %q", string(data[:min(len(data), 64)]))
	}
}
