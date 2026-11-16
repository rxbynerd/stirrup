package cmd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

var httpsPutTraceCapabilities = []string{"deadbeefcafe", "AKIDEXAMPLE", "upload-capability-0451", "/bucket/trace.jsonl"}

// httpsPutTraceRunConfig is a run whose trace is uploaded to dest through
// environment-backed secret references.
func httpsPutTraceRunConfig(t *testing.T, providerURL, destURL string) *types.RunConfig {
	t.Helper()
	t.Setenv("TEST_TRACE_URL", destURL+"/bucket/trace.jsonl"+httpsPutSignedQuery)
	t.Setenv("TEST_TRACE_AUTH", httpsPutAuthValue)
	config := runOutcomeTestConfig(t, providerURL)
	config.TraceEmitter = types.TraceEmitterConfig{
		Type: "https-put",
		HTTPSPut: &types.HTTPSPutConfig{
			URLRef:  "secret://TEST_TRACE_URL",
			Headers: map[string]string{"Authorization": "secret://TEST_TRACE_AUTH"},
		},
	}
	return config
}

func TestRunWithConfig_HTTPSPutTraceEmitterUploadsTheRunTraceAndKeepsTheURLOutOfEveryOutput(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	provider := openAICompatibleSuccessServer(t)
	defer provider.Close()
	dest, uploads := httpsPutDestination(t, http.StatusOK)
	config := httpsPutTraceRunConfig(t, provider.URL, dest.URL)

	stdoutDone := captureStdout(t)
	stderrDone := captureStderr(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout, stderr := stdoutDone(), stderrDone()
	must.NoError(runErr)

	got := uploads()
	must.Len(got, 1)
	check.Equal("/bucket/trace.jsonl"+httpsPutSignedQuery, got[0].Path)
	check.Equal(httpsPutAuthValue, got[0].Header.Get("Authorization"))
	check.Equal("application/x-ndjson", got[0].Header.Get("Content-Type"))

	body := strings.TrimRight(string(got[0].Body), "\n")
	var uploaded types.RunTrace
	must.NoError(json.Unmarshal([]byte(body), &uploaded), "body=%q", body)
	check.Equal(config.RunID, uploaded.ID)
	check.Equal("success", uploaded.Outcome)
	must.NotNil(uploaded.Config.TraceEmitter.HTTPSPut)
	check.Equal("secret://[REDACTED]", uploaded.Config.TraceEmitter.HTTPSPut.URLRef)

	check.Equal("success", stdioDoneEvent(t, stdout).StopReason)
	for _, capability := range httpsPutTraceCapabilities {
		check.NotContains(stdout, capability, "stdout")
		check.NotContains(stderr, capability, "stderr")
		check.NotContains(body, capability, "uploaded trace")
	}
}

func TestRunWithConfig_HTTPSPutTraceEmitterRejectedUploadIsReportedWithoutTheURL(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	provider := openAICompatibleSuccessServer(t)
	defer provider.Close()
	dest, uploads := httpsPutDestination(t, http.StatusForbidden)
	config := httpsPutTraceRunConfig(t, provider.URL, dest.URL)

	stdoutDone := captureStdout(t)
	stderrDone := captureStderr(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout, stderr := stdoutDone(), stderrDone()

	check.Len(uploads(), 1)
	combined := stdout + stderr
	if runErr != nil {
		combined += runErr.Error()
	}
	check.Contains(combined, "HTTP 403", "the failed upload is reported")
	for _, capability := range httpsPutTraceCapabilities {
		check.NotContains(combined, capability)
	}
	must.NotEmpty(decodeStirrupResult(t, stdout).RunID, "the run still reports its result")
}

func TestRunWithConfig_HTTPSPutTraceEmitterUnresolvedURLReferenceFailsBeforeTheRun(t *testing.T) {
	must := require.New(t)
	provider := openAICompatibleSuccessServer(t)
	defer provider.Close()
	dest, uploads := httpsPutDestination(t, http.StatusOK)
	config := httpsPutTraceRunConfig(t, provider.URL, dest.URL)
	config.TraceEmitter.HTTPSPut.URLRef = "secret://TEST_TRACE_URL_NOT_SET"

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	_ = stdoutDone()

	must.ErrorContains(runErr, "building harness: traceEmitter.httpsPut: resolve urlRef")
	must.Empty(uploads())
}
