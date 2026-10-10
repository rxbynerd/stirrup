package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "./" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func uploadedFiles(t *testing.T, body []byte) map[string]string {
	t.Helper()
	gzr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	tr := tar.NewReader(gzr)
	files := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
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
			hostCwd := t.TempDir()
			if err := os.WriteFile(filepath.Join(hostCwd, "host-sentinel.txt"), []byte("host"), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Chdir(hostCwd)
			uploads := installGCSUploadCapture(t)

			cfg := &types.RunConfig{}
			cfg.Executor.Type = typ
			cfg.Executor.WorkspaceExportTo = "gs://bucket/runs/r1/workspace.tar.gz"
			sandbox := &archiverExecutor{stream: sandboxTarStream(t, map[string]string{"sandbox-sentinel.txt": "sandbox"})}

			if _, err := exportWorkspace(context.Background(), cfg, sandbox, cfg.Executor.WorkspaceExportTo); err != nil {
				t.Fatalf("exportWorkspace: %v", err)
			}
			got := uploads()
			if len(got) != 1 {
				t.Fatalf("uploads = %d, want 1", len(got))
			}
			files := uploadedFiles(t, got[0])
			if len(files) != 1 || files["sandbox-sentinel.txt"] != "sandbox" {
				t.Errorf("uploaded entries = %v, want only sandbox-sentinel.txt", files)
			}
		})
	}
}

// TestExportWorkspace_MissingRootNamesTheExecutor pins that a missing
// workspace root fails the export with an error naming the executor, and
// uploads nothing.
func TestExportWorkspace_MissingRootNamesTheExecutor(t *testing.T) {
	uploads := installGCSUploadCapture(t)
	cfg := &types.RunConfig{}
	cfg.Executor.Type = "k8s-sandbox"
	cfg.Executor.WorkspaceExportTo = "gs://bucket/runs/r1/workspace.tar.gz"
	missing := &archiverExecutor{err: fmt.Errorf("archive workspace /workspace: %w", executor.ErrWorkspaceRootMissing)}

	receipt, err := exportWorkspace(context.Background(), cfg, missing, cfg.Executor.WorkspaceExportTo)
	if !errors.Is(err, executor.ErrWorkspaceRootMissing) {
		t.Fatalf("exportWorkspace error = %v, want ErrWorkspaceRootMissing", err)
	}
	if !strings.Contains(err.Error(), "k8s-sandbox executor") {
		t.Errorf("error %q does not name the executor", err)
	}
	if receipt.Delivered() {
		t.Errorf("receipt = %+v, want an undelivered receipt", receipt)
	}
	if n := len(uploads()); n != 0 {
		t.Errorf("uploads = %d, want 0", n)
	}
}

// TestExportWorkspace_EmptyWorkspaceUploadsAnEmptyArchive pins that an
// empty workspace uploads an empty archive and reports status "empty".
func TestExportWorkspace_EmptyWorkspaceUploadsAnEmptyArchive(t *testing.T) {
	uploads := installGCSUploadCapture(t)
	cfg := &types.RunConfig{}
	cfg.Executor.Type = "k8s"
	cfg.Executor.WorkspaceExportTo = "gs://bucket/runs/r1/workspace.tar.gz"

	receipt, err := exportWorkspace(context.Background(), cfg, &archiverExecutor{}, cfg.Executor.WorkspaceExportTo)
	if err != nil {
		t.Fatalf("exportWorkspace: %v", err)
	}
	if receipt.Status != types.ExportStatusEmpty {
		t.Errorf("receipt status = %q, want %q", receipt.Status, types.ExportStatusEmpty)
	}
	got := uploads()
	if len(got) != 1 {
		t.Fatalf("uploads = %d, want 1 (an empty archive)", len(got))
	}
	if files := uploadedFiles(t, got[0]); len(files) != 0 {
		t.Errorf("empty workspace uploaded entries %v", files)
	}
}

// TestExportWorkspace_ExecutorWithoutArchiver pins that an executor lacking
// the archiver capability fails the export without building an exporter.
func TestExportWorkspace_ExecutorWithoutArchiver(t *testing.T) {
	uploads := installGCSUploadCapture(t)
	cfg := &types.RunConfig{}
	cfg.Executor.Type = "none"
	cfg.Executor.WorkspaceExportTo = "gs://bucket/runs/r1/workspace.tar.gz"

	receipt, err := exportWorkspace(context.Background(), cfg, &executor.NoneExecutor{}, cfg.Executor.WorkspaceExportTo)
	if err == nil || !strings.Contains(err.Error(), "cannot archive its workspace") {
		t.Fatalf("error = %v, want a cannot-archive failure", err)
	}
	if receipt.Status != types.ExportStatusFailed || receipt.URI != cfg.Executor.WorkspaceExportTo {
		t.Errorf("receipt = %+v, want a failed receipt for the destination", receipt)
	}
	if n := len(uploads()); n != 0 {
		t.Errorf("uploads = %d, want 0", n)
	}
}

// TestRunWithConfig_ExportsLoopExecutorWorkspace pins that a harness run
// exports through the loop's executor: the upload holds a file seeded in
// the executor's workspace, not the harness process's working directory.
func TestRunWithConfig_ExportsLoopExecutorWorkspace(t *testing.T) {
	srv := openAICompatibleSuccessServer(t)
	defer srv.Close()
	uploads := installGCSUploadCapture(t)
	t.Chdir(t.TempDir())

	config := runOutcomeTestConfig(t, srv.URL)
	config.Executor.WorkspaceExportTo = "gs://bucket/runs/r1/workspace.tar.gz"
	if err := os.WriteFile(filepath.Join(config.Executor.Workspace, "seed.txt"), []byte("from the executor"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdoutDone := captureStdout(t)
	runErr := runWithConfig(config, runOptions{outputMode: "json"})
	_ = stdoutDone()
	if runErr != nil {
		t.Fatalf("runWithConfig: %v", runErr)
	}
	got := uploads()
	if len(got) != 1 {
		t.Fatalf("uploads = %d, want 1", len(got))
	}
	if files := uploadedFiles(t, got[0]); files["seed.txt"] != "from the executor" {
		t.Errorf("uploaded entries = %v, want seed.txt from the executor's workspace", files)
	}
}
