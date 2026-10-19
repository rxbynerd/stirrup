package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// TestWorkspaceArchiver_ImplementorsMatchTypesTable pins types.ExecutorCanArchiveWorkspace,
// which gates executor.workspaceExportTo at config load, against the
// executors that actually implement WorkspaceArchiver.
func TestWorkspaceArchiver_ImplementorsMatchTypesTable(t *testing.T) {
	byType := map[string]Executor{
		"":            &LocalExecutor{},
		"local":       &LocalExecutor{},
		"container":   &ContainerExecutor{},
		"k8s":         &K8sExecutor{},
		"k8s-sandbox": &AgentSandboxExecutor{},
		"api":         &APIExecutor{},
		"none":        &NoneExecutor{},
	}
	for _, typ := range types.ValidExecutorTypeValues() {
		t.Run(fmt.Sprintf("%q", typ), func(t *testing.T) {
			require.Contains(t, byType, typ, "executor type missing from this parity table")
		})
	}
	for typ, exec := range byType {
		t.Run(fmt.Sprintf("%q", typ), func(t *testing.T) {
			_, implements := exec.(WorkspaceArchiver)
			require.Equal(t, implements, types.ExecutorCanArchiveWorkspace(typ),
				"ExecutorCanArchiveWorkspace should match the WorkspaceArchiver implementation")
		})
	}
	_, isArchiver := Executor(&GitLabExecutor{}).(WorkspaceArchiver)
	require.False(t, isArchiver, "GitLabExecutor (an api backend) must not implement WorkspaceArchiver")
}

type tarEntry struct {
	name     string
	typeflag byte
	linkname string
	body     string
	mode     int64
}

func readTarEntries(t *testing.T, data []byte) []tarEntry {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	var out []tarEntry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		require.NoError(t, err, "reading tar header")
		body, err := io.ReadAll(tr)
		require.NoError(t, err, "reading body of %s", h.Name)
		out = append(out, tarEntry{name: h.Name, typeflag: h.Typeflag, linkname: h.Linkname, body: string(body), mode: h.Mode})
	}
}

func newArchiveTestLocal(t *testing.T) (*LocalExecutor, string) {
	t.Helper()
	dir := t.TempDir()
	exec, err := NewLocalExecutor(dir)
	require.NoError(t, err)
	return exec, exec.HostWorkspaceRoot()
}

func TestLocalExecutor_ArchiveWorkspace_IncludesFilesDirsAndSymlinks(t *testing.T) {
	exec, root := newArchiveTestLocal(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "sub", "a.txt"), []byte("alpha"), 0o640))
	require.NoError(t, os.WriteFile(filepath.Join(root, "..cache"), []byte("dots"), 0o644))
	require.NoError(t, os.Symlink("sub/a.txt", filepath.Join(root, "link")))

	var buf bytes.Buffer
	stats, err := exec.ArchiveWorkspace(context.Background(), &buf, 1<<20)
	require.NoError(t, err)
	require.Equal(t, int64(buf.Len()), stats.Bytes, "stats.Bytes should equal the archive length")

	got := map[string]tarEntry{}
	for _, e := range readTarEntries(t, buf.Bytes()) {
		got[e.name] = e
	}
	t.Run("regular file keeps body and mode", func(t *testing.T) {
		e := got["sub/a.txt"]
		require.Equal(t, byte(tar.TypeReg), e.typeflag)
		require.Equal(t, "alpha", e.body)
		require.Equal(t, int64(0o640), e.mode)
	})
	t.Run("directory entry", func(t *testing.T) {
		require.Equal(t, byte(tar.TypeDir), got["sub/"].typeflag)
	})
	t.Run("leading-dot name is not an escape", func(t *testing.T) {
		require.Equal(t, byte(tar.TypeReg), got["..cache"].typeflag)
	})
	t.Run("symlink is not followed", func(t *testing.T) {
		e := got["link"]
		require.Equal(t, byte(tar.TypeSymlink), e.typeflag)
		require.Equal(t, "sub/a.txt", e.linkname)
	})
}

func TestLocalExecutor_ArchiveWorkspace_RefusesEscapingSymlinks(t *testing.T) {
	cases := map[string]func(t *testing.T, root string){
		"outside target": func(t *testing.T, root string) {
			require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(root, "escape")))
		},
		"dangling": func(t *testing.T, root string) {
			require.NoError(t, os.Symlink(filepath.Join(root, "missing", "secret"), filepath.Join(root, "dangling")))
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			exec, root := newArchiveTestLocal(t)
			seed(t, root)
			_, err := exec.ArchiveWorkspace(context.Background(), io.Discard, 1<<20)
			require.ErrorContains(t, err, "refusing path")
		})
	}
}

func TestLocalExecutor_ArchiveWorkspace_ReportsMissingRoot(t *testing.T) {
	exec, root := newArchiveTestLocal(t)
	require.NoError(t, os.Remove(root))
	_, err := exec.ArchiveWorkspace(context.Background(), io.Discard, 1<<20)
	require.ErrorIs(t, err, ErrWorkspaceRootMissing)
}

