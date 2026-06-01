package executor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"regexp"
	"strings"
	"testing"

	utilexec "k8s.io/client-go/util/exec"

	"github.com/rxbynerd/stirrup/types"
)

// TestK8sResolvePath covers the textual workspace-escape check in
// K8sExecutor.ResolvePath. The "/workspacefoo" case is the critical
// security case — a prefix-without-separator must not match the
// workspace root.
func TestK8sResolvePath(t *testing.T) {
	exec := &K8sExecutor{}

	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{"foo.txt", "/workspace/foo.txt", false},
		{"sub/dir/file.go", "/workspace/sub/dir/file.go", false},
		{"/workspace/inside.txt", "/workspace/inside.txt", false},
		// Empty / "." / "/workspace" all resolve to the workspace root with
		// no error here. ResolvePath is the lenient gate (it permits the
		// root, which ListDirectory legitimately lists); resolveFilePath is
		// the strict one that ReadFile/WriteFile use to reject the root —
		// see TestK8sResolveFilePath_RejectsRoot.
		{"", "/workspace", false},
		{".", "/workspace", false},
		{"/workspace", "/workspace", false},
		{"../etc/passwd", "", true},
		{"foo/../../etc/passwd", "", true},
		{"/etc/passwd", "", true},
		{"/workspacefoo", "", true}, // must not match prefix without separator
	}

	for _, tt := range tests {
		got, err := exec.ResolvePath(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ResolvePath(%q): expected error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("ResolvePath(%q): unexpected error: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ResolvePath(%q): got %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestNewK8sExecutor_MissingImage asserts the early-return guard fires
// before any cluster I/O is attempted.
func TestNewK8sExecutor_MissingImage(t *testing.T) {
	_, err := NewK8sExecutor(context.Background(), K8sExecutorConfig{
		Namespace: "default",
	})
	if err == nil {
		t.Fatal("expected error for missing image")
	}
	if !strings.Contains(err.Error(), "image") {
		t.Errorf("error %q should mention 'image'", err)
	}
}

// TestNewK8sExecutor_MissingNamespace asserts the early-return guard
// fires before any cluster I/O is attempted.
func TestNewK8sExecutor_MissingNamespace(t *testing.T) {
	_, err := NewK8sExecutor(context.Background(), K8sExecutorConfig{
		Image: "busybox",
	})
	if err == nil {
		t.Fatal("expected error for missing namespace")
	}
	if !strings.Contains(err.Error(), "namespace") {
		t.Errorf("error %q should mention 'namespace'", err)
	}
}

// TestK8sCapabilities exercises the CanNetwork branch and verifies the
// other capability flags match the documented defaults (CanRead/Write/Exec
// = true per the scaffold spec; MaxTimeout = maxTimeout to match the
// container and local executors).
func TestK8sCapabilities(t *testing.T) {
	tests := []struct {
		name       string
		network    *types.NetworkConfig
		wantCanNet bool
	}{
		{"nil network", nil, false},
		{"mode none", &types.NetworkConfig{Mode: "none"}, false},
		{"mode allowlist", &types.NetworkConfig{Mode: "allowlist"}, true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			exec := &K8sExecutor{network: tt.network}
			caps := exec.Capabilities()
			if caps.CanNetwork != tt.wantCanNet {
				t.Errorf("CanNetwork: got %v, want %v", caps.CanNetwork, tt.wantCanNet)
			}
			if !caps.CanRead {
				t.Errorf("CanRead: got %v, want true", caps.CanRead)
			}
			if !caps.CanWrite {
				t.Errorf("CanWrite: got %v, want true", caps.CanWrite)
			}
			if !caps.CanExec {
				t.Errorf("CanExec: got %v, want true", caps.CanExec)
			}
			if caps.MaxTimeout != maxTimeout {
				t.Errorf("MaxTimeout: got %v, want %v", caps.MaxTimeout, maxTimeout)
			}
		})
	}
}

// TestGeneratePodName_Format asserts the documented pod-name format
// (stirrup-<12 hex chars>) and uniqueness across repeated calls. 6 random
// bytes encoded as hex yield 12 characters; format drift would be visible
// to operators.
func TestGeneratePodName_Format(t *testing.T) {
	re := regexp.MustCompile(`^stirrup-[0-9a-f]{12}$`)
	seen := make(map[string]struct{}, 10)
	for i := 0; i < 10; i++ {
		name, err := generatePodName()
		if err != nil {
			t.Fatalf("generatePodName: %v", err)
		}
		if !re.MatchString(name) {
			t.Errorf("name %q does not match ^stirrup-[0-9a-f]{12}$", name)
		}
		if _, dup := seen[name]; dup {
			t.Errorf("duplicate pod name across calls: %q", name)
		}
		seen[name] = struct{}{}
	}
}

// TestExtractExitCode is the cluster-free unit test for the pure exit-code
// helper that Exec and the file-I/O methods rely on. It covers the three
// cases that matter: a clean (nil) exit, a non-zero CodeExitError carrying a
// status, and a transport-level error that carries no exit status. The
// helper must stay robust to the value/pointer wrapping that errors.As
// performs, so both forms are exercised.
func TestExtractExitCode(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantOK   bool
	}{
		{"nil is clean exit", nil, 0, true},
		{"code exit 7", utilexec.CodeExitError{Err: errors.New("command terminated with exit code 7"), Code: 7}, 7, true},
		{"code exit 0 explicit", utilexec.CodeExitError{Err: errors.New("ok"), Code: 0}, 0, true},
		{"wrapped code exit", fmt.Errorf("stream: %w", utilexec.CodeExitError{Err: errors.New("boom"), Code: 3}), 3, true},
		{"transport error has no status", errors.New("dial tcp: connection refused"), 0, false},
		// The v1/v2 streaming protocols surface a non-zero exit as a plain
		// error string with no structured ExitError, so it reads as a
		// transport error (0, false). Documented as a known limitation on
		// extractExitCode.
		{"v1/v2 string exit has no status", errors.New("error executing remote command: command terminated with non-zero exit code"), 0, false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			code, ok := extractExitCode(tt.err)
			if ok != tt.wantOK {
				t.Fatalf("extractExitCode(%v): ok=%v, want %v", tt.err, ok, tt.wantOK)
			}
			if code != tt.wantCode {
				t.Errorf("extractExitCode(%v): code=%d, want %d", tt.err, code, tt.wantCode)
			}
		})
	}
}

