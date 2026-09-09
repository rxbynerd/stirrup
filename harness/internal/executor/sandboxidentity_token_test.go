package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	corev1 "k8s.io/api/core/v1"
)

// stdinExecCapture records what a hijacked exec start received on stdin
// and which exec-create bodies the mock engine saw.
type stdinExecCapture struct {
	mu      sync.Mutex
	creates []execCreateRequest
	stdin   []string
}

func (c *stdinExecCapture) lastCreate() execCreateRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.creates[len(c.creates)-1]
}

func (c *stdinExecCapture) stdinSeen() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.stdin...)
}

// stdinExecHandlers returns mock Engine API handlers for an exec whose
// start is hijacked: the handler switches protocols, drains stdin until
// the client half-closes, then writes a stderr frame and reports exitCode.
func stdinExecHandlers(t *testing.T, capture *stdinExecCapture, exitCode int, stderr string) map[string]http.HandlerFunc {
	t.Helper()
	return map[string]http.HandlerFunc{
		"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
			var req execCreateRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			capture.mu.Lock()
			capture.creates = append(capture.creates, req)
			capture.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execCreateResponse{ID: "exec-stdin"})
		},
		"POST /exec/*/start": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			if r.Header.Get("Upgrade") != "tcp" {
				t.Errorf("exec start without Upgrade: tcp; stdin cannot be delivered")
				http.Error(w, "expected upgrade", http.StatusBadRequest)
				return
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("mock server does not support hijacking")
			}
			conn, rw, err := hj.Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			defer func() { _ = conn.Close() }()
			_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			_ = rw.Flush()
			in, _ := io.ReadAll(rw)
			capture.mu.Lock()
			capture.stdin = append(capture.stdin, string(in))
			capture.mu.Unlock()
			if stderr != "" {
				writeDockerFrame(rw, 2, []byte(stderr))
			}
			_ = rw.Flush()
		},
		"GET /exec/*/json": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execInspectResponse{ExitCode: exitCode})
		},
	}
}

// TestContainerExecutor_SandboxIdentityToken_MountsPrivateTmpfs asserts
// the flag adds a dedicated tmpfs at SandboxIdentityTokenDir with the same
// nosuid/nodev/noexec hardening as /tmp, and that nothing is mounted
// without the flag.
func TestContainerExecutor_SandboxIdentityToken_MountsPrivateTmpfs(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%v", enabled), func(t *testing.T) {
			var received containerCreateRequest
			sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
				"POST /containers/create": func(w http.ResponseWriter, r *http.Request) {
					_ = json.NewDecoder(r.Body).Decode(&received)
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(containerCreateResponse{ID: "test-id"})
				},
				"POST /containers/*/start": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
				"POST /containers/*/stop":  func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
				"DELETE /containers/*":     func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
			})
			defer cleanup()

			exec, err := NewContainerExecutorWithContext(context.Background(), ContainerExecutorConfig{
				Image:                "ubuntu:26.04",
				HostDir:              "/tmp/workspace",
				SocketPath:           sock,
				SandboxIdentityToken: enabled,
			})
			if err != nil {
				t.Fatalf("NewContainerExecutorWithContext: %v", err)
			}
			defer func() { _ = exec.Close() }()

			opts, mounted := received.HostConfig.Tmpfs[SandboxIdentityTokenDir]
			if mounted != enabled {
				t.Fatalf("tmpfs at %s mounted=%v, want %v (Tmpfs=%v)", SandboxIdentityTokenDir, mounted, enabled, received.HostConfig.Tmpfs)
			}
			if !enabled {
				return
			}
			for _, want := range []string{"nosuid", "nodev", "noexec", "mode=1777", fmt.Sprintf("size=%d", sandboxIdentityTokenMountBytes)} {
				if !strings.Contains(opts, want) {
					t.Errorf("token tmpfs opts %q missing %q", opts, want)
				}
			}
			if strings.HasPrefix(SandboxIdentityTokenDir, containerWorkspace+"/") {
				t.Errorf("token dir %s lies inside the workspace %s", SandboxIdentityTokenDir, containerWorkspace)
			}
		})
	}
}

