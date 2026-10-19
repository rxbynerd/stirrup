package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/rxbynerd/stirrup/types"
)

// TestK8sExecutor_ResolvePath_ConfinesToWorkspace covers the textual
// workspace-escape check in K8sExecutor.ResolvePath. The "/workspacefoo" case
// is the critical security case: a prefix-without-separator must not match
// the workspace root.
func TestK8sExecutor_ResolvePath_ConfinesToWorkspace(t *testing.T) {
	exec := &K8sExecutor{}

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"relative file", "foo.txt", "/workspace/foo.txt", false},
		{"nested relative path", "sub/dir/file.go", "/workspace/sub/dir/file.go", false},
		{"absolute path inside workspace", "/workspace/inside.txt", "/workspace/inside.txt", false},
		// Empty / "." / "/workspace" all resolve to the workspace root with
		// no error here. ResolvePath is the lenient gate (it permits the
		// root, which ListDirectory legitimately lists); resolveFilePath is
		// the strict one that ReadFile/WriteFile use to reject the root —
		// see TestK8sResolveFilePath_RejectsRoot.
		{"empty path resolves to root", "", "/workspace", false},
		{"dot resolves to root", ".", "/workspace", false},
		{"workspace root", "/workspace", "/workspace", false},
		{"parent escape rejected", "../etc/passwd", "", true},
		{"nested parent escape rejected", "foo/../../etc/passwd", "", true},
		{"absolute path outside workspace rejected", "/etc/passwd", "", true},
		{"prefix without separator rejected", "/workspacefoo", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := exec.ResolvePath(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestNewK8sExecutor_RejectsMissingImage asserts the early-return guard fires
// before any cluster I/O is attempted.
func TestNewK8sExecutor_RejectsMissingImage(t *testing.T) {
	_, err := NewK8sExecutor(context.Background(), K8sExecutorConfig{
		Namespace: "default",
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "image")
}

// TestNewK8sExecutor_RejectsMissingNamespace asserts the early-return guard
// fires before any cluster I/O is attempted.
func TestNewK8sExecutor_RejectsMissingNamespace(t *testing.T) {
	_, err := NewK8sExecutor(context.Background(), K8sExecutorConfig{
		Image: "busybox",
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "namespace")
}

// TestNewK8sExecutor_RejectsNilNetwork asserts the fail-closed guard fires
// before any cluster I/O: a nil Network leaves egress posture undefined and
// is rejected at construction.
func TestNewK8sExecutor_RejectsNilNetwork(t *testing.T) {
	_, err := NewK8sExecutor(context.Background(), K8sExecutorConfig{
		Image:     "busybox",
		Namespace: "default",
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "network")
}

// TestNewK8sExecutor_RejectsAllowlistWithoutProxyURL asserts the allowlist-mode
// guard fails closed before any cluster I/O when no EgressProxyURL is set.
// An enforced allowlist NetworkPolicy makes the proxy the only route to the
// network, so omitting its URL is a misconfiguration.
func TestNewK8sExecutor_RejectsAllowlistWithoutProxyURL(t *testing.T) {
	_, err := NewK8sExecutor(context.Background(), K8sExecutorConfig{
		Image:     "busybox",
		Namespace: "default",
		Network:   &types.NetworkConfig{Mode: "allowlist"},
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "proxy")
}

// TestEgressPolicyFor covers the pure NetworkPolicy construction for each
// mode without a cluster: deny-all for "none", proxy+DNS allowlist for
// "allowlist", and an error for a nil or unsupported mode. The shape
// assertions here are the cluster-free half of the manifest-shape contract
// that the integration tests pin against a real API server.
// TestEgressPolicyFor_BuildsPolicyPerMode covers the pure NetworkPolicy
// construction for each mode without a cluster: deny-all for "none",
// proxy+DNS allowlist for "allowlist", and an error for a nil or unsupported
// mode. The shape assertions here are the cluster-free half of the
// manifest-shape contract that the integration tests pin against a real API
// server.
func TestEgressPolicyFor_BuildsPolicyPerMode(t *testing.T) {
	const ns, pod = "default", "stirrup-abc123"

	t.Run("nil network is rejected", func(t *testing.T) {
		_, err := egressPolicyFor(nil, ns, pod, "")
		require.Error(t, err)
	})

	t.Run("unsupported mode is rejected", func(t *testing.T) {
		_, err := egressPolicyFor(&types.NetworkConfig{Mode: "bridge"}, ns, pod, "")
		require.Error(t, err)
	})

	t.Run("none yields deny-all egress", func(t *testing.T) {
		np, err := egressPolicyFor(&types.NetworkConfig{Mode: "none"}, ns, pod, "")
		require.NoError(t, err)
		require.Equal(t, networkPolicyName(pod), np.Name)
		require.Equal(t, ns, np.Namespace)
		require.Equal(t, pod, np.Spec.PodSelector.MatchLabels[k8sPodNameLabel])
		require.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, np.Spec.PolicyTypes)
		require.Empty(t, np.Spec.Egress, "deny-all must have zero egress rules")
	})

	t.Run("allowlist yields dns + proxy egress", func(t *testing.T) {
		np, err := egressPolicyFor(&types.NetworkConfig{Mode: "allowlist"}, ns, pod, "")
		require.NoError(t, err)
		require.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, np.Spec.PolicyTypes)
		require.Len(t, np.Spec.Egress, 2, "allowlist egress rules should be dns and proxy")

		// First rule is DNS: two ports (UDP/TCP 53), no peer restriction.
		dns := np.Spec.Egress[0]
		require.Len(t, dns.Ports, 2, "dns rule needs udp and tcp")
		for _, p := range dns.Ports {
			require.NotNil(t, p.Port)
			require.Equal(t, k8sDNSPort, p.Port.IntValue())
		}

		// Second rule selects the egress proxy by label.
		proxy := np.Spec.Egress[1]
		require.Len(t, proxy.To, 1, "proxy rule must select the proxy pod")
		require.NotNil(t, proxy.To[0].PodSelector, "proxy rule must select the proxy pod")
		require.Equal(t, "stirrup-egress-proxy", proxy.To[0].PodSelector.MatchLabels["app"])

		// The proxy rule must confine egress to the proxy's listen port so an
		// enforcing CNI does not permit reaching the proxy Pod on any port.
		require.Len(t, proxy.Ports, 1, "proxy rule should allow only the listen port")
		require.NotNil(t, proxy.Ports[0].Protocol)
		require.Equal(t, corev1.ProtocolTCP, *proxy.Ports[0].Protocol)
		require.NotNil(t, proxy.Ports[0].Port)
		require.Equal(t, k8sEgressProxyPort, proxy.Ports[0].Port.IntValue())
	})
}

// TestProxyEnvFor_InjectsProxyVarsPerMode covers the pure proxy-env mapping:
// none injects nothing, allowlist requires a URL and injects the proxy
// variables, and a nil or unsupported mode errors.
func TestProxyEnvFor_InjectsProxyVarsPerMode(t *testing.T) {
	t.Run("nil network is rejected", func(t *testing.T) {
		_, err := proxyEnvFor(nil, "")
		require.Error(t, err)
	})

	t.Run("none injects nothing", func(t *testing.T) {
		env, err := proxyEnvFor(&types.NetworkConfig{Mode: "none"}, "")
		require.NoError(t, err)
		require.Empty(t, env)
	})

	t.Run("allowlist without url errors", func(t *testing.T) {
		_, err := proxyEnvFor(&types.NetworkConfig{Mode: "allowlist"}, "")
		require.Error(t, err)
	})

	t.Run("allowlist with url injects proxy vars", func(t *testing.T) {
		const url = "http://stirrup-egress-proxy.default.svc:8080"
		env, err := proxyEnvFor(&types.NetworkConfig{Mode: "allowlist"}, url)
		require.NoError(t, err)

		got := map[string]string{}
		for _, e := range env {
			got[e.Name] = e.Value
		}
		// Both spellings are injected: git honours only the lower-case
		// http_proxy for plain-http destinations.
		for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
			require.Equal(t, url, got[name], "proxy env %s", name)
		}
		for _, name := range []string{"NO_PROXY", "no_proxy"} {
			require.NotEmpty(t, got[name], "%s must be set", name)
		}
	})

	t.Run("unsupported mode errors", func(t *testing.T) {
		_, err := proxyEnvFor(&types.NetworkConfig{Mode: "bridge"}, "http://x")
		require.Error(t, err)
	})
}

// TestK8sExecutor_Capabilities_TracksNetworkMode exercises the CanNetwork
// branch and verifies the other capability flags match the documented
// defaults (CanRead/Write/Exec = true per the scaffold spec; MaxTimeout =
// maxTimeout to match the container and local executors).
func TestK8sExecutor_Capabilities_TracksNetworkMode(t *testing.T) {
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
		t.Run(tt.name, func(t *testing.T) {
			exec := &K8sExecutor{podExecCore: podExecCore{network: tt.network}}
			caps := exec.Capabilities()
			require.Equal(t, tt.wantCanNet, caps.CanNetwork)
			require.True(t, caps.CanRead)
			require.True(t, caps.CanWrite)
			require.True(t, caps.CanExec)
			require.Equal(t, maxTimeout, caps.MaxTimeout)
		})
	}
}

// TestGeneratePodName_FormatAndUniqueness asserts the documented pod-name
// format (stirrup-<12 hex chars>) and uniqueness across repeated calls. 6
// random bytes encoded as hex yield 12 characters; format drift would be
// visible to operators.
func TestGeneratePodName_FormatAndUniqueness(t *testing.T) {
	re := regexp.MustCompile(`^stirrup-[0-9a-f]{12}$`)
	seen := make(map[string]struct{}, 10)
	for i := 0; i < 10; i++ {
		name, err := generatePodName()
		require.NoError(t, err)
		require.Regexp(t, re, name)
		require.NotContains(t, seen, name, "pod name repeated across calls")
		seen[name] = struct{}{}
	}
}

// TestExtractExitCode_HandlesCodeAndTransportErrors is the cluster-free unit
// test for the pure exit-code helper that Exec and the file-I/O methods rely
// on. It covers the three cases that matter: a clean (nil) exit, a non-zero
// CodeExitError carrying a status, and a transport-level error that carries
// no exit status. The helper must stay robust to the value/pointer wrapping
// that errors.As performs, so both forms are exercised.
func TestExtractExitCode_HandlesCodeAndTransportErrors(t *testing.T) {
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
		t.Run(tt.name, func(t *testing.T) {
			code, ok := extractExitCode(tt.err)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.wantCode, code)
		})
	}
}

