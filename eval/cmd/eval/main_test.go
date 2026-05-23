package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/eval/lakehouse"
	"github.com/rxbynerd/stirrup/types"
)

// --- run() dispatch tests ---

// TestRun_Version exercises the --version short-circuit through run() so we
// don't need to shell out to a built binary or fight global state. Each
// accepted spelling (--version / -v / version) must exit 0 and print to
// stdout.
func TestRun_Version(t *testing.T) {
	for _, arg := range []string{"--version", "-v", "version"} {
		t.Run(arg, func(t *testing.T) {
			var stdout bytes.Buffer
			code := run([]string{arg}, &stdout)
			if code != 0 {
				t.Fatalf("run(%q) exit code = %d, want 0", arg, code)
			}
			out := stdout.String()
			if !strings.HasPrefix(out, "stirrup-eval version ") {
				t.Fatalf("stdout = %q, want prefix %q", out, "stirrup-eval version ")
			}
			// Default link-time version when no -ldflags injected.
			if !strings.Contains(out, "dev") {
				t.Fatalf("stdout = %q, want it to contain default version %q", out, "dev")
			}
		})
	}
}

// TestCmdRun_DualWrite pins the backward-compatibility guarantee that
// `eval run` writes result.json in two places: <outputDir>/result.json
// (the legacy location existing CI workflows read) and
// <outputDir>/<suiteID>/result.json (the per-suite canonical location
// that lives alongside per-task artifact directories). Both must exist
// after a run, and the two files must be byte-identical so neither
// reader sees a stale snapshot. A regression that, for example, dropped
// the top-level write would silently break downstream tooling without
// any test catching it.
//
// We use --dry-run to avoid needing a fake harness binary; dry-run
// still walks the output-directory creation and both writeJSON calls
// in cmdRun, which is the surface this test is here to cover.
func TestCmdRun_DualWrite(t *testing.T) {
	dir := t.TempDir()
	suitePath := filepath.Join(dir, "dual-write.hcl")
	hclSrc := `
suite "dual-write-suite" {
  description = "fixture for TestCmdRun_DualWrite"

  task "task-a" {
    prompt = "do task a"
    judge {
      type = "test-command"
      command = "true"
    }
  }

  task "task-b" {
    prompt = "do task b"
    judge {
      type = "test-command"
      command = "true"
    }
  }
}
`
	if err := os.WriteFile(suitePath, []byte(hclSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	outputDir := filepath.Join(dir, "out")
	// Note: cmdRun uses log.Fatalf on error, which would os.Exit the
	// test binary. The success path with --dry-run does not hit that
	// branch, so we can exercise it from inside a test. If this ever
	// regresses to call log.Fatalf on the happy path, the test process
	// will die loudly — which is itself a useful signal.
	exitCode := run([]string{
		"run",
		"--suite", suitePath,
		"--output", outputDir,
		"--dry-run",
	}, io.Discard)
	if exitCode != 0 {
		t.Fatalf("run() exit code = %d, want 0", exitCode)
	}

	topLevel := filepath.Join(outputDir, "result.json")
	perSuite := filepath.Join(outputDir, "dual-write-suite", "result.json")

	topLevelData, err := os.ReadFile(topLevel)
	if err != nil {
		t.Fatalf("reading top-level result.json: %v", err)
	}
	perSuiteData, err := os.ReadFile(perSuite)
	if err != nil {
		t.Fatalf("reading per-suite result.json: %v", err)
	}

	// Both files must contain identical bytes — neither is a partial
	// snapshot or a different serialisation.
	if !bytes.Equal(topLevelData, perSuiteData) {
		t.Errorf("top-level and per-suite result.json differ:\n  top-level:\n%s\n  per-suite:\n%s",
			topLevelData, perSuiteData)
	}

	// And both must parse as a SuiteResult with the right SuiteID, so a
	// regression that wrote an empty file or a different schema would
	// surface here.
	var top, per eval.SuiteResult
	if err := json.Unmarshal(topLevelData, &top); err != nil {
		t.Fatalf("unmarshal top-level: %v", err)
	}
	if err := json.Unmarshal(perSuiteData, &per); err != nil {
		t.Fatalf("unmarshal per-suite: %v", err)
	}
	if top.SuiteID != "dual-write-suite" {
		t.Errorf("top-level SuiteID = %q, want %q", top.SuiteID, "dual-write-suite")
	}
	if per.SuiteID != "dual-write-suite" {
		t.Errorf("per-suite SuiteID = %q, want %q", per.SuiteID, "dual-write-suite")
	}
}

// TestRun_NoArgs documents the empty-args contract: usage goes to stderr,
// stdout stays untouched, exit code is 1.
func TestRun_NoArgs(t *testing.T) {
	var stdout bytes.Buffer
	code := run(nil, &stdout)
	if code != 1 {
		t.Fatalf("run(nil) exit code = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout should be empty for usage error, got %q", stdout.String())
	}
}

// TestLoadSuite_Missing exercises the underlying os error surface
// when the suite path does not exist.
func TestLoadSuite_Missing(t *testing.T) {
	_, err := loadSuite("/nonexistent/suite.hcl")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

// TestLoadSuite_InvalidHCL pins error propagation: if loadSuite ever
// stopped returning the diagnostic from spec.LoadSuiteHCL, this test
// would catch it.
func TestLoadSuite_InvalidHCL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.hcl")
	if err := os.WriteFile(path, []byte("suite \"x\" { {{{"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadSuite(path)
	if err == nil {
		t.Fatal("expected error for invalid HCL")
	}
}

// TestLoadSuite_HCL exercises the happy path: the loader must accept
// a .hcl file and produce the canonical EvalSuite shape.
func TestLoadSuite_HCL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "suite.hcl")
	src := `
suite "hcl-suite" {
  description = "an HCL suite"

  task "t1" {
    description = "first task"
    mode        = "execution"
    prompt      = "hello"

    judge {
      type    = "test-command"
      command = "true"
    }
  }
}
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := loadSuite(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "hcl-suite" {
		t.Errorf("ID = %q, want %q", got.ID, "hcl-suite")
	}
	if len(got.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(got.Tasks))
	}
	if got.Tasks[0].ID != "t1" {
		t.Errorf("Tasks[0].ID = %q, want %q", got.Tasks[0].ID, "t1")
	}
	if got.Tasks[0].Judge.Type != "test-command" {
		t.Errorf("Tasks[0].Judge.Type = %q, want %q", got.Tasks[0].Judge.Type, "test-command")
	}
}

// TestLoadSuite_UnsupportedExtension documents the dispatcher contract
// for unknown file extensions: only .hcl is accepted, and the error
// message must say so. Notably .json is rejected the same way as
// .yaml — the legacy JSON loader is gone.
func TestLoadSuite_UnsupportedExtension(t *testing.T) {
	cases := []struct {
		name string
		ext  string
	}{
		{name: "yaml", ext: ".yaml"},
		{name: "json (legacy format removed)", ext: ".json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "suite"+tc.ext)
			if err := os.WriteFile(path, []byte("anything"), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := loadSuite(path)
			if err == nil {
				t.Fatal("expected error for unsupported extension")
			}
			if !strings.Contains(err.Error(), ".hcl") {
				t.Fatalf("error = %q, want it to mention .hcl", err.Error())
			}
		})
	}
}

func TestLoadResult_Valid(t *testing.T) {
	dir := t.TempDir()
	result := eval.SuiteResult{
		SuiteID:  "s1",
		RunID:    "r1",
		PassRate: 0.5,
		Tasks: []eval.TaskResult{
			{TaskID: "t1", Outcome: "pass"},
			{TaskID: "t2", Outcome: "fail"},
		},
	}
	path := filepath.Join(dir, "result.json")
	writeJSONFile(t, path, result)

	got, err := loadResult(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.RunID != "r1" {
		t.Errorf("RunID = %q, want %q", got.RunID, "r1")
	}
	if len(got.Tasks) != 2 {
		t.Errorf("got %d tasks, want 2", len(got.Tasks))
	}
}

func TestLoadResult_Missing(t *testing.T) {
	_, err := loadResult("/nonexistent/result.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestWriteJSON_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")

	original := eval.SuiteResult{
		SuiteID:  "s1",
		RunID:    "r1",
		PassRate: 0.75,
	}

	if err := writeJSON(path, original); err != nil {
		t.Fatalf("writeJSON error: %v", err)
	}

	got, err := loadResult(path)
	if err != nil {
		t.Fatalf("loadResult error: %v", err)
	}

	if got.SuiteID != original.SuiteID || got.RunID != original.RunID {
		t.Errorf("round-trip mismatch: got %+v", got)
	}
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- parseDate tests ---

func TestParseDate_RFC3339(t *testing.T) {
	got, err := parseDate("2025-03-15T10:30:00Z")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2025, 3, 15, 10, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseDate(RFC3339) = %v, want %v", got, want)
	}
}

func TestParseDate_DateOnly(t *testing.T) {
	got, err := parseDate("2025-03-15")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := time.Date(2025, 3, 15, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("parseDate(date-only) = %v, want %v", got, want)
	}
}

func TestParseDate_Invalid(t *testing.T) {
	_, err := parseDate("not-a-date")
	if err == nil {
		t.Fatal("expected error for invalid date")
	}
}

func TestParseDate_EmptyString(t *testing.T) {
	_, err := parseDate("")
	if err == nil {
		t.Fatal("expected error for empty string")
	}
}

// --- parseDuration tests ---

func TestParseDuration_GoFormat(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
	}{
		{"24h", 24 * time.Hour},
		{"30m", 30 * time.Minute},
		{"1h30m", 90 * time.Minute},
		{"500ms", 500 * time.Millisecond},
	}
	for _, tc := range cases {
		got, err := parseDuration(tc.input)
		if err != nil {
			t.Errorf("parseDuration(%q): unexpected error: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseDuration(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestParseDuration_Days(t *testing.T) {
	got, err := parseDuration("7d")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := 7 * 24 * time.Hour
	if got != want {
		t.Errorf("parseDuration(7d) = %v, want %v", got, want)
	}
}

func TestParseDuration_FractionalDays(t *testing.T) {
	got, err := parseDuration("0.5d")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := 12 * time.Hour
	if got != want {
		t.Errorf("parseDuration(0.5d) = %v, want %v", got, want)
	}
}

func TestParseDuration_Invalid(t *testing.T) {
	_, err := parseDuration("notaduration")
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestParseDuration_BadDays(t *testing.T) {
	_, err := parseDuration("abcd")
	if err == nil {
		t.Fatal("expected error for non-numeric days suffix")
	}
}

// --- mineFailureTasks tests ---

// TestMineFailureTasks_FiltersByEvalOutcome pins #273's mining filter:
// by default only EvalOutcome==failed traces are mined; success traces
// are excluded as before, and inconclusive traces (max_turns,
// budget_exceeded, timeout, max_tokens, stalled, cancelled,
// verification_error) are excluded too unless includeInconclusive is
// set. The old "anything not success" semantics is gone.
func TestMineFailureTasks_FiltersByEvalOutcome(t *testing.T) {
	recordings := []types.RunRecording{
		{
			RunID:  "r1",
			Config: types.RunConfig{Prompt: "fix bug A", Mode: "execution"},
			FinalOutcome: types.RunTrace{
				ID:      "r1",
				Outcome: "success",
			},
		},
		{
			RunID:  "r2",
			Config: types.RunConfig{Prompt: "fix bug B", Mode: "execution"},
			FinalOutcome: types.RunTrace{
				ID:      "r2",
				Outcome: "error",
			},
		},
		{
			RunID:  "r3",
			Config: types.RunConfig{Prompt: "fix bug C", Mode: "planning"},
			FinalOutcome: types.RunTrace{
				ID:      "r3",
				Outcome: "max_turns",
			},
		},
		{
			RunID:  "r4",
			Config: types.RunConfig{Prompt: "fix bug D", Mode: "execution"},
			FinalOutcome: types.RunTrace{
				ID:      "r4",
				Outcome: "success",
			},
		},
	}

	// Default: only the EvalFailed trace is mined.
	tasks := mineFailureTasksFiltered(recordings, 0, false, false)
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1 (default mines only failed)", len(tasks))
	}
	if tasks[0].Prompt != "fix bug B" {
		t.Errorf("task[0].Prompt = %q, want %q", tasks[0].Prompt, "fix bug B")
	}
	if tasks[0].Judge.Type != "test-command" {
		t.Errorf("task[0].Judge.Type = %q, want %q", tasks[0].Judge.Type, "test-command")
	}
	if tasks[0].Judge.Command != "go test ./..." {
		t.Errorf("task[0].Judge.Command = %q, want %q", tasks[0].Judge.Command, "go test ./...")
	}

	// With --include-inconclusive: max_turns is also mined.
	tasksWithInconclusive := mineFailureTasksFiltered(recordings, 0, false, true)
	if len(tasksWithInconclusive) != 2 {
		t.Fatalf("got %d tasks, want 2 (failed + inconclusive)", len(tasksWithInconclusive))
	}
	if tasksWithInconclusive[1].Prompt != "fix bug C" {
		t.Errorf("task[1].Prompt = %q, want %q", tasksWithInconclusive[1].Prompt, "fix bug C")
	}
}

// TestMineFailureTasks_SuccessWithFailingVerifierMined pins that a
// run that terminated with Outcome=="success" but where the verifier
// disagreed is classified EvalFailed and therefore mined by default.
// The old `Outcome != "success"` filter would have excluded these,
// silently dropping a genuine quality failure.
func TestMineFailureTasks_SuccessWithFailingVerifierMined(t *testing.T) {
	recordings := []types.RunRecording{
		{
			RunID:  "r1",
			Config: types.RunConfig{Prompt: "fix bug A", Mode: "execution"},
			FinalOutcome: types.RunTrace{
				ID:      "r1",
				Outcome: "success",
				VerificationResults: []types.VerificationResult{
					{Passed: false, Feedback: "build failed"},
				},
			},
		},
	}
	tasks := mineFailureTasksFiltered(recordings, 0, false, false)
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1 (success+failed-verifier is EvalFailed)", len(tasks))
	}
	if tasks[0].Prompt != "fix bug A" {
		t.Errorf("task[0].Prompt = %q, want %q", tasks[0].Prompt, "fix bug A")
	}
}

func TestMineFailureTasks_RespectsLimit(t *testing.T) {
	recordings := []types.RunRecording{
		{RunID: "r1", Config: types.RunConfig{Prompt: "a"}, FinalOutcome: types.RunTrace{Outcome: "error"}},
		{RunID: "r2", Config: types.RunConfig{Prompt: "b"}, FinalOutcome: types.RunTrace{Outcome: "max_turns"}},
		{RunID: "r3", Config: types.RunConfig{Prompt: "c"}, FinalOutcome: types.RunTrace{Outcome: "error"}},
	}

	tasks := mineFailureTasksFiltered(recordings, 2, false, false)
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
}

func TestMineFailureTasks_NoFailures(t *testing.T) {
	recordings := []types.RunRecording{
		{RunID: "r1", Config: types.RunConfig{Prompt: "a"}, FinalOutcome: types.RunTrace{Outcome: "success"}},
	}

	tasks := mineFailureTasksFiltered(recordings, 0, false, false)
	if len(tasks) != 0 {
		t.Fatalf("got %d tasks, want 0", len(tasks))
	}
}

// makeBatchRecording is the test helper for #138's --include-batch
// branch: a recording whose RunConfig.Provider has Batch.Enabled=true.
// Centralised so the BatchProviderConfig construction is not
// scattered across multiple test cases.
func makeBatchRecording(runID, outcome, prompt string) types.RunRecording {
	return types.RunRecording{
		RunID: runID,
		Config: types.RunConfig{
			Prompt: prompt,
			Mode:   "execution",
			Provider: types.ProviderConfig{
				Type:  "anthropic",
				Batch: &types.BatchProviderConfig{Enabled: true},
			},
		},
		FinalOutcome: types.RunTrace{ID: runID, Outcome: outcome},
	}
}

// TestMineFailureTasksFiltered_ExcludesBatchByDefault pins the
// default behaviour of --include-batch=false (the spec'd default):
// batch failures stay out of the mined suite because their failure
// modes are dominated by provider-side queue dynamics, not the
// agent prompts mine-failures exists to surface (#138).
func TestMineFailureTasksFiltered_ExcludesBatchByDefault(t *testing.T) {
	recordings := []types.RunRecording{
		{
			RunID:        "stream-fail",
			Config:       types.RunConfig{Prompt: "streaming failure", Mode: "execution"},
			FinalOutcome: types.RunTrace{ID: "stream-fail", Outcome: "error"},
		},
		makeBatchRecording("batch-fail", "error", "batch failure"),
	}

	tasks := mineFailureTasksFiltered(recordings, 0, false, false)
	if len(tasks) != 1 {
		t.Fatalf("got %d tasks, want 1 (batch failure must be excluded)", len(tasks))
	}
	if tasks[0].Prompt != "streaming failure" {
		t.Errorf("task[0].Prompt = %q, want streaming failure", tasks[0].Prompt)
	}
}

// TestMineFailureTasksFiltered_IncludesBatchWhenRequested covers the
// --include-batch=true escape hatch. Operators investigating batch-
// specific failure modes (e.g. timeout taxonomies, provider-side
// rejection patterns) need to be able to opt into the wider window.
func TestMineFailureTasksFiltered_IncludesBatchWhenRequested(t *testing.T) {
	recordings := []types.RunRecording{
		{
			RunID:        "stream-fail",
			Config:       types.RunConfig{Prompt: "streaming failure", Mode: "execution"},
			FinalOutcome: types.RunTrace{ID: "stream-fail", Outcome: "error"},
		},
		makeBatchRecording("batch-fail", "error", "batch failure"),
	}

	tasks := mineFailureTasksFiltered(recordings, 0, true, false)
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2 (both failures included)", len(tasks))
	}
}

// writeMineFailuresFixture stores one streaming and one batch failure
// recording into a fresh lakehouse rooted at dir. Centralised so the
// two cmdMineFailures CLI-dispatch tests share an identical input
// surface and any divergence between default vs --include-batch is
// attributable to the flag, not the fixture.
func writeMineFailuresFixture(t *testing.T, dir string) {
	t.Helper()
	store, err := lakehouse.NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	recordings := []types.RunRecording{
		{
			RunID: "stream-fail",
			Config: types.RunConfig{
				Prompt: "streaming failure prompt",
				Mode:   "execution",
			},
			FinalOutcome: types.RunTrace{ID: "stream-fail", Outcome: "error"},
		},
		makeBatchRecording("batch-fail", "error", "batch failure prompt"),
	}
	for _, rec := range recordings {
		if err := store.StoreRecording(context.Background(), rec); err != nil {
			t.Fatalf("StoreRecording %s: %v", rec.RunID, err)
		}
	}
}

// TestRun_MineFailures_DefaultExcludesBatch pins the CLI default at
// the dispatch layer: invoking `eval mine-failures` without
// --include-batch must drop batch failures from the emitted suite.
// The unit tests on mineFailureTasksFiltered cover the helper, but
// only this test exercises the FlagSet registration, the default
// value, and the *includeBatch dereference into the helper — a
// regression that inverted the flag default or wired !*includeBatch
// would slip past every helper-level test (#138 spec B3).
func TestRun_MineFailures_DefaultExcludesBatch(t *testing.T) {
	dir := t.TempDir()
	writeMineFailuresFixture(t, dir)
	outPath := filepath.Join(dir, "mined.hcl")

	code := run([]string{
		"mine-failures",
		"--lakehouse", dir,
		"--output", outPath,
	}, io.Discard)
	if code != 0 {
		t.Fatalf("run() exit code = %d, want 0", code)
	}

	got, err := loadSuite(outPath)
	if err != nil {
		t.Fatalf("loadSuite: %v", err)
	}
	if len(got.Tasks) != 1 {
		t.Fatalf("default suite has %d tasks, want 1 (batch must be excluded)", len(got.Tasks))
	}
	if got.Tasks[0].Prompt != "streaming failure prompt" {
		t.Errorf("task[0].Prompt = %q, want %q", got.Tasks[0].Prompt, "streaming failure prompt")
	}
}

// TestRun_MineFailures_IncludeBatchFlag pins the --include-batch
// escape hatch at the dispatch layer: the flag must opt batch
// failures back into the emitted suite alongside streaming ones.
// Operators investigating batch-specific failure modes rely on this
// flag, so a regression that ignored it or hardcoded the helper's
// includeBatch argument to false would silently break the
// documented opt-in (#138 spec B3).
func TestRun_MineFailures_IncludeBatchFlag(t *testing.T) {
	dir := t.TempDir()
	writeMineFailuresFixture(t, dir)
	outPath := filepath.Join(dir, "mined.hcl")

	code := run([]string{
		"mine-failures",
		"--lakehouse", dir,
		"--output", outPath,
		"--include-batch",
	}, io.Discard)
	if code != 0 {
		t.Fatalf("run() exit code = %d, want 0", code)
	}

	got, err := loadSuite(outPath)
	if err != nil {
		t.Fatalf("loadSuite: %v", err)
	}
	if len(got.Tasks) != 2 {
		t.Fatalf("--include-batch suite has %d tasks, want 2 (both failures included)", len(got.Tasks))
	}
	// Both prompts must be present; recording-iteration order in
	// QueryRecordings is StartedAt-descending (zero-time here, so
	// implementation-defined), so assert on set membership rather
	// than ordering.
	prompts := map[string]bool{}
	for _, task := range got.Tasks {
		prompts[task.Prompt] = true
	}
	for _, want := range []string{"streaming failure prompt", "batch failure prompt"} {
		if !prompts[want] {
			t.Errorf("--include-batch suite missing prompt %q (got %v)", want, prompts)
		}
	}
}

// TestIsBatchRecording pins the classifier so a future refactor of
// the predicate fails this test rather than silently shifting the
// mine-failures default-include surface. Mirrors
// lakehouse.TestIsBatchRun — both must move together.
func TestIsBatchRecording(t *testing.T) {
	cases := []struct {
		name string
		rec  types.RunRecording
		want bool
	}{
		{"no-provider", types.RunRecording{}, false},
		{
			"provider-without-batch",
			types.RunRecording{Config: types.RunConfig{Provider: types.ProviderConfig{Type: "anthropic"}}},
			false,
		},
		{
			"batch-disabled",
			types.RunRecording{Config: types.RunConfig{Provider: types.ProviderConfig{
				Batch: &types.BatchProviderConfig{Enabled: false},
			}}},
			false,
		},
		{
			"batch-enabled",
			types.RunRecording{Config: types.RunConfig{Provider: types.ProviderConfig{
				Batch: &types.BatchProviderConfig{Enabled: true},
			}}},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBatchRecording(tc.rec); got != tc.want {
				t.Errorf("isBatchRecording = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- writeSuiteHCL tests ---

// TestWriteSuiteHCL_RoundTrip ensures the HCL emitted by mine-failures
// is parseable by the canonical loader. Without this, mined suites
// would silently regress to a non-loadable format the moment the
// emitter and loader drift.
func TestWriteSuiteHCL_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mined.hcl")

	original := types.EvalSuite{
		ID:          "mined-suite",
		Description: "starter from mine-failures",
		Tasks: []types.EvalTask{
			{
				ID:          "single",
				Description: "single judge task",
				Repo:        "https://example.invalid/repo",
				Ref:         "main",
				Mode:        "execution",
				Prompt:      "fix the bug",
				Judge: types.EvalJudge{
					Type:    "test-command",
					Command: "go test ./...",
				},
			},
			{
				ID:     "composite",
				Mode:   "execution",
				Prompt: "produce brief.md",
				Judge: types.EvalJudge{
					Type:    "composite",
					Require: "all",
					Judges: []types.EvalJudge{
						{Type: "file-exists", Paths: []string{"brief.md"}},
						{Type: "file-contains", Path: "brief.md", Pattern: "(?i)token"},
					},
				},
			},
		},
	}

	if err := writeSuiteHCL(path, original); err != nil {
		t.Fatalf("writeSuiteHCL: %v", err)
	}

	got, err := loadSuite(path)
	if err != nil {
		t.Fatalf("loadSuite after writeSuiteHCL: %v", err)
	}

	if got.ID != original.ID || got.Description != original.Description {
		t.Errorf("suite metadata mismatch: got %+v want %+v", got, original)
	}
	if len(got.Tasks) != len(original.Tasks) {
		t.Fatalf("got %d tasks, want %d", len(got.Tasks), len(original.Tasks))
	}
	for i, want := range original.Tasks {
		if got.Tasks[i].ID != want.ID ||
			got.Tasks[i].Mode != want.Mode ||
			got.Tasks[i].Prompt != want.Prompt {
			t.Errorf("task[%d] mismatch: got %+v want %+v", i, got.Tasks[i], want)
		}
		if got.Tasks[i].Judge.Type != want.Judge.Type {
			t.Errorf("task[%d].Judge.Type = %q, want %q", i, got.Tasks[i].Judge.Type, want.Judge.Type)
		}
	}
	if got.Tasks[1].Judge.Require != "all" {
		t.Errorf("composite Require = %q, want %q", got.Tasks[1].Judge.Require, "all")
	}
	if len(got.Tasks[1].Judge.Judges) != 2 {
		t.Errorf("composite has %d sub-judges, want 2", len(got.Tasks[1].Judge.Judges))
	}
}

// TestWriteSuiteHCL_QuarantineFlagsRoundTrip pins #115's wire-format
// contract: a suite with QuarantineFlags survives HCL serialise +
// parse with the same flags in the same order. The runner refuses
// to execute a quarantined suite without --accept-quarantine, so
// the load path must surface the flags verbatim.
func TestWriteSuiteHCL_QuarantineFlagsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quarantined.hcl")

	original := types.EvalSuite{
		ID:          "quarantined-suite",
		Description: "mined with QuarantineLargePayload flag",
		QuarantineFlags: []types.QuarantineFlag{
			types.QuarantineLargePayload,
		},
		Tasks: []types.EvalTask{
			{
				ID:     "t1",
				Mode:   "execution",
				Prompt: "fix",
				Judge:  types.EvalJudge{Type: "file-exists", Paths: []string{"x"}},
			},
		},
	}

	if err := writeSuiteHCL(path, original); err != nil {
		t.Fatalf("writeSuiteHCL: %v", err)
	}
	got, err := loadSuite(path)
	if err != nil {
		t.Fatalf("loadSuite: %v", err)
	}
	if len(got.QuarantineFlags) != 1 || got.QuarantineFlags[0] != types.QuarantineLargePayload {
		t.Errorf("QuarantineFlags = %v, want [%s]", got.QuarantineFlags, types.QuarantineLargePayload)
	}
}

// TestWriteSuiteHCL_EscapesInterpolation ensures hclwrite is escaping
// HCL-significant sequences (in particular `${...}` interpolation
// markers) so that user prompts mined out of production traces are
// preserved verbatim through the round trip rather than re-interpreted
// by the loader.
func TestWriteSuiteHCL_EscapesInterpolation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mined.hcl")

	dangerous := `prompt with ${var} interpolation and "quotes" plus a backslash \ end`
	original := types.EvalSuite{
		ID: "mined",
		Tasks: []types.EvalTask{
			{
				ID:     "tricky",
				Prompt: dangerous,
				Judge: types.EvalJudge{
					Type:    "test-command",
					Command: `go test ${PKG}`,
				},
			},
		},
	}

	if err := writeSuiteHCL(path, original); err != nil {
		t.Fatalf("writeSuiteHCL: %v", err)
	}
	got, err := loadSuite(path)
	if err != nil {
		t.Fatalf("loadSuite: %v", err)
	}
	if got.Tasks[0].Prompt != dangerous {
		t.Errorf("Prompt = %q, want %q", got.Tasks[0].Prompt, dangerous)
	}
	if got.Tasks[0].Judge.Command != `go test ${PKG}` {
		t.Errorf("Command = %q, want %q", got.Tasks[0].Judge.Command, `go test ${PKG}`)
	}
}

// --- buildLabVsProductionReport tests ---

func TestBuildLabVsProductionReport_Basic(t *testing.T) {
	prodMetrics := types.TraceMetrics{
		Count:     100,
		PassRate:  0.85,
		MeanTurns: 4.2,
	}

	result := eval.SuiteResult{
		SuiteID:  "test-suite",
		RunID:    "run-1",
		PassRate: 0.90,
		Tasks: []eval.TaskResult{
			{TaskID: "t1", Outcome: "pass", Trace: &types.RunTrace{Turns: 3}},
			{TaskID: "t2", Outcome: "pass", Trace: &types.RunTrace{Turns: 4}},
			{TaskID: "t3", Outcome: "fail", Trace: &types.RunTrace{Turns: 5}},
		},
	}

	report := buildLabVsProductionReport("exp-1", prodMetrics, result)

	if report.ExperimentID != "exp-1" {
		t.Errorf("ExperimentID = %q, want %q", report.ExperimentID, "exp-1")
	}

	// Production baseline
	if report.Production.SampleSize != 100 {
		t.Errorf("Production.SampleSize = %d, want 100", report.Production.SampleSize)
	}
	if math.Abs(report.Production.PassRate-0.85) > 0.001 {
		t.Errorf("Production.PassRate = %f, want 0.85", report.Production.PassRate)
	}
	if math.Abs(report.Production.MeanTurns-4.2) > 0.001 {
		t.Errorf("Production.MeanTurns = %f, want 4.2", report.Production.MeanTurns)
	}

	// Variant
	if len(report.Variants) != 1 {
		t.Fatalf("got %d variants, want 1", len(report.Variants))
	}
	v := report.Variants[0]
	if v.Name != "test-suite" {
		t.Errorf("Variant.Name = %q, want %q", v.Name, "test-suite")
	}
	if math.Abs(v.Results.PassRate-0.90) > 0.001 {
		t.Errorf("Variant.PassRate = %f, want 0.90", v.Results.PassRate)
	}
	// Mean turns = (3 + 4 + 5) / 3 = 4.0 => MedianTurns = 4
	if v.Results.MedianTurns != 4 {
		t.Errorf("Variant.MedianTurns = %d, want 4", v.Results.MedianTurns)
	}
}

func TestBuildLabVsProductionReport_NoTraces(t *testing.T) {
	prodMetrics := types.TraceMetrics{
		Count:    50,
		PassRate: 0.70,
	}

	result := eval.SuiteResult{
		SuiteID:  "no-traces",
		PassRate: 0.50,
		Tasks: []eval.TaskResult{
			{TaskID: "t1", Outcome: "pass"},
			{TaskID: "t2", Outcome: "fail"},
		},
	}

	report := buildLabVsProductionReport("exp-2", prodMetrics, result)

	if len(report.Variants) != 1 {
		t.Fatalf("got %d variants, want 1", len(report.Variants))
	}
	v := report.Variants[0]
	// With no traces, turns should be zero.
	if v.Results.MedianTurns != 0 {
		t.Errorf("Variant.MedianTurns = %d, want 0", v.Results.MedianTurns)
	}
}

func TestBuildLabVsProductionReport_MixedTraces(t *testing.T) {
	prodMetrics := types.TraceMetrics{Count: 10, PassRate: 0.80}

	result := eval.SuiteResult{
		SuiteID:  "mixed",
		PassRate: 0.75,
		Tasks: []eval.TaskResult{
			{TaskID: "t1", Outcome: "pass", Trace: &types.RunTrace{Turns: 2}},
			{TaskID: "t2", Outcome: "error"}, // no trace
			{TaskID: "t3", Outcome: "pass", Trace: &types.RunTrace{Turns: 6}},
		},
	}

	report := buildLabVsProductionReport("exp-3", prodMetrics, result)
	v := report.Variants[0]

	// Mean turns = (2 + 6) / 2 = 4
	if v.Results.MedianTurns != 4 {
		t.Errorf("Variant.MedianTurns = %d, want 4", v.Results.MedianTurns)
	}
}

func TestPrintComparisonSummary_DoesNotPanic(t *testing.T) {
	report := types.LabVsProductionReport{
		ExperimentID: "smoke-test",
		Production: types.BaselineMetrics{
			PassRate:   0.85,
			MeanTurns:  4.2,
			SampleSize: 100,
		},
		Variants: []types.VariantReport{
			{
				Name: "v1",
				Results: types.VariantResults{
					PassRate:    0.90,
					MedianTurns: 3,
				},
			},
		},
	}

	// Should not panic. Using io.Discard avoids the global os.Stderr
	// mutation pattern, which is unsafe under -race / -parallel.
	printComparisonSummary(io.Discard, report)
}

func TestPrintComparisonSummary_EmptyVariants(t *testing.T) {
	report := types.LabVsProductionReport{
		ExperimentID: "empty",
		Production: types.BaselineMetrics{
			PassRate:   0.50,
			SampleSize: 10,
		},
	}

	// Should not panic with zero variants.
	printComparisonSummary(io.Discard, report)
}

// --- buildDriftReport tests ---

func TestBuildDriftReport_ComputesDeltas(t *testing.T) {
	current := types.TraceMetrics{
		Count:            10,
		PassRate:         0.80,
		MeanTurns:        5.0,
		MeanTokens:       1000,
		P50Duration:      200,
		P95Duration:      500,
		BatchP50Duration: 12000,
		BatchP95Duration: 30000,
	}
	baseline := types.TraceMetrics{
		Count:            10,
		PassRate:         0.90,
		MeanTurns:        4.0,
		MeanTokens:       900,
		P50Duration:      180,
		P95Duration:      450,
		BatchP50Duration: 9000,
		BatchP95Duration: 24000,
	}

	report := buildDriftReport(current, baseline)

	if math.Abs(report.Deltas.PassRateDelta-(-0.10)) > 0.001 {
		t.Errorf("PassRateDelta = %f, want -0.10", report.Deltas.PassRateDelta)
	}
	if math.Abs(report.Deltas.MeanTurnsDelta-1.0) > 0.001 {
		t.Errorf("MeanTurnsDelta = %f, want 1.0", report.Deltas.MeanTurnsDelta)
	}
	// Pin the full delta surface: a sign-flip bug (baseline - current
	// rather than current - baseline) would not be caught by the
	// pass-rate or mean-turns assertions alone, since the streaming
	// and batch percentile deltas were entirely unasserted prior to
	// #138 review B4.
	if math.Abs(report.Deltas.MeanTokensDelta-100) > 0.001 {
		t.Errorf("MeanTokensDelta = %f, want 100", report.Deltas.MeanTokensDelta)
	}
	if math.Abs(report.Deltas.P50DurationDelta-20) > 0.001 {
		t.Errorf("P50DurationDelta = %f, want 20", report.Deltas.P50DurationDelta)
	}
	if math.Abs(report.Deltas.P95DurationDelta-50) > 0.001 {
		t.Errorf("P95DurationDelta = %f, want 50", report.Deltas.P95DurationDelta)
	}
	if math.Abs(report.Deltas.BatchP50DurationDelta-3000) > 0.001 {
		t.Errorf("BatchP50DurationDelta = %f, want 3000", report.Deltas.BatchP50DurationDelta)
	}
	if math.Abs(report.Deltas.BatchP95DurationDelta-6000) > 0.001 {
		t.Errorf("BatchP95DurationDelta = %f, want 6000", report.Deltas.BatchP95DurationDelta)
	}
}
