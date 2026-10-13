package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// TestWorkspaceArchiverMatchesTypesTable pins types.ExecutorCanArchiveWorkspace,
// which gates executor.workspaceExportTo at config load, against the
// executors that actually implement WorkspaceArchiver.
func TestWorkspaceArchiverMatchesTypesTable(t *testing.T) {
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
		if _, ok := byType[typ]; !ok {
			t.Errorf("executor type %q missing from this parity table", typ)
		}
	}
	for typ, exec := range byType {
		_, implements := exec.(WorkspaceArchiver)
		if got := types.ExecutorCanArchiveWorkspace(typ); got != implements {
			t.Errorf("types.ExecutorCanArchiveWorkspace(%q) = %v, but WorkspaceArchiver implemented = %v", typ, got, implements)
		}
	}
	if _, ok := Executor(&GitLabExecutor{}).(WorkspaceArchiver); ok {
		t.Error("GitLabExecutor (an api backend) must not implement WorkspaceArchiver")
	}
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
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", h.Name, err)
		}
		out = append(out, tarEntry{name: h.Name, typeflag: h.Typeflag, linkname: h.Linkname, body: string(body), mode: h.Mode})
	}
}

func newArchiveTestLocal(t *testing.T) (*LocalExecutor, string) {
	t.Helper()
	dir := t.TempDir()
	exec, err := NewLocalExecutor(dir)
	if err != nil {
		t.Fatalf("NewLocalExecutor: %v", err)
	}
	return exec, exec.HostWorkspaceRoot()
}

func TestLocalExecutor_ArchiveWorkspace(t *testing.T) {
	exec, root := newArchiveTestLocal(t)
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "a.txt"), []byte("alpha"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "..cache"), []byte("dots"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub/a.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	stats, err := exec.ArchiveWorkspace(context.Background(), &buf, 1<<20)
	if err != nil {
		t.Fatalf("ArchiveWorkspace: %v", err)
	}
	if stats.Bytes != int64(buf.Len()) {
		t.Errorf("stats.Bytes = %d, want %d", stats.Bytes, buf.Len())
	}
	got := map[string]tarEntry{}
	for _, e := range readTarEntries(t, buf.Bytes()) {
		got[e.name] = e
	}
	if e := got["sub/a.txt"]; e.typeflag != tar.TypeReg || e.body != "alpha" || e.mode != 0o640 {
		t.Errorf("sub/a.txt entry = %+v, want a 0640 regular file with body alpha", e)
	}
	if e := got["sub/"]; e.typeflag != tar.TypeDir {
		t.Errorf("sub/ entry = %+v, want a directory", e)
	}
	if e := got["..cache"]; e.typeflag != tar.TypeReg {
		t.Errorf("..cache entry = %+v, want a regular file (a leading-dot name is not an escape)", e)
	}
	if e := got["link"]; e.typeflag != tar.TypeSymlink || e.linkname != "sub/a.txt" {
		t.Errorf("link entry = %+v, want an unfollowed symlink to sub/a.txt", e)
	}
}

func TestLocalExecutor_ArchiveWorkspace_RefusesEscapingSymlinks(t *testing.T) {
	cases := map[string]func(t *testing.T, root string){
		"outside target": func(t *testing.T, root string) {
			if err := os.Symlink(t.TempDir(), filepath.Join(root, "escape")); err != nil {
				t.Fatal(err)
			}
		},
		"dangling": func(t *testing.T, root string) {
			if err := os.Symlink(filepath.Join(root, "missing", "secret"), filepath.Join(root, "dangling")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			exec, root := newArchiveTestLocal(t)
			seed(t, root)
			_, err := exec.ArchiveWorkspace(context.Background(), io.Discard, 1<<20)
			if err == nil || !strings.Contains(err.Error(), "refusing path") {
				t.Fatalf("ArchiveWorkspace error = %v, want a refusal", err)
			}
		})
	}
}

func TestLocalExecutor_ArchiveWorkspace_MissingRoot(t *testing.T) {
	exec, root := newArchiveTestLocal(t)
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	_, err := exec.ArchiveWorkspace(context.Background(), io.Discard, 1<<20)
	if !errors.Is(err, ErrWorkspaceRootMissing) {
		t.Fatalf("ArchiveWorkspace error = %v, want ErrWorkspaceRootMissing", err)
	}
}

func TestLocalExecutor_ArchiveWorkspace_Cap(t *testing.T) {
	exec, root := newArchiveTestLocal(t)
	if err := os.WriteFile(filepath.Join(root, "big"), bytes.Repeat([]byte("x"), 64<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, err := exec.ArchiveWorkspace(context.Background(), &buf, 16<<10)
	if !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("ArchiveWorkspace error = %v, want ErrArchiveTooLarge", err)
	}
	if buf.Len() > 16<<10 {
		t.Errorf("wrote %d bytes past a %d-byte cap", buf.Len(), 16<<10)
	}
}

func TestLocalExecutor_ArchiveWorkspace_Cancelled(t *testing.T) {
	exec, root := newArchiveTestLocal(t)
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := exec.ArchiveWorkspace(ctx, io.Discard, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("ArchiveWorkspace error = %v, want context.Canceled", err)
	}
}

func TestArchiveViaExec(t *testing.T) {
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
				if !errors.Is(err, tc.wantIs) {
					t.Fatalf("error = %v, want %v", err, tc.wantIs)
				}
			case tc.wantSub != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantSub)
				}
			default:
				if err != nil {
					t.Fatalf("error = %v, want nil", err)
				}
				if out.String() != tc.stdout || stats.Bytes != int64(len(tc.stdout)) {
					t.Errorf("forwarded %q (stats %d), want %q", out.String(), stats.Bytes, tc.stdout)
				}
			}
		})
	}
}

func TestWorkspaceArchiveArgv_PassesRootAsParameter(t *testing.T) {
	argv := workspaceArchiveArgv("/bin/sh", "/work space")
	if argv[0] != "/bin/sh" || argv[1] != "-c" || argv[len(argv)-1] != "/work space" {
		t.Fatalf("argv = %q, want the root as the final positional parameter", argv)
	}
	if strings.Contains(argv[2], "/work space") {
		t.Errorf("script %q interpolates the root", argv[2])
	}
}

// TestContainerExecutor_ArchiveWorkspace drives the exec-API archive path
// against the mock engine: the tar command runs in the workspace and its
// stdout reaches the caller verbatim, and the missing-root status maps to
// ErrWorkspaceRootMissing.
func TestContainerExecutor_ArchiveWorkspace(t *testing.T) {
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
			if !slices.Equal(gotReq.Cmd, workspaceArchiveArgv("sh", containerWorkspace)) || gotReq.WorkingDir != containerWorkspace {
				t.Errorf("exec request = %+v, want the workspace archive command in %s", gotReq, containerWorkspace)
			}
			if tc.wantIs != nil {
				if !errors.Is(err, tc.wantIs) {
					t.Fatalf("error = %v, want %v", err, tc.wantIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("ArchiveWorkspace: %v", err)
			}
			if !bytes.Equal(out.Bytes(), archive.Bytes()) {
				t.Errorf("archive bytes differ from the exec stdout")
			}
		})
	}
}
