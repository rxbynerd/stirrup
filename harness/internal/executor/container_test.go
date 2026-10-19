package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// mockEngineServer creates a fake Docker Engine API server on a temporary Unix
// socket. The handler map keys are "METHOD /path" strings; the value is called
// for matching requests. Unmatched requests return 404.
func mockEngineServer(t *testing.T, handlers map[string]http.HandlerFunc) (socketPath string, cleanup func()) {
	t.Helper()

	// Use a short path to avoid macOS 108-char Unix socket limit.
	// t.TempDir() paths are too long on macOS.
	dir, err := os.MkdirTemp("/tmp", "de-")
	require.NoError(t, err, "create temp dir")
	sock := filepath.Join(dir, "s.sock")

	listener, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(dir)
		require.NoError(t, err, "listen on unix socket")
	}

	mux := http.NewServeMux()
	// Register a catch-all that dispatches based on method+path.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Strip the API version prefix for matching.
		apiPath := r.URL.Path
		if idx := strings.Index(apiPath[1:], "/"); idx >= 0 {
			apiPath = apiPath[idx+1:]
		}

		key := r.Method + " " + apiPath
		if h, ok := handlers[key]; ok {
			h(w, r)
			return
		}

		// Try prefix matching for parameterised routes.
		for pattern, h := range handlers {
			parts := strings.SplitN(pattern, " ", 2)
			if len(parts) == 2 && parts[0] == r.Method && matchPath(apiPath, parts[1]) {
				h(w, r)
				return
			}
		}

		t.Logf("unhandled request: %s %s", r.Method, r.URL.Path)
		http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(listener) }()

	return sock, func() {
		_ = srv.Close()
		_ = listener.Close()
		_ = os.RemoveAll(dir)
	}
}

// matchPath does simple prefix matching for routes like /containers/*/exec.
func matchPath(actual, pattern string) bool {
	// Convert pattern like /containers/*/exec to check prefix and suffix.
	if !strings.Contains(pattern, "*") {
		return actual == pattern
	}
	parts := strings.SplitN(pattern, "*", 2)
	return strings.HasPrefix(actual, parts[0]) && strings.HasSuffix(actual, parts[1])
}

// --- API client tests ---

func TestContainerAPIClient_CreateContainer_SendsRequestBody(t *testing.T) {
	var receivedBody containerCreateRequest

	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
			_ = json.NewDecoder(r.Body).Decode(&receivedBody)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(containerCreateResponse{ID: "abc123"})
		},
	})
	defer cleanup()

	client := newContainerAPIClient(sock)
	id, err := client.createContainer(context.Background(), containerCreateRequest{
		Image:      "ubuntu:26.04",
		Cmd:        []string{"sleep", "infinity"},
		WorkingDir: "/workspace",
		HostConfig: &hostConfig{
			Binds:       []string{"/host/dir:/workspace"},
			NetworkMode: "none",
			NanoCPUs:    2000000000,
			Memory:      536870912,
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
		},
	})
	require.NoError(t, err, "createContainer")
	require.Equal(t, "abc123", id)

	// Verify the body was sent correctly.
	require.Equal(t, "ubuntu:26.04", receivedBody.Image)
	require.Equal(t, "none", receivedBody.HostConfig.NetworkMode)
	require.Equal(t, int64(2000000000), receivedBody.HostConfig.NanoCPUs)
	require.Equal(t, []string{"ALL"}, receivedBody.HostConfig.CapDrop)
}

func TestContainerAPIClient_APIErrorSurfacesMessage(t *testing.T) {
	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(apiError{Message: "image not found"})
		},
	})
	defer cleanup()

	client := newContainerAPIClient(sock)
	_, err := client.createContainer(context.Background(), containerCreateRequest{Image: "bad:image"})
	require.ErrorContains(t, err, "image not found")
}

func TestContainerAPIClient_ExecCreateStartInspect(t *testing.T) {
	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
			var req execCreateRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			assert.True(t, req.AttachStdout, "exec should attach stdout")
			assert.True(t, req.AttachStderr, "exec should attach stderr")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execCreateResponse{ID: "exec123"})
		},
		"POST /exec/*/start": func(w http.ResponseWriter, r *http.Request) {
			// Write a multiplexed stream: stdout frame with "hello\n".
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			writeDockerFrame(w, 1, []byte("hello\n"))
			writeDockerFrame(w, 2, []byte("warning\n"))
		},
		"GET /exec/*/json": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execInspectResponse{ExitCode: 0, Running: false})
		},
	})
	defer cleanup()

	client := newContainerAPIClient(sock)

	execID, err := client.createExec(context.Background(), "container1", []string{"echo", "hello"}, "/workspace")
	require.NoError(t, err, "createExec")
	require.Equal(t, "exec123", execID)

	stream, err := client.startExec(context.Background(), execID)
	require.NoError(t, err, "startExec")
	defer func() { _ = stream.Close() }()

	stdout, stderr, err := demuxDockerStream(stream)
	require.NoError(t, err, "demuxDockerStream")
	require.Equal(t, "hello\n", stdout)
	require.Equal(t, "warning\n", stderr)

	exitCode, err := client.inspectExec(context.Background(), execID)
	require.NoError(t, err, "inspectExec")
	require.Zero(t, exitCode)
}

// writeDockerFrame writes a single frame in Docker's multiplexed stream format.
func writeDockerFrame(w io.Writer, streamType byte, data []byte) {
	header := make([]byte, 8)
	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:], uint32(len(data)))
	_, _ = w.Write(header)
	_, _ = w.Write(data)
}

// --- ContainerExecutor tests (with mock server) ---

