package executor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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

	other := classifyTarError("x", "tar: permission denied")
	if errors.Is(other, fs.ErrNotExist) {
		t.Errorf("non-missing stderr should not map to fs.ErrNotExist: %v", other)
	}
	if !strings.Contains(other.Error(), "permission denied") {
		t.Errorf("non-missing stderr should be surfaced verbatim: %v", other)
	}
}
