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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
// The handlers run on the server's goroutines, so they report through assert.
func stdinExecHandlers(t *testing.T, capture *stdinExecCapture, exitCode int, stderr string) map[string]http.HandlerFunc {
	t.Helper()
	check := assert.New(t)
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
			if !check.Equal("tcp", r.Header.Get("Upgrade"), "exec start must request Upgrade: tcp; stdin cannot be delivered") {
				http.Error(w, "expected upgrade", http.StatusBadRequest)
				return
			}
			hj, ok := w.(http.Hijacker)
			if !check.True(ok, "mock server must support hijacking") {
				return
			}
			conn, rw, err := hj.Hijack()
			if !check.NoError(err, "hijack") {
				return
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
	cases := []struct {
		name    string
		enabled bool
	}{
		{name: "enabled mounts a hardened tmpfs", enabled: true},
		{name: "disabled mounts nothing", enabled: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
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
				SandboxIdentityToken: tc.enabled,
			})
			must.NoError(err, "NewContainerExecutorWithContext")
			defer func() { _ = exec.Close() }()

			opts, mounted := received.HostConfig.Tmpfs[SandboxIdentityTokenDir]
			must.Equal(tc.enabled, mounted, "tmpfs at %s (Tmpfs=%v)", SandboxIdentityTokenDir, received.HostConfig.Tmpfs)
			if !tc.enabled {
				return
			}
			for _, want := range []string{"nosuid", "nodev", "noexec", "mode=1777", fmt.Sprintf("size=%d", sandboxIdentityTokenMountBytes)} {
				must.Contains(opts, want)
			}
			must.False(strings.HasPrefix(SandboxIdentityTokenDir, containerWorkspace+"/"),
				"token dir %s must lie outside the workspace %s", SandboxIdentityTokenDir, containerWorkspace)
		})
	}
}

// TestContainerExecutor_WriteSandboxIdentityToken_StdinNotArgv drives the
// hijacked exec path against the mock engine: the token must arrive on
// stdin, the exec-create body must attach stdin and carry only the
// path-bearing write command in argv, and the executor must report success
// from the inspected exit code.
func TestContainerExecutor_WriteSandboxIdentityToken_StdinNotArgv(t *testing.T) {
	must := require.New(t)
	const token = "eyJhbGciOiJFUzI1NiJ9.refresh-payload.signature"
	capture := &stdinExecCapture{}
	exec, cleanup := newMockContainerExecutor(t, stdinExecHandlers(t, capture, 0, ""))
	defer cleanup()
	exec.sandboxIdentity = true

	must.NoError(exec.WriteSandboxIdentityToken(context.Background(), token))

	must.Equal([]string{token}, capture.stdinSeen(), "the engine must receive exactly one delivery of the token on stdin")
	create := capture.lastCreate()
	must.True(create.AttachStdin, "exec create must attach stdin")
	must.Equal(sandboxIdentityWriteCommand(len(token)), create.Cmd, "exec argv must be the token write command")
	for _, arg := range create.Cmd {
		must.NotContains(arg, token, "token must not appear in exec argv")
	}

	script := create.Cmd[2]
	for _, need := range []string{"umask 077", `wc -c < "$2"`, `-eq "$3"`, `mv -f -- "$2" "$1"`, `rm -f -- "$2"`, "exit 4"} {
		must.Contains(script, need)
	}

	must.Equal(SandboxIdentityTokenPath, create.Cmd[4], "write argv final path")
	must.Equal(sandboxIdentityTokenStaging, create.Cmd[5], "write argv staging path")
	must.Equal(strconv.Itoa(len(token)), create.Cmd[6], "write argv byte length")
}

// TestContainerExecutor_WriteSandboxIdentityToken_ShortStreamSurfaces
// asserts that a stdin copy which ends before the announced length is
// reported even when the in-container command reports success, so a
// truncated delivery can never pass as a complete one.
func TestContainerExecutor_WriteSandboxIdentityToken_ShortStreamSurfaces(t *testing.T) {
	must := require.New(t)
	capture := &stdinExecCapture{}
	exec, cleanup := newMockContainerExecutor(t, stdinExecHandlers(t, capture, 0, ""))
	defer cleanup()
	exec.sandboxIdentity = true

	aborted := io.MultiReader(strings.NewReader("eyJhbGciOiJFUzI1NiJ9.half"), iotest.ErrReader(errors.New("client aborted")))
	_, err := exec.execInContainerInput(context.Background(), sandboxIdentityWriteCommand(64), exec.workspace, aborted, containerFileIOTimeout)
	must.ErrorContains(err, "client aborted", "the error must carry the stdin reader's failure")
}

// TestContainerExecutor_WriteSandboxIdentityToken_ReportsFailure asserts a
// non-zero exit surfaces the command's stderr without ever echoing the
// token.
func TestContainerExecutor_WriteSandboxIdentityToken_ReportsFailure(t *testing.T) {
	must := require.New(t)
	const token = "must-not-appear"
	capture := &stdinExecCapture{}
	exec, cleanup := newMockContainerExecutor(t, stdinExecHandlers(t, capture, 1, "sh: can't create /run/stirrup/sandbox-identity/.token.tmp: Permission denied\n"))
	defer cleanup()
	exec.sandboxIdentity = true

	err := exec.WriteSandboxIdentityToken(context.Background(), token)
	must.ErrorContains(err, "Permission denied")
	must.ErrorContains(err, "exit 1")
	must.NotContains(err.Error(), token, "the error must not contain the token")
}

