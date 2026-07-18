package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// baselineRunConfig builds a valid RunConfig satisfying
// types.ValidateRunConfig; tests vary fields from this base.
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
// untouched.
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

// TestMergeOverrides_MultipleFields covers an overlay touching a pointer
// field (Provider) and a scalar field (Mode) together; pointer copies
// are deref'd so the merged config does not alias overlay memory.
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

// TestMergeOverrides_NilBaseline asserts a nil baseline returns nil
// rather than fabricating a half-formed config.
func TestMergeOverrides_NilBaseline(t *testing.T) {
	four := 4
	got := mergeOverrides(nil, &types.RunConfigOverrides{MaxTurns: &four})
	if got != nil {
		t.Errorf("nil baseline must return nil, got %+v", got)
	}
}

// TestBuildMergedConfig_FromInlineBaseline exercises the suite-inline
// baseline path end-to-end: clone, apply overrides, return a fresh
// allocation not aliasing the suite's original RunConfig pointer.
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
// path reads and decodes a RunConfig and applies overrides identically
// to the inline path.
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
// file nor an inline block returns (nil, nil).
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
// unknown JSON fields.
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
			// After the open-then-fstat rewrite, the directory case
			// surfaces via the "not a regular file" guard, which is
			// the same path that catches FIFOs and device files.
			want: "not a regular file",
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
		{
			// One byte over the cap; JSON parseability is irrelevant
			// since the size guard fires before json.Decode.
			name: "oversize",
			setup: func(t *testing.T) string {
				p := filepath.Join(dir, "oversize.json")
				// The size guard fires before json.Decode runs.
				big := bytes.Repeat([]byte("x"), int(maxRunConfigFileBytes)+1)
				if err := os.WriteFile(p, big, 0o600); err != nil {
					t.Fatal(err)
				}
				return p
			},
			want: "exceeds",
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

// TestResolveBaseline_RejectsFIFO confirms a named pipe at
// run_config_file is rejected by the IsRegular() check in
// loadRunConfigFile rather than blocking os.ReadFile indefinitely.
// FIFOs are a POSIX construct; the test skips on non-unix platforms.
func TestResolveBaseline_RejectsFIFO(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFOs unsupported on Windows")
	}
	dir := t.TempDir()
	fifo := filepath.Join(dir, "evil-fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported on this platform: %v", err)
	}

	suite := types.EvalSuite{ID: "s", Tasks: []types.EvalTask{{ID: "t1"}}, RunConfigFile: fifo}
	_, err := resolveBaseline(suite)
	if err == nil {
		t.Fatal("expected error opening a FIFO, got nil")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error = %q, want substring %q", err.Error(), "not a regular file")
	}
}

// TestResolveBaseline_RejectsBothFileAndInlineBlock pins the
// mutual-exclusion guard for Go callers that construct EvalSuite
// directly, bypassing the HCL parser's own check.
func TestResolveBaseline_RejectsBothFileAndInlineBlock(t *testing.T) {
	suite := types.EvalSuite{
		ID:            "dual",
		Tasks:         []types.EvalTask{{ID: "t1"}},
		RunConfigFile: "/tmp/whatever.json",
		RunConfig:     baselineRunConfig(),
	}
	_, err := resolveBaseline(suite)
	if err == nil {
		t.Fatal("expected error when both run_config_file and run_config are set")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %q, want substring %q", err.Error(), "mutually exclusive")
	}
	if !strings.Contains(err.Error(), "dual") {
		t.Errorf("error = %q, want it to name the suite ID", err.Error())
	}
}

