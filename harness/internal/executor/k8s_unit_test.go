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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
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

// TestNewK8sExecutor_NilNetworkRejected asserts the fail-closed guard fires
// before any cluster I/O: a nil Network leaves egress posture undefined and
// is rejected at construction (issue #178). This pins the acceptance
// criterion "nil Network rejected at construction".
func TestNewK8sExecutor_NilNetworkRejected(t *testing.T) {
	_, err := NewK8sExecutor(context.Background(), K8sExecutorConfig{
		Image:     "busybox",
		Namespace: "default",
	})
	if err == nil {
		t.Fatal("expected error for nil network config")
	}
	if !strings.Contains(err.Error(), "network") {
		t.Errorf("error %q should mention 'network'", err)
	}
}

// TestNewK8sExecutor_AllowlistRequiresProxyURL asserts the allowlist-mode
// guard fails closed before any cluster I/O when no EgressProxyURL is set
// (issue #83). An enforced allowlist NetworkPolicy makes the proxy the only
// route to the network, so omitting its URL is a misconfiguration.
func TestNewK8sExecutor_AllowlistRequiresProxyURL(t *testing.T) {
	_, err := NewK8sExecutor(context.Background(), K8sExecutorConfig{
		Image:     "busybox",
		Namespace: "default",
		Network:   &types.NetworkConfig{Mode: "allowlist"},
	})
	if err == nil {
		t.Fatal("expected error for allowlist mode without a proxy URL")
	}
	if !strings.Contains(err.Error(), "proxy") {
		t.Errorf("error %q should mention the missing proxy URL", err)
	}
}

// TestEgressPolicyFor covers the pure NetworkPolicy construction for each
// mode without a cluster: deny-all for "none", proxy+DNS allowlist for
// "allowlist", and an error for a nil or unsupported mode. The shape
// assertions here are the cluster-free half of the manifest-shape contract
// that the integration tests pin against a real API server.
func TestEgressPolicyFor(t *testing.T) {
	const ns, pod = "default", "stirrup-abc123"

	t.Run("nil network is rejected", func(t *testing.T) {
		if _, err := egressPolicyFor(nil, ns, pod); err == nil {
			t.Fatal("expected error for nil network")
		}
	})

	t.Run("unsupported mode is rejected", func(t *testing.T) {
		if _, err := egressPolicyFor(&types.NetworkConfig{Mode: "bridge"}, ns, pod); err == nil {
			t.Fatal("expected error for unsupported mode")
		}
	})

	t.Run("none yields deny-all egress", func(t *testing.T) {
		np, err := egressPolicyFor(&types.NetworkConfig{Mode: "none"}, ns, pod)
		if err != nil {
			t.Fatalf("egressPolicyFor(none): %v", err)
		}
		if np.Name != networkPolicyName(pod) || np.Namespace != ns {
			t.Errorf("policy name/ns = %s/%s, want %s/%s", np.Name, np.Namespace, networkPolicyName(pod), ns)
		}
		if np.Spec.PodSelector.MatchLabels[k8sPodNameLabel] != pod {
			t.Errorf("podSelector = %v, want it to select %s=%s", np.Spec.PodSelector.MatchLabels, k8sPodNameLabel, pod)
		}
		if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
			t.Errorf("policyTypes = %v, want [Egress]", np.Spec.PolicyTypes)
		}
		if len(np.Spec.Egress) != 0 {
			t.Errorf("deny-all must have zero egress rules, got %d", len(np.Spec.Egress))
		}
	})

	t.Run("allowlist yields dns + proxy egress", func(t *testing.T) {
		np, err := egressPolicyFor(&types.NetworkConfig{Mode: "allowlist"}, ns, pod)
		if err != nil {
			t.Fatalf("egressPolicyFor(allowlist): %v", err)
		}
		if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
			t.Errorf("policyTypes = %v, want [Egress]", np.Spec.PolicyTypes)
		}
		if len(np.Spec.Egress) != 2 {
			t.Fatalf("allowlist egress rules = %d, want 2 (dns + proxy)", len(np.Spec.Egress))
		}
		// First rule is DNS: two ports (UDP/TCP 53), no peer restriction.
		dns := np.Spec.Egress[0]
		if len(dns.Ports) != 2 {
			t.Errorf("dns rule ports = %d, want 2 (udp+tcp)", len(dns.Ports))
		}
		for _, p := range dns.Ports {
			if p.Port == nil || p.Port.IntValue() != k8sDNSPort {
				t.Errorf("dns rule port = %v, want %d", p.Port, k8sDNSPort)
			}
		}
		// Second rule selects the egress proxy by label.
		proxy := np.Spec.Egress[1]
		if len(proxy.To) != 1 || proxy.To[0].PodSelector == nil {
			t.Fatalf("proxy rule must select the proxy pod, got %+v", proxy.To)
		}
		if proxy.To[0].PodSelector.MatchLabels["app"] != "stirrup-egress-proxy" {
			t.Errorf("proxy peer selector = %v, want app=stirrup-egress-proxy", proxy.To[0].PodSelector.MatchLabels)
		}
		// The proxy rule must confine egress to the proxy's listen port so an
		// enforcing CNI does not permit reaching the proxy Pod on any port.
		if len(proxy.Ports) != 1 {
			t.Fatalf("proxy rule ports = %d, want 1 (TCP %d)", len(proxy.Ports), k8sEgressProxyPort)
		}
		if proxy.Ports[0].Protocol == nil || *proxy.Ports[0].Protocol != corev1.ProtocolTCP {
			t.Errorf("proxy rule protocol = %v, want TCP", proxy.Ports[0].Protocol)
		}
		if proxy.Ports[0].Port == nil || proxy.Ports[0].Port.IntValue() != k8sEgressProxyPort {
			t.Errorf("proxy rule port = %v, want %d", proxy.Ports[0].Port, k8sEgressProxyPort)
		}
	})
}

