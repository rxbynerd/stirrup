//go:build integration_k8s

package executor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// TestK8sExecutorLifecycle exercises the full create/wait/delete lifecycle
// against a real cluster. It is gated by build tag `integration_k8s` and by
// STIRRUP_TEST_KUBECONFIG so the default `just test` run never touches a
// real cluster.
func TestK8sExecutorLifecycle(t *testing.T) {
	kubeconfig := os.Getenv("STIRRUP_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("STIRRUP_TEST_KUBECONFIG not set; skipping k8s integration test")
	}

	namespace := "default"

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	exec, err := NewK8sExecutor(ctx, K8sExecutorConfig{
		Image:      "busybox:latest",
		Namespace:  namespace,
		Kubeconfig: kubeconfig,
	})
	if err != nil {
		t.Fatalf("NewK8sExecutor: %v", err)
	}

	closed := false
	t.Cleanup(func() {
		if closed {
			return
		}
		_ = exec.Close()
	})

	if err := exec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed = true

	clientset, err := kubeClientFromConfig(t, kubeconfig)
	if err != nil {
		t.Fatalf("build verification clientset: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, getErr := clientset.CoreV1().Pods(namespace).Get(context.Background(), exec.podName, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pod %s still present 5s after Close (last get err: %v)", exec.podName, getErr)
		}
		time.Sleep(200 * time.Millisecond)
	}

	if err := exec.Close(); err != nil {
		t.Fatalf("second Close should be idempotent, got: %v", err)
	}
}

// TestK8sExecutorCloseIdempotent verifies the standalone idempotency
// guarantee: calling Close twice on an executor whose Pod is already gone
// must return nil from the second call.
func TestK8sExecutorCloseIdempotent(t *testing.T) {
	kubeconfig := os.Getenv("STIRRUP_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("STIRRUP_TEST_KUBECONFIG not set; skipping k8s integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	exec, err := NewK8sExecutor(ctx, K8sExecutorConfig{
		Image:      "busybox:latest",
		Namespace:  "default",
		Kubeconfig: kubeconfig,
	})
	if err != nil {
		t.Fatalf("NewK8sExecutor: %v", err)
	}

	if err := exec.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := exec.Close(); err != nil {
		t.Fatalf("second Close should be nil, got: %v", err)
	}
}

func kubeClientFromConfig(t *testing.T, kubeconfig string) (kubernetes.Interface, error) {
	t.Helper()
	restCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(restCfg)
}

// newTestK8sExecutor constructs a K8sExecutor against the kind cluster named
// by STIRRUP_TEST_KUBECONFIG, skipping when it is unset. The image must ship
// /bin/sh, tar, and ls — busybox satisfies all three. The executor's Pod is
// torn down on test cleanup.
func newTestK8sExecutor(t *testing.T) *K8sExecutor {
	t.Helper()
	kubeconfig := os.Getenv("STIRRUP_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("STIRRUP_TEST_KUBECONFIG not set; skipping k8s integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	exec, err := NewK8sExecutor(ctx, K8sExecutorConfig{
		Image:      "busybox:latest",
		Namespace:  "default",
		Kubeconfig: kubeconfig,
	})
	if err != nil {
		t.Fatalf("NewK8sExecutor: %v", err)
	}
	t.Cleanup(func() { _ = exec.Close() })
	return exec
}

// TestK8sExec_Success exercises a clean exit: echo writes to stdout and the
// process exits 0.
func TestK8sExec_Success(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	res, err := exec.Exec(ctx, "echo hello", 10*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.Stdout != "hello\n" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "hello\n")
	}
}

// TestK8sExec_NonZeroExit verifies that a command writing to stderr and
// exiting non-zero surfaces both the exit code and stderr.
func TestK8sExec_NonZeroExit(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	res, err := exec.Exec(ctx, "echo err 1>&2; exit 7", 10*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "err") {
		t.Errorf("Stderr = %q, want it to contain %q", res.Stderr, "err")
	}
}

// TestK8sExec_Timeout verifies that a command exceeding the timeout returns
// the context deadline error (verbatim) at roughly the timeout boundary,
// not after the full sleep.
func TestK8sExec_Timeout(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	start := time.Now()
	_, err := exec.Exec(ctx, "sleep 5", 1*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Exec: err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("Exec took %s, expected ~1s (timeout not honoured)", elapsed)
	}
}

// TestK8sExec_OutputCap verifies the 10 MB output cap fires when a command
// emits more than the limit.
func TestK8sExec_OutputCap(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	// Emit ~11 MB to stdout. `yes` + head produces a deterministic stream;
	// 11*1024*1024 bytes of "x\n" comfortably exceeds the 10 MB cap.
	_, err := exec.Exec(ctx, "yes x | head -c 11534336", 30*time.Second)
	if !errors.Is(err, errK8sOutputCap) {
		t.Fatalf("Exec: err = %v, want errK8sOutputCap", err)
	}
}

// TestK8sFileIO_RoundTrip writes and reads back several payloads, including
// UTF-8 and an embedded NUL byte, confirming the tar-over-exec path is
// byte-exact.
func TestK8sFileIO_RoundTrip(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	cases := map[string]string{
		"ascii.txt":    "hello world\n",
		"utf8.txt":     "héllo — 世界 🌍\n",
		"nul.bin":      "before\x00after",
		"nested/a.txt": "deep\n",
	}

	for name, content := range cases {
		name, content := name, content
		t.Run(name, func(t *testing.T) {
			if err := exec.WriteFile(ctx, name, content); err != nil {
				t.Fatalf("WriteFile(%q): %v", name, err)
			}
			got, err := exec.ReadFile(ctx, name)
			if err != nil {
				t.Fatalf("ReadFile(%q): %v", name, err)
			}
			if got != content {
				t.Errorf("ReadFile(%q) = %q, want %q", name, got, content)
			}
		})
	}
}

// TestK8sReadFile_Missing verifies a missing path maps to fs.ErrNotExist so
// callers can branch with errors.Is.
func TestK8sReadFile_Missing(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	_, err := exec.ReadFile(ctx, "does-not-exist.txt")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadFile missing: err = %v, want fs.ErrNotExist", err)
	}
}

