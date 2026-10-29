package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	utilexec "k8s.io/client-go/util/exec"
)

// runWriteGuard runs the write guard on the host from the workspace's
// parent directory with workspace as $1 and returns its stdout, stderr,
// and exit status.
func runWriteGuard(t *testing.T, env []string, workspace, target string) (stdout, stderr string, code int) {
	t.Helper()
	must := require.New(t)
	argv := workspaceWriteGuardCommand(workspace, target)
	cmd := osexec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Dir = filepath.Dir(workspace)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	var exitErr *osexec.ExitError
	if err != nil {
		must.ErrorAs(err, &exitErr, "run guard")
		return out.String(), errOut.String(), exitErr.ExitCode()
	}
	return out.String(), errOut.String(), 0
}

// TestWorkspaceWriteGuardScript_ConfinesWritesToWorkspace runs the guard
// script against a host directory standing in for /workspace, with a
// sibling directory standing in for the rest of the sandbox filesystem.
func TestWorkspaceWriteGuardScript_ConfinesWritesToWorkspace(t *testing.T) {
	must := require.New(t)
	if err := osexec.Command("readlink", "-f", "/").Run(); err != nil {
		t.Skip("host readlink lacks -f; the guard needs it")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	must.NoError(err)
	ws := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	must.NoError(os.MkdirAll(filepath.Join(ws, "sub"), 0o755))
	must.NoError(os.MkdirAll(outside, 0o755))
	must.NoError(os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("s"), 0o644))
	must.NoError(os.WriteFile(filepath.Join(ws, "real.txt"), []byte("r"), 0o644))
	must.NoError(os.WriteFile(filepath.Join(ws, "plain"), []byte("p"), 0o644))
	must.NoError(os.Symlink(outside, filepath.Join(ws, "escape")))
	must.NoError(os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(ws, "link")))
	must.NoError(os.Symlink(filepath.Join(outside, "absent.txt"), filepath.Join(ws, "dangling")))
	must.NoError(os.Symlink("real.txt", filepath.Join(ws, "alias")))
	must.NoError(os.Symlink("sub", filepath.Join(ws, "subalias")))
	must.NoError(os.Symlink("sub", filepath.Join(ws, "dirlink")))
	// A sibling whose name extends the workspace's must not pass a prefix check.
	sibling := ws + "-evil"
	must.NoError(os.MkdirAll(sibling, 0o755))
	must.NoError(os.Symlink(sibling, filepath.Join(ws, "evil")))
	must.NoError(os.Symlink(filepath.Join(sibling, "f.txt"), filepath.Join(ws, "evilfile")))
	// "nl\n" is a real directory and "nl" escapes; a resolution that loses
	// the trailing newline would write through "nl".
	must.NoError(os.Mkdir(filepath.Join(ws, "nl\n"), 0o755))
	must.NoError(os.Symlink(outside, filepath.Join(ws, "nl")))
	must.NoError(os.Symlink(filepath.Join(ws, "nl\n"), filepath.Join(ws, "m")))
	env := os.Environ()

	for _, tc := range []struct {
		name   string
		target string
		want   string
	}{
		{name: "new file in new nested directories", target: "a/b/new.txt", want: "a/b/new.txt"},
		{name: "existing file at the root", target: "real.txt", want: "real.txt"},
		{name: "symlink inside the workspace writes through", target: "alias", want: "real.txt"},
		{name: "symlinked directory inside the workspace", target: "subalias/x.txt", want: "sub/x.txt"},
		{name: "name with a space", target: "sp ace.txt", want: "sp ace.txt"},
		{name: "name with a leading dash", target: "-lead.txt", want: "-lead.txt"},
		{name: "name with a command substitution", target: "$(touch pwned).txt", want: "$(touch pwned).txt"},
		{name: "name with quotes", target: `q'uo"te.txt`, want: `q'uo"te.txt`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			stdout, stderr, code := runWriteGuard(t, env, ws, filepath.Join(ws, tc.target))
			must.Zero(code, "guard stderr: %s", stderr)
			must.Equal(filepath.Join(ws, tc.want)+"\n", stdout)
		})
	}
	must.DirExists(filepath.Join(ws, "a", "b"), "guard creates missing parents")
	must.NoFileExists(filepath.Join(root, "pwned"), "a path must never be evaluated as shell")

	for _, tc := range []struct {
		name   string
		target string
	}{
		{name: "symlinked destination", target: "link"},
		{name: "dangling symlinked destination", target: "dangling"},
		{name: "symlinked intermediate directory", target: "escape/new.txt"},
		{name: "missing directories under a symlinked directory", target: "escape/made/deep/new.txt"},
		{name: "symlinked directory sharing the workspace prefix", target: "evil/new.txt"},
		{name: "symlinked destination sharing the workspace prefix", target: "evilfile"},
		{name: "symlink to a directory whose name ends in a newline", target: "m/pwned.txt"},
		{name: "requested directory whose name ends in a newline", target: "nl\n/pwned.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			stdout, stderr, code := runWriteGuard(t, env, ws, filepath.Join(ws, tc.target))
			must.Equal(workspaceWriteEscapeExit, code, "guard stdout %q stderr: %s", stdout, stderr)
			must.Empty(stdout)
			must.Contains(stderr, "path escapes workspace")
		})
	}
	must.NoDirExists(filepath.Join(outside, "made"), "mkdir must not run through a symlinked directory")
	entries, err := os.ReadDir(outside)
	must.NoError(err)
	must.Len(entries, 1, "only the seeded file may exist outside the workspace")
	entries, err = os.ReadDir(sibling)
	must.NoError(err)
	must.Empty(entries, "nothing may land in the prefix-sharing sibling")

	for _, tc := range []struct {
		name   string
		target string
	}{
		{name: "directory destination", target: "sub"},
		{name: "symlink to an in-workspace directory", target: "dirlink"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			stdout, stderr, code := runWriteGuard(t, env, ws, filepath.Join(ws, tc.target))
			must.Equal(workspaceWriteIsDirExit, code, "guard stderr: %s", stderr)
			must.Empty(stdout)
		})
	}

	t.Run("regular file as an ancestor is an ordinary failure", func(t *testing.T) {
		must := require.New(t)
		_, stderr, code := runWriteGuard(t, env, ws, filepath.Join(ws, "plain", "x.txt"))
		must.Equal(1, code, "guard stderr: %s", stderr)
	})

	t.Run("missing readlink fails closed", func(t *testing.T) {
		must := require.New(t)
		stdout, _, code := runWriteGuard(t, []string{"PATH=" + t.TempDir()}, ws, filepath.Join(ws, "z.txt"))
		must.Equal(workspaceWriteNoReadlinkExit, code)
		must.Empty(stdout)
		must.NoFileExists(filepath.Join(ws, "z.txt"))
	})
}