// TestWriteCapBuffer verifies the 10 MB streaming cap: writes below the
// limit accumulate, a write that crosses the limit is truncated and flags
// exceeded, and post-limit writes are silently dropped (the SPDY stream
// keeps draining). The Writer always reports the full slice as consumed so
// remotecommand never errors mid-stream on a capped buffer.
func TestWriteCapBuffer(t *testing.T) {
	t.Run("under limit accumulates", func(t *testing.T) {
		w := writeCapBuffer{limit: 16}
		n, err := w.Write([]byte("hello"))
		if err != nil || n != 5 {
			t.Fatalf("Write: n=%d err=%v, want 5/nil", n, err)
		}
		if w.exceeded {
			t.Error("exceeded set below limit")
		}
		if w.String() != "hello" {
			t.Errorf("buffer = %q, want %q", w.String(), "hello")
		}
	})

	t.Run("crossing limit truncates and flags", func(t *testing.T) {
		w := writeCapBuffer{limit: 4}
		n, err := w.Write([]byte("abcdefgh"))
		if err != nil || n != 8 {
			t.Fatalf("Write: n=%d err=%v, want 8/nil (full slice claimed)", n, err)
		}
		if !w.exceeded {
			t.Error("exceeded not set after crossing limit")
		}
		if w.String() != "abcd" {
			t.Errorf("buffer = %q, want %q (truncated at limit)", w.String(), "abcd")
		}
	})

	t.Run("post-limit writes dropped", func(t *testing.T) {
		w := writeCapBuffer{limit: 4}
		_, _ = w.Write([]byte("abcd"))
		_, _ = w.Write([]byte("more"))
		n, err := w.Write([]byte("xyz"))
		if err != nil || n != 3 {
			t.Fatalf("Write after cap: n=%d err=%v, want 3/nil", n, err)
		}
		if w.String() != "abcd" {
			t.Errorf("buffer = %q, want %q (no growth after cap)", w.String(), "abcd")
		}
	})
}

