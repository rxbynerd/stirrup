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

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// baselineRunConfig builds a valid RunConfig satisfying
// types.ValidateRunConfig; tests vary fields from this base.
func baselineRunConfig() *types.RunConfig {
	timeout := 300
	return &types.RunConfig{
		Mode:             "execution",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		MaxTurns:         10,
		Timeout:          &timeout,
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
	}
}

// TestMergeOverrides_NoOverlay confirms a nil overlay leaves the baseline
// untouched.
func TestMergeOverrides_NoOverlay(t *testing.T) {
	baseline := baselineRunConfig()
	got := mergeOverrides(baseline, nil)
	require.NotNil(t, got, "expected non-nil merged config")
	require.Equal(t, 10, got.MaxTurns)
	require.Equal(t, "execution", got.Mode)
}

// TestMergeOverrides_SparseField asserts a sparse overlay only changes
// the named field; every other baseline field passes through unchanged.
func TestMergeOverrides_SparseField(t *testing.T) {
	baseline := baselineRunConfig()
	four := 4
	overlay := &types.RunConfigOverrides{MaxTurns: &four}

	got := mergeOverrides(baseline, overlay)
	require.Equal(t, 4, got.MaxTurns)
	require.Equal(t, "execution", got.Mode, "Mode is not in the overlay")
	require.Equal(t, "anthropic", got.Provider.Type, "Provider is not in the overlay")
}

// TestMergeOverrides_PointerFieldIsCopiedNotAliased covers an overlay
// touching a pointer field (Provider) and a scalar field (Mode) together;
// pointer copies are deref'd so the merged config does not alias overlay
// memory.
func TestMergeOverrides_PointerFieldIsCopiedNotAliased(t *testing.T) {
	baseline := baselineRunConfig()
	overlay := &types.RunConfigOverrides{
		Mode:     "planning",
		Provider: &types.ProviderConfig{Type: "openai-responses", APIKeyRef: "secret://OPENAI_KEY"},
	}

	got := mergeOverrides(baseline, overlay)
	require.Equal(t, "planning", got.Mode)
	require.Equal(t, "openai-responses", got.Provider.Type)
	// Pointer dereference, not alias: mutating the overlay's Provider
	// must not be observable on the merged result.
	overlay.Provider.Type = "mutated"
	require.NotEqual(t, "mutated", got.Provider.Type, "merged Provider aliases overlay-owned memory")
}

// TestMergeOverrides_NilBaseline asserts a nil baseline returns nil
// rather than fabricating a half-formed config.
func TestMergeOverrides_NilBaseline(t *testing.T) {
	four := 4
	got := mergeOverrides(nil, &types.RunConfigOverrides{MaxTurns: &four})
	require.Nil(t, got, "nil baseline must return nil")
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
	require.NoError(t, err)
	require.NotNil(t, baseline, "expected non-nil baseline for inline run_config")

	four := 4
	merged, err := buildMergedConfig(baseline, &types.RunConfigOverrides{MaxTurns: &four})
	require.NoError(t, err)
	require.Equal(t, 4, merged.MaxTurns)
	// Mutating the merged result must not touch the suite spec pointer.
	merged.MaxTurns = 99
	require.NotEqual(t, 99, suite.RunConfig.MaxTurns, "merged config aliases suite.RunConfig; clone must be a deep copy")
}

// TestBuildMergedConfig_FromFileBaseline asserts the run_config_file
// path reads and decodes a RunConfig and applies overrides identically
// to the inline path.
func TestBuildMergedConfig_FromFileBaseline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.json")
	data, err := json.Marshal(baselineRunConfig())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	suite := types.EvalSuite{
		ID:            "s",
		Tasks:         []types.EvalTask{{ID: "t1"}},
		RunConfigFile: path,
	}
	baseline, err := resolveBaseline(suite)
	require.NoError(t, err)
	require.NotNil(t, baseline, "expected non-nil baseline for run_config_file")
	require.Equal(t, "anthropic", baseline.Provider.Type)

	four := 4
	merged, err := buildMergedConfig(baseline, &types.RunConfigOverrides{MaxTurns: &four})
	require.NoError(t, err)
	require.Equal(t, 4, merged.MaxTurns)
}

