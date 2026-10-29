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
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	must := require.New(t)
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
	must.NoError(err, "NewContainerExecutorWithContext")
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
	check := assert.New(t)
	must := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	exec := liveContainerExecutor(t, ctx, true)

	const (
		first  = "eyJhbGciOiJFUzI1NiJ9.first-token-payload.sig"
		second = "eyJhbGciOiJFUzI1NiJ9.second-token-payload.sig"
		third  = "eyJhbGciOiJFUzI1NiJ9.third-token-that-never-fully-arrives.sig"
	)
	for i, token := range []string{first, second} {
		err := exec.WriteSandboxIdentityToken(ctx, token)
		must.NoError(err, "WriteSandboxIdentityToken #%d", i+1)
		res, err := exec.Exec(ctx, "cat "+SandboxIdentityTokenPath, 30*time.Second)
		must.NoError(err, "cat token #%d", i+1)
		must.Equal(token, res.Stdout, "token file after delivery #%d (stderr %q)", i+1, res.Stderr)
	}

	// A client that dies mid-stream must not replace the valid token with
	// a partial one.
	aborted := io.MultiReader(strings.NewReader(third[:12]), iotest.ErrReader(errors.New("client aborted")))
	result, err := exec.execInContainerInput(ctx, sandboxIdentityWriteCommand(len(third)), exec.workspace, aborted, containerFileIOTimeout)
	reportedSuccess := err == nil && (result == nil || result.ExitCode == 0)
	must.False(reportedSuccess, "aborted delivery reported success (result %+v)", result)
	res, err := exec.Exec(ctx, "cat "+SandboxIdentityTokenPath, 30*time.Second)
	must.NoError(err, "cat token after aborted delivery")
	must.Equal(second, res.Stdout, "token file after an aborted delivery")

	res, err = exec.Exec(ctx, "stat -c '%a %U' "+SandboxIdentityTokenPath+" && ls -A "+SandboxIdentityTokenDir, 30*time.Second)
	must.NoError(err, "stat token")
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	check.GreaterOrEqual(len(lines), 2, "stat and ls output %q", res.Stdout)
	check.Equal("600 nobody", lines[0], "token file mode and owner")
	check.NotContains(res.Stdout, ".token.tmp", "staging file left behind")

	_, err = exec.ReadFile(ctx, SandboxIdentityTokenPath)
	must.Error(err, "ReadFile reached the token through the workspace-scoped file API")
	entries, err := exec.ListDirectory(ctx, ".")
	must.NoError(err, "ListDirectory")
	for _, e := range entries {
		check.NotContains(e, "token", "workspace listing contains a token artefact: %q", entries)
	}
}

// TestContainerExecutor_ReadFile_SymlinkEscape_LiveEngine proves the read
// guard closes the symlink class against a real engine: a workspace
// symlink to the token, a symlink to a file outside the workspace, and a
// symlinked directory all fail with a workspace-escape error and return
// no content, while a symlink that stays inside the workspace still reads.
func TestContainerExecutor_ReadFile_SymlinkEscape_LiveEngine(t *testing.T) {
	must := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	exec := liveContainerExecutor(t, ctx, true)

	const token = "SECRET-TOKEN-VALUE-eyJhbGciOiJFUzI1NiJ9"
	must.NoError(exec.WriteSandboxIdentityToken(ctx, token), "WriteSandboxIdentityToken")
	setup := strings.Join([]string{
		"printf inside > /workspace/real.txt",
		"ln -sf real.txt /workspace/alias",
		"ln -sf " + SandboxIdentityTokenPath + " /workspace/link",
		"ln -sf /etc/hostname /workspace/hostlink",
		"ln -sf " + SandboxIdentityTokenDir + " /workspace/dirlink",
	}, " && ")
	res, err := exec.Exec(ctx, setup, 30*time.Second)
	must.NoError(err, "symlink setup")
	must.Zero(res.ExitCode, "symlink setup result %+v", res)

	hostname, err := exec.Exec(ctx, "cat /etc/hostname", 30*time.Second)
	must.NoError(err, "read hostname")

	for _, probe := range []string{"link", "hostlink", "dirlink/token"} {
		t.Run(probe, func(t *testing.T) {
			must := require.New(t)
			emitter := &mockSecurityEmitter{}
			exec.Security = emitter
			content, err := exec.ReadFile(ctx, probe)
			must.ErrorContains(err, "escapes workspace")
			must.NotContains(content, token, "ReadFile disclosed out-of-workspace content")
			if hostname.Stdout != "" {
				must.NotContains(content, strings.TrimSpace(hostname.Stdout), "ReadFile disclosed out-of-workspace content")
			}
			must.Equal(1, emitter.pathTraversalCount, "PathTraversalBlocked emissions")
		})
	}
	exec.Security = nil

	got, err := exec.ReadFile(ctx, "alias")
	must.NoError(err, "ReadFile(alias)")
	must.Equal("inside", got, "in-workspace symlink should read its target")
}

