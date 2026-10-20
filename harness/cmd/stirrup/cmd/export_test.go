package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/workspaceexport"
	"github.com/rxbynerd/stirrup/types"
)

type exportTestBearer struct{}

func (exportTestBearer) Resolve(context.Context) (*credential.Resolved, error) {
	return &credential.Resolved{
		BearerToken: func(context.Context) (string, error) { return "tok", nil },
	}, nil
}

// installGCSUploadCapture points newWorkspaceExporter at a real GCS
// exporter whose uploads land in an httptest server, and returns a func
// reporting the captured upload bodies.
func installGCSUploadCapture(t *testing.T) func() [][]byte {
	t.Helper()
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	prev := newWorkspaceExporter
	t.Cleanup(func() { newWorkspaceExporter = prev })
	newWorkspaceExporter = func() (workspaceexport.Exporter, error) {
		return workspaceexport.NewGCSExporter(workspaceexport.GCSExporterOptions{
			CredentialSource: exportTestBearer{},
			EndpointBaseURL:  srv.URL,
		})
	}
	return func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), bodies...)
	}
}

func sandboxTarStream(t *testing.T, files map[string]string) []byte {
	t.Helper()
	must := require.New(t)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	must.NoError(tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}))
	for name, body := range files {
		must.NoError(tw.WriteHeader(&tar.Header{Name: "./" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}))
		_, err := tw.Write([]byte(body))
		must.NoError(err)
	}
	must.NoError(tw.Close())
	return buf.Bytes()
}

func uploadedFiles(t *testing.T, body []byte) map[string]string {
	t.Helper()
	must := require.New(t)
	gzr, err := gzip.NewReader(bytes.NewReader(body))
	must.NoError(err, "gzip.NewReader")
	tr := tar.NewReader(gzr)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files
		}
		must.NoError(err, "tar.Next")
		data, _ := io.ReadAll(tr)
		files[h.Name] = string(data)
	}
}

// TestExportWorkspace_ArchivesSandboxNotHarnessCwd pins that the Kubernetes
// executors' export carries the sandbox's files and nothing from the
// harness process's working directory.
func TestExportWorkspace_ArchivesSandboxNotHarnessCwd(t *testing.T) {
	for _, typ := range []string{"k8s", "k8s-sandbox"} {
		t.Run(typ, func(t *testing.T) {
			check := assert.New(t)
			must := require.New(t)
			hostCwd := t.TempDir()
			must.NoError(os.WriteFile(filepath.Join(hostCwd, "host-sentinel.txt"), []byte("host"), 0o644))
			t.Chdir(hostCwd)
			uploads := installGCSUploadCapture(t)

			cfg := &types.RunConfig{}
			cfg.Executor.Type = typ
			cfg.Executor.WorkspaceExportTo = "gs://bucket/runs/r1/workspace.tar.gz"
			sandbox := &archiverExecutor{stream: sandboxTarStream(t, map[string]string{"sandbox-sentinel.txt": "sandbox"})}

			_, err := exportWorkspace(context.Background(), cfg, sandbox, cfg.Executor.WorkspaceExportTo)
			must.NoError(err)
			got := uploads()
			must.Len(got, 1)
			check.Equal(map[string]string{"sandbox-sentinel.txt": "sandbox"}, uploadedFiles(t, got[0]))
		})
	}
}

// TestExportWorkspace_MissingRootNamesTheExecutor pins that a missing
// workspace root fails the export with an error naming the executor, and
// uploads nothing.
func TestExportWorkspace_MissingRootNamesTheExecutor(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	uploads := installGCSUploadCapture(t)
	cfg := &types.RunConfig{}
	cfg.Executor.Type = "k8s-sandbox"
	cfg.Executor.WorkspaceExportTo = "gs://bucket/runs/r1/workspace.tar.gz"
	missing := &archiverExecutor{err: fmt.Errorf("archive workspace /workspace: %w", executor.ErrWorkspaceRootMissing)}

	receipt, err := exportWorkspace(context.Background(), cfg, missing, cfg.Executor.WorkspaceExportTo)
	must.ErrorIs(err, executor.ErrWorkspaceRootMissing)
	must.ErrorContains(err, "k8s-sandbox executor")
	check.False(receipt.Delivered(), "receipt = %+v", receipt)
	check.Empty(uploads())
}

// TestExportWorkspace_EmptyWorkspaceUploadsAnEmptyArchive pins that an
// empty workspace uploads an empty archive and reports status "empty".
func TestExportWorkspace_EmptyWorkspaceUploadsAnEmptyArchive(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	uploads := installGCSUploadCapture(t)
	cfg := &types.RunConfig{}
	cfg.Executor.Type = "k8s"
	cfg.Executor.WorkspaceExportTo = "gs://bucket/runs/r1/workspace.tar.gz"

	receipt, err := exportWorkspace(context.Background(), cfg, &archiverExecutor{}, cfg.Executor.WorkspaceExportTo)
	must.NoError(err)
	check.Equal(types.ExportStatusEmpty, receipt.Status)
	got := uploads()
	must.Len(got, 1, "an empty workspace uploads one archive")
	check.Empty(uploadedFiles(t, got[0]))
}

// TestExportWorkspace_ExecutorWithoutArchiver pins that an executor lacking
// the archiver capability fails the export without building an exporter.
func TestExportWorkspace_ExecutorWithoutArchiver(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	uploads := installGCSUploadCapture(t)
	cfg := &types.RunConfig{}
	cfg.Executor.Type = "none"
	cfg.Executor.WorkspaceExportTo = "gs://bucket/runs/r1/workspace.tar.gz"

	receipt, err := exportWorkspace(context.Background(), cfg, &executor.NoneExecutor{}, cfg.Executor.WorkspaceExportTo)
	must.ErrorContains(err, "cannot archive its workspace")
	check.Equal(types.ExportStatusFailed, receipt.Status)
	check.Equal(cfg.Executor.WorkspaceExportTo, receipt.URI)
	check.Empty(uploads())
}

// TestRunWithConfig_ExportsLoopExecutorWorkspace pins that a harness run
// exports through the loop's executor: the upload holds a file seeded in
// the executor's workspace, not the harness process's working directory.
func TestRunWithConfig_ExportsLoopExecutorWorkspace(t *testing.T) {
	must := require.New(t)
	srv := openAICompatibleSuccessServer(t)
	defer srv.Close()
	uploads := installGCSUploadCapture(t)
	t.Chdir(t.TempDir())

	config := runOutcomeTestConfig(t, srv.URL)
	config.Executor.WorkspaceExportTo = "gs://bucket/runs/r1/workspace.tar.gz"
	must.NoError(os.WriteFile(filepath.Join(config.Executor.Workspace, "seed.txt"), []byte("from the executor"), 0o644))

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	_ = stdoutDone()
	must.NoError(runErr)
	got := uploads()
	must.Len(got, 1)
	files := uploadedFiles(t, got[0])
	must.Equal("from the executor", files["seed.txt"], "uploaded entries = %v", files)
}
