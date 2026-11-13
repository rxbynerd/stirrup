package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/types"
)

const k8sTestNamespace = "stirrup-sandbox"

// readMode is how the fake API server answers a read.
type readMode int

const (
	readForbidden readMode = iota
	readFound
	readMissing
)

// fakeKubeAPI is an HTTP stand-in for a Kubernetes API server that records
// every request. It answers access reviews, namespace and RuntimeClass reads
// and the Agent Sandbox discovery document; any other request is answered
// with an error and reported by unexpected.
type fakeKubeAPI struct {
	srv     *httptest.Server
	release chan struct{}

	mu       sync.Mutex
	requests []string
	reviews  []string
	// denied holds "verb resource[/subresource][.group]" entries the access
	// review refuses.
	denied map[string]bool
	// sandboxAPI makes discovery serve agents.x-k8s.io/v1alpha1.
	sandboxAPI   bool
	namespace    readMode
	runtimeClass readMode
	// hangReviews blocks access reviews until the client gives up.
	hangReviews bool
}

func newFakeKubeAPI(t *testing.T) *fakeKubeAPI {
	t.Helper()
	api := &fakeKubeAPI{denied: map[string]bool{}, namespace: readFound, release: make(chan struct{})}
	api.srv = httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(api.srv.Close)
	t.Cleanup(func() { close(api.release) })
	return api
}

func (a *fakeKubeAPI) recorded() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.requests...)
}

// recordedReviews returns the access reviews received as
// "namespace verb resource[/subresource][.group]".
func (a *fakeKubeAPI) recordedReviews() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]string(nil), a.reviews...)
	sort.Strings(out)
	return out
}

func writeStatus(w http.ResponseWriter, code int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"apiVersion": "v1", "kind": "Status", "status": "Failure", "code": code, "reason": reason,
	})
}

func writeRead(w http.ResponseWriter, mode readMode, found string) {
	switch mode {
	case readFound:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(found))
	case readMissing:
		writeStatus(w, http.StatusNotFound, "NotFound")
	default:
		writeStatus(w, http.StatusForbidden, "Forbidden")
	}
}

func (a *fakeKubeAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.requests = append(a.requests, r.Method+" "+r.URL.Path)
	namespace, runtimeClass, sandboxAPI, hang := a.namespace, a.runtimeClass, a.sandboxAPI, a.hangReviews
	a.mu.Unlock()

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeStatus(w, http.StatusBadRequest, "BadRequest")
			return
		}
		if hang {
			select {
			case <-r.Context().Done():
			case <-a.release:
			}
			return
		}
		obj, _, err := scheme.Codecs.UniversalDeserializer().Decode(body, nil, &authorizationv1.SelfSubjectAccessReview{})
		review, isReview := obj.(*authorizationv1.SelfSubjectAccessReview)
		if err != nil || !isReview || review.Spec.ResourceAttributes == nil {
			writeStatus(w, http.StatusBadRequest, "BadRequest")
			return
		}
		attrs := review.Spec.ResourceAttributes
		key := attrs.Verb + " " + attrs.Resource
		if attrs.Subresource != "" {
			key += "/" + attrs.Subresource
		}
		if attrs.Group != "" {
			key += "." + attrs.Group
		}
		a.mu.Lock()
		a.reviews = append(a.reviews, attrs.Namespace+" "+key)
		denied := a.denied[key]
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": "authorization.k8s.io/v1",
			"kind":       "SelfSubjectAccessReview",
			"status":     map[string]any{"allowed": !denied, "reason": "no RBAC policy matched"},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/apis/agents.x-k8s.io/v1alpha1":
		if !sandboxAPI {
			writeStatus(w, http.StatusNotFound, "NotFound")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"agents.x-k8s.io/v1alpha1","resources":[{"name":"sandboxes","namespaced":true,"kind":"Sandbox","verbs":["get"]}]}`))
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/"+k8sTestNamespace:
		writeRead(w, namespace, `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"`+k8sTestNamespace+`"}}`)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/apis/node.k8s.io/v1/runtimeclasses/"):
		writeRead(w, runtimeClass, `{"apiVersion":"node.k8s.io/v1","kind":"RuntimeClass","metadata":{"name":"x"},"handler":"runsc"}`)
	default:
		writeStatus(w, http.StatusInternalServerError, "InternalError")
	}
}

// unexpected returns the recorded requests other than the access reviews and
// the reads the probe is allowed to make. A WebSocket exec is a GET on an exec
// path, so it is reported here.
func (a *fakeKubeAPI) unexpected() []string {
	var out []string
	for _, req := range a.recorded() {
		switch {
		case req == "POST /apis/authorization.k8s.io/v1/selfsubjectaccessreviews",
			req == "GET /apis/agents.x-k8s.io/v1alpha1",
			req == "GET /api/v1/namespaces/"+k8sTestNamespace,
			strings.HasPrefix(req, "GET /apis/node.k8s.io/v1/runtimeclasses/"):
		default:
			out = append(out, req)
		}
	}
	return out
}

func writeKubeconfig(t *testing.T, serverURL string) string {
	t.Helper()
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: fake
  cluster:
    server: %s
contexts:
- name: fake
  context:
    cluster: fake
    user: fake
current-context: fake
users:
- name: fake
  user: {}
`, serverURL)
	must.NoError(os.WriteFile(path, []byte(content), 0o600))
	return path
}

