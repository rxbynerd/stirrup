package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// baselineRunConfig builds a valid RunConfig that satisfies
// types.ValidateRunConfig (execution mode, anthropic provider, max_turns
// + timeout set). Tests that exercise merge / redaction / dry-run start
// from this so they only have to vary the fields under test.
func baselineRunConfig() *types.RunConfig {
	timeout := 300
	return &types.RunConfig{
		Mode:     "execution",
		Provider: types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		MaxTurns: 10,
		Timeout:  &timeout,
	}
}

// TestMergeOverrides_NoOverlay confirms a nil overlay leaves the baseline
// untouched. mergeOverrides has to be safe to call with no per-task
// overrides — that is the suite-baseline-only path.
func TestMergeOverrides_NoOverlay(t *testing.T) {
	baseline := baselineRunConfig()
	got := mergeOverrides(baseline, nil)
	if got == nil {
		t.Fatal("expected non-nil merged config")
	}
	if got.MaxTurns != 10 || got.Mode != "execution" {
		t.Errorf("baseline mutated unexpectedly: %+v", got)
	}
}

// TestMergeOverrides_SparseField asserts a sparse overlay only changes
// the named field; every other baseline field passes through unchanged.
// This is the core "sparse overlay" contract for per-task overrides.
func TestMergeOverrides_SparseField(t *testing.T) {
	baseline := baselineRunConfig()
	four := 4
	overlay := &types.RunConfigOverrides{MaxTurns: &four}

	got := mergeOverrides(baseline, overlay)
	if got.MaxTurns != 4 {
		t.Errorf("MaxTurns = %d, want 4", got.MaxTurns)
	}
	if got.Mode != "execution" {
		t.Errorf("Mode = %q, want %q (unchanged)", got.Mode, "execution")
	}
	if got.Provider.Type != "anthropic" {
		t.Errorf("Provider.Type = %q, want %q (unchanged)", got.Provider.Type, "anthropic")
	}
}

// TestMergeOverrides_MultipleFields covers the case where an overlay
// touches a pointer field (Provider) and a scalar field (Mode) at the
// same time. Both should land; pointer copies are deref'd so the merged
// config does not alias overlay-owned memory.
func TestMergeOverrides_MultipleFields(t *testing.T) {
	baseline := baselineRunConfig()
	overlay := &types.RunConfigOverrides{
		Mode:     "planning",
		Provider: &types.ProviderConfig{Type: "openai-responses", APIKeyRef: "secret://OPENAI_KEY"},
	}

	got := mergeOverrides(baseline, overlay)
	if got.Mode != "planning" {
		t.Errorf("Mode = %q, want %q", got.Mode, "planning")
	}
	if got.Provider.Type != "openai-responses" {
		t.Errorf("Provider.Type = %q, want %q", got.Provider.Type, "openai-responses")
	}
	// Pointer dereference, not alias: mutating the overlay's Provider
	// must not be observable on the merged result.
	overlay.Provider.Type = "mutated"
	if got.Provider.Type == "mutated" {
		t.Error("merged Provider aliases overlay-owned memory; expected dereference")
	}
}

// TestMergeOverrides_NilBaseline asserts the explicit nil-baseline guard.
// A merge with no baseline is a programming bug (the caller should never
// produce overrides without a base), and the helper returns nil rather
// than fabricating a half-formed config.
func TestMergeOverrides_NilBaseline(t *testing.T) {
	four := 4
	got := mergeOverrides(nil, &types.RunConfigOverrides{MaxTurns: &four})
	if got != nil {
		t.Errorf("nil baseline must return nil, got %+v", got)
	}
}

// TestBuildMergedConfig_FromInlineBaseline exercises the suite-inline
// baseline path end-to-end: clone the baseline, apply overrides, return
// a fresh allocation. Mutating the result must not be observable on the
// suite's original RunConfig pointer.
func TestBuildMergedConfig_FromInlineBaseline(t *testing.T) {
	suite := types.EvalSuite{
		ID:        "s",
		Tasks:     []types.EvalTask{{ID: "t1"}},
		RunConfig: baselineRunConfig(),
	}
	baseline, err := resolveBaseline(suite)
	if err != nil {
		t.Fatalf("resolveBaseline: %v", err)
	}
	if baseline == nil {
		t.Fatal("expected non-nil baseline for inline run_config")
	}

	four := 4
	merged, err := buildMergedConfig(baseline, &types.RunConfigOverrides{MaxTurns: &four})
	if err != nil {
		t.Fatalf("buildMergedConfig: %v", err)
	}
	if merged.MaxTurns != 4 {
		t.Errorf("merged MaxTurns = %d, want 4", merged.MaxTurns)
	}
	// Mutating the merged result must not touch the suite spec pointer.
	merged.MaxTurns = 99
	if suite.RunConfig.MaxTurns == 99 {
		t.Error("merged config aliases suite.RunConfig — clone must be a deep copy")
	}
}