// TestContainerExecutor_WriteFile_SymlinkEscape_LiveEngine proves the write
// guard closes the symlink class against a real engine: writes through a
// symlinked directory (to /etc, to the writable /tmp, and to the identity
// token directory, including via a directory whose name ends in a newline)
// and through a symlinked file all fail with a workspace-escape error and
// leave everything outside the workspace untouched. Writes inside the
// workspace, including through an in-workspace file symlink, still land,
// and a symlink to an in-workspace directory is refused without touching it.
func TestContainerExecutor_WriteFile_SymlinkEscape_LiveEngine(t *testing.T) {
	must := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	exec := liveContainerExecutor(t, ctx, true)

	const token = "SECRET-TOKEN-VALUE-eyJhbGciOiJFUzI1NiJ9"
	must.NoError(exec.WriteSandboxIdentityToken(ctx, token), "WriteSandboxIdentityToken")
	setup := strings.Join([]string{
		"printf original > /tmp/target.txt",
		"printf inside > /workspace/real.txt",
		"ln -sf /etc /workspace/escape",
		"ln -sf /tmp /workspace/tmpdir",
		"ln -sf /tmp/target.txt /workspace/filelink",
		"ln -sf " + SandboxIdentityTokenPath + " /workspace/tokenlink",
		"ln -sf real.txt /workspace/alias",
		"mkdir '/workspace/nl\n' '/workspace/tk\n'",
		"ln -sf /tmp /workspace/nl",
		"ln -sf " + SandboxIdentityTokenDir + " /workspace/tk",
		"ln -sf '/workspace/nl\n' /workspace/m",
		"ln -sf '/workspace/tk\n' /workspace/k",
		"mkdir /workspace/src && printf keep > /workspace/src/keep.txt",
		"ln -sf src /workspace/dirlink",
	}, " && ")
	res, err := exec.Exec(ctx, setup, 30*time.Second)
	must.NoError(err, "symlink setup")
	must.Zero(res.ExitCode, "symlink setup result %+v", res)

	for _, probe := range []string{
		"escape/stirrup-escape.txt",
		"escape/stirrup-newdir/x.txt",
		"tmpdir/stirrup-escape.txt",
		"tmpdir/stirrup-newdir/x.txt",
		"filelink",
		"tokenlink",
		"m/stirrup-escape.txt",
		"k/token",
	} {
		t.Run(probe, func(t *testing.T) {
			must := require.New(t)
			emitter := &mockSecurityEmitter{}
			exec.Security = emitter
			err := exec.WriteFile(ctx, probe, "ATTACKER")
			must.ErrorContains(err, "path escapes workspace")
			must.Equal(1, emitter.pathTraversalCount, "PathTraversalBlocked emissions")
		})
	}
	exec.Security = nil

	err = exec.WriteFile(ctx, "nl\n/stirrup-escape.txt", "ATTACKER")
	must.ErrorContains(err, "control character", "a requested path with a newline")
	err = exec.WriteFile(ctx, "dirlink", "ATTACKER")
	must.ErrorContains(err, "is a directory", "a symlink to an in-workspace directory")
	got, err := exec.ReadFile(ctx, "src/keep.txt")
	must.NoError(err, "ReadFile(src/keep.txt)")
	must.Equal("keep", got, "the linked directory must survive a refused write")
	must.NoError(exec.WriteSandboxIdentityToken(ctx, token+"-2"), "token rotation after refused writes")

	verify := strings.Join([]string{
		"[ ! -e /etc/stirrup-escape.txt ]",
		"[ ! -e /etc/stirrup-newdir ]",
		"[ ! -e /tmp/stirrup-escape.txt ]",
		"[ ! -e /tmp/stirrup-newdir ]",
		`[ "$(cat /tmp/target.txt)" = original ]`,
		`[ "$(cat ` + SandboxIdentityTokenPath + `)" = ` + token + `-2 ]`,
		"! grep -rq ATTACKER /etc /tmp " + SandboxIdentityTokenDir,
	}, " && ")
	res, err = exec.Exec(ctx, verify, 30*time.Second)
	must.NoError(err, "verify outside paths")
	must.Zero(res.ExitCode, "something landed outside the workspace: %+v", res)

	must.NoError(exec.WriteFile(ctx, "alias", "rewritten"), "WriteFile(alias)")
	got, err = exec.ReadFile(ctx, "real.txt")
	must.NoError(err, "ReadFile(real.txt)")
	must.Equal("rewritten", got, "an in-workspace symlink should be written through")

	must.NoError(exec.WriteFile(ctx, "new/nested/file.txt", "nested"), "WriteFile(new/nested/file.txt)")
	got, err = exec.ReadFile(ctx, "new/nested/file.txt")
	must.NoError(err, "ReadFile(new/nested/file.txt)")
	must.Equal("nested", got)
}

