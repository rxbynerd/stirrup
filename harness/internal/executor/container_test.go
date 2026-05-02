package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	sock := filepath.Join(dir, "s.sock")

	listener, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("listen on unix socket: %v", err)
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

func TestContainerAPIClient_CreateContainer(t *testing.T) {
	var receivedBody containerCreateRequest

	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			if ct := r.Header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("expected Content-Type application/json, got %q", ct)
			}
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
	if err != nil {
		t.Fatalf("createContainer: %v", err)
	}
	if id != "abc123" {
		t.Errorf("got id %q, want %q", id, "abc123")
	}

	// Verify the body was sent correctly.
	if receivedBody.Image != "ubuntu:26.04" {
		t.Errorf("image: got %q, want %q", receivedBody.Image, "ubuntu:26.04")
	}
	if receivedBody.HostConfig.NetworkMode != "none" {
		t.Errorf("network mode: got %q, want %q", receivedBody.HostConfig.NetworkMode, "none")
	}
	if receivedBody.HostConfig.NanoCPUs != 2000000000 {
		t.Errorf("NanoCPUs: got %d, want %d", receivedBody.HostConfig.NanoCPUs, 2000000000)
	}
	if len(receivedBody.HostConfig.CapDrop) != 1 || receivedBody.HostConfig.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop: got %v, want [ALL]", receivedBody.HostConfig.CapDrop)
	}
}

func TestContainerAPIClient_APIError(t *testing.T) {
	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(apiError{Message: "image not found"})
		},
	})
	defer cleanup()

	client := newContainerAPIClient(sock)
	_, err := client.createContainer(context.Background(), containerCreateRequest{Image: "bad:image"})
	if err == nil {
		t.Fatal("expected error for API error response")
	}
	if !strings.Contains(err.Error(), "image not found") {
		t.Errorf("error should contain API message, got: %v", err)
	}
}

func TestContainerAPIClient_ExecFlow(t *testing.T) {
	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
			var req execCreateRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if !req.AttachStdout || !req.AttachStderr {
				t.Error("exec should attach stdout and stderr")
			}
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
	if err != nil {
		t.Fatalf("createExec: %v", err)
	}
	if execID != "exec123" {
		t.Errorf("exec id: got %q, want %q", execID, "exec123")
	}

	stream, err := client.startExec(context.Background(), execID)
	if err != nil {
		t.Fatalf("startExec: %v", err)
	}
	defer func() { _ = stream.Close() }()

	stdout, stderr, err := demuxDockerStream(stream)
	if err != nil {
		t.Fatalf("demuxDockerStream: %v", err)
	}
	if stdout != "hello\n" {
		t.Errorf("stdout: got %q, want %q", stdout, "hello\n")
	}
	if stderr != "warning\n" {
		t.Errorf("stderr: got %q, want %q", stderr, "warning\n")
	}

	exitCode, err := client.inspectExec(context.Background(), execID)
	if err != nil {
		t.Fatalf("inspectExec: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("exit code: got %d, want 0", exitCode)
	}
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
		input   string
		want    string
		wantErr bool
	}{
		{"foo.txt", "/workspace/foo.txt", false},
		{"sub/dir/file.go", "/workspace/sub/dir/file.go", false},
		{"/workspace/inside.txt", "/workspace/inside.txt", false},
		{"../etc/passwd", "", true},
		{"foo/../../etc/passwd", "", true},
		{"/etc/passwd", "", true},
		{"/workspacefoo", "", true}, // must not match prefix without separator
	}

	for _, tt := range tests {
		got, err := exec.ResolvePath(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ResolvePath(%q): expected error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("ResolvePath(%q): unexpected error: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ResolvePath(%q): got %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestContainerExecutor_ResolvePath_SecurityEmitter(t *testing.T) {
	emitter := &mockSecurityEmitter{}
	exec := &ContainerExecutor{workspace: containerWorkspace, Security: emitter}

	_, err := exec.ResolvePath("../escape")
	if err == nil {
		t.Fatal("expected error")
	}
	if emitter.pathTraversalCount != 1 {
		t.Errorf("expected PathTraversalBlocked to be called once, got %d", emitter.pathTraversalCount)
	}
}

func TestContainerExecutor_ReadFile(t *testing.T) {
	fileContent := "hello from container"

	exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
		"GET /containers/*/archive": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-tar")
			tw := tar.NewWriter(w)
			_ = tw.WriteHeader(&tar.Header{
				Name: "test.txt",
				Size: int64(len(fileContent)),
				Mode: 0o644,
			})
			_, _ = tw.Write([]byte(fileContent))
			_ = tw.Close()
		},
	})
	defer cleanup()

	got, err := exec.ReadFile(context.Background(), "test.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got != fileContent {
		t.Errorf("got %q, want %q", got, fileContent)
	}
}