func k8sPreflightConfig(t *testing.T, executorType string, api *fakeKubeAPI) *types.RunConfig {
	t.Helper()
	provider, _ := metadataOnlyServer(t)
	t.Cleanup(provider.Close)
	cfg := preflightTestConfig(t, provider.URL+"/v1")
	cfg.Executor = types.ExecutorConfig{
		Type:          executorType,
		Image:         "stirrup-sandbox:test",
		K8sNamespace:  k8sTestNamespace,
		K8sKubeconfig: writeKubeconfig(t, api.srv.URL),
		Runtime:       "gvisor",
		Network:       &types.NetworkConfig{Mode: "none"},
	}
	if executorType == "k8s-sandbox" {
		cfg.Executor.Runtime = ""
	}
	cfg.GitStrategy = types.GitStrategyConfig{Type: "deterministic"}
	return cfg
}

func stepHint(report *PreflightReport, name string) string {
	for _, s := range report.Steps {
		if s.Name == name {
			return s.Hint
		}
	}
	return ""
}

func TestPreflight_K8sExecutors_ReadOnly(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")

	tests := []struct {
		executorType string
		wantReviews  []string
	}{
		{"k8s", []string{
			k8sTestNamespace + " create networkpolicies.networking.k8s.io",
			k8sTestNamespace + " create pods",
			k8sTestNamespace + " create pods/exec",
			k8sTestNamespace + " delete networkpolicies.networking.k8s.io",
			k8sTestNamespace + " delete pods",
			k8sTestNamespace + " get pods",
		}},
		{"k8s-sandbox", []string{
			k8sTestNamespace + " create networkpolicies.networking.k8s.io",
			k8sTestNamespace + " create pods/exec",
			k8sTestNamespace + " create sandboxes.agents.x-k8s.io",
			k8sTestNamespace + " delete networkpolicies.networking.k8s.io",
			k8sTestNamespace + " delete sandboxes.agents.x-k8s.io",
			k8sTestNamespace + " get sandboxes.agents.x-k8s.io",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.executorType, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			api := newFakeKubeAPI(t)
			api.sandboxAPI = true
			api.runtimeClass = readFound
			cfg := k8sPreflightConfig(t, tt.executorType, api)

			report, err := Preflight(context.Background(), cfg, PreflightOptions{})
			must.NoError(err)

			detail, _ := stepDetail(report, "executor")
			check.Equal(tt.executorType+" executor not constructed in dry-run", detail)

			status, found := stepStatus(report, "executor-probe")
			must.True(found, "no executor-probe step")
			detail, _ = stepDetail(report, "executor-probe")
			check.Equal(PreflightOK, status, "executor-probe detail: %s", detail)
			check.Contains(detail, "6 required permissions granted")

			status, found = stepStatus(report, "git-strategy-probe")
			must.True(found, "no git-strategy-probe step")
			check.Equal(PreflightSkip, status)
			detail, _ = stepDetail(report, "git-strategy-probe")
			check.Contains(detail, "not started in a dry-run")

			check.Equal(tt.wantReviews, api.recordedReviews(), "every review names the namespace and API group")
			check.Empty(api.unexpected(), "a dry run must not create, update, delete or exec anything")
			check.True(report.OK, "steps: %+v", report.Steps)
		})
	}
}