// TestWriteCapBuffer_CapsAtLimit verifies the 10 MB streaming cap: writes
// below the limit accumulate, a write that crosses the limit is truncated and
// flags exceeded, and post-limit writes are silently dropped (the SPDY stream
// keeps draining). The Writer always reports the full slice as consumed so
// remotecommand never errors mid-stream on a capped buffer.
func TestWriteCapBuffer_CapsAtLimit(t *testing.T) {
	t.Run("under limit accumulates", func(t *testing.T) {
		w := writeCapBuffer{limit: 16}
		n, err := w.Write([]byte("hello"))
		require.NoError(t, err)
		require.Equal(t, 5, n)
		require.False(t, w.exceeded, "exceeded must stay unset below the limit")
		require.Equal(t, "hello", w.String())
	})

	t.Run("crossing limit truncates and flags", func(t *testing.T) {
		w := writeCapBuffer{limit: 4}
		n, err := w.Write([]byte("abcdefgh"))
		require.NoError(t, err)
		require.Equal(t, 8, n, "the full slice is reported as consumed")
		require.True(t, w.exceeded)
		require.Equal(t, "abcd", w.String(), "buffer is truncated at the limit")
	})

	t.Run("post-limit writes dropped", func(t *testing.T) {
		w := writeCapBuffer{limit: 4}
		_, _ = w.Write([]byte("abcd"))
		_, _ = w.Write([]byte("more"))
		n, err := w.Write([]byte("xyz"))
		require.NoError(t, err)
		require.Equal(t, 3, n)
		require.Equal(t, "abcd", w.String(), "buffer must not grow after the cap")
	})
}

