package workspaceexport

import (
	"archive/tar"
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

// TestGCSExporter_ContainerExecutor_UploadsSandboxWorkspace exports a real
// container's workspace end to end: tar runs in the sandbox, the stream is
// repacked, and the upload holds exactly what the sandbox wrote. Gated on
// STIRRUP_TEST_CONTAINER_SOCKET; STIRRUP_TEST_CONTAINER_IMAGE (default
// busybox:latest) must already be present locally.
func TestGCSExporter_ContainerExecutor_UploadsSandboxWorkspace(t *testing.T) {
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
	require.NoError(t, os.Chmod(hostDir, 0o777))
	exec, err := executor.NewContainerExecutorWithContext(ctx, executor.ContainerExecutorConfig{
		Image:      image,
		HostDir:    hostDir,
		SocketPath: sock,
		Network:    &types.NetworkConfig{Mode: "none"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = exec.Close() })

	res, err := exec.Exec(ctx, "mkdir -p sub && printf top > top.txt && printf nested > sub/nested.txt && ln -s top.txt alias && ln top.txt hard && ln -s ../alias sub/via", 30*time.Second)
	require.NoError(t, err, "seed workspace")
	require.Equal(t, 0, res.ExitCode, "seed workspace result %+v", res)

	srv := &captureUploadServer{}
	exp := newTestExporter(t, srv)
	receipt, err := exp.Export(ctx, exec, "gs://b/runs/r1/workspace.tar.gz")
	require.NoError(t, err)

	t.Run("receipt reports uploaded with five files", func(t *testing.T) {
		require.Equal(t, types.ExportStatusUploaded, receipt.Status, "receipt %+v", receipt)
		require.Equal(t, 5, receipt.Files, "receipt %+v", receipt)
	})

	got := map[string]string{}
	kinds := map[string]byte{}
	for _, h := range readUploaded(t, srv.last().Body) {
		got[h.Name] = h.Linkname
		kinds[h.Name] = h.Typeflag
	}

	t.Run("upload contains every seeded entry", func(t *testing.T) {
		for _, name := range []string{"top.txt", "sub/", "sub/nested.txt", "alias", "hard", "sub/via"} {
			require.Contains(t, got, name)
		}
	})

	t.Run("alias links to top.txt", func(t *testing.T) {
		require.Equal(t, "top.txt", got["alias"])
	})

	t.Run("hard link is stored as a hard link entry", func(t *testing.T) {
		require.True(t, kinds["hard"] == tar.TypeLink || kinds["top.txt"] == tar.TypeLink, "kinds %v", kinds)
	})
}
