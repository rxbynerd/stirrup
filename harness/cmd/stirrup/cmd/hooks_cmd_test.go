package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

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
// that a fatal preRun hook failure (outcome "setup_failed") still
// emits a STIRRUP_RESULT line despite loop.Run() returning a non-nil
// error, while runWithConfig itself still returns that error.
func TestRunWithConfig_FatalPreRunHookFailure_StillEmitsRunResult(t *testing.T) {
	must := require.New(t)
	config := fatalPreRunHookConfig(t)

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	must.Error(runErr)

	// stdout also carries the default stdio transport's own JSON event
	// stream, so the STIRRUP_RESULT line is not necessarily the first
	// line; find it.
	var resultLine string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "STIRRUP_RESULT ") {
			resultLine = line
			break
		}
	}
	must.NotEmpty(resultLine, "no STIRRUP_RESULT line despite the fatal error; stdout: %q", stdout)

	payload := strings.TrimPrefix(resultLine, "STIRRUP_RESULT ")
	var result types.RunResult
	must.NoError(json.Unmarshal([]byte(payload), &result), "payload: %s", payload)
	must.Equal("setup_failed", result.Outcome)
	must.NotZero(result.HookFailures)
}