// TestTimeoutExecResult_OutputTruncated pins the ctx-ending branch of
// podExecCore.Exec: a deadline/cancellation racing the output cap must set
// OutputTruncated on the (still-returned, not discarded) partial result.
// podExecCore.Exec itself is exercised over a real pods/exec stream, so
// this drives the extracted result-construction logic directly with
// writeCapBuffer state built the same way Exec's Write calls would leave
// it, rather than requiring a fake streaming apparatus.
func TestTimeoutExecResult_OutputTruncated(t *testing.T) {
	t.Run("neither stream capped", func(t *testing.T) {
		var stdout, stderr writeCapBuffer
		stdout.limit, stderr.limit = 16, 16
		_, _ = stdout.Write([]byte("hello"))

		result := timeoutExecResult(&stdout, &stderr)
		require.False(t, result.OutputTruncated)
		require.Equal(t, "hello", result.Stdout)
	})

	t.Run("stdout capped", func(t *testing.T) {
		var stdout, stderr writeCapBuffer
		stdout.limit, stderr.limit = 4, 4
		_, _ = stdout.Write([]byte("abcdefgh"))

		result := timeoutExecResult(&stdout, &stderr)
		require.True(t, result.OutputTruncated)
	})

	t.Run("stderr capped", func(t *testing.T) {
		var stdout, stderr writeCapBuffer
		stdout.limit, stderr.limit = 4, 4
		_, _ = stderr.Write([]byte("abcdefgh"))

		result := timeoutExecResult(&stdout, &stderr)
		require.True(t, result.OutputTruncated)
	})

	t.Run("ctx-ended exec exit code is -1", func(t *testing.T) {
		result := timeoutExecResult(&writeCapBuffer{}, &writeCapBuffer{})
		require.Equal(t, -1, result.ExitCode, "-1 is the sentinel for a ctx-ended exec")
	})
}

