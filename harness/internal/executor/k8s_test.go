//go:build integration_k8s

package executor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/rxbynerd/stirrup/types"
)

// testK8sConfig carries the cluster-specific knobs the integration suite
// needs to run against an arbitrary cluster, not just the untainted
// single-node kind dev cluster. Every value falls back to the original
// kind-friendly default when its env var is unset, so `just kind-smoke`
// style runs are unchanged; a real cluster (e.g. GKE, whose node pools are
// tainted and gated on a RuntimeClass) is selected by exporting:
//
//	STIRRUP_TEST_KUBECONFIG       path to a kubeconfig (also the suite gate)
//	STIRRUP_TEST_NAMESPACE        sandbox namespace            (default "default")
//	STIRRUP_TEST_RUNTIME_CLASS    Pod RuntimeClassName         (default "")
//	STIRRUP_TEST_IMAGE            sandbox image                (default "busybox:latest")
//	STIRRUP_TEST_EGRESS_PROXY_URL allowlist proxy URL          (default the default-ns svc)
//
// On a cluster whose only schedulable nodes are tainted for a sandbox
// runtime (GKE Sandbox taints the gVisor pool, and a no-RuntimeClass Pod
// has nowhere to land), STIRRUP_TEST_RUNTIME_CLASS must name the sandbox
// RuntimeClass (e.g. "gvisor") or every Pod times out at readiness.
type testK8sConfig struct {
	kubeconfig   string
	namespace    string
	runtimeClass string
	image        string
	proxyURL     string
}