// TestClassifyTarError covers the missing-file mapping that ReadFile and
// ListDirectory depend on: a tar/ls "No such file or directory" stderr must
// surface as fs.ErrNotExist so callers can branch with errors.Is. Other
// stderr is passed through.
func TestClassifyTarError(t *testing.T) {
	notExist := classifyTarError("missing.txt", "tar: missing.txt: No such file or directory")
	if !errors.Is(notExist, fs.ErrNotExist) {
		t.Errorf("missing-file stderr: got %v, want fs.ErrNotExist", notExist)
	}

	// The "not found" substring (busybox phrasing) must also map to
	// fs.ErrNotExist.
	notFound := classifyTarError("missing.txt", "ls: missing.txt: not found")
	if !errors.Is(notFound, fs.ErrNotExist) {
		t.Errorf("'not found' stderr: got %v, want fs.ErrNotExist", notFound)
	}

	other := classifyTarError("x", "tar: permission denied")
	if errors.Is(other, fs.ErrNotExist) {
		t.Errorf("non-missing stderr should not map to fs.ErrNotExist: %v", other)
	}
	if !strings.Contains(other.Error(), "permission denied") {
		t.Errorf("non-missing stderr should be surfaced verbatim: %v", other)
	}

	// Empty stderr (a non-zero exit with nothing on stderr) must still
	// produce a usable, non-nil error that names the target.
	empty := classifyTarError("ghost", "")
	if empty == nil {
		t.Fatal("empty stderr: got nil error, want a non-nil failure")
	}
	if errors.Is(empty, fs.ErrNotExist) {
		t.Errorf("empty stderr should not map to fs.ErrNotExist: %v", empty)
	}
	if !strings.Contains(empty.Error(), "ghost") {
		t.Errorf("empty-stderr error should name the target: %v", empty)
	}
}

// TestK8sResolveFilePath_RejectsRoot pins finding #1: resolveFilePath (used
// by ReadFile/WriteFile) must reject any input that resolves to the
// workspace root, so WriteFile never drives `mkdir -p /` / `tar -C /`. The
// inputs "", ".", and "/workspace" all collapse to the root.
func TestK8sResolveFilePath_RejectsRoot(t *testing.T) {
	exec := &K8sExecutor{}
	for _, in := range []string{"", ".", "/workspace", "/workspace/"} {
		if _, err := exec.resolveFilePath(in); err == nil {
			t.Errorf("resolveFilePath(%q): expected workspace-root rejection", in)
		}
	}
	// A real file path still resolves cleanly.
	got, err := exec.resolveFilePath("a.txt")
	if err != nil {
		t.Fatalf("resolveFilePath(\"a.txt\"): unexpected error: %v", err)
	}
	if got != "/workspace/a.txt" {
		t.Errorf("resolveFilePath(\"a.txt\") = %q, want /workspace/a.txt", got)
	}
}

// TestK8sWriteFile_OutputCapUnit verifies the 10 MB write cap fires on
// len(content) before any cluster I/O, so the invariant runs under the
// default `just test` (no cluster). The receiver is a zero-value executor;
// the size check returns before any clientset use.
func TestK8sWriteFile_OutputCapUnit(t *testing.T) {
	exec := &K8sExecutor{}
	big := strings.Repeat("a", int(k8sMaxOutput)+1)
	err := exec.WriteFile(context.Background(), "big.txt", big)
	if !errors.Is(err, errK8sOutputCap) {
		t.Fatalf("WriteFile oversized: err = %v, want errK8sOutputCap", err)
	}
}

