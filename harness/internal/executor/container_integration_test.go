package executor

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// liveContainerExecutor builds a hardened container against a real
// Docker/Podman engine, gated on STIRRUP_TEST_CONTAINER_SOCKET (the
// engine's Unix socket) so the default `just test` run never needs an
// engine; STIRRUP_TEST_CONTAINER_IMAGE (default busybox:latest) must
// already be present locally.
func liveContainerExecutor(t *testing.T, ctx context.Context, sandboxIdentity bool) *ContainerExecutor {
	t.Helper()
	return newLiveContainerExecutor(t, ctx, sandboxIdentity, Lifecycle{})
}

// liveContainerExecutorWithLifecycle is liveContainerExecutor with the
// sandbox lifetime and run label set.
func liveContainerExecutorWithLifecycle(t *testing.T, ctx context.Context, life Lifecycle) *ContainerExecutor {
	t.Helper()
	return newLiveContainerExecutor(t, ctx, false, life)
}

func newLiveContainerExecutor(t *testing.T, ctx context.Context, sandboxIdentity bool, life Lifecycle) *ContainerExecutor {
	t.Helper()
	sock := os.Getenv("STIRRUP_TEST_CONTAINER_SOCKET")
	if sock == "" {
		t.Skip("STIRRUP_TEST_CONTAINER_SOCKET not set; skipping live container engine test")
	}
	image := os.Getenv("STIRRUP_TEST_CONTAINER_IMAGE")
	if image == "" {
		image = "busybox:latest"
	}

	exec, err := NewContainerExecutorWithContext(ctx, ContainerExecutorConfig{
		Image:                image,
		HostDir:              t.TempDir(),
		SocketPath:           sock,
		Network:              &types.NetworkConfig{Mode: "none"},
		SandboxIdentityToken: sandboxIdentity,
		Lifecycle:            life,
	})
	if err != nil {
		t.Fatalf("NewContainerExecutorWithContext: %v", err)
	}
	t.Cleanup(func() { _ = exec.Close() })
	return exec
}

// TestContainerExecutor_SandboxIdentityToken_LiveEngine drives the token
// delivery path against a real engine: a hardened container (read-only
// rootfs, unprivileged uid) receives a token over the hijacked exec's
// stdin, the file lands at SandboxIdentityTokenPath with mode 0600, a
// second delivery replaces it in place, an aborted delivery leaves the
// previous token intact, and the workspace-scoped file API cannot reach
// the file.
func TestContainerExecutor_SandboxIdentityToken_LiveEngine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	exec := liveContainerExecutor(t, ctx, true)

	const (
		first  = "eyJhbGciOiJFUzI1NiJ9.first-token-payload.sig"
		second = "eyJhbGciOiJFUzI1NiJ9.second-token-payload.sig"
		third  = "eyJhbGciOiJFUzI1NiJ9.third-token-that-never-fully-arrives.sig"
	)
	for i, token := range []string{first, second} {
		if err := exec.WriteSandboxIdentityToken(ctx, token); err != nil {
			t.Fatalf("WriteSandboxIdentityToken #%d: %v", i+1, err)
		}
		res, err := exec.Exec(ctx, "cat "+SandboxIdentityTokenPath, 30*time.Second)
		if err != nil {
			t.Fatalf("cat token #%d: %v", i+1, err)
		}
		if res.Stdout != token {
			t.Errorf("token file after delivery #%d = %q, want %q (stderr %q)", i+1, res.Stdout, token, res.Stderr)
		}
	}

	// A client that dies mid-stream must not replace the valid token with
	// a partial one.
	aborted := io.MultiReader(strings.NewReader(third[:12]), iotest.ErrReader(errors.New("client aborted")))
	result, err := exec.execInContainerInput(ctx, sandboxIdentityWriteCommand(len(third)), exec.workspace, aborted, containerFileIOTimeout)
	if err == nil && (result == nil || result.ExitCode == 0) {
		t.Fatalf("aborted delivery reported success (result %+v)", result)
	}
	res, err := exec.Exec(ctx, "cat "+SandboxIdentityTokenPath, 30*time.Second)
	if err != nil {
		t.Fatalf("cat token after aborted delivery: %v", err)
	}
	if res.Stdout != second {
		t.Errorf("token file after an aborted delivery = %q, want the previous token %q", res.Stdout, second)
	}

	res, err = exec.Exec(ctx, "stat -c '%a %U' "+SandboxIdentityTokenPath+" && ls -A "+SandboxIdentityTokenDir, 30*time.Second)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) < 2 || lines[0] != "600 nobody" {
		t.Errorf("token file mode/owner = %q, want \"600 nobody\" (output %q)", lines, res.Stdout)
	}
	if strings.Contains(res.Stdout, ".token.tmp") {
		t.Errorf("staging file left behind: %q", res.Stdout)
	}

	if _, err := exec.ReadFile(ctx, SandboxIdentityTokenPath); err == nil {
		t.Error("ReadFile reached the token through the workspace-scoped file API")
	}
	entries, err := exec.ListDirectory(ctx, ".")
	if err != nil {
		t.Fatalf("ListDirectory: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e, "token") {
			t.Errorf("workspace listing contains a token artefact: %q", entries)
		}
	}
}