// newMockContainerExecutor creates a ContainerExecutor backed by a mock
// Engine API server. The default handlers simulate a working container.
func newMockContainerExecutor(t *testing.T, extraHandlers map[string]http.HandlerFunc) (*ContainerExecutor, func()) {
	t.Helper()

	handlers := map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(containerCreateResponse{ID: "test-container-id"})
		},
		"POST /containers/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"POST /containers/*/stop": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"DELETE /containers/*": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
	}
	for k, v := range extraHandlers {
		handlers[k] = v
	}

	sock, sockCleanup := mockEngineServer(t, handlers)
	// Mock exec handlers that hang would otherwise stall the follow-up kill
	// exec for the full production bound.
	withShortTimeout(t, &groupKillTimeout, time.Second)

	exec := &ContainerExecutor{
		api:         newContainerAPIClient(sock),
		containerID: "test-container-id",
		workspace:   containerWorkspace,
		hostDir:     "/tmp/test-workspace",
		networkMode: "none",
	}

	return exec, sockCleanup
}

func TestContainerExecutor_ResolvePath(t *testing.T) {
	exec := &ContainerExecutor{workspace: containerWorkspace}

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "relative file", input: "foo.txt", want: "/workspace/foo.txt"},
		{name: "nested relative path", input: "sub/dir/file.go", want: "/workspace/sub/dir/file.go"},
		{name: "absolute path inside workspace", input: "/workspace/inside.txt", want: "/workspace/inside.txt"},
		{name: "parent traversal rejected", input: "../etc/passwd", wantErr: true},
		{name: "traversal after descent rejected", input: "foo/../../etc/passwd", wantErr: true},
		{name: "absolute path outside workspace rejected", input: "/etc/passwd", wantErr: true},
		{name: "prefix sibling rejected", input: "/workspacefoo", wantErr: true}, // must not match prefix without separator
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := exec.ResolvePath(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestContainerExecutor_ResolvePath_EmitsPathTraversalBlocked(t *testing.T) {
	emitter := &mockSecurityEmitter{}
	exec := &ContainerExecutor{workspace: containerWorkspace, Security: emitter}

	_, err := exec.ResolvePath("../escape")
	require.Error(t, err)
	require.Equal(t, 1, emitter.pathTraversalCount, "PathTraversalBlocked calls")
}

func TestContainerExecutor_ReadFile(t *testing.T) {
	fileContent := "hello from container"

	capture := &readExecCapture{}
	exec, cleanup := newMockContainerExecutor(t, readExecHandlers(capture, singleFileTar(t, "workspace/test.txt", fileContent), "", 0))
	defer cleanup()

	got, err := exec.ReadFile(context.Background(), "test.txt")
	require.NoError(t, err, "ReadFile")
	require.Equal(t, fileContent, got)

	argv := capture.last()
	require.Len(t, argv, 6, "guarded read command, got %q", argv)
	require.Equal(t, "/workspace/test.txt", argv[5], "guarded read target")
}

func TestContainerExecutor_ReadFile_TooLarge(t *testing.T) {
	// A header declaring more than the cap is enough: the size check
	// precedes any content read.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "workspace/big.bin", Size: maxFileSize + 1, Mode: 0o644})
	_ = tw.Close()

	exec, cleanup := newMockContainerExecutor(t, readExecHandlers(nil, buf.Bytes(), "", 0))
	defer cleanup()

	_, err := exec.ReadFile(context.Background(), "big.bin")
	require.ErrorContains(t, err, "too large")
}

func TestContainerExecutor_ReadFile_Directory(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "workspace/subdir/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.Close()

	exec, cleanup := newMockContainerExecutor(t, readExecHandlers(nil, buf.Bytes(), "", 0))
	defer cleanup()

	_, err := exec.ReadFile(context.Background(), "subdir")
	require.ErrorContains(t, err, "directory")
}

func TestContainerExecutor_ReadFile_Missing(t *testing.T) {
	exec, cleanup := newMockContainerExecutor(t, readExecHandlers(nil, nil, "tar: workspace/missing.txt: No such file or directory\n", 2))
	defer cleanup()

	_, err := exec.ReadFile(context.Background(), "missing.txt")
	require.ErrorIs(t, err, fs.ErrNotExist)
}

func TestContainerExecutor_ReadFile_PathTraversal(t *testing.T) {
	exec, cleanup := newMockContainerExecutor(t, nil)
	defer cleanup()

	_, err := exec.ReadFile(context.Background(), "../etc/passwd")
	require.ErrorContains(t, err, "escapes workspace")
}

func TestContainerExecutor_WriteFile(t *testing.T) {
	var uploadedContent string
	var uploadDest string

	exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
		"PUT /containers/*/archive": func(w http.ResponseWriter, r *http.Request) {
			uploadDest = r.URL.Query().Get("path")
			tr := tar.NewReader(r.Body)
			hdr, _ := tr.Next()
			if hdr != nil {
				data, _ := io.ReadAll(tr)
				uploadedContent = string(data)
			}
			w.WriteHeader(http.StatusOK)
		},
		"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
			// mkdir -p call
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execCreateResponse{ID: "mkdir-exec"})
		},
		"POST /exec/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		},
		"GET /exec/*/json": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execInspectResponse{ExitCode: 0})
		},
	})
	defer cleanup()

	err := exec.WriteFile(context.Background(), "sub/dir/test.txt", "file content")
	require.NoError(t, err, "WriteFile")
	require.Equal(t, "file content", uploadedContent)
	require.Equal(t, "/workspace/sub/dir", uploadDest)
}

func TestContainerExecutor_WriteFile_TooLarge(t *testing.T) {
	exec, cleanup := newMockContainerExecutor(t, nil)
	defer cleanup()

	bigContent := strings.Repeat("x", maxFileSize+1)
	err := exec.WriteFile(context.Background(), "big.txt", bigContent)
	require.ErrorContains(t, err, "too large")
}

func TestContainerExecutor_WriteFile_PathTraversal(t *testing.T) {
	exec, cleanup := newMockContainerExecutor(t, nil)
	defer cleanup()

	err := exec.WriteFile(context.Background(), "../escape.txt", "bad")
	require.Error(t, err, "expected error for path traversal write")
}

func TestContainerExecutor_Exec_WrapsCommandAndReportsExitCode(t *testing.T) {
	exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
		"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
			var req execCreateRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			// The command runs through the process-group wrapper, which
			// passes the original command as its last argument.
			if assert.Len(t, req.Cmd, 7, "exec cmd should be the sh -c group wrapper around the command") {
				assert.Equal(t, []string{"sh", "-c"}, req.Cmd[:2])
				assert.Equal(t, "echo output", req.Cmd[6])
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execCreateResponse{ID: "exec-run"})
		},
		"POST /exec/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			writeDockerFrame(w, 1, []byte("output\n"))
		},
		"GET /exec/*/json": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execInspectResponse{ExitCode: 42})
		},
	})
	defer cleanup()

	result, err := exec.Exec(context.Background(), "echo output", 0)
	require.NoError(t, err, "Exec")
	require.Equal(t, 42, result.ExitCode)
	require.Equal(t, "output", strings.TrimSpace(result.Stdout))
}

func TestContainerExecutor_Exec_OutputTruncation(t *testing.T) {
	bigOutput := strings.Repeat("A", maxOutputSize+1000)

	exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
		"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execCreateResponse{ID: "exec-big"})
		},
		"POST /exec/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			// Write in chunks since the frame size is uint32.
			writeDockerFrame(w, 1, []byte(bigOutput))
		},
		"GET /exec/*/json": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execInspectResponse{ExitCode: 0})
		},
	})
	defer cleanup()

	result, err := exec.Exec(context.Background(), "cat bigfile", 10*time.Second)
	require.NoError(t, err, "Exec")
	require.True(t, strings.HasSuffix(result.Stdout, truncatedSuffix), "expected truncation suffix on large output")
	require.True(t, result.OutputTruncated, "expected ExecResult.OutputTruncated true on capped output")
}

// hangingExecStartHandler returns a "POST /exec/*/start" handler that
// writes one stdout frame, flushes it, then blocks until the request's ctx
// ends (the client cancels or its deadline elapses) — simulating a stalled
// command whose exec stream never closes on its own. Used to exercise both
// the timeout and cancellation paths of ContainerExecutor.Exec against a
// command that is genuinely still running when the ctx ends.
func hangingExecStartHandler(partialOutput string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		writeDockerFrame(w, 1, []byte(partialOutput))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}
}