// TestProxyEnvFor covers the pure proxy-env mapping: none injects nothing,
// allowlist requires a URL and injects the three proxy variables, and a nil
// or unsupported mode errors.
func TestProxyEnvFor(t *testing.T) {
	t.Run("nil network is rejected", func(t *testing.T) {
		if _, err := proxyEnvFor(nil, ""); err == nil {
			t.Fatal("expected error for nil network")
		}
	})

	t.Run("none injects nothing", func(t *testing.T) {
		env, err := proxyEnvFor(&types.NetworkConfig{Mode: "none"}, "")
		if err != nil {
			t.Fatalf("proxyEnvFor(none): %v", err)
		}
		if len(env) != 0 {
			t.Errorf("none must inject no env, got %v", env)
		}
	})

	t.Run("allowlist without url errors", func(t *testing.T) {
		if _, err := proxyEnvFor(&types.NetworkConfig{Mode: "allowlist"}, ""); err == nil {
			t.Fatal("expected error for allowlist without a proxy URL")
		}
	})

	t.Run("allowlist with url injects proxy vars", func(t *testing.T) {
		const url = "http://stirrup-egress-proxy.default.svc:8080"
		env, err := proxyEnvFor(&types.NetworkConfig{Mode: "allowlist"}, url)
		if err != nil {
			t.Fatalf("proxyEnvFor(allowlist): %v", err)
		}
		got := map[string]string{}
		for _, e := range env {
			got[e.Name] = e.Value
		}
		if got["HTTP_PROXY"] != url || got["HTTPS_PROXY"] != url {
			t.Errorf("proxy env = %v, want HTTP(S)_PROXY=%s", got, url)
		}
		if got["NO_PROXY"] == "" {
			t.Errorf("NO_PROXY must be set, env = %v", got)
		}
	})

	t.Run("unsupported mode errors", func(t *testing.T) {
		if _, err := proxyEnvFor(&types.NetworkConfig{Mode: "bridge"}, "http://x"); err == nil {
			t.Fatal("expected error for unsupported mode")
		}
	})
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
			exec := &K8sExecutor{podExecCore: podExecCore{network: tt.network}}
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
	exec := &K8sExecutor{podExecCore: podExecCore{Security: rec}}
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
	exec := &K8sExecutor{podExecCore: podExecCore{Security: rec}}

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

// TestClassifyPodCreateError covers the RuntimeClass-aware error wrap. An
// IsInvalid (built-in admission) or IsForbidden (webhook/OPA) error with a
// non-empty RuntimeClass gets the friendly hint; an empty RuntimeClass or
// an unrelated error falls through to the plain "create pod" wrap. The
// original error must remain reachable via errors.Is/As in every case.
func TestClassifyPodCreateError(t *testing.T) {
	invalid := apierrors.NewInvalid(
		schema.GroupKind{Group: "", Kind: "Pod"},
		"stirrup-abc",
		field.ErrorList{field.Invalid(
			field.NewPath("spec", "runtimeClassName"), "gvisor", "not found",
		)},
	)
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: "", Resource: "pods"},
		"stirrup-abc",
		errors.New("RuntimeClass \"gvisor\" is not allowed by policy"),
	)
	transport := errors.New("connection refused")

	t.Run("invalid with runtime class gets hint", func(t *testing.T) {
		err := classifyPodCreateError("gvisor", invalid)
		if !strings.Contains(err.Error(), "RuntimeClass \"gvisor\"") {
			t.Errorf("expected RuntimeClass hint, got: %v", err)
		}
		if !strings.Contains(err.Error(), "kubectl get runtimeclass") {
			t.Errorf("expected actionable hint, got: %v", err)
		}
		if !apierrors.IsInvalid(err) {
			t.Errorf("wrapped error must still be IsInvalid (errors.As), got: %v", err)
		}
	})

	t.Run("forbidden with runtime class gets hint", func(t *testing.T) {
		err := classifyPodCreateError("gvisor", forbidden)
		if !strings.Contains(err.Error(), "RuntimeClass \"gvisor\"") {
			t.Errorf("expected RuntimeClass hint, got: %v", err)
		}
		if !strings.Contains(err.Error(), "kubectl get runtimeclass") {
			t.Errorf("expected actionable hint, got: %v", err)
		}
		if !apierrors.IsForbidden(err) {
			t.Errorf("wrapped error must still be IsForbidden (errors.As), got: %v", err)
		}
	})

	t.Run("forbidden without runtime class is plain", func(t *testing.T) {
		err := classifyPodCreateError("", forbidden)
		// The synthetic forbidden error carries "RuntimeClass" in its own
		// text, so assert on the hint phrase the wrapper adds rather than
		// the bare word.
		if strings.Contains(err.Error(), "kubectl get runtimeclass") {
			t.Errorf("empty runtime class must not add the RuntimeClass hint, got: %v", err)
		}
		if !apierrors.IsForbidden(err) {
			t.Errorf("wrapped error must still be IsForbidden, got: %v", err)
		}
	})

	t.Run("invalid without runtime class is plain", func(t *testing.T) {
		err := classifyPodCreateError("", invalid)
		if strings.Contains(err.Error(), "RuntimeClass") {
			t.Errorf("empty runtime class must not add the RuntimeClass hint, got: %v", err)
		}
		if !strings.HasPrefix(err.Error(), "create pod:") {
			t.Errorf("expected plain create-pod wrap, got: %v", err)
		}
	})

	t.Run("non-invalid error with runtime class is plain", func(t *testing.T) {
		err := classifyPodCreateError("gvisor", transport)
		if strings.Contains(err.Error(), "RuntimeClass") {
			t.Errorf("a non-Invalid error must not get the RuntimeClass hint, got: %v", err)
		}
		if !errors.Is(err, transport) {
			t.Errorf("original error must remain wrapped, got: %v", err)
		}
	})
}
