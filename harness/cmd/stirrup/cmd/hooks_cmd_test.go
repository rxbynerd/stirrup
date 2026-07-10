package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// fatalPreRunHookConfig builds a minimal, valid RunConfig whose single
// preRun hook fails fatally ("exit 1", no continueOnError). Because
// preRun hooks run before Git.Setup and before any turn — and
// therefore before the loop ever streams from the provider — this
// config drives runWithConfig end-to-end without needing a live (or
// mocked) provider endpoint: BuildLoop only constructs the adapter, it
// never calls it.
func fatalPreRunHookConfig(t *testing.T) *types.RunConfig {
	t.Helper()
	t.Setenv("TEST_HOOKS_CMD_KEY", "unused-never-called")
	timeout := 30
	return &types.RunConfig{
		RunID:            "cmd-hooks-fatal-test",
		Mode:             "planning",
		Prompt:           "irrelevant, never reached",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://TEST_HOOKS_CMD_KEY"},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "claude-sonnet-4-6"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"read_file"}},
		MaxTurns:         2,
		Timeout:          &timeout,
		Hooks: &types.HooksConfig{
			PreRun: []types.HookConfig{{Name: "boom", Command: "exit 1"}},
		},
	}
}

// TestRunWithConfig_FatalPreRunHookFailure_StillEmitsRunResult pins
// issue #461 finding #2's cmd-layer half: runWithConfig previously
// short-circuited on any non-nil error from loop.Run(), skipping
// emitRunOutput entirely — so a preRun hook failure (outcome
// "setup_failed", a RunResult.HookFailures-bearing case) produced no
// STIRRUP_RESULT line at all despite loop.Run() having returned a
// valid RunTrace. runWithConfig must still emit it, and must still
// return a non-nil error (the CLI's own exit-status contract is
// unchanged).
func TestRunWithConfig_FatalPreRunHookFailure_StillEmitsRunResult(t *testing.T) {
	config := fatalPreRunHookConfig(t)

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	if runErr == nil {
		t.Fatal("expected a non-nil error from a fatal preRun hook failure")
	}

	// stdout also carries the default stdio transport's own JSON event
	// stream (the "error"/"done" HarnessEvents finding #2 also fixed —
	// see TestLoop_Hooks_PreRunFatalFailure_EmitsDoneEvent), so the
	// STIRRUP_RESULT line is not necessarily the first line; find it.
	var resultLine string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "STIRRUP_RESULT ") {
			resultLine = line
			break
		}
	}
	if resultLine == "" {
		t.Fatalf("expected a STIRRUP_RESULT line despite the fatal error, got stdout: %q", stdout)
	}
	payload := strings.TrimPrefix(resultLine, "STIRRUP_RESULT ")
	var result types.RunResult
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("STIRRUP_RESULT payload should parse as RunResult: %v\npayload: %s", err, payload)
	}
	if result.Outcome != "setup_failed" {
		t.Errorf("RunResult.Outcome = %q, want setup_failed", result.Outcome)
	}
	if result.HookFailures == 0 {
		t.Error("RunResult.HookFailures = 0, want non-zero")
	}
}