// TestMergeOverrides_AllOverlayFields fans the merge contract over
// every pointer-typed overlay field (ModelRouter, ContextStrategy,
// EditStrategy, Verifier, MaxTurns).
func TestMergeOverrides_AllOverlayFields(t *testing.T) {
	t.Run("ModelRouter", func(t *testing.T) {
		baseline := baselineRunConfig()
		overlay := &types.RunConfigOverrides{
			ModelRouter: &types.ModelRouterConfig{Type: "static", Model: "claude-haiku-4-5"},
		}
		got := mergeOverrides(baseline, overlay)
		if got.ModelRouter.Type != "static" || got.ModelRouter.Model != "claude-haiku-4-5" {
			t.Errorf("ModelRouter = %#v, want {Type:static Model:claude-haiku-4-5}", got.ModelRouter)
		}
	})

	t.Run("ContextStrategy", func(t *testing.T) {
		baseline := baselineRunConfig()
		overlay := &types.RunConfigOverrides{
			ContextStrategy: &types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 12000},
		}
		got := mergeOverrides(baseline, overlay)
		if got.ContextStrategy.Type != "sliding-window" || got.ContextStrategy.MaxTokens != 12000 {
			t.Errorf("ContextStrategy = %#v, want {Type:sliding-window MaxTokens:12000}", got.ContextStrategy)
		}
	})

	t.Run("EditStrategy", func(t *testing.T) {
		baseline := baselineRunConfig()
		threshold := 0.7
		overlay := &types.RunConfigOverrides{
			EditStrategy: &types.EditStrategyConfig{Type: "multi", FuzzyThreshold: &threshold},
		}
		got := mergeOverrides(baseline, overlay)
		if got.EditStrategy.Type != "multi" {
			t.Errorf("EditStrategy.Type = %q, want multi", got.EditStrategy.Type)
		}
		if got.EditStrategy.FuzzyThreshold == nil || *got.EditStrategy.FuzzyThreshold != 0.7 {
			t.Errorf("EditStrategy.FuzzyThreshold = %v, want pointer to 0.7", got.EditStrategy.FuzzyThreshold)
		}
	})

	t.Run("Verifier", func(t *testing.T) {
		baseline := baselineRunConfig()
		overlay := &types.RunConfigOverrides{
			Verifier: &types.VerifierConfig{Type: "test-runner", Command: "go test ./..."},
		}
		got := mergeOverrides(baseline, overlay)
		if got.Verifier.Type != "test-runner" || got.Verifier.Command != "go test ./..." {
			t.Errorf("Verifier = %#v, want {Type:test-runner Command:go test ./...}", got.Verifier)
		}
	})

	t.Run("MaxTurns", func(t *testing.T) {
		baseline := baselineRunConfig()
		six := 6
		overlay := &types.RunConfigOverrides{MaxTurns: &six}
		got := mergeOverrides(baseline, overlay)
		if got.MaxTurns != 6 {
			t.Errorf("MaxTurns = %d, want 6", got.MaxTurns)
		}
	})
}

// TestMergeOverrides_ZeroModeDoesNotOverwrite confirms the Mode
// sentinel contract: the empty string on the overlay means "unset"
// and must not clobber a baseline that already carries a concrete
// mode. Without this, an overlay constructed with only pointer-typed
// fields would zero the baseline's Mode.
func TestMergeOverrides_ZeroModeDoesNotOverwrite(t *testing.T) {
	baseline := baselineRunConfig() // Mode = "execution"
	four := 4
	overlay := &types.RunConfigOverrides{MaxTurns: &four}

	got := mergeOverrides(baseline, overlay)
	if got.Mode != "execution" {
		t.Errorf("Mode = %q, want execution (unchanged by zero-value overlay)", got.Mode)
	}
}

// TestBuildMergedConfig_FileBaselineWithTaskOverride covers the
// combined path: a file-based baseline plus a per-task overlay. The
// merged config must reflect both the baseline's provider/timeout and
// the overlay's MaxTurns.
func TestBuildMergedConfig_FileBaselineWithTaskOverride(t *testing.T) {
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

	six := 6
	overlay := &types.RunConfigOverrides{
		MaxTurns: &six,
		Provider: &types.ProviderConfig{Type: "openai-responses", APIKeyRef: "secret://OPENAI_KEY"},
	}
	merged, err := buildMergedConfig(baseline, overlay)
	if err != nil {
		t.Fatalf("buildMergedConfig: %v", err)
	}
	if merged.MaxTurns != 6 {
		t.Errorf("MaxTurns = %d, want 6 (overlay)", merged.MaxTurns)
	}
	if merged.Provider.Type != "openai-responses" {
		t.Errorf("Provider.Type = %q, want openai-responses (overlay)", merged.Provider.Type)
	}
	if merged.Timeout == nil || *merged.Timeout != 300 {
		t.Errorf("Timeout = %v, want pointer to 300 (baseline)", merged.Timeout)
	}
}

