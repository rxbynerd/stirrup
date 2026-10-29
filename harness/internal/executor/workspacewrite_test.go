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
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	utilexec "k8s.io/client-go/util/exec"
)

// runWriteGuard runs the write guard on the host with workspace as $1 and
// returns its stdout, stderr, and exit status.
func runWriteGuard(t *testing.T, env []string, workspace, target string) (stdout, stderr string, code int) {
	t.Helper()
	must := require.New(t)
	argv := workspaceWriteGuardCommand(workspace, target)
	cmd := osexec.Command(argv[0], argv[1:]...)
	cmd.Env = env
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			stdout, stderr, code := runWriteGuard(t, env, ws, filepath.Join(ws, tc.target))
			must.Zero(code, "guard stderr: %s", stderr)
			must.Equal(filepath.Join(ws, tc.want)+"\n", stdout)
		})
	}
	must.DirExists(filepath.Join(ws, "a", "b"), "guard creates missing parents")

	for _, tc := range []struct {
		name   string
		target string
	}{
		{name: "symlinked destination", target: "link"},
		{name: "dangling symlinked destination", target: "dangling"},
		{name: "symlinked intermediate directory", target: "escape/new.txt"},
		{name: "missing directories under a symlinked directory", target: "escape/made/deep/new.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			stdout, stderr, code := runWriteGuard(t, env, ws, filepath.Join(ws, tc.target))
			must.Equal(workspaceWriteEscapeExit, code, "guard stderr: %s", stderr)
			must.Empty(stdout)
			must.Contains(stderr, "path escapes workspace")
		})
	}
	must.NoDirExists(filepath.Join(outside, "made"), "mkdir must not run through a symlinked directory")
	entries, err := os.ReadDir(outside)
	must.NoError(err)
	must.Len(entries, 1, "only the seeded file may exist outside the workspace")

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
		stdout string
		want   string
	}{
		{stdout: "/workspace/a.txt\n", want: "/workspace/a.txt"},
		{stdout: "/workspace/sub/line\nbreak\n", want: "/workspace/sub/line\nbreak"},
		{stdout: ""},
		{stdout: "/workspace\n"},
		{stdout: "/workspace/\n"},
		{stdout: "/workspacefoo/a\n"},
		{stdout: "/workspace/../etc/passwd\n"},
		{stdout: "/etc/passwd\n"},
	} {
		t.Run(tc.stdout, func(t *testing.T) {
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

// writeGuardHandlers serves the write guard over the mock exec API and
// records the archive upload, if any.
func writeGuardHandlers(capture *readExecCapture, stdout, stderr string, exitCode int, uploads *atomic.Int32, uploadDest *string, uploadName *string) map[string]http.HandlerFunc {
	handlers := readExecHandlers(capture, []byte(stdout), stderr, exitCode)
	handlers["PUT /containers/*/archive"] = func(w http.ResponseWriter, r *http.Request) {
		uploads.Add(1)
		*uploadDest = r.URL.Query().Get("path")
		if hdr, err := tar.NewReader(r.Body).Next(); err == nil {
			*uploadName = hdr.Name
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
			var uploads atomic.Int32
			var dest, name string
			exec, cleanup := newMockContainerExecutor(t, writeGuardHandlers(capture, "", "path escapes workspace: /workspace/"+tc.target+"\n", workspaceWriteEscapeExit, &uploads, &dest, &name))
			defer cleanup()
			emitter := &mockSecurityEmitter{}
			exec.Security = emitter

			err := exec.WriteFile(context.Background(), tc.target, "payload")
			must.ErrorContains(err, "path escapes workspace")
			must.Equal(1, emitter.pathTraversalCount, "PathTraversalBlocked emissions")
			must.Zero(uploads.Load(), "archive endpoint must not be hit")
			must.Equal(workspaceWriteGuardCommand(containerWorkspace, "/workspace/"+tc.target), capture.last())
		})
	}
}

// TestContainerExecutor_WriteFile_UploadsToGuardResolvedPath pins that the
// archive goes to the path the guard printed, not the requested one.
func TestContainerExecutor_WriteFile_UploadsToGuardResolvedPath(t *testing.T) {
	must := require.New(t)
	var uploads atomic.Int32
	var dest, name string
	exec, cleanup := newMockContainerExecutor(t, writeGuardHandlers(nil, "/workspace/docs/README.md\n", "", 0, &uploads, &dest, &name))
	defer cleanup()

	must.NoError(exec.WriteFile(context.Background(), "README.md", "hello"))
	must.Equal(int32(1), uploads.Load())
	must.Equal("/workspace/docs", dest)
	must.Equal("README.md", name)
}

func TestContainerExecutor_WriteFile_GuardOutputOutsideWorkspaceRefused(t *testing.T) {
	must := require.New(t)
	var uploads atomic.Int32
	var dest, name string
	exec, cleanup := newMockContainerExecutor(t, writeGuardHandlers(nil, "/etc/passwd\n", "", 0, &uploads, &dest, &name))
	defer cleanup()
	emitter := &mockSecurityEmitter{}
	exec.Security = emitter

	err := exec.WriteFile(context.Background(), "a.txt", "x")
	must.ErrorContains(err, "path escapes workspace")
	must.Equal(1, emitter.pathTraversalCount)
	must.Zero(uploads.Load(), "archive endpoint must not be hit")
}

func TestContainerExecutor_WriteFile_RejectsWorkspaceRoot(t *testing.T) {
	for _, p := range []string{"", ".", "/workspace"} {
		t.Run(p, func(t *testing.T) {
			must := require.New(t)
			capture := &readExecCapture{}
			exec, cleanup := newMockContainerExecutor(t, readExecHandlers(capture, nil, "", 0))
			defer cleanup()
			emitter := &mockSecurityEmitter{}
			exec.Security = emitter

			err := exec.WriteFile(context.Background(), p, "x")
			must.ErrorContains(err, "workspace root")
			must.Nil(capture.last(), "no exec may run")
			must.Zero(emitter.pathTraversalCount)
		})
	}
}

// guardExecCore returns a podExecCore whose first exec answers as the
// write guard with the given stdout and exit status; later execs (the tar
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
		if guardCode != 0 {
			_, _ = io.WriteString(stderr, "path escapes workspace\n")
			return utilexec.CodeExitError{Err: errors.New("command terminated with non-zero exit code"), Code: guardCode}
		}
		return nil
	}
	return core, &calls, &extracted
}

func TestPodExecCore_WriteFile_SymlinkEscapeRefused(t *testing.T) {
	for _, target := range []string{"link", "escape/passwd"} {
		t.Run(target, func(t *testing.T) {
			must := require.New(t)
			core, calls, _ := guardExecCore("", workspaceWriteEscapeExit)
			emitter := &mockSecurityEmitter{}
			core.Security = emitter

			err := core.WriteFile(context.Background(), target, "payload")
			must.ErrorContains(err, "path escapes workspace")
			must.Equal(1, emitter.pathTraversalCount, "PathTraversalBlocked emissions")
			must.Len(*calls, 1, "tar must not run after a refused guard")
			must.Equal(workspaceWriteGuardCommand(k8sWorkspace, k8sWorkspace+"/"+target), (*calls)[0])
		})
	}
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