func TestWorkspaceWriteGuardCommand_PassesPathsAsArguments(t *testing.T) {
	must := require.New(t)
	cmd := workspaceWriteGuardCommand("/workspace", "/workspace/notes.md")
	must.Len(cmd, 6)
	must.Equal([]string{"sh", "-c"}, cmd[:2])
	must.Equal([]string{"sh", "/workspace", "/workspace/notes.md"}, cmd[3:])
	must.NotContains(cmd[2], "/workspace/notes.md",
		"the requested path must reach the script as an argument, not by interpolation")
}

// TestClassifyWorkspaceWriteExit_EscapeReadlinkAndOther shares one emitter
// across the calls: only the escape exit may emit PathTraversalBlocked.
func TestClassifyWorkspaceWriteExit_EscapeReadlinkAndOther(t *testing.T) {
	must := require.New(t)
	emitter := &mockSecurityEmitter{}

	err := classifyWorkspaceWriteExit(workspaceWriteEscapeExit, "path escapes workspace: /workspace/link\n", "link", "/workspace", emitter)
	must.ErrorContains(err, "path escapes workspace: link")
	must.Equal(1, emitter.pathTraversalCount, "escape exit must emit PathTraversalBlocked")

	err = classifyWorkspaceWriteExit(workspaceWriteNoReadlinkExit, "readlink is required for workspace writes\n", "a.txt", "/workspace", emitter)
	must.ErrorContains(err, "readlink")

	err = classifyWorkspaceWriteExit(1, "mkdir: can't create directory '/workspace/f/x': Not a directory\n", "f/x/a.txt", "/workspace", emitter)
	must.ErrorContains(err, "Not a directory")

	err = classifyWorkspaceWriteExit(2, "", "a.txt", "/workspace", emitter)
	must.ErrorContains(err, "guard exited 2")
	must.Equal(1, emitter.pathTraversalCount, "non-escape exits must not emit PathTraversalBlocked")
}

func TestParseWorkspaceWriteTarget_AcceptsOnlyCleanPathsInsideWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stdout string
		want   string
	}{
		{name: "file inside the workspace", stdout: "/workspace/a.txt\n", want: "/workspace/a.txt"},
		{name: "embedded newline", stdout: "/workspace/sub\n/a.txt\n"},
		{name: "doubled trailing newline", stdout: "/workspace/a.txt\n\n"},
		{name: "empty output", stdout: ""},
		{name: "workspace root", stdout: "/workspace\n"},
		{name: "workspace root with a slash", stdout: "/workspace/\n"},
		{name: "sibling sharing the prefix", stdout: "/workspacefoo/a\n"},
		{name: "unclean path", stdout: "/workspace/../etc/passwd\n"},
		{name: "outside the workspace", stdout: "/etc/passwd\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			emitter := &mockSecurityEmitter{}
			got, err := parseWorkspaceWriteTarget(tc.stdout, "a.txt", "/workspace", emitter)
			if tc.want == "" {
				must.ErrorContains(err, "path escapes workspace")
				must.Equal(1, emitter.pathTraversalCount)
				return
			}
			must.NoError(err)
			must.Equal(tc.want, got)
			must.Zero(emitter.pathTraversalCount)
		})
	}
}

