package cmd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

const (
	httpsPutSignedQuery = "?X-Amz-Signature=deadbeefcafe&X-Amz-Credential=AKIDEXAMPLE"
	httpsPutAuthValue   = "Bearer upload-capability-0451"
)

var httpsPutCapabilities = []string{"deadbeefcafe", "AKIDEXAMPLE", "upload-capability-0451", "/bucket/workspace.tar.gz"}

type httpsPutUpload struct {
	Path   string
	Header http.Header
	Body   []byte
}

// httpsPutDestination serves an upload endpoint answering with status and
// returns a func reporting the uploads it received.
func httpsPutDestination(t *testing.T, status int) (*httptest.Server, func() []httpsPutUpload) {
	t.Helper()
	var mu sync.Mutex
	var uploads []httpsPutUpload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		uploads = append(uploads, httpsPutUpload{Path: r.URL.RequestURI(), Header: r.Header.Clone(), Body: body})
		mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []httpsPutUpload {
		mu.Lock()
		defer mu.Unlock()
		return append([]httpsPutUpload(nil), uploads...)
	}
}

// httpsPutRunConfig is a run whose workspace exports to dest through
// environment-backed secret references.
func httpsPutRunConfig(t *testing.T, providerURL, destURL string) (*types.RunConfig, string) {
	t.Helper()
	t.Setenv("TEST_EXPORT_URL", destURL+"/bucket/workspace.tar.gz"+httpsPutSignedQuery)
	t.Setenv("TEST_EXPORT_AUTH", httpsPutAuthValue)
	config := runOutcomeTestConfig(t, providerURL)
	require.NoError(t, os.WriteFile(filepath.Join(config.Executor.Workspace, "kept.txt"), []byte("kept"), 0o644))
	tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
	config.TraceEmitter = types.TraceEmitterConfig{Type: "jsonl", FilePath: tracePath}
	config.Executor.WorkspaceExportHTTPSPut = &types.HTTPSPutConfig{
		URLRef:  "secret://TEST_EXPORT_URL",
		Headers: map[string]string{"Authorization": "secret://TEST_EXPORT_AUTH", "x-ms-blob-type": "BlockBlob"},
	}
	return config, tracePath
}

