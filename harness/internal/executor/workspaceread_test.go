package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// singleFileTar builds the archive a workspace read streams back for one
// regular file.
func singleFileTar(t *testing.T, name, content string) []byte {
	t.Helper()
	return tarWithEntry(t, &tar.Header{Name: name, Size: int64(len(content)), Mode: 0o644}, []byte(content))
}

// tarWithEntry returns an archive holding one entry with the given header
// and body.
func tarWithEntry(t *testing.T, hdr *tar.Header, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(hdr), "write tar header")
	if len(body) > 0 {
		_, err := tw.Write(body)
		require.NoError(t, err, "write tar content")
	}
	require.NoError(t, tw.Close(), "close tar")
	return buf.Bytes()
}

// readExecCapture records the argv of each exec a mock engine served.
type readExecCapture struct {
	mu   sync.Mutex
	argv [][]string
}

func (c *readExecCapture) last() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.argv) == 0 {
		return nil
	}
	return c.argv[len(c.argv)-1]
}

// readExecHandlers serves a workspace read over the mock exec API: stdout
// carries the archive, stderr the message, and inspect reports exitCode.
func readExecHandlers(capture *readExecCapture, stdout []byte, stderr string, exitCode int) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"POST /containers/*/exec": func(w http.ResponseWriter, r *http.Request) {
			var req execCreateRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if capture != nil {
				capture.mu.Lock()
				capture.argv = append(capture.argv, req.Cmd)
				capture.mu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execCreateResponse{ID: "exec-read"})
		},
		"POST /exec/*/start": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
			if len(stdout) > 0 {
				writeDockerFrame(w, 1, stdout)
			}
			if stderr != "" {
				writeDockerFrame(w, 2, []byte(stderr))
			}
		},
		"GET /exec/*/json": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(execInspectResponse{ExitCode: exitCode})
		},
	}
}

// TestWorkspaceReadCommand_GuardScriptAndArguments pins the properties the
// read guard rests on: the real path is resolved before tar runs, an escape
// and a missing readlink each exit with their reserved status, tar receives
// the resolved path, and the workspace root and requested path travel as
// arguments rather than being interpolated.
func TestWorkspaceReadCommand_GuardScriptAndArguments(t *testing.T) {
	cmd := workspaceReadCommand("/workspace", "/workspace/notes.md")
	require.Len(t, cmd, 6)
	require.Equal(t, []string{"sh", "-c"}, cmd[:2])
	require.Equal(t, []string{"sh", "/workspace", "/workspace/notes.md"}, cmd[3:])

	script := cmd[2]
	for _, want := range []string{
		"command -v readlink",
		"exit 112",
		`readlink -f -- "$2"`,
		`"$1"/*) ;;`,
		"path escapes workspace",
		"exit 113",
		`exec tar -C / -cf - -- "${t#/}"`,
	} {
		assert.Contains(t, script, want)
	}
	require.NotContains(t, script, "/workspace/notes.md",
		"the requested path must reach the script as an argument, not by interpolation")
}

// TestClassifyWorkspaceReadExit_EscapeReadlinkAndNotFound shares one
// emitter across the calls: only the escape exit may emit PathTraversalBlocked.
func TestClassifyWorkspaceReadExit_EscapeReadlinkAndNotFound(t *testing.T) {
	emitter := &mockSecurityEmitter{}

	err := classifyWorkspaceReadExit(workspaceReadEscapeExit, "path escapes workspace: link\n", "link", "/workspace", emitter)
	require.ErrorContains(t, err, "escapes workspace")
	require.Equal(t, 1, emitter.pathTraversalCount, "escape exit must emit PathTraversalBlocked")

	err = classifyWorkspaceReadExit(workspaceReadNoReadlinkExit, "readlink is required for workspace reads\n", "a.txt", "/workspace", emitter)
	require.ErrorContains(t, err, "readlink")

	err = classifyWorkspaceReadExit(2, "tar: workspace/missing.txt: No such file or directory\n", "missing.txt", "/workspace", emitter)
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.Equal(t, 1, emitter.pathTraversalCount, "non-escape exits must not emit PathTraversalBlocked")
}