func TestPreflight_K8sExecutors_SkipExecutorMakesNoClusterCall(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")

	for _, executorType := range []string{"k8s", "k8s-sandbox"} {
		t.Run(executorType, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			api := newFakeKubeAPI(t)
			cfg := k8sPreflightConfig(t, executorType, api)
			cfg.Executor.K8sKubeconfig = filepath.Join(t.TempDir(), "missing-kubeconfig")

			report, err := Preflight(context.Background(), cfg, PreflightOptions{SkipExecutor: true})
			must.NoError(err)

			status, found := stepStatus(report, "executor-probe")
			must.True(found, "no executor-probe step")
			check.Equal(PreflightSkip, status)
			detail, _ := stepDetail(report, "executor-probe")
			check.Contains(detail, "--no-probe-executor")
			detail, _ = stepDetail(report, "executor")
			check.NotContains(detail, "checked", "the executor step must not claim access was checked")
			check.Empty(api.recorded(), "--no-probe-executor must make no cluster call")
			check.True(report.OK, "steps: %+v", report.Steps)
		})
	}
}

func TestPreflight_K8sExecutors_DeniedPermissionFailsStepWithRoleHint(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")

	tests := []struct {
		executorType string
		denied       string
		wantInDetail string
		wantManifest string
	}{
		{"k8s", "create pods/exec", `"create pods/exec"`, "examples/k8s/rbac.yaml"},
		{"k8s-sandbox", "create sandboxes.agents.x-k8s.io", `"create sandboxes.agents.x-k8s.io"`, "rbac-agent-sandbox.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.executorType, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			api := newFakeKubeAPI(t)
			api.sandboxAPI = true
			api.runtimeClass = readFound
			api.denied[tt.denied] = true
			cfg := k8sPreflightConfig(t, tt.executorType, api)

			report, err := Preflight(context.Background(), cfg, PreflightOptions{})
			must.NoError(err)

			status, found := stepStatus(report, "executor-probe")
			must.True(found, "no executor-probe step")
			check.Equal(PreflightFail, status)
			detail, _ := stepDetail(report, "executor-probe")
			check.Contains(detail, tt.wantInDetail)
			check.Contains(detail, `"stirrup-sandbox"`)
			hint := stepHint(report, "executor-probe")
			check.Contains(hint, "bind a Role")
			check.Contains(hint, tt.wantManifest)
			check.NotContains(hint, "reachable")
			check.NotContains(hint, "runtimeclass")
			check.NotContains(hint, "Agent Sandbox controller")
			check.False(report.OK)
			check.Empty(api.unexpected())
		})
	}
}

func TestPreflight_K8sSandbox_UnservedAPIFailsStepWithControllerHint(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	api := newFakeKubeAPI(t)
	api.runtimeClass = readFound
	cfg := k8sPreflightConfig(t, "k8s-sandbox", api)

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	status, _ := stepStatus(report, "executor-probe")
	check.Equal(PreflightFail, status)
	detail, _ := stepDetail(report, "executor-probe")
	check.Contains(detail, "agents.x-k8s.io/v1alpha1 is not served")
	hint := stepHint(report, "executor-probe")
	check.Contains(hint, "Agent Sandbox controller")
	check.NotContains(hint, "bind a Role")
	check.NotContains(hint, "reachable")
}

func TestPreflight_K8sExecutor_MissingRuntimeClassFailsStepWithRuntimeClassHint(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	api := newFakeKubeAPI(t)
	api.runtimeClass = readMissing
	cfg := k8sPreflightConfig(t, "k8s", api)

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	status, _ := stepStatus(report, "executor-probe")
	check.Equal(PreflightFail, status)
	detail, _ := stepDetail(report, "executor-probe")
	check.Contains(detail, `RuntimeClass "gvisor" does not exist`)
	hint := stepHint(report, "executor-probe")
	check.Contains(hint, "kubectl get runtimeclass")
	check.NotContains(hint, "bind a Role")
	check.NotContains(hint, "reachable")
}

