package executor

import (
	"context"
	"regexp"
	"strings"
	"testing"

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

// TestK8sExecutor_StubMethodsReturnNotImplemented locks the documented
// "not implemented" contract on the four stub methods. The constructor
// is bypassed so no cluster is needed.
func TestK8sExecutor_StubMethodsReturnNotImplemented(t *testing.T) {
	exec := &K8sExecutor{}
	ctx := context.Background()

	t.Run("ReadFile", func(t *testing.T) {
		_, err := exec.ReadFile(ctx, "x")
		if err == nil || !strings.Contains(err.Error(), "not implemented") {
			t.Errorf("ReadFile: got %v, want error containing 'not implemented'", err)
		}
	})

	t.Run("WriteFile", func(t *testing.T) {
		err := exec.WriteFile(ctx, "x", "y")
		if err == nil || !strings.Contains(err.Error(), "not implemented") {
			t.Errorf("WriteFile: got %v, want error containing 'not implemented'", err)
		}
	})

	t.Run("ListDirectory", func(t *testing.T) {
		_, err := exec.ListDirectory(ctx, "x")
		if err == nil || !strings.Contains(err.Error(), "not implemented") {
			t.Errorf("ListDirectory: got %v, want error containing 'not implemented'", err)
		}
	})

	t.Run("Exec", func(t *testing.T) {
		_, err := exec.Exec(ctx, "echo", 0)
		if err == nil || !strings.Contains(err.Error(), "not implemented") {
			t.Errorf("Exec: got %v, want error containing 'not implemented'", err)
		}
	})
}