// TestClassifyTarError_MapsMissingFilesToErrNotExist covers the missing-file
// mapping that ReadFile and ListDirectory depend on: a tar/ls "No such file or
// directory" stderr must surface as fs.ErrNotExist so callers can branch with
// errors.Is. Other stderr is passed through.
func TestClassifyTarError_MapsMissingFilesToErrNotExist(t *testing.T) {
	t.Run("no such file", func(t *testing.T) {
		err := classifyTarError("missing.txt", "tar: missing.txt: No such file or directory")
		require.ErrorIs(t, err, fs.ErrNotExist)
	})

	// The "not found" substring (busybox phrasing) must also map to
	// fs.ErrNotExist.
	t.Run("busybox not found", func(t *testing.T) {
		err := classifyTarError("missing.txt", "ls: missing.txt: not found")
		require.ErrorIs(t, err, fs.ErrNotExist)
	})

	t.Run("other stderr is passed through", func(t *testing.T) {
		err := classifyTarError("x", "tar: permission denied")
		require.NotErrorIs(t, err, fs.ErrNotExist)
		require.ErrorContains(t, err, "permission denied")
	})

	// Empty stderr (a non-zero exit with nothing on stderr) must still
	// produce a usable, non-nil error that names the target.
	t.Run("empty stderr names the target", func(t *testing.T) {
		err := classifyTarError("ghost", "")
		require.Error(t, err)
		require.NotErrorIs(t, err, fs.ErrNotExist)
		require.ErrorContains(t, err, "ghost")
	})
}

// TestK8sResolveFilePath_RejectsRoot asserts resolveFilePath (used by
// ReadFile/WriteFile) rejects any input that resolves to the workspace
// root, so WriteFile never drives `mkdir -p /` / `tar -C /`. The inputs "",
// ".", and "/workspace" all collapse to the root.
func TestK8sResolveFilePath_RejectsRoot(t *testing.T) {
	exec := &K8sExecutor{}

	t.Run("root-resolving inputs are rejected", func(t *testing.T) {
		for _, in := range []string{"", ".", "/workspace", "/workspace/"} {
			_, err := exec.resolveFilePath(in)
			require.Error(t, err, "resolveFilePath(%q) should reject the workspace root", in)
		}
	})

	t.Run("file path resolves under workspace", func(t *testing.T) {
		got, err := exec.resolveFilePath("a.txt")
		require.NoError(t, err)
		require.Equal(t, "/workspace/a.txt", got)
	})
}

