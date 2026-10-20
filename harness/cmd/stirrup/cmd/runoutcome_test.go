package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// TestRunOutcomeError_OnlySuccessIsNil pins the outcome→exit mapping
// against every value documented on types.RunTrace.Outcome. "success"
// is the only outcome that must not produce an error; a future outcome
// added to the enum without updating runSuccessOutcomes fails this
// test rather than silently exiting 0.
func TestRunOutcomeError_OnlySuccessIsNil(t *testing.T) {
	nonSuccessCases := []struct {
		name    string
		outcome string
	}{
		{"error", "error"},
		{"max turns", "max_turns"},
		{"verification failed", "verification_failed"},
		{"verification error", "verification_error"},
		{"budget exceeded", "budget_exceeded"},
		{"stalled", "stalled"},
		{"tool failures", "tool_failures"},
		{"cancelled", "cancelled"},
		{"timeout", "timeout"},
		{"max tokens", "max_tokens"},
		{"setup failed", "setup_failed"},
		{"hook failed", "hook_failed"},
		{"handoff failed", "handoff_failed"},
		{"structured output failed", "structured_output_failed"},
		{"empty outcome", ""},
		{"unknown future outcome", "some_future_outcome_not_yet_invented"},
	}
	for _, tc := range nonSuccessCases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			err := runOutcomeError(&types.RunTrace{Outcome: tc.outcome})
			must.Error(err)
			must.Equal(1, classifyExitCode(err), "failed and cancelled runs exit 1")
		})
	}

	t.Run("success", func(t *testing.T) {
		must := require.New(t)
		must.NoError(runOutcomeError(&types.RunTrace{Outcome: "success"}))
	})

	// Callers return their own error before a nil trace reaches this check.
	t.Run("nil trace", func(t *testing.T) {
		must := require.New(t)
		must.NoError(runOutcomeError(nil))
	})
}

// openAICompatibleErrorServer replies to every request with a 401,
// mirroring an invalid API key against a real provider without
// requiring live credentials or network access.
func openAICompatibleErrorServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"message":"invalid api key"}}`)
	}))
}

// openAICompatibleSuccessServer replies to every request with a single
// streamed turn that emits a text answer and finishes cleanly, driving
// the loop to outcome "success" without a tool call or a second turn.
func openAICompatibleSuccessServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: "+
			`{"id":"resp","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`+
			"\n\n")
		_, _ = fmt.Fprint(w, "data: "+
			`{"id":"resp","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+
			"\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// runOutcomeTestConfig builds a minimal, valid RunConfig pointed at an
// openai-compatible server under test. Mirrors fatalPreRunHookConfig's
// shape (read-only planning mode, deny-side-effects, no git) so the
// only variable between the error and success cases is the fake
// server's response.
func runOutcomeTestConfig(t *testing.T, baseURL string) *types.RunConfig {
	t.Helper()
	t.Setenv("TEST_RUNOUTCOME_KEY", "unused-fake-key")
	timeout := 30
	return &types.RunConfig{
		RunID:            "cmd-runoutcome-test",
		Mode:             "planning",
		Prompt:           "Say hello.",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_RUNOUTCOME_KEY", BaseURL: baseURL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "gpt-4o-mini"},
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
	}
}

// TestRunWithConfig_ProviderErrorOutcome_ExitsNonZero pins that
// runWithConfig converts a trace-only failure (AgenticLoop.Run returns
// (trace, nil) with Outcome=="error" for a mid-run provider failure)
// into a non-nil error, so the process does not exit 0 on a failed run.
func TestRunWithConfig_ProviderErrorOutcome_ExitsNonZero(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := openAICompatibleErrorServer(t)
	defer srv.Close()

	config := runOutcomeTestConfig(t, srv.URL)

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	must.Error(runErr, "an outcome of error must still fail the process")
	check.Equal(1, classifyExitCode(runErr))

	result := decodeStirrupResult(t, stdout)
	check.Equal("error", result.Outcome)
}

// TestRunWithConfig_SuccessOutcome_ExitsZero is the success-path
// counterpart: a run that completes normally must still return a nil
// error (and therefore exit 0).
func TestRunWithConfig_SuccessOutcome_ExitsZero(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := openAICompatibleSuccessServer(t)
	defer srv.Close()

	config := runOutcomeTestConfig(t, srv.URL)

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	must.NoError(runErr, "a successful run must return a nil error")
	check.Equal(0, classifyExitCode(runErr))

	result := decodeStirrupResult(t, stdout)
	check.Equal("success", result.Outcome)
}

// decodeStirrupResult finds and parses the STIRRUP_RESULT line out of
// captured stdout. Stdout also carries the stdio transport's JSON event
// stream, so the result line is not necessarily first.
func decodeStirrupResult(t *testing.T, stdout string) types.RunResult {
	t.Helper()
	must := require.New(t)
	var resultLine string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "STIRRUP_RESULT ") {
			resultLine = line
			break
		}
	}
	must.NotEmpty(resultLine, "expected a STIRRUP_RESULT line in stdout: %q", stdout)
	payload := strings.TrimPrefix(resultLine, "STIRRUP_RESULT ")
	var result types.RunResult
	must.NoError(json.Unmarshal([]byte(payload), &result), "STIRRUP_RESULT payload should parse as RunResult: %s", payload)
	return result
}