func TestContainerExecutor_ReadFile_TooLarge(t *testing.T) {
	exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
		"GET /containers/*/archive": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-tar")
			tw := tar.NewWriter(w)
			_ = tw.WriteHeader(&tar.Header{
				Name: "big.bin",
				Size: maxFileSize + 1,
				Mode: 0o644,
			})
			// Don't need to write the actual data; the header size check catches it.
			_ = tw.Close()
		},
	})
	defer cleanup()

	_, err := exec.ReadFile(context.Background(), "big.bin")
	if err == nil {
		t.Fatal("expected error for oversized file")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestContainerExecutor_ReadFile_Directory(t *testing.T) {
	exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
		"GET /containers/*/archive": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/x-tar")
			tw := tar.NewWriter(w)
			_ = tw.WriteHeader(&tar.Header{
				Name:     "subdir/",
				Typeflag: tar.TypeDir,
				Mode:     0o755,
			})
			_ = tw.Close()
		},
	})
	defer cleanup()

	_, err := exec.ReadFile(context.Background(), "subdir")
	if err == nil {
		t.Fatal("expected error reading directory as file")
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error should mention directory, got: %v", err)
	}
}

func TestContainerExecutor_ReadFile_PathTraversal(t *testing.T) {
	exec, cleanup := newMockContainerExecutor(t, nil)
	defer cleanup()

	_, err := exec.ReadFile(context.Background(), "../etc/passwd")
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
	if !strings.Contains(err.Error(), "escapes workspace") {
		t.Errorf("error should mention workspace escape, got: %v", err)
	}
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
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if uploadedContent != "file content" {
		t.Errorf("uploaded content: got %q, want %q", uploadedContent, "file content")
	}
	if uploadDest != "/workspace/sub/dir" {
		t.Errorf("upload dest: got %q, want %q", uploadDest, "/workspace/sub/dir")
	}
}

func TestContainerExecutor_WriteFile_TooLarge(t *testing.T) {
	exec, cleanup := newMockContainerExecutor(t, nil)
	defer cleanup()

	bigContent := strings.Repeat("x", maxFileSize+1)
	err := exec.WriteFile(context.Background(), "big.txt", bigContent)
	if err == nil {
		t.Fatal("expected error for oversized content")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestContainerExecutor_WriteFile_PathTraversal(t *testing.T) {
	exec, cleanup := newMockContainerExecutor(t, nil)
	defer cleanup()

	err := exec.WriteFile(context.Background(), "../escape.txt", "bad")
	if err == nil {
		t.Fatal("expected error for path traversal write")
	}
}

func TestContainerExecutor_Exec(t *testing.T) {
	exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
		"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
			var req execCreateRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			// Verify it wraps in sh -c
			if len(req.Cmd) != 3 || req.Cmd[0] != "sh" || req.Cmd[1] != "-c" {
				t.Errorf("exec cmd should be sh -c ..., got %v", req.Cmd)
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
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 42 {
		t.Errorf("exit code: got %d, want 42", result.ExitCode)
	}
	if strings.TrimSpace(result.Stdout) != "output" {
		t.Errorf("stdout: got %q, want %q", result.Stdout, "output\n")
	}
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
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.HasSuffix(result.Stdout, truncatedSuffix) {
		t.Error("expected truncation suffix on large output")
	}
}

func TestContainerExecutor_ListDirectory(t *testing.T) {
	exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
		"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execCreateResponse{ID: "exec-ls"})
		},
		"POST /exec/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			writeDockerFrame(w, 1, []byte(".\n..\nfile.txt\nsubdir\n"))
		},
		"GET /exec/*/json": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execInspectResponse{ExitCode: 0})
		},
	})
	defer cleanup()

	entries, err := exec.ListDirectory(context.Background(), ".")
	if err != nil {
		t.Fatalf("ListDirectory: %v", err)
	}

	// Should exclude . and ..
	found := map[string]bool{}
	for _, e := range entries {
		found[e] = true
	}
	if !found["file.txt"] {
		t.Error("missing file.txt")
	}
	if !found["subdir"] {
		t.Error("missing subdir")
	}
	if found["."] || found[".."] {
		t.Error(". and .. should be filtered out")
	}
}