func TestDecodeSingleFileArchive_ClassifiesEntries(t *testing.T) {
	t.Run("regular file returns its content and size", func(t *testing.T) {
		content, size, err := decodeSingleFileArchive(singleFileTar(t, "workspace/a.txt", "hello"), 1024)
		require.NoError(t, err)
		require.Equal(t, "hello", content)
		require.Equal(t, int64(5), size)
	})

	t.Run("empty archive is not found", func(t *testing.T) {
		_, _, err := decodeSingleFileArchive(nil, 1024)
		require.ErrorIs(t, err, fs.ErrNotExist)
	})

	t.Run("directory entry is rejected", func(t *testing.T) {
		archive := tarWithEntry(t, &tar.Header{Name: "workspace/sub/", Typeflag: tar.TypeDir, Mode: 0o755}, nil)
		_, _, err := decodeSingleFileArchive(archive, 1024)
		require.ErrorIs(t, err, errArchiveEntryIsDir)
	})

	t.Run("dangling symlink is not found", func(t *testing.T) {
		archive := tarWithEntry(t, &tar.Header{Name: "workspace/dangling", Typeflag: tar.TypeSymlink, Linkname: "gone", Mode: 0o777}, nil)
		_, _, err := decodeSingleFileArchive(archive, 1024)
		require.ErrorIs(t, err, fs.ErrNotExist)
	})

	t.Run("entry over the limit reports its size", func(t *testing.T) {
		_, size, err := decodeSingleFileArchive(singleFileTar(t, "workspace/big", strings.Repeat("x", 2048)), 1024)
		require.ErrorIs(t, err, errArchiveEntryTooLarge)
		require.Equal(t, int64(2048), size)
	})
}

// TestContainerExecutor_ReadFile_EscapeReportedAndBlocked drives a read
// whose in-sandbox resolution left the workspace: the guard's exit status
// must surface as a workspace-escape error, emit PathTraversalBlocked, and
// return no content.
func TestContainerExecutor_ReadFile_EscapeReportedAndBlocked(t *testing.T) {
	capture := &readExecCapture{}
	exec, cleanup := newMockContainerExecutor(t, readExecHandlers(capture, nil, "path escapes workspace: /workspace/link\n", workspaceReadEscapeExit))
	defer cleanup()
	emitter := &mockSecurityEmitter{}
	exec.Security = emitter

	content, err := exec.ReadFile(context.Background(), "link")
	require.ErrorContains(t, err, "escapes workspace", "ReadFile(link) returned content %q", content)
	require.Empty(t, content, "content must be empty on an escape")
	require.Equal(t, 1, emitter.pathTraversalCount, "PathTraversalBlocked emissions")

	argv := capture.last()
	require.Len(t, argv, 6, "read ran with argv %q", argv)
	require.Equal(t, []string{containerWorkspace, "/workspace/link"}, argv[4:], "guarded read targets /workspace/link")
}

// TestContainerExecutor_ReadFile_UsesExecNotArchive pins that a container
// read never touches the engine's archive endpoint, which dereferences
// symlinks.
func TestContainerExecutor_ReadFile_UsesExecNotArchive(t *testing.T) {
	handlers := readExecHandlers(nil, singleFileTar(t, "workspace/a.txt", "via exec"), "", 0)
	var archiveHits atomic.Int32
	handlers["GET /containers/*/archive"] = func(w http.ResponseWriter, r *http.Request) {
		archiveHits.Add(1)
		http.Error(w, "archive reads are not permitted", http.StatusForbidden)
	}
	exec, cleanup := newMockContainerExecutor(t, handlers)
	defer cleanup()

	got, err := exec.ReadFile(context.Background(), "a.txt")
	require.NoError(t, err)
	require.Equal(t, "via exec", got)
	require.Zero(t, archiveHits.Load(), "archive endpoint must not be hit")
}