// TestK8sReadFile_Directory verifies that reading a directory is rejected
// with an "is a directory" error rather than returning archive bytes.
func TestK8sReadFile_Directory(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	if err := exec.WriteFile(ctx, "adir/keep.txt", "x"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := exec.ReadFile(ctx, "adir")
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("ReadFile directory: err = %v, want 'is a directory'", err)
	}
}

// TestK8sListDirectory lists a directory seeded via WriteFile and confirms
// the written entry appears (and that "." / ".." are excluded).
func TestK8sListDirectory(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	if err := exec.WriteFile(ctx, "listdir/one.txt", "1"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	entries, err := exec.ListDirectory(ctx, "listdir")
	if err != nil {
		t.Fatalf("ListDirectory: %v", err)
	}
	found := false
	for _, e := range entries {
		if e == "." || e == ".." {
			t.Errorf("ListDirectory returned %q, want it excluded", e)
		}
		if e == "one.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("ListDirectory = %v, want it to contain one.txt", entries)
	}
}

// TestK8sListDirectory_Missing verifies a missing directory maps to
// fs.ErrNotExist.
func TestK8sListDirectory_Missing(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	_, err := exec.ListDirectory(ctx, "no-such-dir")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ListDirectory missing: err = %v, want fs.ErrNotExist", err)
	}
}

// TestK8sListDirectory_WorkspaceRoot verifies that listing the workspace
// root is permitted (an empty path resolves to /workspace). ReadFile and
// WriteFile reject the root via resolveFilePath, but enumerating it is a
// legitimate listing operation — this pins that asymmetry against a cluster.
func TestK8sListDirectory_WorkspaceRoot(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	if err := exec.WriteFile(ctx, "rootfile.txt", "x"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	entries, err := exec.ListDirectory(ctx, "")
	if err != nil {
		t.Fatalf("ListDirectory(\"\"): %v", err)
	}
	found := false
	for _, e := range entries {
		if e == "rootfile.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("ListDirectory(\"\") = %v, want it to contain rootfile.txt", entries)
	}
}

// TestK8sWriteFile_RejectsWorkspaceRoot verifies finding #1 against a
// cluster: a write whose path resolves to the workspace root is rejected
// before any tar extraction runs.
func TestK8sWriteFile_RejectsWorkspaceRoot(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	for _, p := range []string{"", ".", "/workspace"} {
		if err := exec.WriteFile(ctx, p, "data"); err == nil {
			t.Errorf("WriteFile(%q): expected workspace-root rejection", p)
		}
	}
}