// TestK8sWriteFile_RejectsOversizedContent verifies the 10 MB write cap fires
// on len(content) before any cluster I/O, so the invariant runs under the
// default `just test` (no cluster). The receiver is a zero-value executor;
// the size check returns before any clientset use.
func TestK8sWriteFile_RejectsOversizedContent(t *testing.T) {
	exec := &K8sExecutor{}
	big := strings.Repeat("a", int(k8sMaxOutput)+1)
	err := exec.WriteFile(context.Background(), "big.txt", big)
	require.ErrorIs(t, err, errK8sOutputCap)
}

// TestK8sWriteFile_OversizedEmitsFileSizeEvent verifies the oversized-write
// path reports FileSizeLimitExceeded to a wired emitter (the cluster-free
// prefix of the cap path). A nil emitter must stay silent, which
// TestK8sWriteFile_RejectsOversizedContent covers with a zero-value executor.
func TestK8sWriteFile_OversizedEmitsFileSizeEvent(t *testing.T) {
	rec := &recordingSecurityEmitter{}
	exec := &K8sExecutor{podExecCore: podExecCore{Security: rec}}
	big := strings.Repeat("a", int(k8sMaxOutput)+1)
	err := exec.WriteFile(context.Background(), "big.txt", big)
	require.ErrorIs(t, err, errK8sOutputCap)

	require.Len(t, rec.fileSizeEvents, 1)
	ev := rec.fileSizeEvents[0]
	require.Equal(t, "big.txt", ev.path)
	require.Equal(t, k8sMaxOutput, ev.limit)
	require.Greater(t, ev.size, k8sMaxOutput, "reported size should exceed the cap")
}

// TestK8sResolvePath_EmitsPathTraversal verifies the escape branch reports
// PathTraversalBlocked when an emitter is wired, and stays silent for an
// in-bounds path. This is the monitoring hook the container/local executors
// already provide.
func TestK8sResolvePath_EmitsPathTraversal(t *testing.T) {
	rec := &recordingSecurityEmitter{}
	exec := &K8sExecutor{podExecCore: podExecCore{Security: rec}}

	_, err := exec.ResolvePath("../etc/passwd")
	require.Error(t, err)
	require.Len(t, rec.pathTraversalEvents, 1)
	got := rec.pathTraversalEvents[0]
	require.Equal(t, "../etc/passwd", got.path)
	require.Equal(t, "/workspace", got.workspace)

	_, err = exec.ResolvePath("ok.txt")
	require.NoError(t, err)
	require.Len(t, rec.pathTraversalEvents, 1, "in-bounds path emitted a traversal event: %+v", rec.pathTraversalEvents)
}