// TestContainerExecutor_ListDirectory_MarksDirectories_LiveEngine pins the
// cross-executor listing contract against a real engine: directories carry
// a trailing slash, files do not, and "." and ".." are absent.
func TestContainerExecutor_ListDirectory_MarksDirectories_LiveEngine(t *testing.T) {
	must := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	exec := liveContainerExecutor(t, ctx, false)

	res, err := exec.Exec(ctx, "mkdir -p sub && echo hi > top.txt && echo nested > sub/nested.txt", 30*time.Second)
	must.NoError(err, "seed workspace")
	must.Zero(res.ExitCode, "seed workspace result %+v", res)
	entries, err := exec.ListDirectory(ctx, ".")
	must.NoError(err, "ListDirectory")
	must.Equal([]string{"sub/", "top.txt"}, entries)
}

// TestContainerExecutor_ArchiveWorkspace_LiveEngine pins that the archive
// is produced by tar inside the container: it carries the files the sandbox
// wrote and records a symlink as a symlink entry.
func TestContainerExecutor_ArchiveWorkspace_LiveEngine(t *testing.T) {
	must := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	exec := liveContainerExecutor(t, ctx, false)

	res, err := exec.Exec(ctx, "mkdir -p sub && echo hi > top.txt && echo nested > sub/nested.txt && ln -s top.txt alias", 30*time.Second)
	must.NoError(err, "seed workspace")
	must.Zero(res.ExitCode, "seed workspace result %+v", res)
	var buf strings.Builder
	stats, err := exec.ArchiveWorkspace(ctx, &buf, 1<<20)
	must.NoError(err, "ArchiveWorkspace")

	got := map[string]tarEntry{}
	for _, e := range readTarEntries(t, []byte(buf.String())) {
		got[strings.TrimPrefix(e.name, "./")] = e
	}
	t.Run("stats count archived bytes", func(t *testing.T) {
		must := require.New(t)
		must.Equal(int64(buf.Len()), stats.Bytes)
	})
	t.Run("regular file keeps its body", func(t *testing.T) {
		must := require.New(t)
		e := got["top.txt"]
		must.EqualValues(tar.TypeReg, e.typeflag)
		must.Equal("hi\n", e.body)
	})
	t.Run("nested file keeps its body", func(t *testing.T) {
		must := require.New(t)
		must.Equal("nested\n", got["sub/nested.txt"].body)
	})
	t.Run("symlink is archived as a symlink", func(t *testing.T) {
		must := require.New(t)
		e := got["alias"]
		must.EqualValues(tar.TypeSymlink, e.typeflag)
		must.Equal("top.txt", e.linkname)
	})
}