// TestBuildMergedConfig_InjectsDefaultTimeoutWhenAbsent pins the
// timeout-injection contract: the HCL grammar does not surface
// `timeout`, so a merged config from an inline block arrives with
// Timeout == nil and must get the runner's default injected.
func TestBuildMergedConfig_InjectsDefaultTimeoutWhenAbsent(t *testing.T) {
	baseline := &types.RunConfig{
		Mode:     "execution",
		Provider: types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		MaxTurns: 10,
		// Timeout intentionally left nil — emulates an inline run_config
		// block where the HCL grammar does not expose `timeout`.
	}
	merged, err := buildMergedConfig(baseline, nil)
	if err != nil {
		t.Fatalf("buildMergedConfig: %v", err)
	}
	if merged == nil {
		t.Fatal("expected non-nil merged config")
	}
	if merged.Timeout == nil {
		t.Fatal("expected default Timeout to be injected, got nil")
	}
	if *merged.Timeout != defaultTaskTimeoutSeconds {
		t.Errorf("Timeout = %d, want %d (default)", *merged.Timeout, defaultTaskTimeoutSeconds)
	}
}

// TestBuildMergedConfig_PreservesExplicitTimeout confirms a Timeout
// already pinned by the baseline survives the merge unchanged rather
// than being overwritten by the default.
func TestBuildMergedConfig_PreservesExplicitTimeout(t *testing.T) {
	explicit := 900
	baseline := &types.RunConfig{
		Mode:     "execution",
		Provider: types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		MaxTurns: 10,
		Timeout:  &explicit,
	}
	merged, err := buildMergedConfig(baseline, nil)
	if err != nil {
		t.Fatalf("buildMergedConfig: %v", err)
	}
	if merged.Timeout == nil || *merged.Timeout != 900 {
		t.Errorf("Timeout = %v, want pointer to 900 (baseline value preserved)", merged.Timeout)
	}
}

// TestDryRun_InlineConfigWithoutTimeoutPasses asserts a suite with an
// inline run_config block that omits `timeout` still passes dry-run
// validation via the timeout-injection contract.
func TestDryRun_InlineConfigWithoutTimeoutPasses(t *testing.T) {
	baseline := &types.RunConfig{
		Mode:     "execution",
		Provider: types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		MaxTurns: 10,
	}
	suite := types.EvalSuite{
		ID:        "inline-no-timeout",
		Tasks:     []types.EvalTask{{ID: "t1", Prompt: "p"}},
		RunConfig: baseline,
	}
	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(result.Tasks))
	}
	if result.Tasks[0].Outcome != "pass" {
		t.Errorf("outcome = %q (error %q), want pass — inline configs without timeout should pass dry-run after injection",
			result.Tasks[0].Outcome, result.Tasks[0].Error)
	}
}