// TestClampInt_SaturatesInsteadOfWrapping verifies the int64->int narrowing
// saturates rather than wrapping, guarding the security-emitter sizes on
// 32-bit builds.
func TestClampInt_SaturatesInsteadOfWrapping(t *testing.T) {
	t.Run("zero is unchanged", func(t *testing.T) {
		require.Equal(t, 0, clampInt(0))
	})

	t.Run("output cap fits in int", func(t *testing.T) {
		require.Equal(t, int(k8sMaxOutput), clampInt(k8sMaxOutput))
	})

	t.Run("int64 max saturates to int max", func(t *testing.T) {
		require.Equal(t, math.MaxInt, clampInt(math.MaxInt64))
	})
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

// TestClassifyPodCreateError_RuntimeClassHint covers the RuntimeClass-aware error wrap. An
// IsInvalid (built-in admission) or IsForbidden (webhook/OPA) error with a
// non-empty RuntimeClass gets the friendly hint; an empty RuntimeClass or
// an unrelated error falls through to the plain "create pod" wrap. The
// original error must remain reachable via errors.Is/As in every case.
func TestClassifyPodCreateError_RuntimeClassHint(t *testing.T) {
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
		require.ErrorContains(t, err, `RuntimeClass "gvisor"`)
		require.ErrorContains(t, err, "kubectl get runtimeclass")
		require.True(t, apierrors.IsInvalid(err), "wrapped error must remain IsInvalid via errors.As")
	})

	t.Run("forbidden with runtime class gets hint", func(t *testing.T) {
		err := classifyPodCreateError("gvisor", forbidden)
		require.ErrorContains(t, err, `RuntimeClass "gvisor"`)
		require.ErrorContains(t, err, "kubectl get runtimeclass")
		require.True(t, apierrors.IsForbidden(err), "wrapped error must remain IsForbidden via errors.As")
	})

	t.Run("forbidden without runtime class is plain", func(t *testing.T) {
		err := classifyPodCreateError("", forbidden)
		// The synthetic forbidden error carries "RuntimeClass" in its own
		// text, so assert on the hint phrase the wrapper adds rather than
		// the bare word.
		require.NotContains(t, err.Error(), "kubectl get runtimeclass")
		require.True(t, apierrors.IsForbidden(err), "wrapped error must remain IsForbidden")
	})

	t.Run("invalid without runtime class is plain", func(t *testing.T) {
		err := classifyPodCreateError("", invalid)
		require.NotContains(t, err.Error(), "RuntimeClass")
		require.True(t, strings.HasPrefix(err.Error(), "create pod:"), "expected plain create-pod wrap")
	})

	t.Run("non-invalid error with runtime class is plain", func(t *testing.T) {
		err := classifyPodCreateError("gvisor", transport)
		require.NotContains(t, err.Error(), "RuntimeClass")
		require.ErrorIs(t, err, transport)
	})
}

// recordingExecCore returns a podExecCore whose exec calls are appended to
// calls. The first (run) call blocks until its ctx ends when hang is set; the
// follow-up kill call always returns at once.
func recordingExecCore(hang bool) (*podExecCore, *[][]string) {
	var calls [][]string
	core := &podExecCore{}
	core.execFn = func(ctx context.Context, command []string, _ io.Reader, _, _ io.Writer) error {
		calls = append(calls, command)
		if hang && len(calls) == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	return core, &calls
}

// assertKillFollowedRun checks that a timed-out command was followed by a
// group-kill exec that targets the run's pid file.
func assertKillFollowedRun(t *testing.T, calls [][]string) {
	t.Helper()
	require.Len(t, calls, 2, "want the run followed by one kill")
	run, kill := calls[0], calls[1]
	require.Equal(t, "stirrup-exec", run[3], "run exec script")
	require.Equal(t, "stirrup-kill", kill[3], "kill exec script")
	assert.Equal(t, groupKillScript, kill[2], "kill exec command")
	assert.Equal(t, run[5], kill[4], "kill pid file")
}

func TestPodExecCore_ExecKillsGroupAfterTimeout(t *testing.T) {
	core, calls := recordingExecCore(true)
	_, err := core.Exec(context.Background(), "sleep 600", 50*time.Millisecond)
	require.ErrorIs(t, err, ErrTimeout)
	assertKillFollowedRun(t, *calls)
}

func TestPodExecCore_ExecStreamKillsGroupAfterTimeout(t *testing.T) {
	core, calls := recordingExecCore(true)
	_, err := core.ExecStream(context.Background(), "sleep 600", 50*time.Millisecond, io.Discard, io.Discard)
	require.ErrorIs(t, err, ErrTimeout)
	assertKillFollowedRun(t, *calls)
}

func TestPodExecCore_CompletedCommandIsNotKilled(t *testing.T) {
	core, calls := recordingExecCore(false)
	_, err := core.Exec(context.Background(), "true", time.Second)
	require.NoError(t, err, "Exec")
	_, err = core.ExecStream(context.Background(), "true", time.Second, io.Discard, io.Discard)
	require.NoError(t, err, "ExecStream")
	require.Len(t, *calls, 2, "want one run per command and no kill")
	for _, c := range *calls {
		assert.Equal(t, "stirrup-exec", c[3], "unexpected kill exec: %q", c)
	}
}