// containerProcessGone runs a probe inside the container that succeeds when
// the pid recorded in pidFile no longer names a live process. A killed
// process whose parent never reaps it stays as a zombie, which counts as gone.
func containerProcessGone(t *testing.T, ctx context.Context, exec *ContainerExecutor, pidFile string) bool {
	t.Helper()
	must := require.New(t)
	probe := `pid=$(cat ` + pidFile + `); [ -e /proc/$pid/stat ] || exit 0; read -r l </proc/$pid/stat; case "${l##*) }" in Z*) exit 0 ;; esac; exit 1`
	res, err := exec.Exec(ctx, probe, 30*time.Second)
	must.NoError(err, "probe %s", pidFile)
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
			must := require.New(t)
			sleeperPID := "/tmp/" + name + "-sleeper.pid"
			marker := "/tmp/" + name + "-after-timeout"
			command := "sh -c 'sleep 3; touch " + marker + "' >/dev/null 2>&1 & sleep 600 & echo $! > " + sleeperPID + "; wait"
			err := call(command)
			must.ErrorIs(err, ErrTimeout)
			must.True(containerProcessGone(t, ctx, exec, sleeperPID), "sleep 600 survived the timeout")
			time.Sleep(4 * time.Second)
			res, err := exec.Exec(ctx, "test ! -e "+marker, 30*time.Second)
			must.NoError(err, "marker check")
			must.Zero(res.ExitCode, "detached grandchild survived the timeout and wrote its marker")
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

	t.Run("normal exit keeps status and output", func(t *testing.T) {
		must := require.New(t)
		res, err := exec.Exec(ctx, "echo out; echo err >&2; exit 7", 30*time.Second)
		must.NoError(err, "Exec")
		must.Equal(7, res.ExitCode, "exit code")
		must.Equal("out\n", res.Stdout, "stdout")
		must.Equal("err\n", res.Stderr, "stderr")
	})
}

const (
	orphanHelperEnv = "STIRRUP_TEST_ORPHAN_HELPER"
	orphanReadyTag  = "ORPHAN_READY "
	// orphanLifetime is the sandbox lifetime the orphan tests give the
	// helper process; short enough to wait out, long enough to inspect.
	orphanLifetime = 20 * time.Second
)

// TestContainerExecutor_OrphanHelperProcess is the body of the child process
// the orphan test kills. It builds a container executor with a short lifetime,
// reports the container ID, and then waits to be killed; it does nothing when
// run as part of the normal suite.
func TestContainerExecutor_OrphanHelperProcess(t *testing.T) {
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
	must := require.New(t)
	sock := os.Getenv("STIRRUP_TEST_CONTAINER_SOCKET")
	if sock == "" {
		t.Skip("STIRRUP_TEST_CONTAINER_SOCKET not set; skipping live container engine test")
	}
	api := newContainerAPIClient(sock)

	cmd := exec.Command(os.Args[0], "-test.run=^TestContainerExecutor_OrphanHelperProcess$", "-test.v")
	cmd.Env = append(os.Environ(), orphanHelperEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	must.NoError(err, "stdout pipe")
	must.NoError(cmd.Start(), "start helper")
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
		must.Fail("helper did not report a container ID")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, found, err := inspectContainer(ctx, api, id)
	must.NoError(err, "inspect running sandbox")
	must.True(found, "inspect running sandbox")
	must.True(c.State.Running, "sandbox should be running")
	must.Equal("true", c.Config.Labels["stirrup.sandbox"], "stirrup.sandbox label")
	must.Equal("orphan-test", c.Config.Labels["stirrup.run-id"], "stirrup.run-id label")

	must.NoError(cmd.Process.Kill(), "SIGKILL helper")
	killed = true
	_ = cmd.Wait()

	deadline := time.Now().Add(orphanLifetime + 30*time.Second)
	for {
		_, found, err := inspectContainer(ctx, api, id)
		must.NoError(err, "inspect after kill")
		if !found {
			return
		}
		if time.Now().After(deadline) {
			must.Failf("container still present", "container %s still present %v after the harness was killed", id, orphanLifetime+30*time.Second)
		}
		time.Sleep(time.Second)
	}
}
