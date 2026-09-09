package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// singleFileTar builds the archive a workspace read streams back for one
// regular file.
func singleFileTar(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(content)), Mode: 0o644}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatalf("write tar content: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
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

// TestWorkspaceReadCommand_Shape pins the properties the read guard rests
// on: the real path is resolved before tar runs, an escape and a missing
// readlink each exit with their reserved status, tar receives the resolved
// path, and the workspace root and requested path travel as arguments
// rather than being interpolated.
func TestWorkspaceReadCommand_Shape(t *testing.T) {
	cmd := workspaceReadCommand("/workspace", "/workspace/notes.md")
	if len(cmd) != 6 || cmd[0] != "sh" || cmd[1] != "-c" || cmd[3] != "sh" || cmd[4] != "/workspace" || cmd[5] != "/workspace/notes.md" {
		t.Fatalf("unexpected argv shape: %q", cmd)
	}
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
		if !strings.Contains(script, want) {
			t.Errorf("read script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "/workspace/notes.md") {
		t.Error("the requested path must reach the script as an argument, not by interpolation")
	}
}

func TestClassifyWorkspaceReadExit(t *testing.T) {
	emitter := &mockSecurityEmitter{}

	err := classifyWorkspaceReadExit(workspaceReadEscapeExit, "path escapes workspace: link\n", "link", "/workspace", emitter)
	if err == nil || !strings.Contains(err.Error(), "escapes workspace") {
		t.Errorf("escape exit: got %v, want a workspace-escape error", err)
	}
	if emitter.pathTraversalCount != 1 {
		t.Errorf("escape exit: PathTraversalBlocked emitted %d times, want 1", emitter.pathTraversalCount)
	}

	err = classifyWorkspaceReadExit(workspaceReadNoReadlinkExit, "readlink is required for workspace reads\n", "a.txt", "/workspace", emitter)
	if err == nil || !strings.Contains(err.Error(), "readlink") {
		t.Errorf("missing readlink: got %v, want an error naming readlink", err)
	}

	err = classifyWorkspaceReadExit(2, "tar: workspace/missing.txt: No such file or directory\n", "missing.txt", "/workspace", emitter)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("tar not-found: got %v, want fs.ErrNotExist", err)
	}
	if emitter.pathTraversalCount != 1 {
		t.Errorf("non-escape exits must not emit PathTraversalBlocked, count now %d", emitter.pathTraversalCount)
	}
}

func TestDecodeSingleFileArchive(t *testing.T) {
	content, size, err := decodeSingleFileArchive(singleFileTar(t, "workspace/a.txt", "hello"), 1024)
	if err != nil || content != "hello" || size != 5 {
		t.Errorf("regular file: got (%q, %d, %v)", content, size, err)
	}

	if _, _, err := decodeSingleFileArchive(nil, 1024); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("empty archive: got %v, want fs.ErrNotExist", err)
	}

	var dir bytes.Buffer
	tw := tar.NewWriter(&dir)
	_ = tw.WriteHeader(&tar.Header{Name: "workspace/sub/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.Close()
	if _, _, err := decodeSingleFileArchive(dir.Bytes(), 1024); !errors.Is(err, errArchiveEntryIsDir) {
		t.Errorf("directory: got %v, want errArchiveEntryIsDir", err)
	}

	var link bytes.Buffer
	tw = tar.NewWriter(&link)
	_ = tw.WriteHeader(&tar.Header{Name: "workspace/dangling", Typeflag: tar.TypeSymlink, Linkname: "gone", Mode: 0o777})
	_ = tw.Close()
	if _, _, err := decodeSingleFileArchive(link.Bytes(), 1024); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("dangling symlink: got %v, want fs.ErrNotExist", err)
	}

	_, size, err = decodeSingleFileArchive(singleFileTar(t, "workspace/big", strings.Repeat("x", 2048)), 1024)
	if !errors.Is(err, errArchiveEntryTooLarge) || size != 2048 {
		t.Errorf("oversized: got (size %d, %v), want errArchiveEntryTooLarge with size 2048", size, err)
	}
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
	if err == nil || !strings.Contains(err.Error(), "escapes workspace") {
		t.Fatalf("ReadFile(link) = (%q, %v), want a workspace-escape error", content, err)
	}
	if content != "" {
		t.Errorf("content must be empty on an escape, got %q", content)
	}
	if emitter.pathTraversalCount != 1 {
		t.Errorf("PathTraversalBlocked emitted %d times, want 1", emitter.pathTraversalCount)
	}
	argv := capture.last()
	if len(argv) != 6 || argv[4] != containerWorkspace || argv[5] != "/workspace/link" {
		t.Errorf("read ran with argv %q, want the guarded read command for /workspace/link", argv)
	}
}

// TestContainerExecutor_ReadFile_UsesExecNotArchive pins that a container
// read never touches the engine's archive endpoint, which dereferences
// symlinks.
func TestContainerExecutor_ReadFile_UsesExecNotArchive(t *testing.T) {
	handlers := readExecHandlers(nil, singleFileTar(t, "workspace/a.txt", "via exec"), "", 0)
	archiveHits := 0
	handlers["GET /containers/*/archive"] = func(w http.ResponseWriter, r *http.Request) {
		archiveHits++
		http.Error(w, "archive reads are not permitted", http.StatusForbidden)
	}
	exec, cleanup := newMockContainerExecutor(t, handlers)
	defer cleanup()

	got, err := exec.ReadFile(context.Background(), "a.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got != "via exec" {
		t.Errorf("got %q, want %q", got, "via exec")
	}
	if archiveHits != 0 {
		t.Errorf("archive endpoint hit %d times, want 0", archiveHits)
	}
}