// TestDryRun_NoBaselineIsNoOp pins the backwards-compat contract: a
// suite with no run_config block must still dry-run as "pass" for
// every task.
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
// a per-task "error" outcome without aborting siblings: planning mode
// plus a write tool in tools.builtIn.
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
	// Every task inherits the same broken baseline, so every task
	// should surface the same validation error rather than pass.
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
	// An override that flips the mode to planning without a compatible
	// tools.builtIn list triggers the read-only-mode rule; the other
	// task uses no override so the baseline stays valid.
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
// backwards-compat invariant: a suite with no run_config_file /
// run_config block must invoke the harness with the legacy five flags
// only — no --config wire, no redacted artifact. The fake harness
// writes its argv to a sidecar file so the test can inspect it.
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
// baseline invocation path: the runner must (a) invoke the harness
// with --config <path> and no shadowing flags, (b) write the merged
// config with TraceEmitter.FilePath set to the runner's trace path,
// and (c) retain a run_config.redacted.json alongside the trace
// artifacts.
func TestRunSuite_WithBaselineWritesConfigAndRedactedArtifact(t *testing.T) {
	logDir := t.TempDir()
	argLog := filepath.Join(logDir, "args.log")
	configCapture := filepath.Join(logDir, "config-capture.json")
	// The fake harness reads the trace path out of the merged config.
	script := fmt.Sprintf(`#!/bin/sh
CONFIG=""
echo "$@" >> %q
while [ $# -gt 0 ]; do
  case "$1" in
    --config) CONFIG="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$CONFIG" ]; then
  cp "$CONFIG" %q
  # Extract trace_emitter.file_path with a sed grep (the merged
  # config is single-line JSON). Tolerant of leading/trailing whitespace.
  TRACE=$(sed -n 's/.*"filePath":"\([^"]*\)".*/\1/p' "$CONFIG")
  [ -n "$TRACE" ] && echo '{"id":"t","turns":1,"outcome":"success"}' > "$TRACE"
fi
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

	// (a) --config was passed, and the shadowing flags were not.
	logData, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatalf("reading arg log: %v", err)
	}
	logStr := string(logData)
	if !strings.Contains(logStr, "--config") {
		t.Errorf("expected --config in harness args; got %q", logStr)
	}
	for _, banned := range []string{"--timeout", "--mode", "--trace"} {
		if strings.Contains(logStr, banned) {
			t.Errorf("merged-config invocation must not pass %s; got args: %q", banned, logStr)
		}
	}

	// (b) Captured config decodes back to the same RunConfig shape,
	// with TraceEmitter.FilePath populated by the runner.
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
	if got.TraceEmitter.FilePath == "" {
		t.Errorf("captured config TraceEmitter.FilePath is empty; runner must inject the per-task trace path")
	}
	if !strings.HasSuffix(got.TraceEmitter.FilePath, "trace.jsonl") {
		t.Errorf("captured config TraceEmitter.FilePath = %q, want a path ending in trace.jsonl", got.TraceEmitter.FilePath)
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

	// (d) retained redacted config must be mode 0o600 — it carries
	// operator posture and must not be world-readable on shared CI runners.
	info, err := os.Stat(redactedPath)
	if err != nil {
		t.Fatalf("stat redacted artifact: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("redacted artifact perm = %o, want 0600", perm)
	}
}

// TestRunSuite_HarnessFailWithTracePreservesVerdict covers the
// runTask branch where the harness exits non-zero but still leaves a
// usable trace: the runner must consult the judge rather than
// discarding the trace and reporting "error".
func TestRunSuite_HarnessFailWithTracePreservesVerdict(t *testing.T) {
	script := `#!/bin/sh
TRACE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --trace) TRACE="$2"; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$TRACE" ] && echo '{"id":"t","turns":1,"outcome":"success"}' > "$TRACE"
# Harness reports a non-zero exit despite emitting a trace — e.g. a
# tool exited 1 but the agent loop closed cleanly. The runner should
# still consult the judge.
exit 1
`
	harness := writeFakeHarness(t, script)

	suite := types.EvalSuite{
		ID: "harness-fail-with-trace",
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "p", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"definitely-not-created.txt"}}},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
	if err != nil {
		t.Fatalf("RunSuite error: %v", err)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(result.Tasks))
	}
	// Judge rejects the missing file, but the trace was consumed.
	if result.Tasks[0].Outcome != "fail" {
		t.Errorf("outcome = %q, want fail (judge rejected, trace consumed)", result.Tasks[0].Outcome)
	}
}

// TestRunSuite_CloneRepoFailureSurfacedAsError exercises the
// runTask repo-clone error path: a nonexistent remote URL makes
// `git clone` fail quickly and the runner must report "error"
// without spawning the harness.
func TestRunSuite_CloneRepoFailureSurfacedAsError(t *testing.T) {
	// Skip when git isn't installed; the error path is otherwise unreachable.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available on PATH")
	}
	script := `#!/bin/sh
echo "should not be invoked" >&2
exit 99
`
	harness := writeFakeHarness(t, script)

	suite := types.EvalSuite{
		ID: "clone-fail",
		Tasks: []types.EvalTask{
			{
				ID:     "t1",
				Prompt: "p",
				Repo:   "https://invalid.localhost.invalid/does-not-exist.git",
				Judge:  types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}},
			},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
	if err != nil {
		t.Fatalf("RunSuite error: %v", err)
	}
	if result.Tasks[0].Outcome != "error" {
		t.Errorf("outcome = %q, want error", result.Tasks[0].Outcome)
	}
	if !strings.Contains(result.Tasks[0].Error, "cloning repo") {
		t.Errorf("error = %q, want it to mention cloning repo", result.Tasks[0].Error)
	}
}

// TestRunSuite_HarnessFailWithoutTraceErrors covers the runTask
// branch where the harness exits non-zero AND fails to leave a
// usable trace. The outcome must be "error" and the surfaced error
// must include the harness's stderr so the operator can diagnose.
func TestRunSuite_HarnessFailWithoutTraceErrors(t *testing.T) {
	script := `#!/bin/sh
echo "harness boot failure" >&2
exit 2
`
	harness := writeFakeHarness(t, script)

	suite := types.EvalSuite{
		ID: "harness-fail-no-trace",
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "p", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
	if err != nil {
		t.Fatalf("RunSuite error: %v", err)
	}
	if result.Tasks[0].Outcome != "error" {
		t.Errorf("outcome = %q, want error", result.Tasks[0].Outcome)
	}
	if !strings.Contains(result.Tasks[0].Error, "harness boot failure") {
		t.Errorf("error = %q, want it to include harness stderr", result.Tasks[0].Error)
	}
}

// TestRunSuite_HarnessSuccessWithoutTraceErrors covers the parse-trace
// error path after a clean harness exit that emits no trace.
func TestRunSuite_HarnessSuccessWithoutTraceErrors(t *testing.T) {
	script := `#!/bin/sh
exit 0
`
	harness := writeFakeHarness(t, script)

	suite := types.EvalSuite{
		ID: "harness-noop",
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "p", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
	if err != nil {
		t.Fatalf("RunSuite error: %v", err)
	}
	if result.Tasks[0].Outcome != "error" {
		t.Errorf("outcome = %q, want error", result.Tasks[0].Outcome)
	}
	if !strings.Contains(result.Tasks[0].Error, "parsing trace") {
		t.Errorf("error = %q, want it to mention parsing trace", result.Tasks[0].Error)
	}
}

// TestBuildMergedConfig_NilBaseline pins the legacy-invocation
// contract on the merge helper: a nil baseline returns (nil, nil)
// so runTask falls through to the legacy flag-only path.
func TestBuildMergedConfig_NilBaseline(t *testing.T) {
	four := 4
	merged, err := buildMergedConfig(nil, &types.RunConfigOverrides{MaxTurns: &four})
	if err != nil {
		t.Fatalf("buildMergedConfig: %v", err)
	}
	if merged != nil {
		t.Errorf("expected nil merged config for nil baseline, got %#v", merged)
	}
}

// TestRunSuite_FailOutcomeOnJudgeReject covers the buildResult
// fail-outcome branch: harness success plus Passed=false must yield
// "fail", not "error".
func TestRunSuite_FailOutcomeOnJudgeReject(t *testing.T) {
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

	suite := types.EvalSuite{
		ID: "legacy-fail",
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "p", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"definitely-not-created.txt"}}},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harness})
	if err != nil {
		t.Fatalf("RunSuite error: %v", err)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(result.Tasks))
	}
	if result.Tasks[0].Outcome != "fail" {
		t.Errorf("outcome = %q, want fail", result.Tasks[0].Outcome)
	}
}

// TestRunTask_RejectsInvalidMergedConfigBeforeSubprocess asserts that
// when the merged RunConfig fails ValidateRunConfig, the runner
// surfaces a per-task "error" outcome without launching the harness.
func TestRunTask_RejectsInvalidMergedConfigBeforeSubprocess(t *testing.T) {
	logDir := t.TempDir()
	argLog := filepath.Join(logDir, "args.log")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
`, argLog)
	harness := writeFakeHarness(t, script)

	timeout := 300
	// Review mode requires a restrictive permission policy; allow-all
	// trips the read-only-mode invariant in ValidateRunConfig.
	bad := &types.RunConfig{
		Mode:             "review",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_KEY"},
		MaxTurns:         10,
		Timeout:          &timeout,
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"read_file"}},
	}
	suite := types.EvalSuite{
		ID:        "ro-suite",
		RunConfig: bad,
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "p", Judge: types.EvalJudge{Type: "file-exists", Paths: []string{"placeholder"}}},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harness,
	})
	if err != nil {
		t.Fatalf("RunSuite error: %v", err)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1", len(result.Tasks))
	}
	if result.Tasks[0].Outcome != "error" {
		t.Errorf("outcome = %q, want error", result.Tasks[0].Outcome)
	}
	if !strings.Contains(result.Tasks[0].Error, "review") {
		t.Errorf("error = %q, want it to name the offending mode", result.Tasks[0].Error)
	}

	// Subprocess must not have launched.
	if _, err := os.Stat(argLog); err == nil {
		t.Errorf("harness was invoked despite validation failure; arg log %s exists", argLog)
	}
}