// TestK8sWriteFile_OutputCapEmitsSecurityEvent verifies the oversized-write
// path reports FileSizeLimitExceeded to a wired emitter (the cluster-free
// prefix of the cap path). A nil emitter must stay silent — exercised by
// TestK8sWriteFile_OutputCapUnit above, which uses a zero-value executor.
func TestK8sWriteFile_OutputCapEmitsSecurityEvent(t *testing.T) {
	rec := &recordingSecurityEmitter{}
	exec := &K8sExecutor{Security: rec}
	big := strings.Repeat("a", int(k8sMaxOutput)+1)
	if err := exec.WriteFile(context.Background(), "big.txt", big); !errors.Is(err, errK8sOutputCap) {
		t.Fatalf("WriteFile oversized: err = %v, want errK8sOutputCap", err)
	}
	if len(rec.fileSizeEvents) != 1 {
		t.Fatalf("FileSizeLimitExceeded calls = %d, want 1", len(rec.fileSizeEvents))
	}
	ev := rec.fileSizeEvents[0]
	if ev.path != "big.txt" || ev.limit != k8sMaxOutput {
		t.Errorf("FileSizeLimitExceeded event = %+v, want path big.txt / limit %d", ev, k8sMaxOutput)
	}
	if ev.size <= k8sMaxOutput {
		t.Errorf("reported size = %d, want > cap %d", ev.size, k8sMaxOutput)
	}
}

// TestK8sResolvePath_EmitsPathTraversal verifies the escape branch reports
// PathTraversalBlocked when an emitter is wired, and stays silent for an
// in-bounds path. This is the monitoring hook the container/local executors
// already provide.
func TestK8sResolvePath_EmitsPathTraversal(t *testing.T) {
	rec := &recordingSecurityEmitter{}
	exec := &K8sExecutor{Security: rec}

	if _, err := exec.ResolvePath("../etc/passwd"); err == nil {
		t.Fatal("ResolvePath(../etc/passwd): expected escape error")
	}
	if len(rec.pathTraversalEvents) != 1 {
		t.Fatalf("PathTraversalBlocked calls = %d, want 1", len(rec.pathTraversalEvents))
	}
	if got := rec.pathTraversalEvents[0]; got.path != "../etc/passwd" || got.workspace != "/workspace" {
		t.Errorf("PathTraversalBlocked event = %+v, want path ../etc/passwd / workspace /workspace", got)
	}

	if _, err := exec.ResolvePath("ok.txt"); err != nil {
		t.Fatalf("ResolvePath(ok.txt): unexpected error: %v", err)
	}
	if len(rec.pathTraversalEvents) != 1 {
		t.Errorf("in-bounds path emitted a traversal event: %+v", rec.pathTraversalEvents)
	}
}

// TestClampInt verifies the int64->int narrowing saturates rather than
// wrapping, guarding the security-emitter sizes on 32-bit builds.
func TestClampInt(t *testing.T) {
	if got := clampInt(0); got != 0 {
		t.Errorf("clampInt(0) = %d, want 0", got)
	}
	if got := clampInt(k8sMaxOutput); got != int(k8sMaxOutput) {
		t.Errorf("clampInt(cap) = %d, want %d", got, int(k8sMaxOutput))
	}
	if got := clampInt(math.MaxInt64); got != math.MaxInt {
		t.Errorf("clampInt(MaxInt64) = %d, want MaxInt %d", got, math.MaxInt)
	}
}

// recordingSecurityEmitter captures security events for assertions. It
// satisfies SecurityEventEmitter.
type recordingSecurityEmitter struct {
	pathTraversalEvents []pathTraversalEvent
	fileSizeEvents      []fileSizeEvent
	outputTruncated     []outputTruncatedEvent
}

type pathTraversalEvent struct{ path, workspace string }
type fileSizeEvent struct {
	path        string
	size, limit int64
}
type outputTruncatedEvent struct {
	command             string
	originalSize, limit int
}

func (r *recordingSecurityEmitter) PathTraversalBlocked(path, workspace string) {
	r.pathTraversalEvents = append(r.pathTraversalEvents, pathTraversalEvent{path, workspace})
}

func (r *recordingSecurityEmitter) FileSizeLimitExceeded(path string, size, limit int64) {
	r.fileSizeEvents = append(r.fileSizeEvents, fileSizeEvent{path, size, limit})
}

func (r *recordingSecurityEmitter) OutputTruncated(command string, originalSize, limit int) {
	r.outputTruncated = append(r.outputTruncated, outputTruncatedEvent{command, originalSize, limit})
}

var _ SecurityEventEmitter = (*recordingSecurityEmitter)(nil)