func TestContainerExecutor_Capabilities(t *testing.T) {
	tests := []struct {
		networkMode string
		wantNetwork bool
	}{
		{"none", false},
		{"bridge", true},
	}

	for _, tt := range tests {
		exec := &ContainerExecutor{
			workspace:   containerWorkspace,
			networkMode: tt.networkMode,
		}
		caps := exec.Capabilities()
		if !caps.CanRead || !caps.CanWrite || !caps.CanExec {
			t.Errorf("networkMode=%q: expected CanRead/CanWrite/CanExec to be true", tt.networkMode)
		}
		if caps.CanNetwork != tt.wantNetwork {
			t.Errorf("networkMode=%q: CanNetwork got %v, want %v", tt.networkMode, caps.CanNetwork, tt.wantNetwork)
		}
		if caps.MaxTimeout != maxTimeout {
			t.Errorf("MaxTimeout: got %v, want %v", caps.MaxTimeout, maxTimeout)
		}
	}
}

func TestContainerExecutor_Close(t *testing.T) {
	stopCalled := false
	removeCalled := false

	exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
		"POST /containers/*/stop": func(w http.ResponseWriter, r *http.Request) {
			stopCalled = true
			w.WriteHeader(http.StatusNoContent)
		},
		"DELETE /containers/*": func(w http.ResponseWriter, r *http.Request) {
			removeCalled = true
			if !strings.Contains(r.URL.RawQuery, "force=true") {
				t.Error("remove should be called with force=true")
			}
			w.WriteHeader(http.StatusNoContent)
		},
	})
	defer cleanup()

	if err := exec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !stopCalled {
		t.Error("stop was not called")
	}
	if !removeCalled {
		t.Error("remove was not called")
	}
}

func TestContainerExecutor_CreateFailure(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected error for container creation failure")
	}
	if !strings.Contains(err.Error(), "image not found") {
		t.Errorf("error should mention API message, got: %v", err)
	}
}

func TestContainerExecutor_StartFailureCleanup(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected error for start failure")
	}
	if !removeCalled {
		t.Error("container should be removed on start failure")
	}
}

func TestContainerExecutor_ResourceLimits(t *testing.T) {
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
	if err != nil {
		t.Fatalf("NewContainerExecutor: %v", err)
	}
	defer func() { _ = exec.Close() }()

	if receivedBody.HostConfig.NanoCPUs != 2000000000 {
		t.Errorf("NanoCPUs: got %d, want 2000000000", receivedBody.HostConfig.NanoCPUs)
	}
	if receivedBody.HostConfig.Memory != 512*1024*1024 {
		t.Errorf("Memory: got %d, want %d", receivedBody.HostConfig.Memory, 512*1024*1024)
	}
	if receivedBody.HostConfig.PidsLimit == nil || *receivedBody.HostConfig.PidsLimit != 256 {
		t.Errorf("PidsLimit: got %v, want 256", receivedBody.HostConfig.PidsLimit)
	}
}

func TestContainerExecutor_NetworkModes(t *testing.T) {
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

	// Default: none
	exec, err := NewContainerExecutor(ContainerExecutorConfig{
		Image:      "ubuntu:26.04",
		HostDir:    "/tmp/workspace",
		SocketPath: sock,
	})
	if err != nil {
		t.Fatalf("NewContainerExecutor: %v", err)
	}
	_ = exec.Close()

	if receivedBody.HostConfig.NetworkMode != "none" {
		t.Errorf("default network mode: got %q, want %q", receivedBody.HostConfig.NetworkMode, "none")
	}

	// Allowlist: bridge
	exec, err = NewContainerExecutor(ContainerExecutorConfig{
		Image:      "ubuntu:26.04",
		HostDir:    "/tmp/workspace",
		SocketPath: sock,
		Network:    &types.NetworkConfig{Mode: "allowlist", Allowlist: []string{"example.com"}},
	})
	if err != nil {
		t.Fatalf("NewContainerExecutor: %v", err)
	}
	_ = exec.Close()

	if receivedBody.HostConfig.NetworkMode != "bridge" {
		t.Errorf("allowlist network mode: got %q, want %q", receivedBody.HostConfig.NetworkMode, "bridge")
	}
}

