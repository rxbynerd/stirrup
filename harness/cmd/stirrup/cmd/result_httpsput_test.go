package cmd

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

var httpsPutResultCapabilities = []string{"deadbeefcafe", "AKIDEXAMPLE", "upload-capability-0451", "/bucket/result.json"}

// httpsPutResultRunConfig is a run whose RunResult is delivered to dest
// through environment-backed secret references.
func httpsPutResultRunConfig(t *testing.T, providerURL, destURL string) (*types.RunConfig, string) {
	t.Helper()
	t.Setenv("TEST_RESULT_URL", destURL+"/bucket/result.json"+httpsPutSignedQuery)
	t.Setenv("TEST_RESULT_AUTH", httpsPutAuthValue)
	config := runOutcomeTestConfig(t, providerURL)
	tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
	config.TraceEmitter = types.TraceEmitterConfig{Type: "jsonl", FilePath: tracePath}
	config.ResultSink = &types.ResultSinkConfig{
		Type: "https-put",
		HTTPSPut: &types.HTTPSPutConfig{
			URLRef:  "secret://TEST_RESULT_URL",
			Headers: map[string]string{"Authorization": "secret://TEST_RESULT_AUTH", "If-None-Match": "*"},
		},
	}
	return config, tracePath
}

func TestRunWithConfig_HTTPSPutResultSinkDeliversTheRunResultAndKeepsTheURLOutOfEveryOutput(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	provider := openAICompatibleSuccessServer(t)
	defer provider.Close()
	dest, uploads := httpsPutDestination(t, http.StatusOK)
	config, tracePath := httpsPutResultRunConfig(t, provider.URL, dest.URL)

	stdoutDone := captureStdout(t)
	stderrDone := captureStderr(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout, stderr := stdoutDone(), stderrDone()
	must.NoError(runErr)

	got := uploads()
	must.Len(got, 1)
	check.Equal("/bucket/result.json"+httpsPutSignedQuery, got[0].Path)
	check.Equal(httpsPutAuthValue, got[0].Header.Get("Authorization"))
	check.Equal("*", got[0].Header.Get("If-None-Match"))
	check.Equal("application/json", got[0].Header.Get("Content-Type"))

	var delivered types.RunResult
	must.NoError(json.Unmarshal(got[0].Body, &delivered))
	check.Equal(decodeStirrupResult(t, stdout), delivered, "the sink receives the same RunResult the stdout line carries")
	check.Equal("success", delivered.Outcome)
	check.Equal(config.RunID, delivered.RunID)

	rt := runFinishedTrace(t, tracePath)
	must.NotNil(rt.Config.ResultSink)
	must.NotNil(rt.Config.ResultSink.HTTPSPut)
	check.Equal("secret://[REDACTED]", rt.Config.ResultSink.HTTPSPut.URLRef)

	traceBytes, err := os.ReadFile(tracePath)
	must.NoError(err)
	for _, capability := range httpsPutResultCapabilities {
		check.NotContains(stdout, capability, "stdout")
		check.NotContains(stderr, capability, "stderr")
		check.NotContains(string(traceBytes), capability, "trace")
		check.NotContains(string(got[0].Body), capability, "delivered RunResult")
	}
}

func TestRunWithConfig_HTTPSPutResultSinkFailureWarnsAndKeepsTheOutcome(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	provider := openAICompatibleSuccessServer(t)
	defer provider.Close()
	dest, uploads := httpsPutDestination(t, http.StatusForbidden)
	config, _ := httpsPutResultRunConfig(t, provider.URL, dest.URL)

	stdoutDone := captureStdout(t)
	stderrDone := captureStderr(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout, stderr := stdoutDone(), stderrDone()

	must.NoError(runErr, "a failed delivery does not change the run's outcome")
	check.Len(uploads(), 1, "a permanent status is not retried")
	check.Equal("success", decodeStirrupResult(t, stdout).Outcome)

	var warning string
	doneAt, warningAt := -1, -1
	for i, line := range strings.Split(stdout, "\n") {
		var ev types.HarnessEvent
		if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "warning" && strings.HasPrefix(ev.Message, "result sink:"):
			warning, warningAt = ev.Message, i
		case ev.Type == "done":
			doneAt = i
		}
	}
	must.NotEmpty(warning, "stdout:\n%s", stdout)
	check.Contains(warning, "HTTP 403")
	check.Less(warningAt, doneAt, "the warning precedes done")
	for _, capability := range httpsPutResultCapabilities {
		check.NotContains(stdout, capability, "stdout")
		check.NotContains(stderr, capability, "stderr")
	}
}

func TestRunWithConfig_HTTPSPutResultSinkUnresolvedURLReferenceFailsBeforeTheRun(t *testing.T) {
	must := require.New(t)
	provider := openAICompatibleSuccessServer(t)
	defer provider.Close()
	dest, uploads := httpsPutDestination(t, http.StatusOK)
	config, _ := httpsPutResultRunConfig(t, provider.URL, dest.URL)
	config.ResultSink.HTTPSPut.URLRef = "secret://TEST_RESULT_URL_NOT_SET"

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	_ = stdoutDone()

	must.ErrorContains(runErr, "building harness: resultSink.httpsPut: resolve urlRef")
	must.Empty(uploads())
}