// testK8sEnv resolves the integration config from the environment, skipping
// the test when STIRRUP_TEST_KUBECONFIG is unset (the existing suite gate).
func testK8sEnv(t *testing.T) testK8sConfig {
	t.Helper()
	kubeconfig := os.Getenv("STIRRUP_TEST_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("STIRRUP_TEST_KUBECONFIG not set; skipping k8s integration test")
	}
	return testK8sConfig{
		kubeconfig:   kubeconfig,
		namespace:    envOr("STIRRUP_TEST_NAMESPACE", "default"),
		runtimeClass: os.Getenv("STIRRUP_TEST_RUNTIME_CLASS"),
		image:        envOr("STIRRUP_TEST_IMAGE", "busybox:latest"),
		proxyURL:     envOr("STIRRUP_TEST_EGRESS_PROXY_URL", "http://stirrup-egress-proxy.default.svc:8080"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// TestK8sExecutorLifecycle exercises the full create/wait/delete lifecycle
// against a real cluster. It is gated by build tag `integration_k8s` and by
// STIRRUP_TEST_KUBECONFIG so the default `just test` run never touches a
// real cluster.
func TestK8sExecutorLifecycle(t *testing.T) {
	cfg := testK8sEnv(t)
	namespace := cfg.namespace

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	exec, err := NewK8sExecutor(ctx, K8sExecutorConfig{
		Image:            cfg.image,
		Namespace:        namespace,
		Kubeconfig:       cfg.kubeconfig,
		RuntimeClassName: cfg.runtimeClass,
		Network:          &types.NetworkConfig{Mode: "none"},
	})
	if err != nil {
		t.Fatalf("NewK8sExecutor: %v", err)
	}

	closed := false
	t.Cleanup(func() {
		if closed {
			return
		}
		_ = exec.Close()
	})

	if err := exec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed = true

	clientset, err := kubeClientFromConfig(t, cfg.kubeconfig)
	if err != nil {
		t.Fatalf("build verification clientset: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, getErr := clientset.CoreV1().Pods(namespace).Get(context.Background(), exec.podName, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pod %s still present 5s after Close (last get err: %v)", exec.podName, getErr)
		}
		time.Sleep(200 * time.Millisecond)
	}

	if err := exec.Close(); err != nil {
		t.Fatalf("second Close should be idempotent, got: %v", err)
	}
}

// TestK8sExecutorCloseIdempotent verifies the standalone idempotency
// guarantee: calling Close twice on an executor whose Pod is already gone
// must return nil from the second call.
func TestK8sExecutorCloseIdempotent(t *testing.T) {
	cfg := testK8sEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	exec, err := NewK8sExecutor(ctx, K8sExecutorConfig{
		Image:            cfg.image,
		Namespace:        cfg.namespace,
		Kubeconfig:       cfg.kubeconfig,
		RuntimeClassName: cfg.runtimeClass,
		Network:          &types.NetworkConfig{Mode: "none"},
	})
	if err != nil {
		t.Fatalf("NewK8sExecutor: %v", err)
	}

	if err := exec.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := exec.Close(); err != nil {
		t.Fatalf("second Close should be nil, got: %v", err)
	}
}

func kubeClientFromConfig(t *testing.T, kubeconfig string) (kubernetes.Interface, error) {
	t.Helper()
	restCfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(restCfg)
}

// newTestK8sExecutor constructs a K8sExecutor against the kind cluster named
// by STIRRUP_TEST_KUBECONFIG, skipping when it is unset. The image must ship
// /bin/sh, tar, and ls — busybox satisfies all three. The executor's Pod is
// torn down on test cleanup.
func newTestK8sExecutor(t *testing.T) *K8sExecutor {
	t.Helper()
	cfg := testK8sEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	exec, err := NewK8sExecutor(ctx, K8sExecutorConfig{
		Image:            cfg.image,
		Namespace:        cfg.namespace,
		Kubeconfig:       cfg.kubeconfig,
		RuntimeClassName: cfg.runtimeClass,
		Network:          &types.NetworkConfig{Mode: "none"},
	})
	if err != nil {
		t.Fatalf("NewK8sExecutor: %v", err)
	}
	t.Cleanup(func() { _ = exec.Close() })
	return exec
}

// TestK8sExec_Success exercises a clean exit: echo writes to stdout and the
// process exits 0.
func TestK8sExec_Success(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	res, err := exec.Exec(ctx, "echo hello", 10*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.Stdout != "hello\n" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "hello\n")
	}
}

// TestK8sExec_NonZeroExit verifies that a command writing to stderr and
// exiting non-zero surfaces both the exit code and stderr.
func TestK8sExec_NonZeroExit(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	res, err := exec.Exec(ctx, "echo err 1>&2; exit 7", 10*time.Second)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "err") {
		t.Errorf("Stderr = %q, want it to contain %q", res.Stderr, "err")
	}
}

// TestK8sExec_Timeout verifies that a command exceeding the timeout returns
// the context deadline error (verbatim) at roughly the timeout boundary,
// not after the full sleep.
func TestK8sExec_Timeout(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	start := time.Now()
	_, err := exec.Exec(ctx, "sleep 5", 1*time.Second)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Exec: err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("Exec took %s, expected ~1s (timeout not honoured)", elapsed)
	}
}

// TestK8sExec_OutputCap verifies the 10 MB output cap fires when a command
// emits more than the limit.
func TestK8sExec_OutputCap(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	// Emit ~11 MB to stdout. `yes` + head produces a deterministic stream;
	// 11*1024*1024 bytes of "x\n" comfortably exceeds the 10 MB cap.
	_, err := exec.Exec(ctx, "yes x | head -c 11534336", 30*time.Second)
	if !errors.Is(err, errK8sOutputCap) {
		t.Fatalf("Exec: err = %v, want errK8sOutputCap", err)
	}
}