func TestContainerExecutor_Runtime(t *testing.T) {
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
	if err != nil {
		t.Fatalf("NewContainerExecutor: %v", err)
	}
	_ = exec.Close()

	if !strings.Contains(string(rawBody), `"Runtime":"runsc"`) {
		t.Errorf("create body missing Runtime=runsc; got: %s", string(rawBody))
	}

	// Hardening defaults must still be present alongside the runtime.
	var parsed containerCreateRequest
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		t.Fatalf("unmarshal create body: %v", err)
	}
	if parsed.HostConfig.Runtime != "runsc" {
		t.Errorf("HostConfig.Runtime: got %q, want %q", parsed.HostConfig.Runtime, "runsc")
	}
	if parsed.HostConfig.NetworkMode != "none" {
		t.Errorf("HostConfig.NetworkMode: got %q, want %q", parsed.HostConfig.NetworkMode, "none")
	}
	if len(parsed.HostConfig.CapDrop) != 1 || parsed.HostConfig.CapDrop[0] != "ALL" {
		t.Errorf("HostConfig.CapDrop: got %v, want [ALL]", parsed.HostConfig.CapDrop)
	}

	// Empty runtime must be omitted entirely (engine picks its default).
	rawBody = nil
	exec, err = NewContainerExecutor(ContainerExecutorConfig{
		Image:      "ubuntu:26.04",
		HostDir:    "/tmp/workspace",
		SocketPath: sock,
	})
	if err != nil {
		t.Fatalf("NewContainerExecutor: %v", err)
	}
	_ = exec.Close()

	if strings.Contains(string(rawBody), `"Runtime"`) {
		t.Errorf("create body should omit Runtime when unset; got: %s", string(rawBody))
	}
}

// TestContainerExecutor_AllowlistMode_WiringConfig verifies that selecting
// Network.Mode == "allowlist" causes the create-container request to:
//
//   - set NetworkMode to "bridge" (so the container can dial the host),
//   - inject ExtraHosts so host.docker.internal resolves to the host gateway,
//   - populate HTTP_PROXY / HTTPS_PROXY / NO_PROXY env in the container.
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
	if err != nil {
		t.Fatalf("NewContainerExecutor: %v", err)
	}
	defer func() { _ = exec.Close() }()

	if receivedBody.HostConfig.NetworkMode != "bridge" {
		t.Errorf("NetworkMode: got %q, want %q", receivedBody.HostConfig.NetworkMode, "bridge")
	}

	wantHost := "host.docker.internal:host-gateway"
	if len(receivedBody.HostConfig.ExtraHosts) != 1 || receivedBody.HostConfig.ExtraHosts[0] != wantHost {
		t.Errorf("ExtraHosts: got %v, want [%q]", receivedBody.HostConfig.ExtraHosts, wantHost)
	}

	envSeen := map[string]string{}
	for _, kv := range receivedBody.Env {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			envSeen[parts[0]] = parts[1]
		}
	}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"} {
		if _, ok := envSeen[k]; !ok {
			t.Errorf("missing env var %s; got Env=%v", k, receivedBody.Env)
		}
	}
	if !strings.HasPrefix(envSeen["HTTP_PROXY"], "http://host.docker.internal:") {
		t.Errorf("HTTP_PROXY: got %q, want prefix http://host.docker.internal:<port>", envSeen["HTTP_PROXY"])
	}
	if envSeen["HTTP_PROXY"] != envSeen["HTTPS_PROXY"] {
		t.Errorf("HTTP_PROXY and HTTPS_PROXY should match; got %q vs %q", envSeen["HTTP_PROXY"], envSeen["HTTPS_PROXY"])
	}
	if !strings.Contains(envSeen["NO_PROXY"], "127.0.0.1") {
		t.Errorf("NO_PROXY should at least cover 127.0.0.1; got %q", envSeen["NO_PROXY"])
	}

	// Hardening defaults should still be present alongside the egress wiring.
	if len(receivedBody.HostConfig.CapDrop) != 1 || receivedBody.HostConfig.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop: got %v, want [ALL]", receivedBody.HostConfig.CapDrop)
	}
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
	if err != nil {
		t.Fatalf("NewContainerExecutor: %v", err)
	}
	defer func() { _ = exec.Close() }()

	if receivedBody.HostConfig.NetworkMode != "none" {
		t.Errorf("NetworkMode: got %q, want %q", receivedBody.HostConfig.NetworkMode, "none")
	}
	if len(receivedBody.HostConfig.ExtraHosts) != 0 {
		t.Errorf("ExtraHosts should be empty for none mode, got %v", receivedBody.HostConfig.ExtraHosts)
	}
	if len(receivedBody.Env) != 0 {
		t.Errorf("Env should be empty for none mode, got %v", receivedBody.Env)
	}
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
	if err == nil {
		t.Fatal("expected error for malformed allowlist")
	}
	if createCalled {
		t.Error("container should not be created when the allowlist is invalid")
	}
}