// TestResolveBaseline_NoSourceReturnsNil pins the legacy path: a suite with
// neither a file nor an inline block returns (nil, nil).
func TestResolveBaseline_NoSourceReturnsNil(t *testing.T) {
	suite := types.EvalSuite{ID: "s", Tasks: []types.EvalTask{{ID: "t1"}}}
	baseline, err := resolveBaseline(suite)
	require.NoError(t, err)
	require.Nil(t, baseline, "expected nil baseline for legacy suite")
}

// TestResolveBaseline_RejectsInvalidFiles covers the failure modes of the
// file loader: missing path, directory, oversized payload, empty file,
// unknown JSON fields.
func TestResolveBaseline_RejectsInvalidFiles(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name        string
		setup       func(t *testing.T) string
		errContains string
	}{
		{
			name:        "missing file",
			setup:       func(*testing.T) string { return filepath.Join(dir, "nope.json") },
			errContains: "reading run_config_file",
		},
		{
			name: "directory",
			setup: func(t *testing.T) string {
				p := filepath.Join(dir, "dir")
				require.NoError(t, os.Mkdir(p, 0o755))
				return p
			},
			// Directories fail the regular-file guard, the same guard that
			// rejects FIFOs and device files.
			errContains: "not a regular file",
		},
		{
			name: "empty file",
			setup: func(t *testing.T) string {
				p := filepath.Join(dir, "empty.json")
				require.NoError(t, os.WriteFile(p, nil, 0o600))
				return p
			},
			errContains: "file is empty",
		},
		{
			name: "unknown field",
			setup: func(t *testing.T) string {
				p := filepath.Join(dir, "bad.json")
				require.NoError(t, os.WriteFile(p, []byte(`{"thisFieldDoesNotExist":true}`), 0o600))
				return p
			},
			errContains: "parsing run_config_file",
		},
		{
			// One byte over the cap; JSON parseability is irrelevant
			// since the size guard fires before json.Decode.
			name: "oversized file",
			setup: func(t *testing.T) string {
				p := filepath.Join(dir, "oversize.json")
				big := bytes.Repeat([]byte("x"), int(maxRunConfigFileBytes)+1)
				require.NoError(t, os.WriteFile(p, big, 0o600))
				return p
			},
			errContains: "exceeds",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.setup(t)
			suite := types.EvalSuite{ID: "s", Tasks: []types.EvalTask{{ID: "t1"}}, RunConfigFile: path}
			_, err := resolveBaseline(suite)
			require.ErrorContains(t, err, tc.errContains)
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
	require.Error(t, err, "expected error opening a FIFO")
	require.ErrorContains(t, err, "not a regular file")
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
	require.Error(t, err, "expected error when both run_config_file and run_config are set")
	require.ErrorContains(t, err, "mutually exclusive")
	require.ErrorContains(t, err, "dual", "error should name the suite ID")
}

// TestMergeOverrides_AllOverlayFields fans the merge contract over
// every pointer-typed overlay field (ModelRouter, ContextStrategy,
// EditStrategy, Verifier, MaxTurns).
func TestMergeOverrides_AllOverlayFields(t *testing.T) {
	t.Run("ModelRouter", func(t *testing.T) {
		baseline := baselineRunConfig()
		overlay := &types.RunConfigOverrides{
			ModelRouter: &types.ModelRouterConfig{Type: "static", Model: "claude-haiku-5-5"},
		}
		got := mergeOverrides(baseline, overlay)
		require.Equal(t, "static", got.ModelRouter.Type)
		require.Equal(t, "claude-haiku-5-5", got.ModelRouter.Model)
	})

	t.Run("ContextStrategy", func(t *testing.T) {
		baseline := baselineRunConfig()
		overlay := &types.RunConfigOverrides{
			ContextStrategy: &types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 12000},
		}
		got := mergeOverrides(baseline, overlay)
		require.Equal(t, "sliding-window", got.ContextStrategy.Type)
		require.Equal(t, 12000, got.ContextStrategy.MaxTokens)
	})

	t.Run("EditStrategy", func(t *testing.T) {
		baseline := baselineRunConfig()
		threshold := 0.7
		overlay := &types.RunConfigOverrides{
			EditStrategy: &types.EditStrategyConfig{Type: "multi", FuzzyThreshold: &threshold},
		}
		got := mergeOverrides(baseline, overlay)
		require.Equal(t, "multi", got.EditStrategy.Type)
		require.NotNil(t, got.EditStrategy.FuzzyThreshold)
		require.InDelta(t, 0.7, *got.EditStrategy.FuzzyThreshold, 1e-9)
	})

	t.Run("Verifier", func(t *testing.T) {
		baseline := baselineRunConfig()
		overlay := &types.RunConfigOverrides{
			Verifier: &types.VerifierConfig{Type: "test-runner", Command: "go test ./..."},
		}
		got := mergeOverrides(baseline, overlay)
		require.Equal(t, "test-runner", got.Verifier.Type)
		require.Equal(t, "go test ./...", got.Verifier.Command)
	})

	t.Run("MaxTurns", func(t *testing.T) {
		baseline := baselineRunConfig()
		six := 6
		overlay := &types.RunConfigOverrides{MaxTurns: &six}
		got := mergeOverrides(baseline, overlay)
		require.Equal(t, 6, got.MaxTurns)
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
	require.Equal(t, "execution", got.Mode, "zero-value overlay Mode must leave baseline Mode unchanged")
}

// TestBuildMergedConfig_FileBaselineWithTaskOverride covers the
// combined path: a file-based baseline plus a per-task overlay. The
// merged config must reflect both the baseline's provider/timeout and
// the overlay's MaxTurns.
func TestBuildMergedConfig_FileBaselineWithTaskOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.json")
	data, err := json.Marshal(baselineRunConfig())
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))

	suite := types.EvalSuite{
		ID:            "s",
		Tasks:         []types.EvalTask{{ID: "t1"}},
		RunConfigFile: path,
	}
	baseline, err := resolveBaseline(suite)
	require.NoError(t, err)

	six := 6
	overlay := &types.RunConfigOverrides{
		MaxTurns: &six,
		Provider: &types.ProviderConfig{Type: "openai-responses", APIKeyRef: "secret://OPENAI_KEY"},
	}
	merged, err := buildMergedConfig(baseline, overlay)
	require.NoError(t, err)
	require.Equal(t, 6, merged.MaxTurns, "overlay MaxTurns should win")
	require.Equal(t, "openai-responses", merged.Provider.Type, "overlay Provider should win")
	require.NotNil(t, merged.Timeout, "baseline Timeout should survive the merge")
	require.Equal(t, 300, *merged.Timeout)
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
	require.NoError(t, err)
	require.NotNil(t, merged, "expected non-nil merged config")
	require.NotNil(t, merged.Timeout, "expected default Timeout to be injected")
	require.Equal(t, defaultTaskTimeoutSeconds, *merged.Timeout)
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
	require.NoError(t, err)
	require.NotNil(t, merged.Timeout)
	require.Equal(t, 900, *merged.Timeout, "baseline Timeout should be preserved")
}