// TestContainerExecutor_WriteSandboxIdentityToken_StdinNotArgv drives the
// hijacked exec path against the mock engine: the token must arrive on
// stdin, the exec-create body must attach stdin and carry only the
// path-bearing write command in argv, and the executor must report success
// from the inspected exit code.
func TestContainerExecutor_WriteSandboxIdentityToken_StdinNotArgv(t *testing.T) {
	const token = "eyJhbGciOiJFUzI1NiJ9.refresh-payload.signature"
	capture := &stdinExecCapture{}
	exec, cleanup := newMockContainerExecutor(t, stdinExecHandlers(t, capture, 0, ""))
	defer cleanup()
	exec.sandboxIdentity = true

	if err := exec.WriteSandboxIdentityToken(context.Background(), token); err != nil {
		t.Fatalf("WriteSandboxIdentityToken: %v", err)
	}

	if got := capture.stdinSeen(); len(got) != 1 || got[0] != token {
		t.Errorf("stdin seen by the engine = %q, want exactly one delivery of the token", got)
	}
	create := capture.lastCreate()
	if !create.AttachStdin {
		t.Error("exec create must attach stdin")
	}
	want := sandboxIdentityWriteCommand(len(token))
	if strings.Join(create.Cmd, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("exec Cmd = %q, want the token write command %q", create.Cmd, want)
	}
	for _, arg := range create.Cmd {
		if strings.Contains(arg, token) {
			t.Fatalf("token appeared in exec argv: %q", arg)
		}
	}
	script := create.Cmd[2]
	for _, need := range []string{"umask 077", `wc -c < "$2"`, `-eq "$3"`, `mv -f -- "$2" "$1"`, `rm -f -- "$2"`, "exit 4"} {
		if !strings.Contains(script, need) {
			t.Errorf("write script missing %q:\n%s", need, script)
		}
	}
	if create.Cmd[4] != SandboxIdentityTokenPath || create.Cmd[5] != sandboxIdentityTokenStaging || create.Cmd[6] != strconv.Itoa(len(token)) {
		t.Errorf("write argv tail = %q, want the final path, the staging path, and the byte length %d", create.Cmd[3:], len(token))
	}
}

// TestContainerExecutor_WriteSandboxIdentityToken_ShortStreamSurfaces
// asserts that a stdin copy which ends before the announced length is
// reported even when the in-container command reports success, so a
// truncated delivery can never pass as a complete one.
func TestContainerExecutor_WriteSandboxIdentityToken_ShortStreamSurfaces(t *testing.T) {
	capture := &stdinExecCapture{}
	exec, cleanup := newMockContainerExecutor(t, stdinExecHandlers(t, capture, 0, ""))
	defer cleanup()
	exec.sandboxIdentity = true

	aborted := io.MultiReader(strings.NewReader("eyJhbGciOiJFUzI1NiJ9.half"), iotest.ErrReader(errors.New("client aborted")))
	_, err := exec.execInContainerInput(context.Background(), sandboxIdentityWriteCommand(64), exec.workspace, aborted, containerFileIOTimeout)
	if err == nil {
		t.Fatal("expected the aborted stdin copy to surface as an error")
	}
	if !strings.Contains(err.Error(), "client aborted") {
		t.Errorf("error %q should carry the stdin reader's failure", err)
	}
}

// TestContainerExecutor_WriteSandboxIdentityToken_ReportsFailure asserts a
// non-zero exit surfaces the command's stderr without ever echoing the
// token.
func TestContainerExecutor_WriteSandboxIdentityToken_ReportsFailure(t *testing.T) {
	const token = "must-not-appear"
	capture := &stdinExecCapture{}
	exec, cleanup := newMockContainerExecutor(t, stdinExecHandlers(t, capture, 1, "sh: can't create /run/stirrup/sandbox-identity/.token.tmp: Permission denied\n"))
	defer cleanup()
	exec.sandboxIdentity = true

	err := exec.WriteSandboxIdentityToken(context.Background(), token)
	if err == nil {
		t.Fatal("expected an error for a non-zero exit")
	}
	if !strings.Contains(err.Error(), "Permission denied") || !strings.Contains(err.Error(), "exit 1") {
		t.Errorf("error %q should carry the exit code and stderr", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error %q must not contain the token", err)
	}
}

// TestContainerExecutor_WriteSandboxIdentityToken_RequiresMount asserts a
// container created without the token tmpfs refuses the write before
// touching the engine, rather than running the command against a
// read-only rootfs.
func TestContainerExecutor_WriteSandboxIdentityToken_RequiresMount(t *testing.T) {
	capture := &stdinExecCapture{}
	exec, cleanup := newMockContainerExecutor(t, stdinExecHandlers(t, capture, 0, ""))
	defer cleanup()

	err := exec.WriteSandboxIdentityToken(context.Background(), "tok")
	if err == nil {
		t.Fatal("expected an error without the sandbox identity mount")
	}
	if got := capture.stdinSeen(); len(got) != 0 {
		t.Errorf("engine received %d exec(s), want none", len(got))
	}
}

// TestContainerAPIClient_StartExecWithStdin_RequiresUpgrade asserts a
// daemon that answers an exec start with a plain 200 instead of switching
// protocols is treated as unable to carry stdin, and the call fails rather
// than running the command without its input.
func TestContainerAPIClient_StartExecWithStdin_RequiresUpgrade(t *testing.T) {
	sock, cleanup := mockEngineServer(t, map[string]http.HandlerFunc{
		"POST /exec/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			w.WriteHeader(http.StatusOK)
		},
	})
	defer cleanup()

	client := newContainerAPIClient(sock)
	stream, err := client.startExecWithStdin(context.Background(), "exec1", strings.NewReader("tok"))
	if err == nil {
		_ = stream.Close()
		t.Fatal("expected an error when the daemon does not upgrade the connection")
	}
	if !strings.Contains(err.Error(), "HTTP 101") {
		t.Errorf("error %q should explain the missing upgrade", err)
	}
}