// TestContainerExecutor_ReadFile_SymlinkEscape_LiveEngine proves the read
// guard closes the symlink class against a real engine: a workspace
// symlink to the token, a symlink to a file outside the workspace, and a
// symlinked directory all fail with a workspace-escape error and return
// no content, while a symlink that stays inside the workspace still reads.
func TestContainerExecutor_ReadFile_SymlinkEscape_LiveEngine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	exec := liveContainerExecutor(t, ctx, true)

	const token = "SECRET-TOKEN-VALUE-eyJhbGciOiJFUzI1NiJ9"
	if err := exec.WriteSandboxIdentityToken(ctx, token); err != nil {
		t.Fatalf("WriteSandboxIdentityToken: %v", err)
	}
	setup := strings.Join([]string{
		"printf inside > /workspace/real.txt",
		"ln -sf real.txt /workspace/alias",
		"ln -sf " + SandboxIdentityTokenPath + " /workspace/link",
		"ln -sf /etc/hostname /workspace/hostlink",
		"ln -sf " + SandboxIdentityTokenDir + " /workspace/dirlink",
	}, " && ")
	if res, err := exec.Exec(ctx, setup, 30*time.Second); err != nil || res.ExitCode != 0 {
		t.Fatalf("symlink setup failed: err=%v result=%+v", err, res)
	}

	hostname, err := exec.Exec(ctx, "cat /etc/hostname", 30*time.Second)
	if err != nil {
		t.Fatalf("read hostname: %v", err)
	}

	for _, probe := range []string{"link", "hostlink", "dirlink/token"} {
		emitter := &mockSecurityEmitter{}
		exec.Security = emitter
		content, err := exec.ReadFile(ctx, probe)
		if err == nil || !strings.Contains(err.Error(), "escapes workspace") {
			t.Errorf("ReadFile(%s) = (%q, %v), want a workspace-escape error", probe, content, err)
		}
		if strings.Contains(content, token) || (hostname.Stdout != "" && strings.Contains(content, strings.TrimSpace(hostname.Stdout))) {
			t.Errorf("ReadFile(%s) disclosed out-of-workspace content: %q", probe, content)
		}
		if emitter.pathTraversalCount != 1 {
			t.Errorf("ReadFile(%s) emitted PathTraversalBlocked %d times, want 1", probe, emitter.pathTraversalCount)
		}
	}
	exec.Security = nil

	got, err := exec.ReadFile(ctx, "alias")
	if err != nil || got != "inside" {
		t.Errorf("ReadFile(alias) = (%q, %v), want an in-workspace symlink to read its target", got, err)
	}
}

// TestContainerExecutor_ListDirectory_MarksDirectories_LiveEngine pins the
// cross-executor listing contract against a real engine: directories carry
// a trailing slash, files do not, and "." and ".." are absent.
func TestContainerExecutor_ListDirectory_MarksDirectories_LiveEngine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	exec := liveContainerExecutor(t, ctx, false)

	res, err := exec.Exec(ctx, "mkdir -p sub && echo hi > top.txt && echo nested > sub/nested.txt", 30*time.Second)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("seed workspace: err %v, result %+v", err, res)
	}
	entries, err := exec.ListDirectory(ctx, ".")
	if err != nil {
		t.Fatalf("ListDirectory: %v", err)
	}
	if !slices.Equal(entries, []string{"sub/", "top.txt"}) {
		t.Errorf("ListDirectory(.) = %q, want [sub/ top.txt]", entries)
	}
}

