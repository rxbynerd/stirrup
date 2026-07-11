package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// TestRunOutcomeError_EnumeratedOutcomes pins the outcome→exit mapping
// (issue #101, v0.1 release blocker B4) against every value documented
// on types.RunTrace.Outcome. "success" is the only outcome that must
// not produce an error; every other documented value — including the
// limit-hit and lifecycle-hook classes, not just "error" — must, so a
// future outcome added to the enum without updating runSuccessOutcomes
// fails this test rather than silently exiting 0.
func TestRunOutcomeError_EnumeratedOutcomes(t *testing.T) {
	nonSuccessOutcomes := []string{
		"error",
		"max_turns",
		"verification_failed",
		"verification_error",
		"budget_exceeded",
		"stalled",
		"tool_failures",
		"cancelled",
		"timeout",
		"max_tokens",
		"setup_failed",
		"hook_failed",
		"", // empty/unknown outcome must not be treated as success
		"some_future_outcome_not_yet_invented",
	}
	for _, outcome := range nonSuccessOutcomes {
		t.Run(outcome, func(t *testing.T) {
			err := runOutcomeError(&types.RunTrace{Outcome: outcome})
			if err == nil {
				t.Fatalf("runOutcomeError(Outcome=%q) = nil, want a non-nil error", outcome)
			}
			if classifyExitCode(err) != 1 {
				t.Errorf("classifyExitCode(runOutcomeError(%q)) = %d, want 1 (the documented default for a failed or cancelled run)", outcome, classifyExitCode(err))
			}
		})
	}

	if err := runOutcomeError(&types.RunTrace{Outcome: "success"}); err != nil {
		t.Errorf("runOutcomeError(Outcome=\"success\") = %v, want nil", err)
	}

	if err := runOutcomeError(nil); err != nil {
		t.Errorf("runOutcomeError(nil) = %v, want nil (nil-trace callers return their own error before reaching this check)", err)
	}
}

// openAICompatibleErrorServer replies to every request with a 401,
// mirroring an invalid API key against a real provider — the same
// failure class as the live ANTHROPIC_API_KEY=fake repro for issue
// #101, just against the openai-compatible adapter so the test needs
// no live credentials or network access.
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

// TestRunWithConfig_ProviderErrorOutcome_ExitsNonZero is the cmd-layer
// regression for issue #101 / v0.1 release blocker B4: AgenticLoop.Run
// returns (trace, nil) for a mid-run provider failure — only the trace
// carries the failure via Outcome=="error" — so runWithConfig must
// convert that into a non-nil error itself, or the process exits 0 on
// a failed run (the ANTHROPIC_API_KEY=fake live repro this pins).
func TestRunWithConfig_ProviderErrorOutcome_ExitsNonZero(t *testing.T) {
	srv := openAICompatibleErrorServer(t)
	defer srv.Close()

	config := runOutcomeTestConfig(t, srv.URL)

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	if runErr == nil {
		t.Fatal("expected a non-nil error from a provider-error outcome, got nil (this is the B4 regression: outcome=error must still fail the process)")
	}
	if classifyExitCode(runErr) != 1 {
		t.Errorf("classifyExitCode(runErr) = %d, want 1", classifyExitCode(runErr))
	}

	result := decodeStirrupResult(t, stdout)
	if result.Outcome != "error" {
		t.Errorf("STIRRUP_RESULT outcome = %q, want \"error\"", result.Outcome)
	}
}

// TestRunWithConfig_SuccessOutcome_ExitsZero is the success-path
// counterpart: a run that completes normally must still return a nil
// error (and therefore exit 0) after the B4 fix, proving the new
// outcome check does not regress the happy path.
func TestRunWithConfig_SuccessOutcome_ExitsZero(t *testing.T) {
	srv := openAICompatibleSuccessServer(t)
	defer srv.Close()

	config := runOutcomeTestConfig(t, srv.URL)

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	if runErr != nil {
		t.Fatalf("expected a nil error from a successful run, got: %v", runErr)
	}
	if classifyExitCode(runErr) != 0 {
		t.Errorf("classifyExitCode(runErr) = %d, want 0", classifyExitCode(runErr))
	}

	result := decodeStirrupResult(t, stdout)
	if result.Outcome != "success" {
		t.Errorf("STIRRUP_RESULT outcome = %q, want \"success\"", result.Outcome)
	}
}

// decodeStirrupResult finds and parses the STIRRUP_RESULT line out of
// captured stdout. Shared by the two runOutcome tests above; stdout
// also carries the stdio transport's own JSON event stream, so the
// result line is not necessarily first.
func decodeStirrupResult(t *testing.T, stdout string) types.RunResult {
	t.Helper()
	var resultLine string
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "STIRRUP_RESULT ") {
			resultLine = line
			break
		}
	}
	if resultLine == "" {
		t.Fatalf("expected a STIRRUP_RESULT line, got stdout: %q", stdout)
	}
	payload := strings.TrimPrefix(resultLine, "STIRRUP_RESULT ")
	var result types.RunResult
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatalf("STIRRUP_RESULT payload should parse as RunResult: %v\npayload: %s", err, payload)
	}
	return result
}