// archiveUploads records the archive uploads a mock engine received.
type archiveUploads struct {
	mu    sync.Mutex
	count int
	dest  string
	name  string
}

func (u *archiveUploads) snapshot() (count int, dest, name string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.count, u.dest, u.name
}

// writeGuardHandlers serves the write guard over the mock exec API (as
// readExecHandlers does for reads) and records any archive upload.
func writeGuardHandlers(capture *readExecCapture, uploads *archiveUploads, stdout, stderr string, exitCode int) map[string]http.HandlerFunc {
	handlers := readExecHandlers(capture, []byte(stdout), stderr, exitCode)
	handlers["PUT /containers/*/archive"] = func(w http.ResponseWriter, r *http.Request) {
		uploads.mu.Lock()
		defer uploads.mu.Unlock()
		uploads.count++
		uploads.dest = r.URL.Query().Get("path")
		if hdr, err := tar.NewReader(r.Body).Next(); err == nil {
			uploads.name = hdr.Name
		}
		w.WriteHeader(http.StatusOK)
	}
	return handlers
}

// TestContainerExecutor_WriteFile_SymlinkEscapeRefused covers a symlinked
// destination and a symlinked intermediate directory: the guard's escape
// exit must surface as a workspace-escape error, emit PathTraversalBlocked,
// and never reach the archive endpoint.
func TestContainerExecutor_WriteFile_SymlinkEscapeRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
	}{
		{name: "symlinked destination", target: "link"},
		{name: "symlinked intermediate directory", target: "escape/passwd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			capture := &readExecCapture{}
			uploads := &archiveUploads{}
			exec, cleanup := newMockContainerExecutor(t, writeGuardHandlers(capture, uploads, "", "path escapes workspace: /workspace/"+tc.target+"\n", workspaceWriteEscapeExit))
			defer cleanup()
			emitter := &mockSecurityEmitter{}
			exec.Security = emitter

			err := exec.WriteFile(context.Background(), tc.target, "payload")
			must.ErrorContains(err, "path escapes workspace")
			must.Equal(1, emitter.pathTraversalCount, "PathTraversalBlocked emissions")
			count, _, _ := uploads.snapshot()
			must.Zero(count, "archive endpoint must not be hit")
			must.Equal(workspaceWriteGuardCommand(containerWorkspace, "/workspace/"+tc.target), capture.last())
		})
	}
}

// TestContainerExecutor_WriteFile_UploadsToGuardResolvedPath pins that the
// archive goes to the path the guard printed, not the requested one.
func TestContainerExecutor_WriteFile_UploadsToGuardResolvedPath(t *testing.T) {
	must := require.New(t)
	uploads := &archiveUploads{}
	exec, cleanup := newMockContainerExecutor(t, writeGuardHandlers(nil, uploads, "/workspace/docs/README.md\n", "", 0))
	defer cleanup()

	must.NoError(exec.WriteFile(context.Background(), "README.md", "hello"))
	count, dest, name := uploads.snapshot()
	must.Equal(1, count)
	must.Equal("/workspace/docs", dest)
	must.Equal("README.md", name)
}

func TestContainerExecutor_WriteFile_GuardOutputOutsideWorkspaceRefused(t *testing.T) {
	must := require.New(t)
	uploads := &archiveUploads{}
	exec, cleanup := newMockContainerExecutor(t, writeGuardHandlers(nil, uploads, "/etc/passwd\n", "", 0))
	defer cleanup()
	emitter := &mockSecurityEmitter{}
	exec.Security = emitter

	err := exec.WriteFile(context.Background(), "a.txt", "x")
	must.ErrorContains(err, "path escapes workspace")
	must.Equal(1, emitter.pathTraversalCount)
	count, _, _ := uploads.snapshot()
	must.Zero(count, "archive endpoint must not be hit")
}

func TestContainerExecutor_WriteFile_DirectoryDestinationRefused(t *testing.T) {
	must := require.New(t)
	uploads := &archiveUploads{}
	exec, cleanup := newMockContainerExecutor(t, writeGuardHandlers(nil, uploads, "", "path is a directory, not a file: src\n", workspaceWriteIsDirExit))
	defer cleanup()
	emitter := &mockSecurityEmitter{}
	exec.Security = emitter

	err := exec.WriteFile(context.Background(), "src", "x")
	must.ErrorContains(err, "is a directory")
	must.Zero(emitter.pathTraversalCount)
	count, _, _ := uploads.snapshot()
	must.Zero(count, "archive endpoint must not be hit")
}