// TestBuildSandboxPodSpec_SandboxIdentityVolume asserts the flag adds a
// memory-backed emptyDir mounted at SandboxIdentityTokenDir outside the
// workspace, and that the Pod spec is unchanged without it.
func TestBuildSandboxPodSpec_SandboxIdentityVolume(t *testing.T) {
	cfg := baseSandboxCfg()
	without := buildSandboxPodSpec(cfg, nil, nil)
	if len(without.Volumes) != 1 || len(without.Containers[0].VolumeMounts) != 1 {
		t.Fatalf("without the flag: %d volumes / %d mounts, want 1 / 1", len(without.Volumes), len(without.Containers[0].VolumeMounts))
	}

	cfg.SandboxIdentityToken = true
	spec := buildSandboxPodSpec(cfg, nil, nil)

	var vol *corev1.Volume
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == k8sSandboxIdentityVolume {
			vol = &spec.Volumes[i]
		}
	}
	if vol == nil {
		t.Fatalf("volume %q missing from %+v", k8sSandboxIdentityVolume, spec.Volumes)
	}
	if vol.EmptyDir == nil || vol.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Errorf("volume %q must be a memory-backed emptyDir, got %+v", k8sSandboxIdentityVolume, vol.VolumeSource)
	}
	if vol.EmptyDir == nil || vol.EmptyDir.SizeLimit == nil || vol.EmptyDir.SizeLimit.String() != k8sSandboxIdentityVolumeSize {
		t.Errorf("volume %q size limit = %v, want %s", k8sSandboxIdentityVolume, vol.EmptyDir, k8sSandboxIdentityVolumeSize)
	}

	var mount *corev1.VolumeMount
	for i := range spec.Containers[0].VolumeMounts {
		if spec.Containers[0].VolumeMounts[i].Name == k8sSandboxIdentityVolume {
			mount = &spec.Containers[0].VolumeMounts[i]
		}
	}
	if mount == nil {
		t.Fatalf("mount for %q missing from %+v", k8sSandboxIdentityVolume, spec.Containers[0].VolumeMounts)
	}
	if mount.MountPath != SandboxIdentityTokenDir {
		t.Errorf("mount path = %q, want %q", mount.MountPath, SandboxIdentityTokenDir)
	}
	if strings.HasPrefix(mount.MountPath, k8sWorkspace+"/") {
		t.Errorf("token dir %s lies inside the workspace %s", mount.MountPath, k8sWorkspace)
	}
	// The workspace volume must survive untouched alongside the new one.
	if spec.Containers[0].VolumeMounts[0].MountPath != k8sWorkspace {
		t.Errorf("first mount = %+v, want the workspace mount", spec.Containers[0].VolumeMounts[0])
	}
}

// TestPodExecCore_WriteSandboxIdentityToken_RequiresVolume asserts a Pod
// created without the token volume refuses the write up front.
func TestPodExecCore_WriteSandboxIdentityToken_RequiresVolume(t *testing.T) {
	core := &podExecCore{}
	if err := core.WriteSandboxIdentityToken(context.Background(), "tok"); err == nil {
		t.Fatal("expected an error without the sandbox identity volume")
	}
}

// TestSandboxIdentityTokenPath_OutsideEveryWorkspace pins the invariant
// the file-backed design rests on: the token path must fail ResolvePath on
// every executor that supports sandbox identity, so read_file cannot reach
// it.
func TestSandboxIdentityTokenPath_OutsideEveryWorkspace(t *testing.T) {
	container := &ContainerExecutor{workspace: containerWorkspace}
	if _, err := container.ResolvePath(SandboxIdentityTokenPath); err == nil {
		t.Errorf("container ResolvePath(%s) succeeded; the token must be unreachable through the workspace", SandboxIdentityTokenPath)
	}
	pod := &podExecCore{}
	if _, err := pod.ResolvePath(SandboxIdentityTokenPath); err == nil {
		t.Errorf("k8s ResolvePath(%s) succeeded; the token must be unreachable through the workspace", SandboxIdentityTokenPath)
	}
}