// TestK8sFileIO_RoundTrip writes and reads back several payloads, including
// UTF-8 and an embedded NUL byte, confirming the tar-over-exec path is
// byte-exact.
func TestK8sFileIO_RoundTrip(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	cases := map[string]string{
		"ascii.txt":    "hello world\n",
		"utf8.txt":     "héllo — 世界 🌍\n",
		"nul.bin":      "before\x00after",
		"nested/a.txt": "deep\n",
	}

	for name, content := range cases {
		name, content := name, content
		t.Run(name, func(t *testing.T) {
			if err := exec.WriteFile(ctx, name, content); err != nil {
				t.Fatalf("WriteFile(%q): %v", name, err)
			}
			got, err := exec.ReadFile(ctx, name)
			if err != nil {
				t.Fatalf("ReadFile(%q): %v", name, err)
			}
			if got != content {
				t.Errorf("ReadFile(%q) = %q, want %q", name, got, content)
			}
		})
	}
}

// TestK8sReadFile_Missing verifies a missing path maps to fs.ErrNotExist so
// callers can branch with errors.Is.
func TestK8sReadFile_Missing(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	_, err := exec.ReadFile(ctx, "does-not-exist.txt")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadFile missing: err = %v, want fs.ErrNotExist", err)
	}
}

// TestK8sReadFile_Directory verifies that reading a directory is rejected
// with an "is a directory" error rather than returning archive bytes.
func TestK8sReadFile_Directory(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	if err := exec.WriteFile(ctx, "adir/keep.txt", "x"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := exec.ReadFile(ctx, "adir")
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("ReadFile directory: err = %v, want 'is a directory'", err)
	}
}

// TestK8sListDirectory lists a directory seeded via WriteFile and confirms
// the written entry appears (and that "." / ".." are excluded).
func TestK8sListDirectory(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	if err := exec.WriteFile(ctx, "listdir/one.txt", "1"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	entries, err := exec.ListDirectory(ctx, "listdir")
	if err != nil {
		t.Fatalf("ListDirectory: %v", err)
	}
	found := false
	for _, e := range entries {
		if e == "." || e == ".." {
			t.Errorf("ListDirectory returned %q, want it excluded", e)
		}
		if e == "one.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("ListDirectory = %v, want it to contain one.txt", entries)
	}
}

// TestK8sListDirectory_Missing verifies a missing directory maps to
// fs.ErrNotExist.
func TestK8sListDirectory_Missing(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	_, err := exec.ListDirectory(ctx, "no-such-dir")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ListDirectory missing: err = %v, want fs.ErrNotExist", err)
	}
}