func TestNewContainerExecutor_MissingImage(t *testing.T) {
	_, err := NewContainerExecutor(ContainerExecutorConfig{
		HostDir: "/tmp/workspace",
	})
	if err == nil {
		t.Fatal("expected error for missing image")
	}
}

func TestNewContainerExecutor_MissingHostDir(t *testing.T) {
	_, err := NewContainerExecutor(ContainerExecutorConfig{
		Image: "ubuntu:26.04",
	})
	if err == nil {
		t.Fatal("expected error for missing host dir")
	}
}

func TestDetectSocket(t *testing.T) {
	// Create a temp file to simulate a socket.
	dir := t.TempDir()
	fakeSock := filepath.Join(dir, "docker.sock")
	_ = os.WriteFile(fakeSock, nil, 0o600)

	// Set DOCKER_HOST to point at it.
	t.Setenv("DOCKER_HOST", "unix://"+fakeSock)

	path, err := detectSocket()
	if err != nil {
		t.Fatalf("detectSocket: %v", err)
	}
	if path != fakeSock {
		t.Errorf("got %q, want %q", path, fakeSock)
	}
}

func TestDetectSocket_DockerHostNotFound(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")

	_, err := detectSocket()
	if err == nil {
		t.Fatal("expected error for nonexistent DOCKER_HOST socket")
	}
}

func TestDemuxDockerStream(t *testing.T) {
	var buf bytes.Buffer
	writeDockerFrame(&buf, 1, []byte("out1"))
	writeDockerFrame(&buf, 2, []byte("err1"))
	writeDockerFrame(&buf, 1, []byte("out2"))

	stdout, stderr, err := demuxDockerStream(&buf)
	if err != nil {
		t.Fatalf("demuxDockerStream: %v", err)
	}
	if stdout != "out1out2" {
		t.Errorf("stdout: got %q, want %q", stdout, "out1out2")
	}
	if stderr != "err1" {
		t.Errorf("stderr: got %q, want %q", stderr, "err1")
	}
}

func TestDemuxDockerStream_Empty(t *testing.T) {
	var buf bytes.Buffer
	stdout, stderr, err := demuxDockerStream(&buf)
	if err != nil {
		t.Fatalf("demuxDockerStream: %v", err)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("expected empty output, got stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestContainerAPIClient_ArchivePathEncoding(t *testing.T) {
	var putPath, getPath string

	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"PUT /containers/*/archive": func(w http.ResponseWriter, r *http.Request) {
			putPath = r.URL.Query().Get("path")
			w.WriteHeader(http.StatusOK)
		},
		"GET /containers/*/archive": func(w http.ResponseWriter, r *http.Request) {
			getPath = r.URL.Query().Get("path")
			// Return a minimal valid tar so getArchive succeeds.
			w.Header().Set("Content-Type", "application/x-tar")
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
	if err != nil {
		t.Fatalf("putArchive: %v", err)
	}
	if putPath != specialPath {
		t.Errorf("putArchive path: got %q, want %q", putPath, specialPath)
	}

	// getArchive: same check.
	body, err := client.getArchive(ctx, "ctr1", specialPath)
	if err != nil {
		t.Fatalf("getArchive: %v", err)
	}
	_ = body.Close()
	if getPath != specialPath {
		t.Errorf("getArchive path: got %q, want %q", getPath, specialPath)
	}
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

// Ensure that mockSecurityEmitter satisfies the interface.
var _ SecurityEventEmitter = (*mockSecurityEmitter)(nil)