// TestContainerExecutor_WriteSandboxIdentityToken_RequiresMount asserts a
// container created without the token tmpfs refuses the write before
// touching the engine, rather than running the command against a
// read-only rootfs.
func TestContainerExecutor_WriteSandboxIdentityToken_RequiresMount(t *testing.T) {
	must := require.New(t)
	capture := &stdinExecCapture{}
	exec, cleanup := newMockContainerExecutor(t, stdinExecHandlers(t, capture, 0, ""))
	defer cleanup()

	err := exec.WriteSandboxIdentityToken(context.Background(), "tok")
	must.Error(err, "an error is expected without the sandbox identity mount")
	must.Empty(capture.stdinSeen(), "the engine must receive no exec")
}

// TestContainerAPIClient_StartExecWithStdin_RequiresUpgrade asserts a
// daemon that answers an exec start with a plain 200 instead of switching
// protocols is treated as unable to carry stdin, and the call fails rather
// than running the command without its input.
func TestContainerAPIClient_StartExecWithStdin_RequiresUpgrade(t *testing.T) {
	must := require.New(t)
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
		// Release the connection before failing on the unexpected success.
		_ = stream.Close()
	}
	must.ErrorContains(err, "HTTP 101", "the error must explain the missing upgrade")
}

// TestBuildSandboxPodSpec_SandboxIdentityVolume asserts the flag adds a
// memory-backed emptyDir mounted at SandboxIdentityTokenDir outside the
// workspace, and that the Pod spec is unchanged without it.
func TestBuildSandboxPodSpec_SandboxIdentityVolume(t *testing.T) {
	must := require.New(t)
	cfg := baseSandboxCfg()
	without := buildSandboxPodSpec(cfg, nil, nil)
	must.Len(without.Volumes, 1, "volumes without the flag")
	must.Len(without.Containers[0].VolumeMounts, 1, "mounts without the flag")

	cfg.SandboxIdentityToken = true
	spec := buildSandboxPodSpec(cfg, nil, nil)

	var vol *corev1.Volume
	for i := range spec.Volumes {
		if spec.Volumes[i].Name == k8sSandboxIdentityVolume {
			vol = &spec.Volumes[i]
		}
	}
	must.NotNil(vol, "volume %q missing from %+v", k8sSandboxIdentityVolume, spec.Volumes)
	must.NotNil(vol.EmptyDir, "volume %q must be an emptyDir, got %+v", k8sSandboxIdentityVolume, vol.VolumeSource)
	must.Equal(corev1.StorageMediumMemory, vol.EmptyDir.Medium, "volume %q must be memory-backed", k8sSandboxIdentityVolume)
	must.NotNil(vol.EmptyDir.SizeLimit, "volume %q must set a size limit", k8sSandboxIdentityVolume)
	must.Equal(k8sSandboxIdentityVolumeSize, vol.EmptyDir.SizeLimit.String(), "volume %q size limit", k8sSandboxIdentityVolume)

	var mount *corev1.VolumeMount
	for i := range spec.Containers[0].VolumeMounts {
		if spec.Containers[0].VolumeMounts[i].Name == k8sSandboxIdentityVolume {
			mount = &spec.Containers[0].VolumeMounts[i]
		}
	}
	must.NotNil(mount, "mount for %q missing from %+v", k8sSandboxIdentityVolume, spec.Containers[0].VolumeMounts)
	must.Equal(SandboxIdentityTokenDir, mount.MountPath, "token mount path")
	must.False(strings.HasPrefix(mount.MountPath, k8sWorkspace+"/"),
		"token dir %s must lie outside the workspace %s", mount.MountPath, k8sWorkspace)
	// The workspace volume must survive untouched alongside the new one.
	must.Equal(k8sWorkspace, spec.Containers[0].VolumeMounts[0].MountPath,
		"the first mount must remain the workspace mount, got %+v", spec.Containers[0].VolumeMounts[0])
}

// TestPodExecCore_WriteSandboxIdentityToken_RequiresVolume asserts a Pod
// created without the token volume refuses the write up front.
func TestPodExecCore_WriteSandboxIdentityToken_RequiresVolume(t *testing.T) {
	must := require.New(t)
	core := &podExecCore{}
	must.Error(core.WriteSandboxIdentityToken(context.Background(), "tok"),
		"an error is expected without the sandbox identity volume")
}

// TestSandboxIdentityTokenPath_OutsideEveryWorkspace pins the invariant
// the file-backed design rests on: the token path must fail ResolvePath on
// every executor that supports sandbox identity, so read_file cannot reach
// it.
func TestSandboxIdentityTokenPath_OutsideEveryWorkspace(t *testing.T) {
	t.Run("container executor", func(t *testing.T) {
		must := require.New(t)
		container := &ContainerExecutor{workspace: containerWorkspace}
		_, err := container.ResolvePath(SandboxIdentityTokenPath)
		must.Error(err, "ResolvePath(%s) must fail: the token must be unreachable through the workspace", SandboxIdentityTokenPath)
	})
	t.Run("k8s pod executor", func(t *testing.T) {
		must := require.New(t)
		pod := &podExecCore{}
		_, err := pod.ResolvePath(SandboxIdentityTokenPath)
		must.Error(err, "ResolvePath(%s) must fail: the token must be unreachable through the workspace", SandboxIdentityTokenPath)
	})
}