func TestContainerExecutor_WriteFile_RejectsWorkspaceRootAndControlCharacters(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{name: "empty path", path: "", want: "workspace root"},
		{name: "dot", path: ".", want: "workspace root"},
		{name: "absolute workspace root", path: "/workspace", want: "workspace root"},
		{name: "newline in a directory name", path: "d\n/x.txt", want: "control character"},
		{name: "carriage return in a file name", path: "x\r.txt", want: "control character"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			capture := &readExecCapture{}
			exec, cleanup := newMockContainerExecutor(t, readExecHandlers(capture, nil, "", 0))
			defer cleanup()
			emitter := &mockSecurityEmitter{}
			exec.Security = emitter

			err := exec.WriteFile(context.Background(), tc.path, "x")
			must.ErrorContains(err, tc.want)
			must.Nil(capture.last(), "no exec may run")
			must.Zero(emitter.pathTraversalCount)
		})
	}
}

// guardExecCore returns a podExecCore whose first exec answers as the
// write guard with the given stdout and outcome (a non-zero guardCode is an
// exit status, a negative one a transport failure); later execs (the tar
// extraction) are recorded with their stdin.
func guardExecCore(guardStdout string, guardCode int) (*podExecCore, *[][]string, *bytes.Buffer) {
	var calls [][]string
	var extracted bytes.Buffer
	core := &podExecCore{}
	core.execFn = func(_ context.Context, command []string, stdin io.Reader, stdout, stderr io.Writer) error {
		calls = append(calls, command)
		if len(calls) > 1 {
			_, err := io.Copy(&extracted, stdin)
			return err
		}
		_, _ = io.WriteString(stdout, guardStdout)
		switch {
		case guardCode < 0:
			return errors.New("stream reset")
		case guardCode > 0:
			_, _ = io.WriteString(stderr, "guard refused\n")
			return utilexec.CodeExitError{Err: errors.New("command terminated with non-zero exit code"), Code: guardCode}
		}
		return nil
	}
	return core, &calls, &extracted
}

func TestPodExecCore_WriteFile_GuardRefusalStopsTheWrite(t *testing.T) {
	for _, tc := range []struct {
		name           string
		target         string
		guardStdout    string
		guardCode      int
		want           string
		wantTraversals int
	}{
		{name: "symlinked destination", target: "link", guardCode: workspaceWriteEscapeExit, want: "path escapes workspace", wantTraversals: 1},
		{name: "symlinked intermediate directory", target: "escape/passwd", guardCode: workspaceWriteEscapeExit, want: "path escapes workspace", wantTraversals: 1},
		{name: "guard output outside the workspace", target: "a.txt", guardStdout: "/etc/passwd\n", want: "path escapes workspace", wantTraversals: 1},
		{name: "directory destination", target: "src", guardCode: workspaceWriteIsDirExit, want: "is a directory"},
		{name: "image without readlink", target: "a.txt", guardCode: workspaceWriteNoReadlinkExit, want: "lacks readlink"},
		{name: "mkdir failure", target: "f/a.txt", guardCode: 1, want: "guard refused"},
		{name: "transport failure", target: "a.txt", guardCode: -1, want: "stream reset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			core, calls, _ := guardExecCore(tc.guardStdout, tc.guardCode)
			emitter := &mockSecurityEmitter{}
			core.Security = emitter

			err := core.WriteFile(context.Background(), tc.target, "payload")
			must.ErrorContains(err, tc.want)
			must.Equal(tc.wantTraversals, emitter.pathTraversalCount, "PathTraversalBlocked emissions")
			must.Len(*calls, 1, "tar must not run after a refused guard")
			must.Equal(workspaceWriteGuardCommand(k8sWorkspace, k8sWorkspace+"/"+tc.target), (*calls)[0])
		})
	}
}

func TestPodExecCore_WriteFile_RejectsControlCharacters(t *testing.T) {
	must := require.New(t)
	core, calls, _ := guardExecCore("", 0)
	err := core.WriteFile(context.Background(), "d\n/x.txt", "payload")
	must.ErrorContains(err, "control character")
	must.Empty(*calls, "no exec may run")
}

func TestPodExecCore_WriteFile_ExtractsIntoGuardResolvedPath(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	core, calls, extracted := guardExecCore(k8sWorkspace+"/docs/README.md\n", 0)

	must.NoError(core.WriteFile(context.Background(), "README.md", "hello"))
	must.Len(*calls, 2)
	check.Equal([]string{"tar", "-C", k8sWorkspace + "/docs", "-xf", "-"}, (*calls)[1])
	hdr, err := tar.NewReader(extracted).Next()
	must.NoError(err)
	check.Equal("README.md", hdr.Name)
}