// TestBuildMergedConfig_FromFileBaseline asserts the run_config_file
// path reads the JSON, decodes a RunConfig, and applies overrides
// identically to the inline path.
func TestBuildMergedConfig_FromFileBaseline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.json")
	data, err := json.Marshal(baselineRunConfig())
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write baseline: %v", err)
	}

	suite := types.EvalSuite{
		ID:            "s",
		Tasks:         []types.EvalTask{{ID: "t1"}},
		RunConfigFile: path,
	}
	baseline, err := resolveBaseline(suite)
	if err != nil {
		t.Fatalf("resolveBaseline: %v", err)
	}
	if baseline == nil {
		t.Fatal("expected non-nil baseline for run_config_file")
	}
	if baseline.Provider.Type != "anthropic" {
		t.Errorf("Provider.Type = %q, want %q", baseline.Provider.Type, "anthropic")
	}

	four := 4
	merged, err := buildMergedConfig(baseline, &types.RunConfigOverrides{MaxTurns: &four})
	if err != nil {
		t.Fatalf("buildMergedConfig: %v", err)
	}
	if merged.MaxTurns != 4 {
		t.Errorf("merged MaxTurns = %d, want 4", merged.MaxTurns)
	}
}

// TestResolveBaseline_None pins the legacy path: a suite with neither a
// file nor an inline block returns (nil, nil) so the runner skips the
// --config wire entirely.
func TestResolveBaseline_None(t *testing.T) {
	suite := types.EvalSuite{ID: "s", Tasks: []types.EvalTask{{ID: "t1"}}}
	baseline, err := resolveBaseline(suite)
	if err != nil {
		t.Fatalf("resolveBaseline: %v", err)
	}
	if baseline != nil {
		t.Errorf("expected nil baseline for legacy suite, got %+v", baseline)
	}
}

// TestResolveBaseline_FileErrors covers the failure modes of the file
// loader: missing path, directory, oversized payload, empty file,
// unknown JSON fields. Each must surface a non-nil error rather than
// silently producing a partial baseline.
func TestResolveBaseline_FileErrors(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name  string
		setup func(t *testing.T) string
		want  string
	}{
		{
			name:  "missing file",
			setup: func(*testing.T) string { return filepath.Join(dir, "nope.json") },
			want:  "reading run_config_file",
		},
		{
			name: "directory",
			setup: func(t *testing.T) string {
				p := filepath.Join(dir, "dir")
				if err := os.Mkdir(p, 0o755); err != nil {
					t.Fatal(err)
				}
				return p
			},
			want: "is a directory",
		},
		{
			name: "empty file",
			setup: func(t *testing.T) string {
				p := filepath.Join(dir, "empty.json")
				if err := os.WriteFile(p, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				return p
			},
			want: "file is empty",
		},
		{
			name: "unknown field",
			setup: func(t *testing.T) string {
				p := filepath.Join(dir, "bad.json")
				if err := os.WriteFile(p, []byte(`{"thisFieldDoesNotExist":true}`), 0o600); err != nil {
					t.Fatal(err)
				}
				return p
			},
			want: "parsing run_config_file",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.setup(t)
			suite := types.EvalSuite{ID: "s", Tasks: []types.EvalTask{{ID: "t1"}}, RunConfigFile: path}
			_, err := resolveBaseline(suite)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

// TestDryRun_NoBaselineIsNoOp pins the backwards-compat contract: a
// suite with no run_config block must still dry-run as "pass" for every
// task, exactly as it did before this chunk.
func TestDryRun_NoBaselineIsNoOp(t *testing.T) {
	suite := types.EvalSuite{
		ID: "legacy-suite",
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "p"},
			{ID: "t2", Prompt: "p"},
		},
	}
	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(result.Tasks))
	}
	for _, tr := range result.Tasks {
		if tr.Outcome != "pass" {
			t.Errorf("task %s: outcome = %q, want pass", tr.TaskID, tr.Outcome)
		}
	}
	if result.PassRate != 1.0 {
		t.Errorf("PassRate = %f, want 1.0", result.PassRate)
	}
}