// TestContainerExecutor_Exec_Timeout verifies that a genuine per-call
// deadline expiry classifies via errors.Is(err, ErrTimeout) (not a
// substring match) and preserves whatever stdout was captured before the
// deadline hit.
func TestContainerExecutor_Exec_Timeout(t *testing.T) {
	exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
		"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execCreateResponse{ID: "exec-timeout"})
		},
		"POST /exec/*/start": hangingExecStartHandler("partial output\n"),
	})
	defer cleanup()

	start := time.Now()
	result, err := exec.Exec(context.Background(), "sleep 30", 200*time.Millisecond)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrTimeout)
	require.LessOrEqual(t, elapsed, 5*time.Second, "Exec should return well under 5s after the 200ms deadline")
	require.NotNil(t, result, "a timed-out exec should still return its result")
	require.Contains(t, result.Stdout, "partial output", "Stdout should keep output captured before the timeout")
}

// TestContainerExecutor_Exec_CancelledContext_NotErrTimeout verifies that a
// parent-context cancellation that is not a deadline expiry must not be
// reported as (or satisfy errors.Is against) ErrTimeout, even with a large
// configured timeout, and that partial output captured before the cancel is
// still returned.
func TestContainerExecutor_Exec_CancelledContext_NotErrTimeout(t *testing.T) {
	exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
		"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execCreateResponse{ID: "exec-cancel"})
		},
		"POST /exec/*/start": hangingExecStartHandler("partial output\n"),
	})
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	result, err := exec.Exec(ctx, "sleep 30", 60*time.Second)
	require.Error(t, err, "expected an error from the cancelled command")
	require.NotErrorIs(t, err, ErrTimeout, "cancelled after 150ms, configured timeout was 60s")
	require.NotContains(t, err.Error(), "timed out", "a cancelled command must not claim it timed out")
	require.NotNil(t, result, "a cancelled exec should still return its result")
	require.Contains(t, result.Stdout, "partial output", "Stdout should keep output captured before the cancel")
}

// Docker API client / file-I/O timeout tests: a wedged daemon must not
// hang the executor forever on any call class, while a slow-but-legitimate
// streaming exec must not be killed by a blanket short timeout. Each test
// shrinks the relevant package-level timeout var so the scenario completes
// well under a second instead of the real 10-60s production window.

// withShortTimeout sets *knob to short for the duration of the test and
// restores the original value via t.Cleanup, so package-level timeout vars
// shared across the executor package tests are never left mutated for
// unrelated tests.
func withShortTimeout(t *testing.T, knob *time.Duration, short time.Duration) {
	t.Helper()
	orig := *knob
	*knob = short
	t.Cleanup(func() { *knob = orig })
}

// TestContainerAPIClient_ControlPlaneCall_BoundedWithoutCallerDeadline
// verifies that a fake daemon accepting the connection but never
// responding does not hang a control-plane call forever even with no
// caller-supplied context deadline — containerControlPlaneTimeout must
// still bound it, classifying via errors.Is(err, ErrTimeout).
func TestContainerAPIClient_ControlPlaneCall_BoundedWithoutCallerDeadline(t *testing.T) {
	withShortTimeout(t, &containerControlPlaneTimeout, 150*time.Millisecond)

	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"GET /_ping": func(w http.ResponseWriter, r *http.Request) {
			// Never write a response. The handler blocks on the server's
			// request context, which is cancelled once our client gives up
			// on its own deadline and the connection closes — so
			// mockEngineServer's Close() doesn't hang waiting for this
			// handler to return.
			<-r.Context().Done()
		},
	})
	defer cleanup()

	client := newContainerAPIClient(sock)

	start := time.Now()
	err := client.ping(context.Background())
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrTimeout)
	require.LessOrEqual(t, elapsed, 5*time.Second, "ping with no caller deadline should be bounded by containerControlPlaneTimeout (~150ms)")
}

// TestContainerAPIClient_ConnectionLevelHang_BoundedByResponseHeaderTimeout
// proves the connection-level backstop independently of any per-call
// context deadline: even with a much larger containerControlPlaneTimeout,
// the Transport's ResponseHeaderTimeout still bounds the wait for headers.
// A pure connection-level bound, so it need not classify via ErrTimeout.
func TestContainerAPIClient_ConnectionLevelHang_BoundedByResponseHeaderTimeout(t *testing.T) {
	withShortTimeout(t, &containerResponseHeaderTimeout, 150*time.Millisecond)
	// Deliberately much larger than the header timeout above, so the
	// ctx-deadline path this test is NOT exercising cannot be the thing
	// that saves us.
	withShortTimeout(t, &containerControlPlaneTimeout, 10*time.Second)

	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"GET /_ping": func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		},
	})
	defer cleanup()

	client := newContainerAPIClient(sock)

	start := time.Now()
	err := client.ping(context.Background())
	elapsed := time.Since(start)

	require.Error(t, err, "expected an error from a daemon that never responds")
	require.LessOrEqual(t, elapsed, 5*time.Second, "ping should be bounded by containerResponseHeaderTimeout (~150ms) despite a 10s control-plane ctx budget")
}

// TestContainerExecutor_WriteFile_MkdirTimeout verifies that WriteFile's
// "mkdir -p" call is bounded by containerMkdirTimeout (shrunk here for
// speed) even with no caller-supplied deadline and a wedged daemon, and
// that the resulting error classifies via ErrTimeout.
func TestContainerExecutor_WriteFile_MkdirTimeout(t *testing.T) {
	withShortTimeout(t, &containerMkdirTimeout, 150*time.Millisecond)

	exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
		"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
			// mkdir -p's exec-create call itself succeeds fast; only the
			// stream never responds, matching a daemon wedged mid-command.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execCreateResponse{ID: "mkdir-exec-hang"})
		},
		"POST /exec/*/start": func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		},
	})
	defer cleanup()

	start := time.Now()

	err := exec.WriteFile(context.Background(), "sub/dir/test.txt", "file content")
	elapsed := time.Since(start)

	require.Error(t, err, "expected an error from a daemon wedged mid-mkdir")
	require.ErrorIs(t, err, ErrTimeout)
	require.LessOrEqual(t, elapsed, 5*time.Second, "WriteFile with no caller-supplied deadline should be bounded by containerMkdirTimeout (~150ms)")
}

// TestContainerExecutor_Exec_SlowStreamNotKilledByControlPlaneTimeout
// verifies the call-class distinction: a legitimate command whose output
// trickles in past the (shrunk) control-plane window, but well within its
// own configured command timeout, must complete successfully rather than
// being killed by a blanket short timeout leaking into the streaming path.
func TestContainerExecutor_Exec_SlowStreamNotKilledByControlPlaneTimeout(t *testing.T) {
	withShortTimeout(t, &containerControlPlaneTimeout, 100*time.Millisecond)

	exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
		"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execCreateResponse{ID: "exec-slow-stream"})
		},
		"POST /exec/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			writeDockerFrame(w, 1, []byte("part1\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Sleep past containerControlPlaneTimeout (100ms) but well
			// within the command's own 2s timeout. startExec deliberately
			// does not apply the control-plane window (see
			// container_api.go), so this trickle must survive.
			time.Sleep(300 * time.Millisecond)
			writeDockerFrame(w, 1, []byte("part2\n"))
		},
		"GET /exec/*/json": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execInspectResponse{ExitCode: 0})
		},
	})
	defer cleanup()

	result, err := exec.Exec(context.Background(), "slow but legitimate command", 2*time.Second)
	require.NoError(t, err, "a slow but legitimate stream should complete")
	require.Contains(t, result.Stdout, "part1")
	require.Contains(t, result.Stdout, "part2", "stream must survive the control-plane window elapsing mid-read")
}