// TestContainerExecutor_ArchiveWorkspace_LiveEngine pins that the archive
// is produced by tar inside the container: it carries the files the sandbox
// wrote and records a symlink as a symlink entry.
func TestContainerExecutor_ArchiveWorkspace_LiveEngine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	exec := liveContainerExecutor(t, ctx, false)

	res, err := exec.Exec(ctx, "mkdir -p sub && echo hi > top.txt && echo nested > sub/nested.txt && ln -s top.txt alias", 30*time.Second)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("seed workspace: err %v, result %+v", err, res)
	}
	var buf strings.Builder
	stats, err := exec.ArchiveWorkspace(ctx, &buf, 1<<20)
	if err != nil {
		t.Fatalf("ArchiveWorkspace: %v", err)
	}
	if stats.Bytes != int64(buf.Len()) {
		t.Errorf("stats.Bytes = %d, want %d", stats.Bytes, buf.Len())
	}
	got := map[string]tarEntry{}
	for _, e := range readTarEntries(t, []byte(buf.String())) {
		got[strings.TrimPrefix(e.name, "./")] = e
	}
	if e := got["top.txt"]; e.typeflag != tar.TypeReg || e.body != "hi\n" {
		t.Errorf("top.txt = %+v, want a regular file with body hi", e)
	}
	if e := got["sub/nested.txt"]; e.body != "nested\n" {
		t.Errorf("sub/nested.txt = %+v, want body nested", e)
	}
	if e := got["alias"]; e.typeflag != tar.TypeSymlink || e.linkname != "top.txt" {
		t.Errorf("alias = %+v, want a symlink to top.txt", e)
	}
}

// containerProcessGone runs a probe inside the container that succeeds when
// the pid recorded in pidFile no longer names a live process. A killed
// process whose parent never reaps it stays as a zombie, which counts as gone.
func containerProcessGone(t *testing.T, ctx context.Context, exec *ContainerExecutor, pidFile string) bool {
	t.Helper()
	probe := `pid=$(cat ` + pidFile + `); [ -e /proc/$pid/stat ] || exit 0; read -r l </proc/$pid/stat; case "${l##*) }" in Z*) exit 0 ;; esac; exit 1`
	res, err := exec.Exec(ctx, probe, 30*time.Second)
	if err != nil {
		t.Fatalf("probe %s: %v", pidFile, err)
	}
	return res.ExitCode == 0
}

// TestContainerExecutor_TimeoutKillsProcessTree_LiveEngine verifies, against
// a real engine, that a timed-out or cancelled command leaves no surviving
// process and that a detached grandchild never writes its marker.
func TestContainerExecutor_TimeoutKillsProcessTree_LiveEngine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	exec := liveContainerExecutor(t, ctx, false)

	run := func(t *testing.T, name string, call func(command string) error) {
		t.Run(name, func(t *testing.T) {
			sleeperPID := "/tmp/" + name + "-sleeper.pid"
			marker := "/tmp/" + name + "-after-timeout"
			command := "sh -c 'sleep 3; touch " + marker + "' >/dev/null 2>&1 & sleep 600 & echo $! > " + sleeperPID + "; wait"
			if err := call(command); !errors.Is(err, ErrTimeout) {
				t.Fatalf("err = %v, want ErrTimeout", err)
			}
			if !containerProcessGone(t, ctx, exec, sleeperPID) {
				t.Error("sleep 600 survived the timeout")
			}
			time.Sleep(4 * time.Second)
			if res, err := exec.Exec(ctx, "test ! -e "+marker, 30*time.Second); err != nil || res.ExitCode != 0 {
				t.Errorf("detached grandchild survived the timeout and wrote its marker: err=%v result=%+v", err, res)
			}
		})
	}
	run(t, "exec", func(command string) error {
		_, err := exec.Exec(ctx, command, time.Second)
		return err
	})
	run(t, "stream", func(command string) error {
		_, err := exec.ExecStream(ctx, command, time.Second, io.Discard, io.Discard)
		return err
	})

	t.Run("unaffected", func(t *testing.T) {
		res, err := exec.Exec(ctx, "echo out; echo err >&2; exit 7", 30*time.Second)
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if res.ExitCode != 7 || res.Stdout != "out\n" || res.Stderr != "err\n" {
			t.Errorf("result = %+v, want exit 7, stdout %q, stderr %q", res, "out\n", "err\n")
		}
	})
}

