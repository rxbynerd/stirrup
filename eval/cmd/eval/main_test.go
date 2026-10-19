package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/eval"
	"github.com/rxbynerd/stirrup/eval/lakehouse"
	"github.com/rxbynerd/stirrup/types"
)

// --- run() dispatch tests ---

// TestRun_VersionFlagPrintsVersion exercises the --version short-circuit
// through run(), so it needs neither a built binary nor global state. Each
// accepted spelling (--version / -v / version) must exit 0 and print to
// stdout.
func TestRun_VersionFlagPrintsVersion(t *testing.T) {
	for _, arg := range []string{"--version", "-v", "version"} {
		t.Run(arg, func(t *testing.T) {
			var stdout bytes.Buffer
			code := run([]string{arg}, &stdout)
			require.Equal(t, 0, code)
			out := stdout.String()
			require.Regexp(t, "^stirrup-eval version ", out)
			// The default link-time version applies when no -ldflags is injected.
			require.Contains(t, out, "dev")
		})
	}
}

// writeFakeHarness writes body as a POSIX-shell harness double and returns
// its path. Skipped on non-Unix runners since /bin/sh is not portable to
// Windows.
func writeFakeHarness(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake harness uses /bin/sh; skipped on Windows")
	}
	harnessDir := t.TempDir()
	path := filepath.Join(harnessDir, "fake-harness")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	return path
}