func TestRunWithConfig_HTTPSPutExportUploadsTheWorkspaceAndKeepsTheURLOutOfEveryOutput(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	provider := openAICompatibleSuccessServer(t)
	defer provider.Close()
	dest, uploads := httpsPutDestination(t, http.StatusOK)
	config, tracePath := httpsPutRunConfig(t, provider.URL, dest.URL)

	stdoutDone := captureStdout(t)
	stderrDone := captureStderr(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout, stderr := stdoutDone(), stderrDone()
	must.NoError(runErr)

	got := uploads()
	must.Len(got, 1)
	check.Equal("/bucket/workspace.tar.gz"+httpsPutSignedQuery, got[0].Path)
	check.Equal(httpsPutAuthValue, got[0].Header.Get("Authorization"))
	check.Equal("BlockBlob", got[0].Header.Get("x-ms-blob-type"))
	check.Equal("application/gzip", got[0].Header.Get("Content-Type"))
	check.Equal(map[string]string{"kept.txt": "kept"}, uploadedFiles(t, got[0].Body))

	done := stdioDoneEvent(t, stdout)
	must.NotNil(done.Trace)
	receipt := done.Trace.WorkspaceExport
	must.NotNil(receipt)
	check.Equal(dest.URL, receipt.URI)
	check.Equal(types.ExportStatusUploaded, receipt.Status)
	check.True(receipt.Required)
	check.Equal(int64(len(got[0].Body)), receipt.Bytes)
	check.Equal(1, receipt.Files)

	rt := runFinishedTrace(t, tracePath)
	must.NotNil(rt.Config.Executor.WorkspaceExportHTTPSPut)
	check.Equal("secret://[REDACTED]", rt.Config.Executor.WorkspaceExportHTTPSPut.URLRef)

	traceBytes, err := os.ReadFile(tracePath)
	must.NoError(err)
	for _, capability := range httpsPutCapabilities {
		check.NotContains(stdout, capability, "stdout")
		check.NotContains(stderr, capability, "stderr")
		check.NotContains(string(traceBytes), capability, "trace")
	}
}

func TestRunWithConfig_HTTPSPutRequiredExportFailureReportsHandoffFailed(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	provider := openAICompatibleSuccessServer(t)
	defer provider.Close()
	dest, _ := httpsPutDestination(t, http.StatusForbidden)
	config, tracePath := httpsPutRunConfig(t, provider.URL, dest.URL)

	stdoutDone := captureStdout(t)
	stderrDone := captureStderr(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout, stderr := stdoutDone(), stderrDone()

	must.Error(runErr)
	check.Equal(exitRunOutcome, classifyExitCode(runErr))
	result := decodeStirrupResult(t, stdout)
	check.Equal("handoff_failed", result.Outcome)
	check.Contains(result.Error, "HTTP 403")
	must.NotNil(result.WorkspaceExport)
	check.Equal(types.ExportStatusFailed, result.WorkspaceExport.Status)
	check.Equal(dest.URL, result.WorkspaceExport.URI)

	traceBytes, err := os.ReadFile(tracePath)
	must.NoError(err)
	for _, capability := range httpsPutCapabilities {
		check.NotContains(runErr.Error(), capability, "returned error")
		check.NotContains(stdout, capability, "stdout")
		check.NotContains(stderr, capability, "stderr")
		check.NotContains(string(traceBytes), capability, "trace")
	}
}

func TestRunWithConfig_HTTPSPutOptionalExportFailureKeepsSuccess(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	provider := openAICompatibleSuccessServer(t)
	defer provider.Close()
	dest, _ := httpsPutDestination(t, http.StatusServiceUnavailable)
	config, _ := httpsPutRunConfig(t, provider.URL, dest.URL)
	optional := false
	config.Executor.WorkspaceExportRequired = &optional

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	stdout := stdoutDone()

	must.NoError(runErr)
	result := decodeStirrupResult(t, stdout)
	check.Equal("success", result.Outcome)
	must.NotNil(result.WorkspaceExport)
	check.Equal(types.ExportStatusFailed, result.WorkspaceExport.Status)
	check.False(result.WorkspaceExport.Required)
	check.Contains(result.WorkspaceExport.Error, "HTTP 503")
}

func TestRunWithConfig_HTTPSPutUnresolvedURLReferenceFailsBeforeTheRun(t *testing.T) {
	must := require.New(t)
	provider := openAICompatibleSuccessServer(t)
	defer provider.Close()
	dest, uploads := httpsPutDestination(t, http.StatusOK)
	config, _ := httpsPutRunConfig(t, provider.URL, dest.URL)
	config.Executor.WorkspaceExportHTTPSPut.URLRef = "secret://TEST_EXPORT_URL_NOT_SET"

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	_ = stdoutDone()

	must.ErrorContains(runErr, "building harness: executor.workspaceExportHttpsPut: resolve urlRef")
	must.Empty(uploads())
}

func TestRunWithConfig_HTTPSPutRemotePlainHTTPURLFailsBeforeTheRunWithoutEchoingIt(t *testing.T) {
	must := require.New(t)
	provider := openAICompatibleSuccessServer(t)
	defer provider.Close()
	dest, _ := httpsPutDestination(t, http.StatusOK)
	config, _ := httpsPutRunConfig(t, provider.URL, dest.URL)
	t.Setenv("TEST_EXPORT_URL", "http://uploads.example/bucket/workspace.tar.gz"+httpsPutSignedQuery)

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	_ = stdoutDone()

	must.ErrorContains(runErr, "accepted only for a loopback host")
	must.NotContains(runErr.Error(), "deadbeefcafe")
}

func TestWorkspaceHandoff_HTTPSPutFollowUpUploadsToTheCurrentURL(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	dest, uploads := httpsPutDestination(t, http.StatusOK)
	t.Setenv("TEST_EXPORT_URL", dest.URL+"/bucket/primary.tar.gz")
	cfg := &types.RunConfig{RunID: "primary"}
	cfg.Executor.Type = "k8s"
	cfg.Executor.WorkspaceExportHTTPSPut = &types.HTTPSPutConfig{URLRef: "secret://TEST_EXPORT_URL"}
	sandbox := &archiverExecutor{stream: sandboxTarStream(t, map[string]string{"a.txt": "alpha"})}
	handoff := workspaceHandoff{primaryRunID: "primary"}

	receipt, err := handoff.Handoff(context.Background(), cfg, sandbox)
	must.NoError(err)
	must.NotNil(receipt)
	check.Equal(dest.URL, receipt.URI)

	t.Setenv("TEST_EXPORT_URL", dest.URL+"/bucket/follow-up.tar.gz")
	followUp := *cfg
	followUp.RunID = "follow-up-1"
	sandbox.stream = sandboxTarStream(t, map[string]string{"a.txt": "alpha"})
	_, err = handoff.Handoff(context.Background(), &followUp, sandbox)
	must.NoError(err)

	got := uploads()
	must.Len(got, 2)
	check.Equal("/bucket/primary.tar.gz", got[0].Path)
	check.Equal("/bucket/follow-up.tar.gz", got[1].Path)
}
