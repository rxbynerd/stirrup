package workspaceexport

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

// TestGCSExporter_ContainerExecutor_LiveEngine exports a real container's
// workspace end to end: tar runs in the sandbox, the stream is repacked,
// and the upload holds exactly what the sandbox wrote. Gated on
// STIRRUP_TEST_CONTAINER_SOCKET; STIRRUP_TEST_CONTAINER_IMAGE (default
// busybox:latest) must already be present locally.
func TestGCSExporter_ContainerExecutor_LiveEngine(t *testing.T) {
	sock := os.Getenv("STIRRUP_TEST_CONTAINER_SOCKET")
	if sock == "" {
		t.Skip("STIRRUP_TEST_CONTAINER_SOCKET not set; skipping live container engine test")
	}
	image := os.Getenv("STIRRUP_TEST_CONTAINER_IMAGE")
	if image == "" {
		image = "busybox:latest"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	hostDir := t.TempDir()
	if err := os.Chmod(hostDir, 0o777); err != nil {
		t.Fatal(err)
	}
	exec, err := executor.NewContainerExecutorWithContext(ctx, executor.ContainerExecutorConfig{
		Image:      image,
		HostDir:    hostDir,
		SocketPath: sock,
		Network:    &types.NetworkConfig{Mode: "none"},
	})
	if err != nil {
		t.Fatalf("NewContainerExecutorWithContext: %v", err)
	}
	t.Cleanup(func() { _ = exec.Close() })

	res, err := exec.Exec(ctx, "mkdir -p sub && printf top > top.txt && printf nested > sub/nested.txt && ln -s top.txt alias", 30*time.Second)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("seed workspace: err %v, result %+v", err, res)
	}

	srv := &captureUploadServer{}
	exp := newTestExporter(t, srv)
	receipt, err := exp.Export(ctx, exec, "gs://b/runs/r1/workspace.tar.gz")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if receipt.Status != types.ExportStatusUploaded || receipt.Files != 3 {
		t.Errorf("receipt = %+v, want uploaded with 3 files", receipt)
	}
	got := map[string]string{}
	for _, h := range readUploaded(t, srv.last().Body) {
		got[h.Name] = h.Linkname
	}
	for _, name := range []string{"top.txt", "sub/", "sub/nested.txt", "alias"} {
		if _, ok := got[name]; !ok {
			t.Errorf("upload is missing %q (entries %v)", name, got)
		}
	}
	if got["alias"] != "top.txt" {
		t.Errorf("alias link target = %q, want top.txt", got["alias"])
	}
}