// TestCmdRun_WritesIdenticalResultToBothPaths pins that a live run writes
// result.json to both <outputDir>/result.json (the legacy path CI reads) and
// <outputDir>/<suiteID>/result.json (the per-suite path). The two copies must
// be byte-identical and parse as a SuiteResult with the right SuiteID.
func TestCmdRun_WritesIdenticalResultToBothPaths(t *testing.T) {
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

	dir := t.TempDir()
	suitePath := filepath.Join(dir, "dual-write.hcl")
	hclSrc := `
suite "dual-write-suite" {
  description = "fixture for TestCmdRun_WritesIdenticalResultToBothPaths"

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
	require.NoError(t, os.WriteFile(suitePath, []byte(hclSrc), 0o644))

	outputDir := filepath.Join(dir, "out")
	// cmdRun calls log.Fatalf on error, which exits the test binary, so only
	// the success path runs here.
	exitCode := run([]string{
		"run",
		"--suite", suitePath,
		"--harness", harnessPath,
		"--output", outputDir,
	}, io.Discard)
	require.Equal(t, 0, exitCode)

	topLevel := filepath.Join(outputDir, "result.json")
	perSuite := filepath.Join(outputDir, "dual-write-suite", "result.json")

	topLevelData, err := os.ReadFile(topLevel)
	require.NoError(t, err, "reading top-level result.json")
	perSuiteData, err := os.ReadFile(perSuite)
	require.NoError(t, err, "reading per-suite result.json")
	assert.Equal(t, string(topLevelData), string(perSuiteData))

	var top, per eval.SuiteResult
	require.NoError(t, json.Unmarshal(topLevelData, &top), "unmarshal top-level")
	require.NoError(t, json.Unmarshal(perSuiteData, &per), "unmarshal per-suite")
	assert.Equal(t, "dual-write-suite", top.SuiteID)
	assert.Equal(t, "dual-write-suite", per.SuiteID)
}

// TestCmdRun_DryRunWritesNoArtifacts pins that `eval run --dry-run`
// without --output must not create result.json (top-level or per-suite)
// anywhere, in particular not in the process's working directory. The
// test runs from a temp cwd and asserts it stays empty.
func TestCmdRun_DryRunWritesNoArtifacts(t *testing.T) {
	cwd := t.TempDir()
	suiteDir := t.TempDir()
	suitePath := filepath.Join(suiteDir, "dry-run.hcl")
	hclSrc := `
suite "dry-run-suite" {
  description = "fixture for TestCmdRun_DryRunWritesNoArtifacts"

  task "task-a" {
    prompt = "do task a"
    judge {
      type = "test-command"
      command = "true"
    }
  }
}
`
	require.NoError(t, os.WriteFile(suitePath, []byte(hclSrc), 0o644))

	origWD, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(cwd))
	defer func() {
		require.NoError(t, os.Chdir(origWD))
	}()

	// cmdRun's summary output goes to the real os.Stdout (only --version
	// writes through the injected io.Writer), so this test asserts on the
	// exit code and filesystem side effects rather than captured output.
	exitCode := run([]string{
		"run",
		"--suite", suitePath,
		"--dry-run",
	}, io.Discard)
	require.Equal(t, 0, exitCode)

	entries, err := os.ReadDir(cwd)
	require.NoError(t, err, "reading cwd")
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	require.Empty(t, names, "working directory must stay empty after --dry-run")
}

// TestCmdRun_DryRunIgnoresExplicitOutput pins that --dry-run writes no
// artifacts even when --output is passed explicitly, rather than only
// suppressing the implicit cwd default.
func TestCmdRun_DryRunIgnoresExplicitOutput(t *testing.T) {
	dir := t.TempDir()
	suitePath := filepath.Join(dir, "dry-run.hcl")
	hclSrc := `
suite "dry-run-suite" {
  description = "fixture for TestCmdRun_DryRunIgnoresExplicitOutput"

  task "task-a" {
    prompt = "do task a"
    judge {
      type = "test-command"
      command = "true"
    }
  }
}
`
	require.NoError(t, os.WriteFile(suitePath, []byte(hclSrc), 0o644))

	outputDir := filepath.Join(dir, "out")
	exitCode := run([]string{
		"run",
		"--suite", suitePath,
		"--output", outputDir,
		"--dry-run",
	}, io.Discard)
	require.Equal(t, 0, exitCode)

	_, err := os.Stat(outputDir)
	require.ErrorIs(t, err, os.ErrNotExist, "--output directory must not be created under --dry-run")
}

// TestRun_NoArgsExitsWithUsageError pins the empty-args contract: usage goes
// to stderr, stdout stays empty, and the exit code is 1.
func TestRun_NoArgsExitsWithUsageError(t *testing.T) {
	var stdout bytes.Buffer
	code := run(nil, &stdout)
	require.Equal(t, 1, code)
	require.Empty(t, stdout.String())
}

// TestLoadSuite_MissingFileErrors exercises the underlying os error surface
// when the suite path does not exist.
func TestLoadSuite_MissingFileErrors(t *testing.T) {
	_, err := loadSuite("/nonexistent/suite.hcl")
	require.Error(t, err)
}

// TestLoadSuite_InvalidHCLErrors pins error propagation: if loadSuite ever
// stopped returning the diagnostic from spec.LoadSuiteHCL, this test
// would catch it.
func TestLoadSuite_InvalidHCLErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.hcl")
	require.NoError(t, os.WriteFile(path, []byte("suite \"x\" { {{{"), 0o644))

	_, err := loadSuite(path)
	require.Error(t, err)
}

// TestLoadSuite_AcceptsHCLSuite exercises the happy path: the loader must
// accept a .hcl file and produce the canonical EvalSuite shape.
func TestLoadSuite_AcceptsHCLSuite(t *testing.T) {
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
	require.NoError(t, os.WriteFile(path, []byte(src), 0o644))

	got, err := loadSuite(path)
	require.NoError(t, err)
	assert.Equal(t, "hcl-suite", got.ID)
	require.Len(t, got.Tasks, 1)
	assert.Equal(t, "t1", got.Tasks[0].ID)
	assert.Equal(t, "test-command", got.Tasks[0].Judge.Type)
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
		{name: "legacy json", ext: ".json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "suite"+tc.ext)
			require.NoError(t, os.WriteFile(path, []byte("anything"), 0o644))

			_, err := loadSuite(path)
			require.ErrorContains(t, err, ".hcl")
		})
	}
}

func TestLoadResult_ReadsValidFile(t *testing.T) {
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
	require.NoError(t, err)
	assert.Equal(t, "r1", got.RunID)
	assert.Len(t, got.Tasks, 2)
}

func TestLoadResult_MissingFileErrors(t *testing.T) {
	_, err := loadResult("/nonexistent/result.json")
	require.Error(t, err)
}

func TestWriteJSON_RoundTripsThroughLoadResult(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")

	original := eval.SuiteResult{
		SuiteID:  "s1",
		RunID:    "r1",
		PassRate: 0.75,
	}

	require.NoError(t, writeJSON(path, original))

	got, err := loadResult(path)
	require.NoError(t, err)

	assert.Equal(t, original.SuiteID, got.SuiteID)
	assert.Equal(t, original.RunID, got.RunID)
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
}

// --- parseDate tests ---

func TestParseDate_AcceptsRFC3339(t *testing.T) {
	got, err := parseDate("2025-03-15T10:30:00Z")
	require.NoError(t, err)
	want := time.Date(2025, 3, 15, 10, 30, 0, 0, time.UTC)
	require.True(t, got.Equal(want), "parseDate = %v, want %v", got, want)
}

func TestParseDate_AcceptsDateOnly(t *testing.T) {
	got, err := parseDate("2025-03-15")
	require.NoError(t, err)
	want := time.Date(2025, 3, 15, 0, 0, 0, 0, time.UTC)
	require.True(t, got.Equal(want), "parseDate = %v, want %v", got, want)
}

func TestParseDate_RejectsInvalidInput(t *testing.T) {
	_, err := parseDate("not-a-date")
	require.Error(t, err)
}

func TestParseDate_RejectsEmptyString(t *testing.T) {
	_, err := parseDate("")
	require.Error(t, err)
}

// --- parseDuration tests ---

func TestParseDuration_AcceptsGoFormat(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  time.Duration
	}{
		{name: "hours", input: "24h", want: 24 * time.Hour},
		{name: "minutes", input: "30m", want: 30 * time.Minute},
		{name: "hours and minutes", input: "1h30m", want: 90 * time.Minute},
		{name: "milliseconds", input: "500ms", want: 500 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDuration(tc.input)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestParseDuration_AcceptsDaySuffix(t *testing.T) {
	got, err := parseDuration("7d")
	require.NoError(t, err)
	require.Equal(t, 7*24*time.Hour, got)
}

func TestParseDuration_AcceptsFractionalDays(t *testing.T) {
	got, err := parseDuration("0.5d")
	require.NoError(t, err)
	require.Equal(t, 12*time.Hour, got)
}

func TestParseDuration_RejectsInvalidInput(t *testing.T) {
	_, err := parseDuration("notaduration")
	require.Error(t, err)
}

func TestParseDuration_RejectsNonNumericDays(t *testing.T) {
	_, err := parseDuration("abcd")
	require.Error(t, err)
}

// --- mineFailureTasks tests ---

// TestMineFailureTasks_FiltersByEvalOutcome pins the mining filter:
// by default only EvalOutcome==failed traces are mined; inconclusive
// traces (max_turns, budget_exceeded, timeout, max_tokens, stalled,
// cancelled, verification_error) are excluded too unless
// includeInconclusive is set.
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

	t.Run("default mines only failed traces", func(t *testing.T) {
		tasks := mineFailureTasksFiltered(recordings, 0, false, false)
		require.Len(t, tasks, 1)
		assert.Equal(t, "fix bug B", tasks[0].Prompt)
		assert.Equal(t, "test-command", tasks[0].Judge.Type)
		assert.Equal(t, "go test ./...", tasks[0].Judge.Command)
	})

	t.Run("include inconclusive also mines max_turns", func(t *testing.T) {
		tasks := mineFailureTasksFiltered(recordings, 0, false, true)
		require.Len(t, tasks, 2)
		assert.Equal(t, "fix bug C", tasks[1].Prompt)
	})
}

// TestMineFailureTasks_SuccessWithFailingVerifierMined pins that a
// run that terminated with Outcome=="success" but where the verifier
// disagreed is classified EvalFailed and therefore mined by default.
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
	require.Len(t, tasks, 1)
	require.Equal(t, "fix bug A", tasks[0].Prompt)
}

func TestMineFailureTasks_RespectsLimit(t *testing.T) {
	recordings := []types.RunRecording{
		{RunID: "r1", Config: types.RunConfig{Prompt: "a"}, FinalOutcome: types.RunTrace{Outcome: "error"}},
		{RunID: "r2", Config: types.RunConfig{Prompt: "b"}, FinalOutcome: types.RunTrace{Outcome: "max_turns"}},
		{RunID: "r3", Config: types.RunConfig{Prompt: "c"}, FinalOutcome: types.RunTrace{Outcome: "error"}},
	}

	tasks := mineFailureTasksFiltered(recordings, 2, false, false)
	require.Len(t, tasks, 2)
}

func TestMineFailureTasks_NoFailures(t *testing.T) {
	recordings := []types.RunRecording{
		{RunID: "r1", Config: types.RunConfig{Prompt: "a"}, FinalOutcome: types.RunTrace{Outcome: "success"}},
	}

	tasks := mineFailureTasksFiltered(recordings, 0, false, false)
	require.Empty(t, tasks)
}

// makeBatchRecording builds a recording whose RunConfig.Provider has
// Batch.Enabled=true, centralised so the construction is not
// scattered across test cases.
func makeBatchRecording(runID, outcome, prompt string) types.RunRecording {
	cfg := types.RunConfig{
		Prompt: prompt,
		Mode:   "execution",
		Provider: types.ProviderConfig{
			Type:  "anthropic",
			Batch: &types.BatchProviderConfig{Enabled: true},
		},
	}
	return types.RunRecording{
		RunID:  runID,
		Config: cfg,
		FinalOutcome: types.RunTrace{
			ID:      runID,
			Outcome: outcome,
			Config:  cfg,
		},
	}
}

// TestMineFailureTasksFiltered_ExcludesBatchByDefault pins the
// default behaviour of --include-batch=false: batch failures stay
// out of the mined suite.
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
	require.Len(t, tasks, 1, "batch failure must be excluded")
	require.Equal(t, "streaming failure", tasks[0].Prompt)
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
	require.Len(t, tasks, 2)
}

// writeMineFailuresFixture stores one streaming and one batch failure
// recording into a fresh lakehouse rooted at dir. Centralised so the
// two cmdMineFailures CLI-dispatch tests share an identical input
// surface and any divergence between default vs --include-batch is
// attributable to the flag, not the fixture.
func writeMineFailuresFixture(t *testing.T, dir string) {
	t.Helper()
	store, err := lakehouse.NewFileStore(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	recordings := []types.RunRecording{
		{
			RunID: "stream-fail",
			Config: types.RunConfig{
				Prompt: "streaming failure prompt",
				Mode:   "execution",
			},
			FinalOutcome: types.RunTrace{
				ID:      "stream-fail",
				Outcome: "error",
				Config: types.RunConfig{
					Prompt: "streaming failure prompt",
					Mode:   "execution",
				},
			},
		},
		makeBatchRecording("batch-fail", "error", "batch failure prompt"),
	}
	for _, rec := range recordings {
		require.NoError(t, store.StoreRecording(context.Background(), rec), "StoreRecording %s", rec.RunID)
		// mine-failures reads QueryTraces first and hydrates
		// recordings opportunistically, so the fixture seeds both.
		require.NoError(t, store.StoreTrace(context.Background(), rec.FinalOutcome), "StoreTrace %s", rec.RunID)
	}
}

// TestRun_MineFailures_DefaultExcludesBatch pins the CLI default at
// the dispatch layer: invoking `eval mine-failures` without
// --include-batch must drop batch failures from the emitted suite.
func TestRun_MineFailures_DefaultExcludesBatch(t *testing.T) {
	dir := t.TempDir()
	writeMineFailuresFixture(t, dir)
	outPath := filepath.Join(dir, "mined.hcl")

	code := run([]string{
		"mine-failures",
		"--lakehouse", dir,
		"--output", outPath,
	}, io.Discard)
	require.Equal(t, 0, code)

	got, err := loadSuite(outPath)
	require.NoError(t, err)
	require.Len(t, got.Tasks, 1, "batch failure must be excluded by default")
	require.Equal(t, "streaming failure prompt", got.Tasks[0].Prompt)
}

// TestRun_MineFailures_IncludeBatchFlag pins the --include-batch
// escape hatch at the dispatch layer: the flag must opt batch
// failures back into the emitted suite alongside streaming ones.
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
	require.Equal(t, 0, code)

	got, err := loadSuite(outPath)
	require.NoError(t, err)
	require.Len(t, got.Tasks, 2)
	// Both prompts must be present; recording-iteration order in
	// QueryRecordings is StartedAt-descending (zero-time here, so
	// implementation-defined), so assert on set membership rather
	// than ordering.
	prompts := make([]string, 0, len(got.Tasks))
	for _, task := range got.Tasks {
		prompts = append(prompts, task.Prompt)
	}
	require.ElementsMatch(t, []string{"streaming failure prompt", "batch failure prompt"}, prompts)
}

// TestIsBatchRecording_RequiresEnabledBatchConfig pins the classifier so a
// future refactor of the predicate fails this test rather than silently
// shifting the mine-failures default-include surface. Mirrors
// lakehouse.TestIsBatchRun_ClassifiesByBatchEnabledFlag — both must move together.
func TestIsBatchRecording_RequiresEnabledBatchConfig(t *testing.T) {
	cases := []struct {
		name string
		rec  types.RunRecording
		want bool
	}{
		{"no provider", types.RunRecording{}, false},
		{
			"provider without batch",
			types.RunRecording{Config: types.RunConfig{Provider: types.ProviderConfig{Type: "anthropic"}}},
			false,
		},
		{
			"batch disabled",
			types.RunRecording{Config: types.RunConfig{Provider: types.ProviderConfig{
				Batch: &types.BatchProviderConfig{Enabled: false},
			}}},
			false,
		},
		{
			"batch enabled",
			types.RunRecording{Config: types.RunConfig{Provider: types.ProviderConfig{
				Batch: &types.BatchProviderConfig{Enabled: true},
			}}},
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isBatchRecording(tc.rec))
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

	require.NoError(t, writeSuiteHCL(path, original))

	got, err := loadSuite(path)
	require.NoError(t, err)

	require.Equal(t, original.ID, got.ID)
	require.Equal(t, original.Description, got.Description)
	require.Len(t, got.Tasks, len(original.Tasks))
	for i, want := range original.Tasks {
		t.Run(want.ID, func(t *testing.T) {
			require.Equal(t, want.ID, got.Tasks[i].ID)
			require.Equal(t, want.Mode, got.Tasks[i].Mode)
			require.Equal(t, want.Prompt, got.Tasks[i].Prompt)
			require.Equal(t, want.Judge.Type, got.Tasks[i].Judge.Type)
		})
	}
	require.Equal(t, "all", got.Tasks[1].Judge.Require)
	require.Len(t, got.Tasks[1].Judge.Judges, 2)
}

func TestWriteSuiteHCL_LLMBlockRoundTrip(t *testing.T) {
	zero, warm := 0.0, 0.7
	cases := []struct {
		name  string
		judge types.EvalJudge
	}{
		{"llm block with all fields", types.EvalJudge{Type: "diff-review", Criteria: "adds a test", LLM: &types.JudgeLLMConfig{
			Provider: "openai-compatible", Model: "openai/gpt-6-luna", BaseURL: "https://openrouter.ai/api/v1",
			APIKeyRef: "secret://OPENROUTER_API_KEY", TimeoutSeconds: 60, MaxInputBytes: 4096, Temperature: &zero,
			MaxTokens: 2048, StructuredOutput: types.JudgeStructuredPromptOnly, AllowTruncated: true,
		}}},
		{"llm block with model only", types.EvalJudge{Type: "diff-review", Criteria: "minimal", LLM: &types.JudgeLLMConfig{Model: "claude-x"}}},
		{"no llm block", types.EvalJudge{Type: "diff-review", Criteria: "no block"}},
		{"composite with nested llm block", types.EvalJudge{Type: "composite", Require: "any", Judges: []types.EvalJudge{
			{Type: "file-exists", Paths: []string{"a.txt"}},
			{Type: "diff-review", Criteria: "nested", LLM: &types.JudgeLLMConfig{Model: "claude-y", Temperature: &warm}},
		}}},
	}
	original := types.EvalSuite{ID: "llm-suite"}
	for i, tc := range cases {
		original.Tasks = append(original.Tasks, types.EvalTask{ID: fmt.Sprintf("t%d", i), Prompt: "p", Judge: tc.judge})
	}
	path := filepath.Join(t.TempDir(), "llm.hcl")
	require.NoError(t, writeSuiteHCL(path, original))
	got, err := loadSuite(path)
	require.NoError(t, err)
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.judge, got.Tasks[i].Judge)
		})
	}
}

// TestWriteSuiteHCL_QuarantineFlagsRoundTrip pins that a suite with
// QuarantineFlags survives HCL serialise + parse with the same flags
// in the same order.
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

	require.NoError(t, writeSuiteHCL(path, original))
	got, err := loadSuite(path)
	require.NoError(t, err)
	require.Equal(t, []types.QuarantineFlag{types.QuarantineLargePayload}, got.QuarantineFlags)
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

	require.NoError(t, writeSuiteHCL(path, original))
	got, err := loadSuite(path)
	require.NoError(t, err)
	require.Equal(t, dangerous, got.Tasks[0].Prompt)
	require.Equal(t, `go test ${PKG}`, got.Tasks[0].Judge.Command)
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

	require.Equal(t, "exp-1", report.ExperimentID)

	// Production baseline
	require.Equal(t, 100, report.Production.SampleSize)
	require.InDelta(t, 0.85, report.Production.PassRate, 0.001)
	require.InDelta(t, 4.2, report.Production.MeanTurns, 0.001)

	// Variant
	require.Len(t, report.Variants, 1)
	v := report.Variants[0]
	require.Equal(t, "test-suite", v.Name)
	require.InDelta(t, 0.90, v.Results.PassRate, 0.001)
	// Mean turns = (3 + 4 + 5) / 3 = 4.0 => MedianTurns = 4
	require.Equal(t, 4, v.Results.MedianTurns)
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

	require.Len(t, report.Variants, 1)
	v := report.Variants[0]
	// With no traces, turns should be zero.
	require.Equal(t, 0, v.Results.MedianTurns)
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
	require.Equal(t, 4, v.Results.MedianTurns)
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

	// io.Discard avoids mutating the global os.Stderr, which is unsafe under -race and -parallel.
	require.NotPanics(t, func() { printComparisonSummary(io.Discard, report) })
}

func TestPrintComparisonSummary_EmptyVariants(t *testing.T) {
	report := types.LabVsProductionReport{
		ExperimentID: "empty",
		Production: types.BaselineMetrics{
			PassRate:   0.50,
			SampleSize: 10,
		},
	}

	require.NotPanics(t, func() { printComparisonSummary(io.Discard, report) })
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

	// Each delta is current minus baseline. Every field is pinned so a sign
	// flip in any one of them fails.
	require.InDelta(t, -0.10, report.Deltas.PassRateDelta, 0.001)
	require.InDelta(t, 1.0, report.Deltas.MeanTurnsDelta, 0.001)
	require.InDelta(t, 100, report.Deltas.MeanTokensDelta, 0.001)
	require.InDelta(t, 20, report.Deltas.P50DurationDelta, 0.001)
	require.InDelta(t, 50, report.Deltas.P95DurationDelta, 0.001)
	require.InDelta(t, 3000, report.Deltas.BatchP50DurationDelta, 0.001)
	require.InDelta(t, 6000, report.Deltas.BatchP95DurationDelta, 0.001)
}

// TestCmdRun_DryRunAcceptanceSuite pins that the shipped acceptance suite
// passes `eval run --dry-run` without a credential or a network.
func TestCmdRun_DryRunAcceptanceSuite(t *testing.T) {
	suitePath, err := filepath.Abs("../../suites/acceptance.hcl")
	require.NoError(t, err)
	require.Equal(t, 0, run([]string{"run", "--suite", suitePath, "--dry-run"}, io.Discard))
}