// TestDryRun_InlineConfigWithoutTimeoutPasses asserts a suite with an
// inline run_config block that omits `timeout` still passes dry-run
// validation via the timeout-injection contract.
func TestDryRun_InlineConfigWithoutTimeoutPasses(t *testing.T) {
	baseline := &types.RunConfig{
		Mode:             "execution",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		MaxTurns:         10,
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
	}
	suite := types.EvalSuite{
		ID:        "inline-no-timeout",
		Tasks:     []types.EvalTask{{ID: "t1", Prompt: "p", Judge: validJudge}},
		RunConfig: baseline,
	}
	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	require.NoError(t, err)
	require.Len(t, result.Tasks, 1)
	require.Equal(t, "pass", result.Tasks[0].Outcome,
		"inline configs without timeout should pass dry-run after injection; task error: %q",
		result.Tasks[0].Error)
}

// TestDryRun_NoBaselineIsNoOp pins the backwards-compat contract: a
// suite with no run_config block must still dry-run as "pass" for
// every task.
func TestDryRun_NoBaselineIsNoOp(t *testing.T) {
	suite := types.EvalSuite{
		ID: "legacy-suite",
		Tasks: []types.EvalTask{
			{ID: "t1", Prompt: "p", Judge: validJudge},
			{ID: "t2", Prompt: "p", Judge: validJudge},
		},
	}
	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	require.NoError(t, err)
	require.Len(t, result.Tasks, 2)
	for _, tr := range result.Tasks {
		t.Run(tr.TaskID, func(t *testing.T) {
			require.Equal(t, "pass", tr.Outcome)
		})
	}
	require.InDelta(t, 1.0, result.PassRate, 1e-9)
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
		Tasks:     []types.EvalTask{{ID: "bad", Prompt: "p", Judge: validJudge}, {ID: "ok", Prompt: "p", Judge: validJudge}},
		RunConfig: bad,
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	require.NoError(t, err)
	require.Len(t, result.Tasks, 2)
	// Every task inherits the same broken baseline, so every task
	// should surface the same validation error rather than pass.
	for _, tr := range result.Tasks {
		t.Run(tr.TaskID, func(t *testing.T) {
			require.Equal(t, "error", tr.Outcome)
			require.Contains(t, tr.Error, "write tool")
			require.Equal(t, types.JudgeStatusError, tr.JudgeVerdict.Status)
		})
	}
}