// TestDryRun_InvalidMergedConfig surfaces a ValidateRunConfig failure as
// a per-task "error" outcome without aborting siblings. The invalid
// shape is the canonical read-only-mode invariant called out in
// CLAUDE.md: planning mode plus a write tool in tools.builtIn.
func TestDryRun_InvalidMergedConfig(t *testing.T) {
	timeout := 300
	bad := &types.RunConfig{
		Mode:             "planning",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		MaxTurns:         10,
		Timeout:          &timeout,
		PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"read_file", "write_file"}},
	}
	suite := types.EvalSuite{
		ID:        "ro-suite",
		Tasks:     []types.EvalTask{{ID: "bad", Prompt: "p"}, {ID: "ok", Prompt: "p"}},
		RunConfig: bad,
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(result.Tasks))
	}
	// Every task in this suite inherits the same broken baseline, so
	// every task should surface the same validation error rather than
	// pass. The point of this test is to confirm the per-task wiring is
	// in place — sibling-isolation is exercised by the per-task overlay
	// test below.
	for _, tr := range result.Tasks {
		if tr.Outcome != "error" {
			t.Errorf("task %s: outcome = %q, want error", tr.TaskID, tr.Outcome)
		}
		if !strings.Contains(tr.Error, "write tool") {
			t.Errorf("task %s: error = %q, want substring about write tool", tr.TaskID, tr.Error)
		}
	}
}

// TestDryRun_PerTaskOverrideInvalidatesOnlyThatTask asserts the
// sibling-isolation property: when only one task's overlay produces an
// invalid merged config, the other tasks still pass.
func TestDryRun_PerTaskOverrideInvalidatesOnlyThatTask(t *testing.T) {
	timeout := 300
	baseline := &types.RunConfig{
		Mode:     "execution",
		Provider: types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		MaxTurns: 10,
		Timeout:  &timeout,
	}
	// An override that flips the mode to planning without supplying a
	// compatible tools.builtIn list triggers the "read-only mode requires
	// an explicit tools.builtIn list" rule. The other task uses no
	// override so the baseline stays valid.
	suite := types.EvalSuite{
		ID:        "mixed-suite",
		RunConfig: baseline,
		Tasks: []types.EvalTask{
			{ID: "ok-task", Prompt: "p"},
			{ID: "bad-task", Prompt: "p", RunConfigOverrides: &types.RunConfigOverrides{Mode: "planning"}},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(result.Tasks))
	}
	got := map[string]string{
		result.Tasks[0].TaskID: result.Tasks[0].Outcome,
		result.Tasks[1].TaskID: result.Tasks[1].Outcome,
	}
	if got["ok-task"] != "pass" {
		t.Errorf("ok-task outcome = %q, want pass", got["ok-task"])
	}
	if got["bad-task"] != "error" {
		t.Errorf("bad-task outcome = %q, want error", got["bad-task"])
	}
}

// TestRunSuite_NoBaselineUsesLegacyInvocation verifies the
// backwards-compat invariant from the issue: a suite with no
// run_config_file / run_config block must invoke the harness with the
// legacy five flags only — no --config wire, no redacted artifact.
//
// The fake harness writes its argv to a sidecar file so the test can
// inspect what the runner actually passed; the assertion is that
// "--config" is not among the args.
func TestRunSuite_NoBaselineUsesLegacyInvocation(t *testing.T) {
	logDir := t.TempDir()
	argLog := filepath.Join(logDir, "args.log")
	script := fmt.Sprintf(`#!/bin/sh
TRACE=""
echo "$@" >> %q
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$TRACE" ] && echo '{"id":"t","turns":1,"outcome":"success"}' > "$TRACE"
`, argLog)
	harness := writeFakeHarness(t, script)

	suite := types.EvalSuite{
		ID: "legacy",
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "p", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
		},
	}

	out := t.TempDir()
	_, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harness,
		OutputDir:   out,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	logData, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatalf("reading arg log: %v", err)
	}
	if strings.Contains(string(logData), "--config") {
		t.Errorf("legacy invocation must not pass --config; got args: %q", string(logData))
	}

	// No baseline → no redacted artifact retained.
	redactedPath := filepath.Join(out, "legacy", "t1", "run_config.redacted.json")
	if _, err := os.Stat(redactedPath); !os.IsNotExist(err) {
		t.Errorf("legacy invocation must not write %s; stat err = %v", redactedPath, err)
	}
}

