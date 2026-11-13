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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/rxbynerd/stirrup/types"
)

// fakeKubeAPI is an HTTP stand-in for a Kubernetes API server that records
// every request. It answers access reviews, the Agent Sandbox discovery
// document and RuntimeClass reads; any other request is recorded as
// unexpected and refused.
type fakeKubeAPI struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []string
	// denied holds "verb resource[/subresource]" entries the access review
	// refuses.
	denied map[string]bool
	// sandboxAPI makes discovery serve agents.x-k8s.io/v1alpha1.
	sandboxAPI bool
	// hangReviews blocks access reviews until the request is cancelled.
	hangReviews bool
}

func newFakeKubeAPI(t *testing.T) *fakeKubeAPI {
	t.Helper()
	api := &fakeKubeAPI{denied: map[string]bool{}}
	api.srv = httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(api.srv.Close)
	return api
}

func (a *fakeKubeAPI) record(r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = append(a.requests, r.Method+" "+r.URL.Path)
}

func (a *fakeKubeAPI) recorded() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.requests...)
}

func writeStatus(w http.ResponseWriter, code int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"apiVersion": "v1", "kind": "Status", "status": "Failure", "code": code, "reason": reason,
	})
}

func (a *fakeKubeAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.record(r)
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeStatus(w, http.StatusBadRequest, "BadRequest")
			return
		}
		if a.hangReviews {
			<-r.Context().Done()
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
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersion": "authorization.k8s.io/v1",
			"kind":       "SelfSubjectAccessReview",
			"status":     map[string]any{"allowed": !a.denied[key], "reason": "no RBAC policy matched"},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/apis/agents.x-k8s.io/v1alpha1":
		if !a.sandboxAPI {
			writeStatus(w, http.StatusNotFound, "NotFound")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"agents.x-k8s.io/v1alpha1","resources":[{"name":"sandboxes","namespaced":true,"kind":"Sandbox","verbs":["get"]}]}`))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/apis/node.k8s.io/v1/runtimeclasses/"):
		writeStatus(w, http.StatusForbidden, "Forbidden")
	default:
		writeStatus(w, http.StatusInternalServerError, "InternalError")
	}
}

// unexpected returns the recorded requests that are neither an access
// review nor a read.
func (a *fakeKubeAPI) unexpected() []string {
	var out []string
	for _, req := range a.recorded() {
		if req == "POST /apis/authorization.k8s.io/v1/selfsubjectaccessreviews" || strings.HasPrefix(req, "GET ") {
			continue
		}
		out = append(out, req)
	}
	return out
}

func (a *fakeKubeAPI) kubeconfig(t *testing.T) string {
	t.Helper()
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
`, a.srv.URL)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
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
		K8sNamespace:  "stirrup-sandbox",
		K8sKubeconfig: api.kubeconfig(t),
		Network:       &types.NetworkConfig{Mode: "none"},
	}
	cfg.GitStrategy = types.GitStrategyConfig{Type: "deterministic"}
	return cfg
}

func TestPreflight_K8sExecutors_ReadOnly(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")

	for _, executorType := range []string{"k8s", "k8s-sandbox"} {
		t.Run(executorType, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			api := newFakeKubeAPI(t)
			api.sandboxAPI = true
			cfg := k8sPreflightConfig(t, executorType, api)

			report, err := Preflight(context.Background(), cfg, PreflightOptions{})
			must.NoError(err)

			detail, _ := stepDetail(report, "executor")
			check.Contains(detail, "not constructed in dry-run")
			check.Contains(detail, "executor-probe")

			status, found := stepStatus(report, "executor-probe")
			must.True(found, "no executor-probe step")
			detail, _ = stepDetail(report, "executor-probe")
			check.Equal(PreflightOK, status, "executor-probe detail: %s", detail)

			status, found = stepStatus(report, "git-strategy-probe")
			must.True(found, "no git-strategy-probe step")
			check.Equal(PreflightSkip, status)
			detail, _ = stepDetail(report, "git-strategy-probe")
			check.Contains(detail, "not started in a dry-run")

			check.Empty(api.unexpected(), "a dry run must not create, update or delete anything")
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

			report, err := Preflight(context.Background(), cfg, PreflightOptions{SkipExecutor: true})
			must.NoError(err)

			status, found := stepStatus(report, "executor-probe")
			must.True(found, "no executor-probe step")
			check.Equal(PreflightSkip, status)
			detail, _ := stepDetail(report, "executor-probe")
			check.Contains(detail, "--no-probe-executor")
			check.Empty(api.recorded(), "--no-probe-executor must make no cluster call")
			check.True(report.OK, "steps: %+v", report.Steps)
		})
	}
}

func TestPreflight_K8sExecutors_DeniedPermissionFailsStep(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")

	tests := []struct {
		executorType string
		denied       string
		wantInDetail string
	}{
		{"k8s", "create pods/exec", `"create pods/exec"`},
		{"k8s-sandbox", "create sandboxes", `"create sandboxes.agents.x-k8s.io"`},
	}
	for _, tt := range tests {
		t.Run(tt.executorType, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			api := newFakeKubeAPI(t)
			api.sandboxAPI = true
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
			check.False(report.OK)
			check.Empty(api.unexpected())
		})
	}
}

func TestPreflight_K8sSandbox_UnservedAPIFailsStep(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	api := newFakeKubeAPI(t)
	cfg := k8sPreflightConfig(t, "k8s-sandbox", api)

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	status, _ := stepStatus(report, "executor-probe")
	check.Equal(PreflightFail, status)
	detail, _ := stepDetail(report, "executor-probe")
	check.Contains(detail, "agents.x-k8s.io/v1alpha1 is not served")
}

func TestPreflight_K8sExecutor_RuntimeClassReadRefusedIsNotChecked(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	api := newFakeKubeAPI(t)
	cfg := k8sPreflightConfig(t, "k8s", api)
	cfg.Executor.Runtime = "gvisor"

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	status, _ := stepStatus(report, "executor-probe")
	check.Equal(PreflightOK, status)
	detail, _ := stepDetail(report, "executor-probe")
	check.Contains(detail, `RuntimeClass "gvisor" not checked`)
}

func TestPreflight_K8sExecutor_HonoursPreflightDeadline(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	api := newFakeKubeAPI(t)
	api.hangReviews = true
	cfg := k8sPreflightConfig(t, "k8s", api)

	started := time.Now()
	report, err := Preflight(context.Background(), cfg, PreflightOptions{Timeout: 300 * time.Millisecond})
	must.NoError(err)

	status, _ := stepStatus(report, "executor-probe")
	check.Equal(PreflightFail, status)
	check.Less(time.Since(started), 5*time.Second, "the access probe must stop at the preflight deadline")
	for _, step := range report.Steps {
		if step.Name == "executor-probe" {
			check.NotEmpty(step.Hint)
		}
	}
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