// TestDryRun_PerTaskOverrideInvalidatesOnlyThatTask asserts the
// sibling-isolation property: when only one task's overlay produces an
// invalid merged config, the other tasks still pass.
func TestDryRun_PerTaskOverrideInvalidatesOnlyThatTask(t *testing.T) {
	timeout := 300
	baseline := &types.RunConfig{
		Mode:             "execution",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		MaxTurns:         10,
		Timeout:          &timeout,
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
	}
	// An override that flips the mode to planning without a compatible
	// tools.builtIn list triggers the read-only-mode rule; the other
	// task uses no override so the baseline stays valid.
	suite := types.EvalSuite{
		ID:        "mixed-suite",
		RunConfig: baseline,
		Tasks: []types.EvalTask{
			{ID: "ok-task", Prompt: "p", Judge: validJudge},
			{ID: "bad-task", Prompt: "p", Judge: validJudge, RunConfigOverrides: &types.RunConfigOverrides{Mode: "planning"}},
		},
	}

	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	require.NoError(t, err)
	require.Len(t, result.Tasks, 2)
	outcomes := map[string]string{
		result.Tasks[0].TaskID: result.Tasks[0].Outcome,
		result.Tasks[1].TaskID: result.Tasks[1].Outcome,
	}
	require.Equal(t, "pass", outcomes["ok-task"], "ok-task outcome")
	require.Equal(t, "error", outcomes["bad-task"], "bad-task outcome")
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
	require.NoError(t, err)

	logData, err := os.ReadFile(argLog)
	require.NoError(t, err, "reading arg log")
	require.NotContains(t, string(logData), "--config", "legacy invocation must not pass --config")

	// No baseline → no redacted artifact retained.
	redactedPath := filepath.Join(out, "legacy", "t1", "run_config.redacted.json")
	_, err = os.Stat(redactedPath)
	require.ErrorIs(t, err, os.ErrNotExist, "legacy invocation must not write %s", redactedPath)
}