// TestK8sListDirectory_WorkspaceRoot verifies that listing the workspace
// root is permitted (an empty path resolves to /workspace). ReadFile and
// WriteFile reject the root via resolveFilePath, but enumerating it is a
// legitimate listing operation — this pins that asymmetry against a cluster.
func TestK8sListDirectory_WorkspaceRoot(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	if err := exec.WriteFile(ctx, "rootfile.txt", "x"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	entries, err := exec.ListDirectory(ctx, "")
	if err != nil {
		t.Fatalf("ListDirectory(\"\"): %v", err)
	}
	found := false
	for _, e := range entries {
		if e == "rootfile.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("ListDirectory(\"\") = %v, want it to contain rootfile.txt", entries)
	}
}

// TestK8sWriteFile_RejectsWorkspaceRoot verifies finding #1 against a
// cluster: a write whose path resolves to the workspace root is rejected
// before any tar extraction runs.
func TestK8sWriteFile_RejectsWorkspaceRoot(t *testing.T) {
	exec := newTestK8sExecutor(t)
	ctx := context.Background()

	for _, p := range []string{"", ".", "/workspace"} {
		if err := exec.WriteFile(ctx, p, "data"); err == nil {
			t.Errorf("WriteFile(%q): expected workspace-root rejection", p)
		}
	}
}

// TestK8sRuntimeClass_Admitted exercises the RuntimeClassName plumbing
// against a real cluster: the Pod must be admitted and reach Ready for
// each RuntimeClass the platform supports.
//
//   - ""       — cluster default RuntimeClass (always present).
//   - "runc"   — vanilla runc; assumes a `runc` RuntimeClass is registered
//     (kind installs one in the gvisor-enabled image setup).
//   - "gvisor" — the gVisor RuntimeClass. Requires kind to be built with
//     the runsc shim (`containerd` + RuntimeClass node.k8s.io
//     "gvisor"). When the cluster lacks it, NewK8sExecutor
//     returns the friendly classifyPodCreateError wrap; the test
//     skips rather than fails so a non-gVisor kind cluster still
//     passes the rest of the suite.
//
// VERIFY AGAINST REAL RUN: the assertion that gVisor is actually in force
// (vs. the Pod silently falling back to runc) must be pinned to what a real
// kind+runsc run produces. The kernel signature `uname -r` returns under
// gVisor differs from the host kernel (gVisor reports a synthetic version,
// historically "4.4.0"), but the exact string depends on the runsc release
// the cluster ships. Do not hard-code a fabricated uname here — capture the
// real value from a `kubectl exec ... uname -r` against the gVisor Pod and
// pin it once observed.
func TestK8sRuntimeClass_Admitted(t *testing.T) {
	cfg := testK8sEnv(t)

	// The default set assumes a kind cluster (untainted node + a `runc`
	// RuntimeClass registered by kind-up.sh). On a cluster whose only
	// schedulable nodes are sandbox-tainted (GKE Sandbox) the cluster-default
	// and runc cases have nowhere to land, so the operator narrows the set,
	// e.g. STIRRUP_TEST_RUNTIME_CLASSES=gvisor.
	runtimeClasses := []string{"", "runc", "gvisor"}
	if v := os.Getenv("STIRRUP_TEST_RUNTIME_CLASSES"); v != "" {
		runtimeClasses = nil
		for _, rc := range strings.Split(v, ",") {
			runtimeClasses = append(runtimeClasses, strings.TrimSpace(rc))
		}
	}

	for _, runtimeClass := range runtimeClasses {
		name := runtimeClass
		if name == "" {
			name = "cluster-default"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			exec, err := NewK8sExecutor(ctx, K8sExecutorConfig{
				Image:            cfg.image,
				Namespace:        cfg.namespace,
				Kubeconfig:       cfg.kubeconfig,
				RuntimeClassName: runtimeClass,
				Network:          &types.NetworkConfig{Mode: "none"},
			})
			if err != nil {
				// An unregistered RuntimeClass surfaces as the friendly
				// admission wrap. Skip (not fail) so the suite still passes
				// on a kind cluster that lacks the gVisor shim.
				if runtimeClass != "" && strings.Contains(err.Error(), "RuntimeClass") {
					t.Skipf("RuntimeClass %q not registered on this cluster: %v", runtimeClass, err)
				}
				t.Fatalf("NewK8sExecutor(runtimeClass=%q): %v", runtimeClass, err)
			}
			t.Cleanup(func() { _ = exec.Close() })

			// The Pod is Ready (NewK8sExecutor blocks on readiness). A trivial
			// exec confirms the sandbox is actually executing commands.
			res, err := exec.Exec(ctx, "echo ok", 10*time.Second)
			if err != nil {
				t.Fatalf("Exec under runtimeClass %q: %v", runtimeClass, err)
			}
			if strings.TrimSpace(res.Stdout) != "ok" {
				t.Errorf("Exec stdout = %q, want \"ok\"", res.Stdout)
			}

			// Confirm gVisor is actually in force rather than the Pod silently
			// falling back to the host runtime. Two independent signals,
			// observed against this GKE Sandbox cluster (handler "gvisor"):
			//   - `dmesg` prints the gVisor boot banner ("Starting gVisor...").
			//     This is the robust signal — it is a literal product string,
			//     stable across releases (and what scripts/dev/smoke-test.sh
			//     keys on).
			//   - `uname -r` reports a synthetic kernel version ("4.4.0" on the
			//     observed release) distinct from the host's. Logged, not hard
			//     asserted, since the exact string tracks the runsc build.
			if runtimeClass == "gvisor" {
				dmesgRes, dmesgErr := exec.Exec(ctx, "dmesg 2>/dev/null | head -n 50", 10*time.Second)
				if dmesgErr != nil {
					t.Fatalf("dmesg under gvisor: %v", dmesgErr)
				}
				if !strings.Contains(strings.ToLower(dmesgRes.Stdout), "gvisor") {
					t.Errorf("dmesg under gvisor has no gVisor banner; sandbox may not be active. dmesg=%q", dmesgRes.Stdout)
				}
				unameRes, unameErr := exec.Exec(ctx, "uname -r", 10*time.Second)
				if unameErr != nil {
					t.Fatalf("uname under gvisor: %v", unameErr)
				}
				t.Logf("gvisor in force: uname -r = %q, dmesg banner present", strings.TrimSpace(unameRes.Stdout))
			}
		})
	}
}