func TestPreflight_K8sExecutor_MissingNamespaceFailsStepWithNamespaceHint(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	api := newFakeKubeAPI(t)
	api.namespace = readMissing
	api.runtimeClass = readFound
	cfg := k8sPreflightConfig(t, "k8s", api)

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	status, _ := stepStatus(report, "executor-probe")
	check.Equal(PreflightFail, status)
	detail, _ := stepDetail(report, "executor-probe")
	check.Contains(detail, `namespace "stirrup-sandbox" does not exist`)
	hint := stepHint(report, "executor-probe")
	check.Contains(hint, "executor.k8sNamespace")
	check.NotContains(hint, "bind a Role")
	check.NotContains(hint, "kubectl get runtimeclass")
}

func TestPreflight_K8sExecutor_RefusedOptionalReadsAreNotCheckedAndStayOK(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	api := newFakeKubeAPI(t)
	api.namespace = readForbidden
	api.runtimeClass = readForbidden
	cfg := k8sPreflightConfig(t, "k8s", api)

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	status, _ := stepStatus(report, "executor-probe")
	check.Equal(PreflightOK, status)
	detail, _ := stepDetail(report, "executor-probe")
	check.Contains(detail, `RuntimeClass "gvisor" not checked`)
	check.Contains(detail, `namespace "stirrup-sandbox" not checked`)
	check.True(report.OK)
}

func TestPreflight_K8sExecutor_HonoursPreflightDeadline(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	api := newFakeKubeAPI(t)
	api.hangReviews = true
	cfg := k8sPreflightConfig(t, "k8s", api)

	done := make(chan *PreflightReport, 1)
	go func() {
		report, _ := Preflight(context.Background(), cfg, PreflightOptions{Timeout: 300 * time.Millisecond})
		done <- report
	}()
	var report *PreflightReport
	select {
	case report = <-done:
	case <-time.After(5 * time.Second):
		must.FailNow("the access probe did not stop at the preflight deadline")
	}

	status, _ := stepStatus(report, "executor-probe")
	check.Equal(PreflightFail, status)
	detail, _ := stepDetail(report, "executor-probe")
	check.Contains(detail, "context deadline exceeded")
	check.Contains(api.recorded(), "POST /apis/authorization.k8s.io/v1/selfsubjectaccessreviews", "the probe must have reached the API server")
	hint := stepHint(report, "executor-probe")
	check.Contains(hint, "--dry-run-timeout")
	check.NotContains(hint, "bind a Role")
}

func TestPreflight_K8sExecutor_UnreachableAPIServerFailsStepWithReachabilityHint(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	api := newFakeKubeAPI(t)
	cfg := k8sPreflightConfig(t, "k8s", api)
	cfg.Executor.K8sKubeconfig = writeKubeconfig(t, "https://127.0.0.1:1")

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	status, _ := stepStatus(report, "executor-probe")
	check.Equal(PreflightFail, status)
	detail, _ := stepDetail(report, "executor-probe")
	check.Contains(detail, `access review for "create pods"`)
	hint := stepHint(report, "executor-probe")
	check.Contains(hint, "reachable")
	check.NotContains(hint, "bind a Role")
	check.NotContains(hint, "kubectl get runtimeclass")
}

// TestPreflight_K8sExecutors_ConstructorNotReached runs a dry run against a
// kubeconfig that cannot be loaded. Only the probe reads the kubeconfig, so a
// construction step that succeeded proves the real constructors were not run.
func TestPreflight_K8sExecutors_ConstructorNotReached(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")

	for _, executorType := range []string{"k8s", "k8s-sandbox"} {
		t.Run(executorType, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			api := newFakeKubeAPI(t)
			cfg := k8sPreflightConfig(t, executorType, api)
			cfg.Executor.K8sKubeconfig = filepath.Join(t.TempDir(), "missing-kubeconfig")

			report, err := Preflight(context.Background(), cfg, PreflightOptions{})
			must.NoError(err)

			status, _ := stepStatus(report, "executor")
			check.Equal(PreflightOK, status, "the executor step must not construct anything")
			status, _ = stepStatus(report, "executor-probe")
			check.Equal(PreflightFail, status)
			detail, _ := stepDetail(report, "executor-probe")
			check.Contains(detail, "build kube rest config")
			hint := stepHint(report, "executor-probe")
			check.Contains(hint, "--k8s-kubeconfig")
			check.NotContains(hint, "bind a Role")
			check.NotContains(hint, "reachable")
		})
	}
}