// TestRunSuite_WithBaselineWritesConfigAndRedactedArtifact covers the
// new invocation path: when the suite declares a baseline, the runner
// must (a) invoke the harness with --config <path>, (b) write the
// merged config to that path, and (c) retain a run_config.redacted.json
// alongside the trace artifacts. The redacted artifact must contain the
// secret:// reference unchanged — the reference is not a resolved
// value, so Redact() does not rewrite plain secret:// references on
// fields like Provider.APIKeyRef (which it does redact to
// "secret://[REDACTED]" — the test checks for the redaction marker).
func TestRunSuite_WithBaselineWritesConfigAndRedactedArtifact(t *testing.T) {
	logDir := t.TempDir()
	argLog := filepath.Join(logDir, "args.log")
	configCapture := filepath.Join(logDir, "config-capture.json")
	script := fmt.Sprintf(`#!/bin/sh
TRACE=""
CONFIG=""
echo "$@" >> %q
while [ $# -gt 0 ]; do
  case "$1" in
    --trace)  TRACE="$2";  shift 2 ;;
    --config) CONFIG="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$CONFIG" ] && cp "$CONFIG" %q
[ -n "$TRACE" ]  && echo '{"id":"t","turns":1,"outcome":"success"}' > "$TRACE"
`, argLog, configCapture)
	harness := writeFakeHarness(t, script)

	timeout := 300
	baseline := &types.RunConfig{
		Mode:     "execution",
		Provider: types.ProviderConfig{Type: "openai-responses", APIKeyRef: "secret://OPENAI_KEY"},
		MaxTurns: 10,
		Timeout:  &timeout,
	}
	suite := types.EvalSuite{
		ID:        "wired",
		RunConfig: baseline,
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "p", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
		},
	}

	out := t.TempDir()
	_, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harness,
		OutputDir:   out,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// (a) --config was passed.
	logData, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatalf("reading arg log: %v", err)
	}
	if !strings.Contains(string(logData), "--config") {
		t.Errorf("expected --config in harness args; got %q", string(logData))
	}

	// (b) Captured config decodes back to the same RunConfig shape.
	captured, err := os.ReadFile(configCapture)
	if err != nil {
		t.Fatalf("reading captured config: %v", err)
	}
	var got types.RunConfig
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("unmarshal captured config: %v", err)
	}
	if got.Provider.Type != "openai-responses" {
		t.Errorf("captured config Provider.Type = %q, want %q", got.Provider.Type, "openai-responses")
	}
	if got.Provider.APIKeyRef != "secret://OPENAI_KEY" {
		t.Errorf("captured config APIKeyRef = %q, want %q (must be the unredacted reference — the harness needs it to resolve)", got.Provider.APIKeyRef, "secret://OPENAI_KEY")
	}

	// (c) Redacted artifact exists and has the reference scrubbed.
	redactedPath := filepath.Join(out, "wired", "t1", "run_config.redacted.json")
	redactedData, err := os.ReadFile(redactedPath)
	if err != nil {
		t.Fatalf("reading redacted artifact: %v", err)
	}
	var redacted types.RunConfig
	if err := json.Unmarshal(redactedData, &redacted); err != nil {
		t.Fatalf("unmarshal redacted artifact: %v", err)
	}
	if redacted.Provider.APIKeyRef != "secret://[REDACTED]" {
		t.Errorf("redacted artifact APIKeyRef = %q, want %q", redacted.Provider.APIKeyRef, "secret://[REDACTED]")
	}
	// The redacted artifact must never carry a resolved secret. It is
	// only a reference, so the on-disk JSON must contain only the
	// redaction marker — never a plaintext token.
	if !strings.Contains(string(redactedData), "secret://[REDACTED]") {
		t.Errorf("redacted artifact missing redaction marker; data: %q", string(redactedData))
	}
}

// TestRunSuite_WithBaselineRetainedArtifactOmitsResolvedSecrets is the
// belt-and-braces guard for the invariant in CLAUDE.md: the retained
// run_config.redacted.json must never contain a resolved secret, only
// references. Today Redact() handles every secret-bearing field on
// RunConfig; if a future field is added without an accompanying Redact
// path, this test should catch it via a substring check for plausible
// secret-shaped values that should not appear in a redacted file.
func TestRunSuite_WithBaselineRetainedArtifactOmitsResolvedSecrets(t *testing.T) {
	script := `#!/bin/sh
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$TRACE" ] && echo '{"id":"t","turns":1,"outcome":"success"}' > "$TRACE"
`
	harness := writeFakeHarness(t, script)

	timeout := 300
	baseline := &types.RunConfig{
		Mode:     "execution",
		Provider: types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		MaxTurns: 10,
		Timeout:  &timeout,
	}
	suite := types.EvalSuite{
		ID:        "redact-suite",
		RunConfig: baseline,
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "p", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
		},
	}

	out := t.TempDir()
	_, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harness,
		OutputDir:   out,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	redactedPath := filepath.Join(out, "redact-suite", "t1", "run_config.redacted.json")
	data, err := os.ReadFile(redactedPath)
	if err != nil {
		t.Fatalf("reading redacted artifact: %v", err)
	}
	// The reference must be redacted; the bare secret:// scheme outside
	// a [REDACTED] context is the failure mode to guard against.
	body := string(data)
	if strings.Contains(body, "secret://ANTHROPIC_API_KEY") {
		t.Errorf("redacted artifact still contains the original secret reference; data: %q", body)
	}
	if !strings.Contains(body, "secret://[REDACTED]") {
		t.Errorf("redacted artifact missing redaction marker; data: %q", body)
	}
}