const (
	orphanHelperEnv = "STIRRUP_TEST_ORPHAN_HELPER"
	orphanReadyTag  = "ORPHAN_READY "
	// orphanLifetime is the sandbox lifetime the orphan tests give the
	// helper process; short enough to wait out, long enough to inspect.
	orphanLifetime = 20 * time.Second
)

// TestContainerOrphanHelper is the body of the child process the orphan test
// kills. It builds a container executor with a short lifetime, reports the
// container ID, and then waits to be killed; it does nothing when run as part
// of the normal suite.
func TestContainerOrphanHelper(t *testing.T) {
	if os.Getenv(orphanHelperEnv) == "" {
		t.Skip("helper process for TestContainerExecutor_OrphanReapedAfterHarnessKill")
	}
	sandbox := liveContainerExecutorWithLifecycle(t, context.Background(), Lifecycle{RunID: "orphan-test", MaxLifetime: orphanLifetime})
	fmt.Println(orphanReadyTag + sandbox.containerID)
	time.Sleep(10 * time.Minute)
}

type inspectedContainer struct {
	State  struct{ Running bool }
	Config struct{ Labels map[string]string }
}

// inspectContainer reports whether the engine still has the container; a 404
// is the absence the orphan test waits for.
func inspectContainer(ctx context.Context, api *containerAPIClient, id string) (*inspectedContainer, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api.url("/containers/"+id+"/json"), nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := api.doRequest(req)
	if err != nil {
		var statusErr *apiStatusError
		if errors.As(err, &statusErr) && statusErr.status == http.StatusNotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out inspectedContainer
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, false, err
	}
	return &out, true, nil
}

// TestContainerExecutor_OrphanReapedAfterHarnessKill SIGKILLs a process that
// owns a sandbox container, so no Close runs, and verifies that the labelled
// container disappears on its own within its lifetime.
func TestContainerExecutor_OrphanReapedAfterHarnessKill(t *testing.T) {
	sock := os.Getenv("STIRRUP_TEST_CONTAINER_SOCKET")
	if sock == "" {
		t.Skip("STIRRUP_TEST_CONTAINER_SOCKET not set; skipping live container engine test")
	}
	api := newContainerAPIClient(sock)

	cmd := exec.Command(os.Args[0], "-test.run=^TestContainerOrphanHelper$", "-test.v")
	cmd.Env = append(os.Environ(), orphanHelperEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	killed := false
	var id string
	t.Cleanup(func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		if id != "" {
			_ = api.removeContainer(context.Background(), id, true)
		}
	})

	ready := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if rest, ok := strings.CutPrefix(sc.Text(), orphanReadyTag); ok {
				ready <- rest
				break
			}
		}
		_, _ = io.Copy(io.Discard, stdout)
	}()
	select {
	case id = <-ready:
	case <-time.After(2 * time.Minute):
		t.Fatal("helper did not report a container ID")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, found, err := inspectContainer(ctx, api, id)
	if err != nil || !found {
		t.Fatalf("inspect running sandbox: found=%v err=%v", found, err)
	}
	if !c.State.Running || c.Config.Labels["stirrup.sandbox"] != "true" || c.Config.Labels["stirrup.run-id"] != "orphan-test" {
		t.Fatalf("sandbox = running %v labels %v, want running with stirrup labels", c.State.Running, c.Config.Labels)
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL helper: %v", err)
	}
	killed = true
	_ = cmd.Wait()

	deadline := time.Now().Add(orphanLifetime + 30*time.Second)
	for {
		_, found, err := inspectContainer(ctx, api, id)
		if err != nil {
			t.Fatalf("inspect after kill: %v", err)
		}
		if !found {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s still present %v after the harness was killed", id, orphanLifetime+30*time.Second)
		}
		time.Sleep(time.Second)
	}
}