func TestPreflight_K8sExecutors_UnsupportedNetworkModeFailsConfigValidation(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")

	for _, executorType := range []string{"k8s", "k8s-sandbox"} {
		t.Run(executorType, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			api := newFakeKubeAPI(t)
			cfg := k8sPreflightConfig(t, executorType, api)
			cfg.Executor.Network = &types.NetworkConfig{Mode: "host"}

			report, err := Preflight(context.Background(), cfg, PreflightOptions{})
			must.NoError(err)

			status, _ := stepStatus(report, "config-validation")
			check.Equal(PreflightFail, status)
			detail, _ := stepDetail(report, "config-validation")
			check.Contains(detail, `executor.network.mode must be "none" or "allowlist"`)
			_, hasExecutorStep := stepStatus(report, "executor")
			check.False(hasExecutorStep, "no component is built after a failed config-validation")
			check.Empty(api.recorded(), "an invalid network mode must be rejected before any cluster call")
		})
	}
}

func TestPreflight_K8sExecutor_EmptyRuntimeWarnsAboutClusterDefaultClass(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")

	for _, skipExecutor := range []bool{false, true} {
		t.Run(fmt.Sprintf("skipExecutor=%t", skipExecutor), func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			api := newFakeKubeAPI(t)
			cfg := k8sPreflightConfig(t, "k8s", api)
			cfg.Executor.Runtime = ""

			report, err := Preflight(context.Background(), cfg, PreflightOptions{SkipExecutor: skipExecutor})
			must.NoError(err)

			status, found := stepStatus(report, "executor-runtime")
			must.True(found, "no executor-runtime step; steps: %+v", report.Steps)
			check.Equal(PreflightWarn, status)
			detail, _ := stepDetail(report, "executor-runtime")
			check.Equal(executor.K8sDefaultRuntimeClassWarning, detail)
			check.True(report.OK, "a warning must not fail the report")
			if !skipExecutor {
				detail, _ = stepDetail(report, "executor-probe")
				check.NotContains(detail, "RuntimeClass", "no class is requested, so none is read")
			}
		})
	}
}

func TestPreflight_K8sExecutors_NamedRuntimeRecordsNoRuntimeWarning(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")

	for _, executorType := range []string{"k8s", "k8s-sandbox"} {
		t.Run(executorType, func(t *testing.T) {
			must := require.New(t)
			api := newFakeKubeAPI(t)
			api.sandboxAPI = true
			api.runtimeClass = readFound
			cfg := k8sPreflightConfig(t, executorType, api)

			report, err := Preflight(context.Background(), cfg, PreflightOptions{})
			must.NoError(err)

			_, found := stepStatus(report, "executor-runtime")
			must.False(found, "steps: %+v", report.Steps)
		})
	}
}

func TestPreflight_K8sExecutors_LazyToolsPreviewWorksWithoutAnExecutor(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")

	for _, executorType := range []string{"k8s", "k8s-sandbox"} {
		t.Run(executorType, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			api := newFakeKubeAPI(t)
			cfg := k8sPreflightConfig(t, executorType, api)
			cfg.Tools.LazyLoading = types.LazyToolLoadingConfig{Enabled: true}

			report, err := Preflight(context.Background(), cfg, PreflightOptions{SkipExecutor: true})
			must.NoError(err)

			status, found := stepStatus(report, "lazy-tools")
			must.True(found, "no lazy-tools step; steps: %+v", report.Steps)
			check.Equal(PreflightOK, status)
			detail, _ := stepDetail(report, "lazy-tools")
			check.Contains(detail, "strategy ")
			check.Empty(api.recorded())
		})
	}
}