func TestContainerExecutor_ListDirectory_MarksSubdirectories(t *testing.T) {
	var gotCmd []string
	exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
		"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
			var req execCreateRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			gotCmd = req.Cmd
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execCreateResponse{ID: "exec-ls"})
		},
		"POST /exec/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			writeDockerFrame(w, 1, []byte("file.txt\nsubdir/\n"))
		},
		"GET /exec/*/json": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execInspectResponse{ExitCode: 0})
		},
	})
	defer cleanup()

	entries, err := exec.ListDirectory(context.Background(), ".")
	require.NoError(t, err)

	require.Contains(t, entries, "file.txt")
	require.Contains(t, entries, "subdir/", "directories should carry a trailing slash")
	require.Equal(t, []string{"ls", "-1Ap", "--", "/workspace"}, gotCmd)
}

func TestContainerExecutor_Capabilities_ByNetworkMode(t *testing.T) {
	tests := []struct {
		name        string
		networkMode string
		wantNetwork bool
	}{
		{"none mode has no network", "none", false},
		{"bridge mode has network", "bridge", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &ContainerExecutor{
				workspace:   containerWorkspace,
				networkMode: tt.networkMode,
			}
			caps := exec.Capabilities()
			require.True(t, caps.CanRead, "CanRead")
			require.True(t, caps.CanWrite, "CanWrite")
			require.True(t, caps.CanExec, "CanExec")
			require.Equal(t, tt.wantNetwork, caps.CanNetwork)
			require.Equal(t, maxTimeout, caps.MaxTimeout)
		})
	}
}

func TestContainerExecutor_CreateRequestCarriesLifecycle(t *testing.T) {
	var got containerCreateRequest

	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(containerCreateResponse{ID: "test-id"})
		},
		"POST /containers/*/start": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
		"POST /containers/*/stop":  func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
		"DELETE /containers/*":     func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
	})
	defer cleanup()

	exec, err := NewContainerExecutor(ContainerExecutorConfig{
		Image:      "ubuntu:26.04",
		HostDir:    "/tmp/workspace",
		SocketPath: sock,
		Lifecycle:  Lifecycle{RunID: "run-42", MaxLifetime: 90*time.Minute + 500*time.Millisecond},
	})
	require.NoError(t, err)
	defer func() { _ = exec.Close() }()

	require.Equal(t, []string{"sleep", "5401"}, got.Cmd, "lifetime should round up to whole seconds")
	require.True(t, got.HostConfig.AutoRemove, "container should be auto-removed")
	require.Equal(t, "true", got.Labels["stirrup.sandbox"])
	require.Equal(t, "run-42", got.Labels["stirrup.run-id"])
}

func TestContainerExecutor_CreateRequestWithoutLifetimeKeepsAlive(t *testing.T) {
	var got containerCreateRequest

	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(containerCreateResponse{ID: "test-id"})
		},
		"POST /containers/*/start": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
		"POST /containers/*/stop":  func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
		"DELETE /containers/*":     func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
	})
	defer cleanup()

	exec, err := NewContainerExecutor(ContainerExecutorConfig{
		Image:      "ubuntu:26.04",
		HostDir:    "/tmp/workspace",
		SocketPath: sock,
	})
	require.NoError(t, err)
	defer func() { _ = exec.Close() }()

	require.Equal(t, []string{"sleep", "infinity"}, got.Cmd)
	require.NotContains(t, got.Labels, "stirrup.run-id", "an empty run ID should not set a run-id label")
}