// TestK8sRuntimeClass_KataValidatedAsString covers the kata RuntimeClass
// names at the validation boundary without requiring a Kata-capable host.
// kind cannot run Kata (it needs nested virtualisation the kind node
// container does not provide), so the Pod-creation half is skipped; the
// purpose here is to document that these names are accepted by the type
// validator (validK8sRuntimes) and flow through to the Pod spec unchanged.
func TestK8sRuntimeClass_KataValidatedAsString(t *testing.T) {
	for _, runtimeClass := range []string{"kata-qemu", "kata-fc", "kata-clh"} {
		t.Run(runtimeClass, func(t *testing.T) {
			// The RuntimeClassName flows verbatim into the Pod spec via
			// optionalRuntimeClassName; that pure mapping is asserted here.
			if got := optionalRuntimeClassName(runtimeClass); got == nil || *got != runtimeClass {
				t.Fatalf("optionalRuntimeClassName(%q) = %v, want pointer to %q", runtimeClass, got, runtimeClass)
			}
			t.Skip("kind cannot host Kata Containers (needs nested virtualisation); skipping the live Pod-creation half")
		})
	}
}

// TestK8sEgress_NoneInstallsDenyAllPolicy verifies MANIFEST SHAPE (issue
// #178): a Mode=="none" run installs a deny-all egress NetworkPolicy that
// selects exactly this Pod, carries the Egress policy type with no egress
// rules, and is torn down on Close. The Pod is also labelled
// stirrup-sandbox=true.
//
// MANIFEST-SHAPE ONLY: kindnet accepts the NetworkPolicy but does NOT enforce
// it (see allowlistEgressPolicy / K8sExecutorConfig CNI caveat). This test
// proves the object is created with the right shape, NOT that egress is
// actually denied. Enforcement is only verifiable on a Cilium/Calico cluster.
func TestK8sEgress_NoneInstallsDenyAllPolicy(t *testing.T) {
	cfg := testK8sEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	exec, err := NewK8sExecutor(ctx, K8sExecutorConfig{
		Image:            cfg.image,
		Namespace:        cfg.namespace,
		Kubeconfig:       cfg.kubeconfig,
		RuntimeClassName: cfg.runtimeClass,
		Network:          &types.NetworkConfig{Mode: "none"},
	})
	if err != nil {
		t.Fatalf("NewK8sExecutor: %v", err)
	}
	t.Cleanup(func() { _ = exec.Close() })

	clientset, err := kubeClientFromConfig(t, cfg.kubeconfig)
	if err != nil {
		t.Fatalf("build verification clientset: %v", err)
	}

	// The Pod carries the sandbox marker label.
	pod, err := clientset.CoreV1().Pods(cfg.namespace).Get(ctx, exec.podName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	if pod.Labels[k8sSandboxLabel] != "true" {
		t.Errorf("pod label %s = %q, want \"true\"", k8sSandboxLabel, pod.Labels[k8sSandboxLabel])
	}

	// Mode=="none" injects no proxy env.
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == "HTTP_PROXY" || e.Name == "HTTPS_PROXY" {
			t.Errorf("Mode==none should inject no proxy env, found %s=%s", e.Name, e.Value)
		}
	}

	np, err := clientset.NetworkingV1().NetworkPolicies(cfg.namespace).Get(ctx, networkPolicyName(exec.podName), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get networkpolicy: %v", err)
	}
	if np.Spec.PodSelector.MatchLabels[k8sPodNameLabel] != exec.podName {
		t.Errorf("policy podSelector = %v, want it to select %s=%s", np.Spec.PodSelector.MatchLabels, k8sPodNameLabel, exec.podName)
	}
	if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Errorf("policyTypes = %v, want [Egress]", np.Spec.PolicyTypes)
	}
	if len(np.Spec.Egress) != 0 {
		t.Errorf("deny-all policy must have no egress rules, got %d", len(np.Spec.Egress))
	}

	// Close removes the policy.
	if err := exec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, getErr := clientset.NetworkingV1().NetworkPolicies(cfg.namespace).Get(context.Background(), networkPolicyName(exec.podName), metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("networkpolicy %s still present 5s after Close (last get err: %v)", networkPolicyName(exec.podName), getErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestK8sEgress_AllowlistInstallsPolicyAndInjectsProxy verifies MANIFEST
// SHAPE (issue #83): a Mode=="allowlist" run installs an egress policy that
// permits DNS plus the egress-proxy peer, and injects HTTP_PROXY/HTTPS_PROXY/
// NO_PROXY pointing at EgressProxyURL.
//
// MANIFEST-SHAPE ONLY: kindnet does not enforce NetworkPolicy, so this proves
// the object/env shape, not that egress is actually confined to the proxy.
func TestK8sEgress_AllowlistInstallsPolicyAndInjectsProxy(t *testing.T) {
	cfg := testK8sEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	proxyURL := cfg.proxyURL
	exec, err := NewK8sExecutor(ctx, K8sExecutorConfig{
		Image:            cfg.image,
		Namespace:        cfg.namespace,
		Kubeconfig:       cfg.kubeconfig,
		RuntimeClassName: cfg.runtimeClass,
		Network:          &types.NetworkConfig{Mode: "allowlist", Allowlist: []string{"api.example.com"}},
		EgressProxyURL:   proxyURL,
	})
	if err != nil {
		t.Fatalf("NewK8sExecutor: %v", err)
	}
	t.Cleanup(func() { _ = exec.Close() })

	clientset, err := kubeClientFromConfig(t, cfg.kubeconfig)
	if err != nil {
		t.Fatalf("build verification clientset: %v", err)
	}

	pod, err := clientset.CoreV1().Pods(cfg.namespace).Get(ctx, exec.podName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["HTTP_PROXY"] != proxyURL || env["HTTPS_PROXY"] != proxyURL {
		t.Errorf("proxy env = %v, want HTTP(S)_PROXY=%s", env, proxyURL)
	}
	if env["NO_PROXY"] == "" {
		t.Errorf("NO_PROXY should be set in allowlist mode, env = %v", env)
	}

	np, err := clientset.NetworkingV1().NetworkPolicies(cfg.namespace).Get(ctx, networkPolicyName(exec.podName), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get networkpolicy: %v", err)
	}
	if len(np.Spec.Egress) != 2 {
		t.Fatalf("allowlist policy egress rules = %d, want 2 (dns + proxy)", len(np.Spec.Egress))
	}
	// The proxy egress rule must be port-confined to the proxy's listen port,
	// not open to every port on the proxy Pod.
	proxyRule := np.Spec.Egress[1]
	if len(proxyRule.Ports) != 1 || proxyRule.Ports[0].Port == nil || proxyRule.Ports[0].Port.IntValue() != k8sEgressProxyPort {
		t.Errorf("proxy egress rule ports = %v, want a single TCP %d", proxyRule.Ports, k8sEgressProxyPort)
	}
}

// TestK8sEgress_AllowlistRequiresProxyURL verifies the construction guard
// fails closed when Mode=="allowlist" but no EgressProxyURL is supplied. This
// runs without a cluster — the guard fires before any cluster I/O — so it is
// not skipped on a missing kubeconfig.
func TestK8sEgress_AllowlistRequiresProxyURL(t *testing.T) {
	_, err := NewK8sExecutor(context.Background(), K8sExecutorConfig{
		Image:     "busybox:latest",
		Namespace: "default",
		Network:   &types.NetworkConfig{Mode: "allowlist", Allowlist: []string{"api.example.com"}},
	})
	if err == nil {
		t.Fatal("expected error: allowlist mode without an egress proxy URL must fail closed")
	}
	if !strings.Contains(err.Error(), "proxy") {
		t.Errorf("error %q should mention the missing proxy URL", err)
	}
}

// TestK8sEgress_NoneEnforced verifies ACTUAL egress enforcement, not just
// manifest shape: on a NetworkPolicy-enforcing CNI (GKE Dataplane V2, Cilium,
// Calico) a Mode=="none" sandbox cannot reach the internet. It is gated by
// STIRRUP_TEST_ENFORCE_EGRESS (in addition to the kubeconfig gate) so it is
// skipped on kindnet — there the deny-all policy is inert and the Pod WOULD
// reach the internet, a false negative this test must never produce. The
// companion shape test (TestK8sEgress_NoneInstallsDenyAllPolicy) always runs
// and proves the policy object is correct; this proves the policy bites.
//
// The probe is a raw TCP connect to a literal public IP (1.1.1.1:443),
// deliberately DNS-free: the deny-all policy also blocks DNS, so resolving a
// name would fail for the wrong reason and muddy the signal. Under an
// enforcing CNI the SYN is dropped, the connect never completes, and the
// wrapper reports a non-zero status; an unconfined Pod connects and prints
// rc=0.
func TestK8sEgress_NoneEnforced(t *testing.T) {
	cfg := testK8sEnv(t)
	if os.Getenv("STIRRUP_TEST_ENFORCE_EGRESS") == "" {
		t.Skip("STIRRUP_TEST_ENFORCE_EGRESS not set; skipping egress-enforcement test (requires a NetworkPolicy-enforcing CNI)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	exec, err := NewK8sExecutor(ctx, K8sExecutorConfig{
		Image:            cfg.image,
		Namespace:        cfg.namespace,
		Kubeconfig:       cfg.kubeconfig,
		RuntimeClassName: cfg.runtimeClass,
		Network:          &types.NetworkConfig{Mode: "none"},
	})
	if err != nil {
		t.Fatalf("NewK8sExecutor: %v", err)
	}
	t.Cleanup(func() { _ = exec.Close() })

	// `nc` ships with busybox; the guard turns a missing tool into an explicit
	// skip rather than a non-zero status that would masquerade as "blocked".
	const probe = "command -v nc >/dev/null 2>&1 || { echo NONC; exit 0; }; " +
		"timeout 8 nc -w 6 1.1.1.1 443 </dev/null >/dev/null 2>&1; echo rc=$?"
	res, err := exec.Exec(ctx, probe, 30*time.Second)
	if err != nil {
		t.Fatalf("egress probe exec: %v", err)
	}
	out := strings.TrimSpace(res.Stdout)
	t.Logf("deny-all egress probe output: %q (stderr=%q)", out, strings.TrimSpace(res.Stderr))
	switch {
	case out == "NONC":
		t.Skip("sandbox image has no `nc`; cannot probe egress enforcement")
	case out == "rc=0":
		t.Fatalf("Mode==none sandbox reached 1.1.1.1:443 (rc=0) — deny-all egress is NOT enforced by this CNI")
	case strings.HasPrefix(out, "rc="):
		// Non-zero status: the connect was blocked, as required.
	default:
		t.Fatalf("unexpected probe output %q (want rc=<nonzero>)", out)
	}
}