func TestLocalExecutor_ArchiveWorkspace_StopsAtByteCap(t *testing.T) {
	exec, root := newArchiveTestLocal(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "big"), bytes.Repeat([]byte("x"), 64<<10), 0o644))
	var buf bytes.Buffer
	_, err := exec.ArchiveWorkspace(context.Background(), &buf, 16<<10)
	require.ErrorIs(t, err, ErrArchiveTooLarge)
	require.LessOrEqual(t, buf.Len(), 16<<10, "bytes written past the cap")
}

func TestLocalExecutor_ArchiveWorkspace_StopsWhenContextCancelled(t *testing.T) {
	exec, root := newArchiveTestLocal(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o644))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := exec.ArchiveWorkspace(ctx, io.Discard, 1<<20)
	require.ErrorIs(t, err, context.Canceled)
}

func TestArchiveViaExec_MapsExecOutcomes(t *testing.T) {
	transport := errors.New("stream reset")
	cases := []struct {
		name    string
		code    int
		runErr  error
		stdout  string
		stderr  string
		max     int64
		wantIs  error
		wantSub string
	}{
		{name: "clean", stdout: "tarbytes", max: 64},
		{name: "missing root", code: workspaceRootMissingExit, max: 64, wantIs: ErrWorkspaceRootMissing},
		{name: "tar failure", code: 2, stderr: "tar: ./x: Permission denied\n", max: 64, wantSub: "status 2: tar: ./x: Permission denied"},
		{name: "transport error", runErr: transport, max: 64, wantIs: transport},
		{name: "over cap", stdout: strings.Repeat("y", 65), max: 64, wantIs: ErrArchiveTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			stats, err := archiveViaExec("/workspace", &out, tc.max, func(stdout, stderr io.Writer) (int, error) {
				if _, err := io.WriteString(stdout, tc.stdout); err != nil {
					return -1, err
				}
				_, _ = io.WriteString(stderr, tc.stderr)
				return tc.code, tc.runErr
			})
			switch {
			case tc.wantIs != nil:
				require.ErrorIs(t, err, tc.wantIs)
			case tc.wantSub != "":
				require.ErrorContains(t, err, tc.wantSub)
			default:
				require.NoError(t, err)
				require.Equal(t, tc.stdout, out.String(), "forwarded bytes")
				require.Equal(t, int64(len(tc.stdout)), stats.Bytes, "stats.Bytes")
			}
		})
	}
}

func TestWorkspaceArchiveArgv_PassesRootAsParameter(t *testing.T) {
	argv := workspaceArchiveArgv("/bin/sh", "/work space")
	require.Equal(t, "/bin/sh", argv[0])
	require.Equal(t, "-c", argv[1])
	require.Equal(t, "/work space", argv[len(argv)-1], "argv = %q: root should be the final positional parameter", argv)
	require.NotContains(t, argv[2], "/work space", "script should not interpolate the root")
}

// TestContainerExecutor_ArchiveWorkspace_RunsTarInWorkspace drives the exec-API archive path
// against the mock engine: the tar command runs in the workspace and its
// stdout reaches the caller verbatim, and the missing-root status maps to
// ErrWorkspaceRootMissing.
func TestContainerExecutor_ArchiveWorkspace_RunsTarInWorkspace(t *testing.T) {
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	_ = tw.WriteHeader(&tar.Header{Name: "./sandbox.txt", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("ok"))
	_ = tw.Close()

	for _, tc := range []struct {
		name   string
		exit   int
		wantIs error
	}{
		{name: "success"},
		{name: "missing root", exit: workspaceRootMissingExit, wantIs: ErrWorkspaceRootMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotReq execCreateRequest
			exec, cleanup := newMockContainerExecutor(t, map[string]http.HandlerFunc{
				"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
					_ = json.NewDecoder(r.Body).Decode(&gotReq)
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(execCreateResponse{ID: "exec-archive"})
				},
				"POST /exec/*/start": func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
					if tc.exit == 0 {
						writeDockerFrame(w, 1, archive.Bytes())
					}
				},
				"GET /exec/*/json": func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(execInspectResponse{ExitCode: tc.exit})
				},
			})
			defer cleanup()

			var out bytes.Buffer
			_, err := exec.ArchiveWorkspace(context.Background(), &out, 1<<20)
			require.Equal(t, workspaceArchiveArgv("sh", containerWorkspace), gotReq.Cmd, "exec command")
			require.Equal(t, containerWorkspace, gotReq.WorkingDir, "exec working directory")
			if tc.wantIs != nil {
				require.ErrorIs(t, err, tc.wantIs)
				return
			}
			require.NoError(t, err)
			require.Equal(t, archive.Bytes(), out.Bytes(), "archive bytes should match the exec stdout")
		})
	}
}