// TestContainerExecutor_CloseToleratesAutoRemovedContainer covers the engine
// having already reaped the container (its keep-alive sleep ended, or the
// stop triggered auto-removal) before the explicit delete arrives.
func TestContainerExecutor_CloseToleratesAutoRemovedContainer(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"not found", http.StatusNotFound, `{"message":"No such container: test-container-id"}`, false},
		{"removal already in progress", http.StatusConflict, `{"message":"removal of container test-container-id is already in progress"}`, false},
		{"other conflict", http.StatusConflict, `{"message":"container is paused"}`, true},
		{"server error", http.StatusInternalServerError, `{"message":"boom"}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
				"POST /containers/*/stop": func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusNotFound)
				},
				"DELETE /containers/*": func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tt.status)
					_, _ = w.Write([]byte(tt.body))
				},
			})
			defer cleanup()

			err := exec.Close()
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestContainerExecutor_Close_StopsAndRemovesContainer(t *testing.T) {
	stopCalled := false
	removeCalled := false

	exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
		"POST /containers/*/stop": func(w http.ResponseWriter, r *http.Request) {
			stopCalled = true
			w.WriteHeader(http.StatusNoContent)
		},
		"DELETE /containers/*": func(w http.ResponseWriter, r *http.Request) {
			removeCalled = true
			// Runs on the server goroutine, so assert rather than require.
			assert.Contains(t, r.URL.RawQuery, "force=true", "remove should be called with force=true")
			w.WriteHeader(http.StatusNoContent)
		},
	})
	defer cleanup()

	require.NoError(t, exec.Close())
	require.True(t, stopCalled, "stop should be called")
	require.True(t, removeCalled, "remove should be called")
}

func TestContainerExecutor_CreateFailure_SurfacesAPIMessage(t *testing.T) {
	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(apiError{Message: "image not found"})
		},
	})
	defer cleanup()

	_, err := NewContainerExecutor(ContainerExecutorConfig{
		Image:      "nonexistent:latest",
		HostDir:    "/tmp/workspace",
		SocketPath: sock,
	})
	require.ErrorContains(t, err, "image not found")
}

func TestContainerExecutor_StartFailure_RemovesContainer(t *testing.T) {
	removeCalled := false

	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(containerCreateResponse{ID: "orphan-container"})
		},
		"POST /containers/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(apiError{Message: "start failed"})
		},
		"DELETE /containers/*": func(w http.ResponseWriter, r *http.Request) {
			removeCalled = true
			w.WriteHeader(http.StatusNoContent)
		},
	})
	defer cleanup()

	_, err := NewContainerExecutor(ContainerExecutorConfig{
		Image:      "ubuntu:26.04",
		HostDir:    "/tmp/workspace",
		SocketPath: sock,
	})
	require.Error(t, err, "expected error for start failure")
	require.True(t, removeCalled, "container should be removed on start failure")
}

func TestContainerExecutor_ResourceLimits_MapToHostConfig(t *testing.T) {
	var receivedBody containerCreateRequest

	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&receivedBody)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(containerCreateResponse{ID: "test-id"})
		},
		"POST /containers/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"POST /containers/*/stop": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"DELETE /containers/*": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
	})
	defer cleanup()

	exec, err := NewContainerExecutor(ContainerExecutorConfig{
		Image:      "ubuntu:26.04",
		HostDir:    "/tmp/workspace",
		SocketPath: sock,
		Resources: &types.ResourceLimits{
			CPUs:     2.0,
			MemoryMB: 512,
			PIDs:     256,
		},
	})
	require.NoError(t, err)
	defer func() { _ = exec.Close() }()

	assert.Equal(t, int64(2000000000), receivedBody.HostConfig.NanoCPUs, "NanoCPUs")
	assert.Equal(t, int64(512*1024*1024), receivedBody.HostConfig.Memory, "Memory")
	require.NotNil(t, receivedBody.HostConfig.PidsLimit, "PidsLimit")
	assert.Equal(t, int64(256), *receivedBody.HostConfig.PidsLimit, "PidsLimit")
}

func TestContainerExecutor_NetworkMode_SelectsNoneOrBridge(t *testing.T) {
	var receivedBody containerCreateRequest

	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&receivedBody)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(containerCreateResponse{ID: "test-id"})
		},
		"POST /containers/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"POST /containers/*/stop": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"DELETE /containers/*": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
	})
	defer cleanup()

	tests := []struct {
		name     string
		network  *types.NetworkConfig
		wantMode string
	}{
		{"default network is none", nil, "none"},
		{"allowlist network is bridge", &types.NetworkConfig{Mode: "allowlist", Allowlist: []string{"example.com"}}, "bridge"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec, err := NewContainerExecutor(ContainerExecutorConfig{
				Image:      "ubuntu:26.04",
				HostDir:    "/tmp/workspace",
				SocketPath: sock,
				Network:    tt.network,
			})
			require.NoError(t, err)
			_ = exec.Close()

			require.Equal(t, tt.wantMode, receivedBody.HostConfig.NetworkMode)
		})
	}
}

func TestContainerExecutor_Runtime_SetAndOmittedWhenEmpty(t *testing.T) {
	// Capture the raw POST body so we can assert exactly what bytes go on
	// the wire. The Engine API treats a missing Runtime field as "use the
	// daemon default", so it matters that we (a) include "Runtime":"runsc"
	// when set and (b) omit the field entirely when not set — otherwise
	// older Docker versions can fail with `runtime "" not registered`.
	var rawBody []byte

	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			rawBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(containerCreateResponse{ID: "test-id"})
		},
		"POST /containers/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"POST /containers/*/stop": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"DELETE /containers/*": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
	})
	defer cleanup()

	// Configured runtime must appear in the create-container body.
	exec, err := NewContainerExecutor(ContainerExecutorConfig{
		Image:      "ubuntu:26.04",
		HostDir:    "/tmp/workspace",
		SocketPath: sock,
		Runtime:    "runsc",
	})
	require.NoError(t, err)
	_ = exec.Close()

	require.Contains(t, string(rawBody), `"Runtime":"runsc"`, "create body should carry the configured runtime")

	// Hardening defaults must still be present alongside the runtime.
	var parsed containerCreateRequest
	require.NoError(t, json.Unmarshal(rawBody, &parsed), "unmarshal create body")
	assert.Equal(t, "runsc", parsed.HostConfig.Runtime)
	assert.Equal(t, "none", parsed.HostConfig.NetworkMode)
	assert.Equal(t, []string{"ALL"}, parsed.HostConfig.CapDrop)

	// Empty runtime must be omitted entirely (engine picks its default).
	rawBody = nil
	exec, err = NewContainerExecutor(ContainerExecutorConfig{
		Image:      "ubuntu:26.04",
		HostDir:    "/tmp/workspace",
		SocketPath: sock,
	})
	require.NoError(t, err)
	_ = exec.Close()

	require.NotContains(t, string(rawBody), `"Runtime"`, "create body should omit Runtime when unset")
}

// TestContainerExecutor_AllowlistMode_WiringConfig verifies that selecting
// Network.Mode == "allowlist" causes the create-container request to:
//
//   - set NetworkMode to "bridge" (so the container can dial the host),
//   - inject ExtraHosts so host.docker.internal resolves to the host gateway,
//   - populate both spellings of HTTP_PROXY / HTTPS_PROXY / NO_PROXY env in
//     the container.
//
// The proxy itself is started on a real local port; we don't exercise it
// over the wire here (a planted-curl integration test belongs behind a
// build tag — see container_integration_test.go).
func TestContainerExecutor_AllowlistMode_WiringConfig(t *testing.T) {
	var receivedBody containerCreateRequest

	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&receivedBody)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(containerCreateResponse{ID: "test-id"})
		},
		"POST /containers/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"POST /containers/*/stop": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"DELETE /containers/*": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
	})
	defer cleanup()

	exec, err := NewContainerExecutor(ContainerExecutorConfig{
		Image:      "ubuntu:26.04",
		HostDir:    "/tmp/workspace",
		SocketPath: sock,
		Network: &types.NetworkConfig{
			Mode:      "allowlist",
			Allowlist: []string{"api.github.com"},
		},
	})
	require.NoError(t, err)
	defer func() { _ = exec.Close() }()

	assert.Equal(t, "bridge", receivedBody.HostConfig.NetworkMode)
	assert.Equal(t, []string{"host.docker.internal:host-gateway"}, receivedBody.HostConfig.ExtraHosts)

	envSeen := map[string]string{}
	for _, kv := range receivedBody.Env {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			envSeen[parts[0]] = parts[1]
		}
	}
	// Both spellings are injected: git honours only the lower-case
	// http_proxy for plain-http destinations.
	for _, k := range []string{
		"HTTP_PROXY", "http_proxy",
		"HTTPS_PROXY", "https_proxy",
		"NO_PROXY", "no_proxy",
	} {
		assert.Contains(t, envSeen, k, "Env=%v", receivedBody.Env)
	}
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		assert.Truef(t, strings.HasPrefix(envSeen[k], "http://host.docker.internal:"), "%s: got %q, want prefix http://host.docker.internal:<port>", k, envSeen[k])
		assert.Equal(t, envSeen["HTTP_PROXY"], envSeen[k], "%s should match HTTP_PROXY", k)
	}
	for _, k := range []string{"NO_PROXY", "no_proxy"} {
		assert.Contains(t, envSeen[k], "127.0.0.1", "%s should at least cover 127.0.0.1", k)
	}

	// Hardening defaults should still be present alongside the egress wiring.
	assert.Equal(t, []string{"ALL"}, receivedBody.HostConfig.CapDrop)
}

// TestContainerExecutor_NoneMode_NoProxyWiring confirms that the proxy
// lifecycle is only triggered when Network.Mode == "allowlist". For mode
// "none" the executor must not set ExtraHosts, must not set proxy env,
// and must keep NetworkMode == "none".
func TestContainerExecutor_NoneMode_NoProxyWiring(t *testing.T) {
	var receivedBody containerCreateRequest

	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&receivedBody)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(containerCreateResponse{ID: "test-id"})
		},
		"POST /containers/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"POST /containers/*/stop": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"DELETE /containers/*": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
	})
	defer cleanup()

	exec, err := NewContainerExecutor(ContainerExecutorConfig{
		Image:      "ubuntu:26.04",
		HostDir:    "/tmp/workspace",
		SocketPath: sock,
		Network:    &types.NetworkConfig{Mode: "none"},
	})
	require.NoError(t, err)
	defer func() { _ = exec.Close() }()

	assert.Equal(t, "none", receivedBody.HostConfig.NetworkMode)
	assert.Empty(t, receivedBody.HostConfig.ExtraHosts, "ExtraHosts should be empty for none mode")
	assert.Empty(t, receivedBody.Env, "Env should be empty for none mode")
}

// TestContainerExecutor_AllowlistMode_BadAllowlistFailsFast verifies that a
// malformed allowlist entry surfaces a construction error rather than a
// half-started container.
func TestContainerExecutor_AllowlistMode_BadAllowlistFailsFast(t *testing.T) {
	createCalled := false
	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			createCalled = true
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(containerCreateResponse{ID: "should-not-happen"})
		},
	})
	defer cleanup()

	_, err := NewContainerExecutor(ContainerExecutorConfig{
		Image:      "ubuntu:26.04",
		HostDir:    "/tmp/workspace",
		SocketPath: sock,
		Network: &types.NetworkConfig{
			Mode:      "allowlist",
			Allowlist: []string{"example.com:not-a-port"},
		},
	})
	require.Error(t, err, "malformed allowlist should be rejected")
	require.False(t, createCalled, "container should not be created when the allowlist is invalid")
}

// TestContainerExecutor_CreateRequest_CarriesHardeningProfile asserts that the
// create-container request carries the full sandbox-hardening profile: a read-only rootfs, a
// sized /tmp tmpfs and /dev/shm, the nobody uid:gid, nosuid/nodev/noexec on
// the tmpfs scratch mounts, and the retained PidsLimit. The mock engine only
// captures the request, so this proves what stirrup puts on the wire — not
// that a live daemon honours every flag.
func TestContainerExecutor_CreateRequest_CarriesHardeningProfile(t *testing.T) {
	var receivedBody containerCreateRequest
	var rawBody []byte

	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			rawBody, _ = io.ReadAll(r.Body)
			_ = json.Unmarshal(rawBody, &receivedBody)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(containerCreateResponse{ID: "test-id"})
		},
		"POST /containers/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"POST /containers/*/stop": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"DELETE /containers/*": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
	})
	defer cleanup()

	exec, err := NewContainerExecutorWithContext(context.Background(), ContainerExecutorConfig{
		Image:      "ubuntu:26.04",
		HostDir:    "/tmp/workspace",
		SocketPath: sock,
		Resources:  &types.ResourceLimits{PIDs: 256},
	})
	require.NoError(t, err, "NewContainerExecutorWithContext")
	defer func() { _ = exec.Close() }()

	hc := receivedBody.HostConfig
	assert.True(t, hc.ReadonlyRootfs, "ReadonlyRootfs")
	// ReadonlyRootfs must appear literally on the wire: a bool with omitempty
	// would drop false silently, so the field is emitted unconditionally.
	assert.Contains(t, string(rawBody), `"ReadonlyRootfs":true`, "create body should carry the literal field")
	assert.NotContains(t, string(rawBody), `"ShmSize"`, "create body should not carry a separate ShmSize field")
	assert.Equal(t, "65534:65534", receivedBody.User, "User")
	require.NotNil(t, hc.PidsLimit, "PidsLimit")
	assert.Equal(t, int64(256), *hc.PidsLimit, "PidsLimit")

	require.Contains(t, hc.Tmpfs, "/tmp", "Tmpfs should mount /tmp")
	assert.Contains(t, hc.Tmpfs["/tmp"], "nosuid")
	assert.Contains(t, hc.Tmpfs["/tmp"], "nodev")
	assert.Contains(t, hc.Tmpfs["/tmp"], "noexec")
	assert.Contains(t, hc.Tmpfs["/tmp"], "size=268435456")

	require.Contains(t, hc.Tmpfs, "/dev/shm", "Tmpfs should mount /dev/shm")
	assert.Contains(t, hc.Tmpfs["/dev/shm"], "nosuid")
	assert.Contains(t, hc.Tmpfs["/dev/shm"], "nodev")
	assert.Contains(t, hc.Tmpfs["/dev/shm"], "noexec")
	assert.Contains(t, hc.Tmpfs["/dev/shm"], "size=67108864")

	// The hardening profile must not displace the existing baseline.
	assert.Equal(t, "none", hc.NetworkMode, "NetworkMode")
	assert.Equal(t, []string{"ALL"}, hc.CapDrop, "CapDrop")
	assert.Equal(t, []string{"no-new-privileges"}, hc.SecurityOpt, "SecurityOpt")
}

// TestContainerExecutor_RegistryAllowlist_Default verifies the built-in
// allowlist: a Docker Hub official image (library/*) and a ghcr.io/rxbynerd
// image construct, while a third-party reference is rejected before any
// container is created.
func TestContainerExecutor_RegistryAllowlist_Default(t *testing.T) {
	createCalled := false
	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			createCalled = true
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(containerCreateResponse{ID: "test-id"})
		},
		"POST /containers/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"POST /containers/*/stop": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"DELETE /containers/*": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
	})
	defer cleanup()

	allowed := []struct {
		name, image string
	}{
		{"ubuntu tag", "ubuntu:26.04"},
		{"explicit docker hub library path", "docker.io/library/alpine"},
		{"ghcr base image", "ghcr.io/rxbynerd/base:latest"},
		// The documented quick-start sandbox image (docs/executors/k8s.md,
		// README.md) must construct under the default allowlist.
		{"quick-start sandbox image", "ghcr.io/rxbynerd/stirrup-sandbox:latest"},
		// The Docker Hub pull aliases must fold to docker.io and match the
		// default docker.io/library/* pattern, or operators hit a false deny.
		{"index.docker.io alias", "index.docker.io/library/ubuntu"},
		{"registry-1.docker.io alias", "registry-1.docker.io/library/ubuntu:26.04"},
	}
	for _, tt := range allowed {
		t.Run("allows "+tt.name, func(t *testing.T) {
			exec, err := NewContainerExecutorWithContext(context.Background(), ContainerExecutorConfig{
				Image:      tt.image,
				HostDir:    "/tmp/workspace",
				SocketPath: sock,
			})
			require.NoError(t, err, "image should be allowed by default")
			_ = exec.Close()
		})
	}

	createCalled = false
	denied := []struct {
		name, image string
	}{
		{"untrusted registry", "evil.example.com/malware:latest"},
		{"unknown docker hub namespace", "docker.io/someuser/app"},
		{"unknown ghcr namespace", "ghcr.io/attacker/img"},
		// ghcr.io/stirrup is not a namespace the project publishes under;
		// confirms the default was not left admitting a dead org.
		{"unpublished ghcr org", "ghcr.io/stirrup/base"},
	}
	for _, tt := range denied {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			_, err := NewContainerExecutorWithContext(context.Background(), ContainerExecutorConfig{
				Image:      tt.image,
				HostDir:    "/tmp/workspace",
				SocketPath: sock,
			})
			require.Error(t, err, "image should be rejected by the default allowlist")
			require.False(t, createCalled, "container must not be created when the image is disallowed")
		})
	}
}

// TestContainerExecutor_RegistryAllowlist_Override verifies an operator can
// widen the allowlist, and that the override fully replaces (does not extend)
// the default set.
func TestContainerExecutor_RegistryAllowlist_Override(t *testing.T) {
	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(containerCreateResponse{ID: "test-id"})
		},
		"POST /containers/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"POST /containers/*/stop": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
		"DELETE /containers/*": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
	})
	defer cleanup()

	// A custom registry the operator trusts.
	exec, err := NewContainerExecutorWithContext(context.Background(), ContainerExecutorConfig{
		Image:             "registry.internal:5000/team/runner@sha256:" + strings.Repeat("a", 64),
		HostDir:           "/tmp/workspace",
		SocketPath:        sock,
		RegistryAllowlist: []string{"registry.internal:5000/team/*"},
	})
	require.NoError(t, err, "custom-registry image should be allowed by override")
	_ = exec.Close()

	// The override replaces the default, so a previously-default image is now
	// rejected.
	_, err = NewContainerExecutorWithContext(context.Background(), ContainerExecutorConfig{
		Image:             "ubuntu:26.04",
		HostDir:           "/tmp/workspace",
		SocketPath:        sock,
		RegistryAllowlist: []string{"registry.internal:5000/team/*"},
	})
	require.Error(t, err, "ubuntu:26.04 should be rejected when the override does not include docker.io/library/*")
}

func TestNormaliseImageRef_ReturnsCanonicalForm(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"bare name defaults to docker hub library", "ubuntu", "docker.io/library/ubuntu"},
		{"tag is dropped", "ubuntu:26.04", "docker.io/library/ubuntu"},
		{"namespaced name defaults to docker hub", "myorg/app:tag", "docker.io/myorg/app"},
		{"explicit registry is kept", "ghcr.io/stirrup/base", "ghcr.io/stirrup/base"},
		{"explicit registry drops tag", "ghcr.io/stirrup/base:latest", "ghcr.io/stirrup/base"},
		{"digest is dropped", "registry:5000/team/img@sha256:" + strings.Repeat("b", 64), "registry:5000/team/img"},
		{"localhost without port is kept", "localhost/dev", "localhost/dev"},
		{"localhost with port drops tag", "localhost:5000/dev:tag", "localhost:5000/dev"},
		{"docker hub library path is unchanged", "docker.io/library/alpine", "docker.io/library/alpine"},
		{"index.docker.io folds to docker.io", "index.docker.io/library/ubuntu", "docker.io/library/ubuntu"},
		{"registry-1.docker.io folds to docker.io", "registry-1.docker.io/library/ubuntu:26.04", "docker.io/library/ubuntu"},
		{"index.docker.io namespace folds to docker.io", "index.docker.io/myorg/app", "docker.io/myorg/app"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, normaliseImageRef(tt.in))
		})
	}
}

// TestCheckImageAllowed_RejectsEmptyRepo verifies a reference whose
// normalised form has an empty final segment (trailing "/") is rejected with
// a clear error rather than slipping through because the "*" glob matches the
// empty string.
func TestCheckImageAllowed_RejectsEmptyRepo(t *testing.T) {
	tests := []struct {
		name, image string
	}{
		{"ghcr namespace with no repository", "ghcr.io/stirrup/"},
		{"docker hub library with no repository", "docker.io/library/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkImageAllowed(tt.image, nil)
			require.Error(t, err, "image with empty repository should be rejected")
			require.ErrorContains(t, err, "empty repository")
		})
	}
}

func TestNewContainerExecutor_MissingImage(t *testing.T) {
	_, err := NewContainerExecutor(ContainerExecutorConfig{
		HostDir: "/tmp/workspace",
	})
	require.Error(t, err, "missing image should be rejected")
}

func TestNewContainerExecutor_MissingHostDir(t *testing.T) {
	_, err := NewContainerExecutor(ContainerExecutorConfig{
		Image: "ubuntu:26.04",
	})
	require.Error(t, err, "missing host dir should be rejected")
}

func TestDetectSocket_HonoursDockerHost(t *testing.T) {
	// Create a temp file to simulate a socket.
	dir := t.TempDir()
	fakeSock := filepath.Join(dir, "docker.sock")
	_ = os.WriteFile(fakeSock, nil, 0o600)

	// Set DOCKER_HOST to point at it.
	t.Setenv("DOCKER_HOST", "unix://"+fakeSock)

	path, err := detectSocket()
	require.NoError(t, err, "detectSocket")
	require.Equal(t, fakeSock, path)
}

func TestDetectSocket_DockerHostNotFound(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")

	_, err := detectSocket()
	require.Error(t, err, "nonexistent DOCKER_HOST socket should be an error")
}

func TestDemuxDockerStream_SplitsStdoutAndStderr(t *testing.T) {
	var buf bytes.Buffer
	writeDockerFrame(&buf, 1, []byte("out1"))
	writeDockerFrame(&buf, 2, []byte("err1"))
	writeDockerFrame(&buf, 1, []byte("out2"))

	stdout, stderr, err := demuxDockerStream(&buf)
	require.NoError(t, err, "demuxDockerStream")
	require.Equal(t, "out1out2", stdout, "stdout")
	require.Equal(t, "err1", stderr, "stderr")
}

func TestDemuxDockerStream_Empty(t *testing.T) {
	var buf bytes.Buffer
	stdout, stderr, err := demuxDockerStream(&buf)
	require.NoError(t, err, "demuxDockerStream")
	require.Empty(t, stdout, "stdout")
	require.Empty(t, stderr, "stderr")
}

func TestContainerAPIClient_ArchivePathEncoding(t *testing.T) {
	var putPath string

	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"PUT /containers/*/archive": func(w http.ResponseWriter, r *http.Request) {
			putPath = r.URL.Query().Get("path")
			w.WriteHeader(http.StatusOK)
		},
	})
	defer cleanup()

	client := newContainerAPIClient(sock)
	ctx := context.Background()

	// Path with spaces, hash, and other characters that need URL encoding.
	specialPath := "/workspace/my dir/file #1 (copy).txt"

	// putArchive: verify the path query parameter is decoded correctly by the server.
	err := client.putArchive(ctx, "ctr1", specialPath, strings.NewReader(""))
	require.NoError(t, err, "putArchive")
	require.Equal(t, specialPath, putPath, "putArchive path")
}

// TestContainerAPIClient_InspectExec_WaitsForExit pins that an exit code
// read while the daemon still reports the instance running is not trusted:
// inspect is retried until Running clears, and gives up with an error
// rather than a placeholder code.
func TestContainerAPIClient_InspectExec_WaitsForExit(t *testing.T) {
	prevRetries, prevDelay := execInspectRetries, execInspectRetryDelay
	execInspectRetries, execInspectRetryDelay = 3, time.Millisecond
	defer func() { execInspectRetries, execInspectRetryDelay = prevRetries, prevDelay }()

	var inspects int
	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"GET /exec/*/json": func(w http.ResponseWriter, r *http.Request) {
			inspects++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execInspectResponse{ExitCode: 0, Running: inspects < 3})
		},
	})
	defer cleanup()
	client := newContainerAPIClient(sock)

	code, err := client.inspectExec(context.Background(), "exec-settling")
	require.NoError(t, err, "inspectExec should return once Running clears")
	require.Equal(t, 0, code, "exit code")
	require.Equal(t, 3, inspects, "inspected count: two Running responses, then one finished")

	inspects = 0
	sockStuck, cleanupStuck := mockEngineServer(t, map[string]http.HandlerFunc{
		"GET /exec/*/json": func(w http.ResponseWriter, r *http.Request) {
			inspects++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execInspectResponse{ExitCode: 0, Running: true})
		},
	})
	defer cleanupStuck()
	stuck := newContainerAPIClient(sockStuck)

	_, err = stuck.inspectExec(context.Background(), "exec-stuck")
	require.Error(t, err, "an instance that never stops running must not yield a trusted exit code")
}

// --- Helpers ---

type mockSecurityEmitter struct {
	pathTraversalCount int
	fileSizeLimitCount int
	outputTruncCount   int
}

func (m *mockSecurityEmitter) PathTraversalBlocked(_, _ string) {
	m.pathTraversalCount++
}

func (m *mockSecurityEmitter) FileSizeLimitExceeded(_ string, _, _ int64) {
	m.fileSizeLimitCount++
}

func (m *mockSecurityEmitter) OutputTruncated(_ string, _, _ int) {
	m.outputTruncCount++
}

var _ SecurityEventEmitter = (*mockSecurityEmitter)(nil)

// TestContainerExecutor_ExecStream_DemuxesUncappedOutput verifies the
// streaming path demuxes stdout and stderr frames into the caller's writers
// without the inline 1 MiB cap, and carries the inspected exit code.
func TestContainerExecutor_ExecStream_DemuxesUncappedOutput(t *testing.T) {
	big := strings.Repeat("B", maxOutputSize+1000)
	exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
		"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execCreateResponse{ID: "exec-stream"})
		},
		"POST /exec/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			writeDockerFrame(w, 1, []byte(big))
			writeDockerFrame(w, 2, []byte("warning\n"))
		},
		"GET /exec/*/json": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execInspectResponse{ExitCode: 3})
		},
	})
	defer cleanup()

	var stdout, stderr bytes.Buffer
	result, err := exec.ExecStream(context.Background(), "make noise", 10*time.Second, &stdout, &stderr)
	require.NoError(t, err, "ExecStream")
	require.Equal(t, 3, result.ExitCode, "exit code")
	require.Equal(t, len(big), stdout.Len(), "stream path must not cap stdout")
	require.Equal(t, "warning\n", stderr.String(), "stderr")
}

// failAfterWriter accepts n bytes then fails every subsequent write,
// mimicking a command-output spool writer that hit its capture limit.
type failAfterWriter struct {
	remaining int
	err       error
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if len(p) <= w.remaining {
		w.remaining -= len(p)
		return len(p), nil
	}
	return 0, w.err
}

// TestContainerExecutor_ExecStream_WriterErrorAborts verifies a caller
// writer failure (e.g. the spool's capture limit) surfaces as an error
// instead of being swallowed.
func TestContainerExecutor_ExecStream_WriterErrorAborts(t *testing.T) {
	exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
		"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execCreateResponse{ID: "exec-abort"})
		},
		"POST /exec/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			writeDockerFrame(w, 1, []byte(strings.Repeat("C", 4096)))
			writeDockerFrame(w, 1, []byte(strings.Repeat("C", 4096)))
		},
		"GET /exec/*/json": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execInspectResponse{ExitCode: 0})
		},
	})
	defer cleanup()

	limitErr := errors.New("capture limit exceeded")
	var stderr bytes.Buffer
	_, err := exec.ExecStream(context.Background(), "make noise", 10*time.Second, &failAfterWriter{remaining: 4096, err: limitErr}, &stderr)
	require.ErrorContains(t, err, "capture limit exceeded", "writer failure should surface")
}

// recordedExecs collects the Cmd of every exec the mock engine is asked to
// create, in order, so tests can see the follow-up kill exec.
type recordedExecs struct {
	mu   sync.Mutex
	cmds [][]string
}

func (r *recordedExecs) add(cmd []string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, cmd)
	return len(r.cmds)
}

func (r *recordedExecs) all() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.cmds...)
}

// killTrackingHandlers serves a first exec that hangs (the command) and
// answers any later exec (the kill) immediately with exit 0.
func killTrackingHandlers(execs *recordedExecs) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
			var req execCreateRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			n := execs.add(req.Cmd)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execCreateResponse{ID: fmt.Sprintf("exec-%d", n)})
		},
		"POST /exec/*/start": func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/start"), "exec-1") {
				hangingExecStartHandler("partial\n")(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		},
		"GET /exec/*/json": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execInspectResponse{ExitCode: 0})
		},
	}
}

func assertKillExecFollowsRun(t *testing.T, cmds [][]string) {
	t.Helper()
	require.Len(t, cmds, 2, "want the command plus one kill exec: %v", cmds)
	run, kill := cmds[0], cmds[1]
	require.Len(t, run, 7, "run exec should be the group wrapper: %v", run)
	require.Equal(t, "sleep 600", run[6], "run exec should wrap the command")
	require.Len(t, kill, 5, "kill exec should be the group kill script: %v", kill)
	require.Equal(t, []string{"sh", "-c", groupKillScript}, kill[:3], "kill exec should run the group kill script")
	require.Equal(t, run[5], kill[4], "kill pid file must match the run pid file")
	require.True(t, strings.HasPrefix(kill[4], groupPidFileDir+"/.stirrup-exec-"),
		"kill pid file %q should sit under %s", kill[4], groupPidFileDir)
}

func TestContainerExecutor_Exec_TimeoutKillsProcessGroup(t *testing.T) {
	execs := &recordedExecs{}
	exec, cleanup := newMockContainerExecutor(t, killTrackingHandlers(execs))
	defer cleanup()

	_, err := exec.Exec(context.Background(), "sleep 600", 200*time.Millisecond)
	require.ErrorIs(t, err, ErrTimeout)
	assertKillExecFollowsRun(t, execs.all())
}

func TestContainerExecutor_Exec_CancelKillsProcessGroup(t *testing.T) {
	execs := &recordedExecs{}
	exec, cleanup := newMockContainerExecutor(t, killTrackingHandlers(execs))
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, err := exec.Exec(ctx, "sleep 600", 60*time.Second)
	require.Error(t, err, "cancelled exec should return an error")
	require.NotErrorIs(t, err, ErrTimeout, "cancellation should not be reported as a timeout")
	assertKillExecFollowsRun(t, execs.all())
}

func TestContainerExecutor_ExecStream_TimeoutKillsProcessGroup(t *testing.T) {
	execs := &recordedExecs{}
	exec, cleanup := newMockContainerExecutor(t, killTrackingHandlers(execs))
	defer cleanup()

	var stdout, stderr bytes.Buffer
	_, err := exec.ExecStream(context.Background(), "sleep 600", 200*time.Millisecond, &stdout, &stderr)
	require.ErrorIs(t, err, ErrTimeout)
	assertKillExecFollowsRun(t, execs.all())
}

func TestContainerExecutor_Exec_CompletedCommandIssuesNoKill(t *testing.T) {
	execs := &recordedExecs{}
	handlers := killTrackingHandlers(execs)
	handlers["POST /exec/*/start"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		writeDockerFrame(w, 1, []byte("done\n"))
	}
	exec, cleanup := newMockContainerExecutor(t, handlers)
	defer cleanup()

	result, err := exec.Exec(context.Background(), "echo done", 10*time.Second)
	require.NoError(t, err, "Exec")
	require.Zero(t, result.ExitCode, "exit code")
	require.Equal(t, "done\n", result.Stdout, "stdout")
	require.Len(t, execs.all(), 1, "only the command itself should be executed")
}
