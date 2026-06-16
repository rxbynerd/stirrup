//go:build integration_k8s

package executor

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	"github.com/rxbynerd/stirrup/types"
)

// testAgentSandboxEnv resolves the shared integration config and additionally
// gates the Agent Sandbox suite on STIRRUP_TEST_AGENT_SANDBOX. The k8s suite's
// STIRRUP_TEST_KUBECONFIG gate still applies (via testK8sEnv); this second gate
// keeps the CRD-backed tests off by default even on a cluster the raw-Pod k8s
// suite already runs against, because the Agent Sandbox controller and its
// secure-sandbox-policy admission are a separate cluster prerequisite.
func testAgentSandboxEnv(t *testing.T) testK8sConfig {
	t.Helper()
	if os.Getenv("STIRRUP_TEST_AGENT_SANDBOX") == "" {
		t.Skip("STIRRUP_TEST_AGENT_SANDBOX not set; skipping Agent Sandbox CRD integration test (requires the agents.x-k8s.io controller + GKE secure-sandbox-policy)")
	}
	return testK8sEnv(t)
}

// dynamicClientFromConfig builds a dynamic client for driving the Sandbox CR
// directly in teardown assertions. It mirrors kubeClientFromConfig but returns
// the dynamic interface the agents.x-k8s.io group needs.
func dynamicClientFromConfig(t *testing.T, kubeconfig string) dynamic.Interface {
	t.Helper()
	restCfg, err := buildRESTConfig(kubeconfig)
	if err != nil {
		t.Fatalf("build rest config: %v", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		t.Fatalf("build dynamic client: %v", err)
	}
	return dyn
}

// TestAgentSandbox_Lifecycle exercises the full provision→exec→file-I/O→teardown
// lifecycle of the Agent Sandbox executor against a live cluster. It mirrors
// TestK8sExecutorLifecycle but provisions through the agents.x-k8s.io/v1alpha1
// Sandbox CRD (controller-created Pod) rather than a raw Pod. The constructor
// forces the gVisor runtime, so this run is sandboxed regardless of the
// RuntimeClass the env supplies.
func TestAgentSandbox_Lifecycle(t *testing.T) {
	cfg := testAgentSandboxEnv(t)
	namespace := cfg.namespace

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	exec, err := NewAgentSandboxExecutor(ctx, K8sExecutorConfig{
		Image:            cfg.image,
		Namespace:        namespace,
		Kubeconfig:       cfg.kubeconfig,
		RuntimeClassName: "gvisor",
		Network:          &types.NetworkConfig{Mode: "none"},
	})
	if err != nil {
		t.Fatalf("NewAgentSandboxExecutor: %v", err)
	}

	closed := false
	t.Cleanup(func() {
		if closed {
			return
		}
		_ = exec.Close()
	})

	// The backing Pod name (cold-create path) equals the Sandbox name; capture
	// both so teardown can assert each object is gone by name. The executor does
	// not export the Sandbox name, but on the cold path it equals podExecCore's
	// podName, which the test reads via the embedded field. This is more robust
	// than re-discovering the object by label after Close, when it may already
	// be gone.
	sandboxName := exec.sandboxName
	podName := exec.podName
	if sandboxName == "" || podName == "" {
		t.Fatalf("executor did not record sandbox/pod names (sandbox=%q pod=%q)", sandboxName, podName)
	}
	if podName != sandboxName {
		t.Fatalf("cold-create path expected pod name == sandbox name, got pod=%q sandbox=%q", podName, sandboxName)
	}

	// (a) gVisor is in force: the controller honoured runtimeClassName=gvisor on
	// the podTemplate. uname -r reports a synthetic kernel ("4.4.0" on the
	// observed runsc release) distinct from the host's. This is the same signal
	// the raw-Pod k8s suite keys on (see TestK8sRuntimeClass_Admitted).
	unameRes, err := exec.Exec(ctx, "uname -r", 10*time.Second)
	if err != nil {
		t.Fatalf("uname under gvisor: %v", err)
	}
	if !strings.Contains(unameRes.Stdout, "4.4.0") {
		t.Errorf("uname -r = %q, want it to contain %q (gVisor may not be in force)", strings.TrimSpace(unameRes.Stdout), "4.4.0")
	}

	// (b) File round-trip under /workspace exercises the shared tar-over-exec
	// path on the controller-created Pod.
	const relPath = "roundtrip/data.txt"
	const content = "agent-sandbox round-trip — 世界 🌍\n"
	if err := exec.WriteFile(ctx, relPath, content); err != nil {
		t.Fatalf("WriteFile(%q): %v", relPath, err)
	}
	got, err := exec.ReadFile(ctx, relPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", relPath, err)
	}
	if got != content {
		t.Errorf("ReadFile(%q) = %q, want %q", relPath, got, content)
	}
	entries, err := exec.ListDirectory(ctx, "roundtrip")
	if err != nil {
		t.Fatalf("ListDirectory: %v", err)
	}
	found := false
	for _, e := range entries {
		if e == "data.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("ListDirectory(roundtrip) = %v, want it to contain data.txt", entries)
	}

	// (c) Teardown. Close deletes the Sandbox with foreground propagation (so the
	// Sandbox lingers until its dependent Pod is gone) and then removes the
	// per-pod NetworkPolicy. After Close the three objects must all be gone:
	//   - the Sandbox CR     (dynamic client Get → IsNotFound)
	//   - the backing Pod     (clientset Get → IsNotFound; cold-path name == Sandbox)
	//   - the NetworkPolicy    (clientset Get on "<pod>-egress" → IsNotFound)
	if err := exec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed = true

	dyn := dynamicClientFromConfig(t, cfg.kubeconfig)
	clientset, err := kubeClientFromConfig(t, cfg.kubeconfig)
	if err != nil {
		t.Fatalf("build verification clientset: %v", err)
	}

	// Close blocks on the Sandbox being gone before removing the policy, so by
	// the time Close returns the Sandbox should already be absent; a short poll
	// absorbs read-after-delete lag on the Sandbox/Pod/policy reads.
	assertGone(t, "sandbox", func() error {
		_, getErr := dyn.Resource(sandboxGVR).Namespace(namespace).Get(context.Background(), sandboxName, metav1.GetOptions{})
		return getErr
	})
	assertGone(t, "pod", func() error {
		_, getErr := clientset.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
		return getErr
	})
	assertGone(t, "networkpolicy", func() error {
		_, getErr := clientset.NetworkingV1().NetworkPolicies(namespace).Get(context.Background(), networkPolicyName(podName), metav1.GetOptions{})
		return getErr
	})

	// Second Close must be idempotent (the Sandbox is already gone → NotFound is
	// success).
	if err := exec.Close(); err != nil {
		t.Fatalf("second Close should be idempotent, got: %v", err)
	}
}

// assertGone polls get until it reports IsNotFound, failing if the object is
// still present after a short deadline. The poll absorbs read-after-delete lag
// without masking a genuine leak.
func assertGone(t *testing.T, kind string, get func() error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := get()
		if apierrors.IsNotFound(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("%s still present 10s after Close (last get err: %v)", kind, err)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestAgentSandbox_AllowlistEnforced verifies ACTUAL allowlist egress
// enforcement for the Agent Sandbox path on a NetworkPolicy-enforcing CNI with a
// real egress proxy deployed. It mirrors TestK8sEgress_AllowlistEnforced: the
// per-Pod NetworkPolicy (installed by the executor before the Sandbox) must
// block direct internet egress while permitting the proxy peer, and the proxy's
// FQDN allowlist must admit github.com and refuse example.com.
//
// The egress plumbing is identical to the raw-Pod path — the AgentSandboxExecutor
// reuses egressPolicyFor/proxyEnvFor and the controller propagates the
// podTemplate labels the policy selects on — so this test confirms that
// equivalence holds end-to-end through the controller, not just in the manifest.
//
// Gated by STIRRUP_TEST_ENFORCE_EGRESS (in addition to the Agent Sandbox gate),
// and skipped unless a proxy peer (app=stirrup-egress-proxy) is present in the
// namespace and STIRRUP_TEST_EGRESS_PROXY_URL points at it.
func TestAgentSandbox_AllowlistEnforced(t *testing.T) {
	cfg := testAgentSandboxEnv(t)
	if os.Getenv("STIRRUP_TEST_ENFORCE_EGRESS") == "" {
		t.Skip("STIRRUP_TEST_ENFORCE_EGRESS not set; skipping egress-enforcement test (requires a NetworkPolicy-enforcing CNI)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	clientset, err := kubeClientFromConfig(t, cfg.kubeconfig)
	if err != nil {
		t.Fatalf("build verification clientset: %v", err)
	}
	peers, err := clientset.CoreV1().Pods(cfg.namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + k8sEgressProxyLabel})
	if err != nil {
		t.Fatalf("list egress proxy peers: %v", err)
	}
	if len(peers.Items) == 0 {
		t.Skipf("no egress proxy peer (app=%s) in namespace %q; deploy one to run this test", k8sEgressProxyLabel, cfg.namespace)
	}

	proxyAddr, err := url.Parse(cfg.proxyURL)
	if err != nil {
		t.Fatalf("parse proxy URL %q: %v", cfg.proxyURL, err)
	}
	proxyHost, proxyPort := proxyAddr.Hostname(), proxyAddr.Port()
	if proxyPort == "" {
		proxyPort = "8080"
	}

	exec, err := NewAgentSandboxExecutor(ctx, K8sExecutorConfig{
		Image:            cfg.image,
		Namespace:        cfg.namespace,
		Kubeconfig:       cfg.kubeconfig,
		RuntimeClassName: "gvisor",
		Network:          &types.NetworkConfig{Mode: "allowlist", Allowlist: []string{"github.com"}},
		EgressProxyURL:   cfg.proxyURL,
	})
	if err != nil {
		t.Fatalf("NewAgentSandboxExecutor: %v", err)
	}
	t.Cleanup(func() { _ = exec.Close() })

	// (a) Direct egress to the internet (DNS-free literal IP) must be blocked by
	// the allowlist NetworkPolicy: only DNS and the proxy peer are permitted.
	direct, err := exec.Exec(ctx, "command -v nc >/dev/null 2>&1 || { echo NONC; exit 0; }; timeout 8 nc -w 6 1.1.1.1 443 </dev/null >/dev/null 2>&1; echo rc=$?", 30*time.Second)
	if err != nil {
		t.Fatalf("direct-egress probe exec: %v", err)
	}
	directOut := strings.TrimSpace(direct.Stdout)
	t.Logf("direct-egress probe: %q", directOut)
	if directOut == "NONC" {
		t.Skip("sandbox image has no `nc`; cannot probe egress enforcement")
	}
	if directOut == "rc=0" {
		t.Errorf("allowlist sandbox reached 1.1.1.1:443 directly (rc=0) — NetworkPolicy is not confining egress to the proxy")
	}

	// (b) The egress proxy peer must be reachable on its listen port (the
	// NetworkPolicy permits this peer); nc connecting then closing yields rc=0.
	peer, err := exec.Exec(ctx, fmt.Sprintf("timeout 8 nc -w 6 %s %s </dev/null >/dev/null 2>&1; echo rc=$?", proxyHost, proxyPort), 30*time.Second)
	if err != nil {
		t.Fatalf("proxy-peer probe exec: %v", err)
	}
	peerOut := strings.TrimSpace(peer.Stdout)
	t.Logf("proxy-peer reach probe (%s:%s): %q", proxyHost, proxyPort, peerOut)
	if peerOut != "rc=0" {
		t.Errorf("egress proxy peer %s:%s not reachable (%q); the allowlist NetworkPolicy should permit it", proxyHost, proxyPort, peerOut)
	}

	// (c)/(d) The proxy's FQDN allowlist, verified with a REAL TLS client. The
	// stirrup proxy validates the tunnelled connection's TLS SNI, not merely the
	// plaintext CONNECT host, so the allowed path must complete a handshake. curl
	// is required for this half; when the sandbox image lacks it the test skips
	// (not fails) — the executor-contract assertions above already passed. Run
	// with STIRRUP_TEST_IMAGE=curlimages/curl:latest to exercise the full chain.
	haveCurl, err := exec.Exec(ctx, "command -v curl >/dev/null 2>&1 && echo yes || echo no", 10*time.Second)
	if err != nil {
		t.Fatalf("curl detection exec: %v", err)
	}
	if strings.TrimSpace(haveCurl.Stdout) != "yes" {
		t.Skipf("sandbox image %q has no curl; executor NetworkPolicy assertions passed. Set STIRRUP_TEST_IMAGE to a curl-capable image to exercise the proxy TLS allowlist.", cfg.image)
	}

	httpCode := func(host string) string {
		cmd := fmt.Sprintf("curl -sS -o /dev/null -w '%%{http_code}' --max-time 15 -x %s https://%s/ 2>/dev/null; echo", cfg.proxyURL, host)
		res, execErr := exec.Exec(ctx, cmd, 30*time.Second)
		if execErr != nil {
			t.Fatalf("curl %s probe exec: %v", host, execErr)
		}
		return strings.TrimSpace(res.Stdout)
	}

	// (c) An allowlisted host must complete TLS through the proxy (2xx/3xx).
	allowed := httpCode("github.com")
	t.Logf("HTTPS via proxy to github.com (allowlisted) → http_code %q", allowed)
	if !strings.HasPrefix(allowed, "2") && !strings.HasPrefix(allowed, "3") {
		t.Errorf("allowlisted github.com via proxy returned http_code %q, want 2xx/3xx", allowed)
	}

	// (d) A non-allowlisted host must be refused by the proxy (no successful
	// response; curl reports http_code 000 after the proxy's 403).
	denied := httpCode("example.com")
	t.Logf("HTTPS via proxy to example.com (not allowlisted) → http_code %q", denied)
	if strings.HasPrefix(denied, "2") || strings.HasPrefix(denied, "3") {
		t.Errorf("non-allowlisted example.com via proxy returned http_code %q, want a failure (proxy 403)", denied)
	}
}