// TestRunSuite_WithBaselineRetainedArtifactOmitsResolvedSecrets asserts
// the retained run_config.redacted.json never contains a resolved
// secret, only references.
func TestRunSuite_WithBaselineRetainedArtifactOmitsResolvedSecrets(t *testing.T) {
	// The fake harness reads the trace path out of the merged config.
	script := `#!/bin/sh
CONFIG=""
while [ $# -gt 0 ]; do
  case "$1" in
    --config) CONFIG="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$CONFIG" ]; then
  TRACE=$(sed -n 's/.*"filePath":"\([^"]*\)".*/\1/p' "$CONFIG")
  [ -n "$TRACE" ] && echo '{"id":"t","turns":1,"outcome":"success"}' > "$TRACE"
fi
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
	// The bare secret:// scheme outside a [REDACTED] context is the
	// failure mode to guard against.
	body := string(data)
	if strings.Contains(body, "secret://ANTHROPIC_API_KEY") {
		t.Errorf("redacted artifact still contains the original secret reference; data: %q", body)
	}
	if !strings.Contains(body, "secret://[REDACTED]") {
		t.Errorf("redacted artifact missing redaction marker; data: %q", body)
	}
}

// TestWarnIfRawAPIKeyRef pins the defense-in-depth warning: it must
// fire for every secret-bearing field on the merged config when a raw
// (non secret://) apiKeyRef reaches the retain-artifact path.
func TestWarnIfRawAPIKeyRef(t *testing.T) {
	cfg := &types.RunConfig{
		Provider: types.ProviderConfig{Type: "anthropic", APIKeyRef: "sk-ant-raw"},
		Executor: types.ExecutorConfig{
			VcsBackend: &types.VcsBackendConfig{Type: "github", APIKeyRef: "ghp_rawvalue"},
		},
		Providers: map[string]types.ProviderConfig{
			"backup": {Type: "openai", APIKeyRef: "sk-openai-raw"},
		},
		Tools: types.ToolsConfig{
			MCPServers: []types.MCPServerConfig{
				{Name: "test", URI: "stdio:///test", APIKeyRef: "raw-mcp-key"},
			},
		},
	}

	stderr := captureStderr(t, func() { warnIfRawAPIKeyRef("t1", cfg) })

	for _, want := range []string{
		"provider.apiKeyRef",
		"executor.vcsBackend.apiKeyRef",
		"providers[backup].apiKeyRef",
		"tools.mcpServers[0].apiKeyRef",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q does not warn about %s", stderr, want)
		}
	}
}

// TestWarnIfRawAPIKeyRef_AllSecretRefsStaysSilent confirms the warning
// only fires for raw values; an all-secret:// config produces no
// stderr output.
func TestWarnIfRawAPIKeyRef_AllSecretRefsStaysSilent(t *testing.T) {
	cfg := &types.RunConfig{
		Provider: types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_KEY"},
		Executor: types.ExecutorConfig{
			VcsBackend: &types.VcsBackendConfig{Type: "github", APIKeyRef: "secret://GITHUB_TOKEN"},
		},
	}
	stderr := captureStderr(t, func() { warnIfRawAPIKeyRef("t1", cfg) })
	if stderr != "" {
		t.Errorf("expected no warnings for all-secret refs; got %q", stderr)
	}
}

// captureStderr replaces os.Stderr with a pipe for the duration of fn
// and returns whatever fn wrote. os.Stderr is process-global, so
// concurrent tests must not run this helper in parallel.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()

	fn()

	_ = w.Close()
	os.Stderr = origStderr
	<-done
	_ = r.Close()
	return buf.String()
}