// TestRunSuite_WithBaselineWritesConfigAndRedactedArtifact covers the
// baseline invocation path: the harness is invoked with --config and no
// shadowing flags, the merged config carries the runner's trace path,
// and the retained run_config.redacted.json is scrubbed and private.
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
		Mode:             "execution",
		Provider:         types.ProviderConfig{Type: "openai-responses", APIKeyRef: "secret://OPENAI_KEY"},
		MaxTurns:         10,
		Timeout:          &timeout,
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
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
	require.NoError(t, err)

	redactedPath := filepath.Join(out, "wired", "t1", "run_config.redacted.json")

	t.Run("invokes harness with config and without shadowing flags", func(t *testing.T) {
		logData, err := os.ReadFile(argLog)
		require.NoError(t, err, "reading arg log")
		logStr := string(logData)
		require.Contains(t, logStr, "--config")
		for _, banned := range []string{"--timeout", "--mode", "--trace"} {
			require.NotContains(t, logStr, banned, "merged-config invocation must not pass %s", banned)
		}
	})

	t.Run("captured config keeps the reference and gets a trace path", func(t *testing.T) {
		captured, err := os.ReadFile(configCapture)
		require.NoError(t, err, "reading captured config")
		var got types.RunConfig
		require.NoError(t, json.Unmarshal(captured, &got), "unmarshal captured config")
		require.Equal(t, "openai-responses", got.Provider.Type)
		require.Equal(t, "secret://OPENAI_KEY", got.Provider.APIKeyRef,
			"the harness needs the unredacted reference to resolve the secret")
		require.NotEmpty(t, got.TraceEmitter.FilePath, "runner must inject the per-task trace path")
		require.True(t, strings.HasSuffix(got.TraceEmitter.FilePath, "trace.jsonl"),
			"trace path %q must end in trace.jsonl", got.TraceEmitter.FilePath)
	})

	t.Run("redacted artifact replaces the api key reference with a marker", func(t *testing.T) {
		redactedData, err := os.ReadFile(redactedPath)
		require.NoError(t, err, "reading redacted artifact")
		var redacted types.RunConfig
		require.NoError(t, json.Unmarshal(redactedData, &redacted), "unmarshal redacted artifact")
		require.Equal(t, "secret://[REDACTED]", redacted.Provider.APIKeyRef)
		// The artifact holds only the reference, so the raw JSON must
		// contain the redaction marker and never a plaintext token.
		require.Contains(t, string(redactedData), "secret://[REDACTED]")
	})

	t.Run("redacted artifact is mode 0600", func(t *testing.T) {
		// The file carries operator posture and must not be world-readable
		// on shared CI runners.
		info, err := os.Stat(redactedPath)
		require.NoError(t, err, "stat redacted artifact")
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	})
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
	require.NoError(t, err)
	require.Len(t, result.Tasks, 1)
	// Judge rejects the missing file, but the trace was consumed.
	require.Equal(t, "fail", result.Tasks[0].Outcome)
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
	require.NoError(t, err)
	require.Equal(t, "error", result.Tasks[0].Outcome)
	require.Contains(t, result.Tasks[0].Error, "cloning repo")
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
	require.NoError(t, err)
	require.Equal(t, "error", result.Tasks[0].Outcome)
	require.Contains(t, result.Tasks[0].Error, "harness boot failure")
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
	require.NoError(t, err)
	require.Equal(t, "error", result.Tasks[0].Outcome)
	require.Contains(t, result.Tasks[0].Error, "parsing trace")
}

// TestBuildMergedConfig_NilBaseline pins the legacy-invocation
// contract on the merge helper: a nil baseline returns (nil, nil)
// so runTask falls through to the legacy flag-only path.
func TestBuildMergedConfig_NilBaseline(t *testing.T) {
	four := 4
	merged, err := buildMergedConfig(nil, &types.RunConfigOverrides{MaxTurns: &four})
	require.NoError(t, err)
	require.Nil(t, merged)
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
	require.NoError(t, err)
	require.Len(t, result.Tasks, 1)
	require.Equal(t, "fail", result.Tasks[0].Outcome)
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
	require.NoError(t, err)
	require.Len(t, result.Tasks, 1)
	require.Equal(t, "error", result.Tasks[0].Outcome)
	require.Contains(t, result.Tasks[0].Error, "review", "error must name the offending mode")

	_, err = os.Stat(argLog)
	require.ErrorIs(t, err, os.ErrNotExist, "harness must not be invoked after validation failure")
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
		Mode:             "execution",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		MaxTurns:         10,
		Timeout:          &timeout,
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
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
	require.NoError(t, err)

	redactedPath := filepath.Join(out, "redact-suite", "t1", "run_config.redacted.json")
	data, err := os.ReadFile(redactedPath)
	require.NoError(t, err, "reading redacted artifact")
	// The bare secret:// scheme outside a [REDACTED] context is the
	// failure mode to guard against.
	body := string(data)
	require.NotContains(t, body, "secret://ANTHROPIC_API_KEY")
	require.Contains(t, body, "secret://[REDACTED]")
}

// TestWarnIfRawAPIKeyRef_WarnsForEachRawSecretField pins the
// defense-in-depth warning: it must fire for every secret-bearing field
// on the merged config when a raw (non secret://) apiKeyRef reaches the
// retain-artifact path.
func TestWarnIfRawAPIKeyRef_WarnsForEachRawSecretField(t *testing.T) {
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

	for _, field := range []string{
		"provider.apiKeyRef",
		"executor.vcsBackend.apiKeyRef",
		"providers[backup].apiKeyRef",
		"tools.mcpServers[0].apiKeyRef",
	} {
		t.Run("warns about "+field, func(t *testing.T) {
			require.Contains(t, stderr, field)
		})
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
	require.Empty(t, stderr)
}

// captureStderr replaces os.Stderr with a pipe for the duration of fn
// and returns whatever fn wrote. os.Stderr is process-global, so
// concurrent tests must not run this helper in parallel.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err, "creating stderr pipe")
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
