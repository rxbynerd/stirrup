package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// preflightTestConfig returns a minimal, validator-clean RunConfig
// pointing its openai-compatible provider at baseURL. mode=execution so
// the read-only-mode tool/permission invariants do not constrain the
// permission policy choice.
func preflightTestConfig(t *testing.T, baseURL string) *types.RunConfig {
	t.Helper()
	timeout := 30
	return &types.RunConfig{
		RunID:            "preflight-test",
		Mode:             "execution",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_PREFLIGHT_KEY", BaseURL: baseURL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "test"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		MaxTurns:         2,
		Timeout:          &timeout,
	}
}

// metadataOnlyServer serves GET /v1/models (the probe target) and records
// any hit to /chat/completions so the no-completion-endpoint invariant can
// be asserted at the integration level.
func metadataOnlyServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var completionHits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			completionHits.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv, &completionHits
}

func stepStatus(report *PreflightReport, name string) (PreflightStatus, bool) {
	for _, s := range report.Steps {
		if s.Name == name {
			return s.Status, true
		}
	}
	return "", false
}

func TestPreflight_AllOK_NoCompletionRequest(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, completionHits := metadataOnlyServer(t)
	defer srv.Close()

	report, err := Preflight(context.Background(), preflightTestConfig(t, srv.URL+"/v1"), PreflightOptions{})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !report.OK {
		t.Fatalf("expected report.OK; steps: %+v", report.Steps)
	}
	if got := completionHits.Load(); got != 0 {
		t.Errorf("completion endpoint hit %d times; a dry-run must never call it", got)
	}
	if st, ok := stepStatus(report, "provider-probe:openai-compatible"); !ok || st != PreflightOK {
		t.Errorf("provider probe step status = %v (ok=%v), want ok", st, ok)
	}
	if st, _ := stepStatus(report, "credentials"); st != PreflightOK {
		t.Errorf("credentials step = %v, want ok", st)
	}
}

func TestPreflight_BadCredentialFails(t *testing.T) {
	// No env var set -> credential resolution fails during provider build.
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	report, err := Preflight(context.Background(), preflightTestConfig(t, srv.URL+"/v1"), PreflightOptions{})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if report.OK {
		t.Fatal("expected report.OK == false for missing credential")
	}
	st, ok := stepStatus(report, "credentials")
	if !ok || st != PreflightFail {
		t.Errorf("credentials step status = %v (found=%v), want fail", st, ok)
	}
}

func TestPreflight_SkipProvider(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	report, err := Preflight(context.Background(), preflightTestConfig(t, srv.URL+"/v1"), PreflightOptions{SkipProvider: true})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	st, ok := stepStatus(report, "provider-probe:openai-compatible")
	if !ok || st != PreflightSkip {
		t.Errorf("provider probe step = %v (found=%v), want skip", st, ok)
	}
	if !report.OK {
		t.Errorf("a skipped probe must not fail the report; steps: %+v", report.Steps)
	}
}

func TestPreflight_ProviderProbeFailure(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	report, err := Preflight(context.Background(), preflightTestConfig(t, srv.URL+"/v1"), PreflightOptions{})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if report.OK {
		t.Fatal("expected report.OK == false for a 401 provider probe")
	}
	st, _ := stepStatus(report, "provider-probe:openai-compatible")
	if st != PreflightFail {
		t.Errorf("provider probe step = %v, want fail", st)
	}
}

func TestPreflight_ValidationFailureStopsEarly(t *testing.T) {
	cfg := preflightTestConfig(t, "http://127.0.0.1:1/v1")
	cfg.MaxTurns = 0 // invalid

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if report.OK {
		t.Fatal("expected report.OK == false for invalid config")
	}
	if st, _ := stepStatus(report, "config-validation"); st != PreflightFail {
		t.Errorf("config-validation step = %v, want fail", st)
	}
	// Validation failure short-circuits, so no later steps run.
	if _, ok := stepStatus(report, "credentials"); ok {
		t.Error("credentials step should not run after a validation failure")
	}
}

// mcpProbeServer answers MCP tools/list with an empty list (ok) or a 500
// (fail), so MCP probe paths can be exercised through core.Preflight.
func mcpProbeServer(t *testing.T, fail bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
	}))
}

func TestPreflight_MCP_Skip(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()
	mcpSrv := mcpProbeServer(t, false)
	defer mcpSrv.Close()

	cfg := preflightTestConfig(t, srv.URL+"/v1")
	cfg.Tools.MCPServers = []types.MCPServerConfig{{Name: "docs", URI: mcpSrv.URL}}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{SkipMCP: true})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	st, ok := stepStatus(report, "mcp:docs")
	if !ok || st != PreflightSkip {
		t.Errorf("mcp step = %v (found=%v), want skip", st, ok)
	}
}

func TestPreflight_MCP_OK(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()
	mcpSrv := mcpProbeServer(t, false)
	defer mcpSrv.Close()

	cfg := preflightTestConfig(t, srv.URL+"/v1")
	cfg.Tools.MCPServers = []types.MCPServerConfig{{Name: "docs", URI: mcpSrv.URL}}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	st, ok := stepStatus(report, "mcp:docs")
	if !ok || st != PreflightOK {
		t.Errorf("mcp step = %v (found=%v), want ok; steps: %+v", st, ok, report.Steps)
	}
}

func TestPreflight_MCP_Fail(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()
	mcpSrv := mcpProbeServer(t, true)
	defer mcpSrv.Close()

	cfg := preflightTestConfig(t, srv.URL+"/v1")
	cfg.Tools.MCPServers = []types.MCPServerConfig{{Name: "docs", URI: mcpSrv.URL}}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if report.OK {
		t.Fatal("a failing MCP probe must fail the report")
	}
	st, _ := stepStatus(report, "mcp:docs")
	if st != PreflightFail {
		t.Errorf("mcp step = %v, want fail", st)
	}
}

func TestPreflight_Egress_Skip(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	cfg := preflightTestConfig(t, srv.URL+"/v1")
	cfg.Executor = types.ExecutorConfig{
		Type:    "container",
		Image:   "ubuntu:26.04",
		Network: &types.NetworkConfig{Mode: "allowlist", Allowlist: []string{"example.com"}},
	}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{SkipEgress: true})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	st, ok := stepStatus(report, "egress")
	if !ok || st != PreflightSkip {
		t.Errorf("egress step = %v (found=%v), want skip", st, ok)
	}
}

// stepDetail returns the Detail of a named step.
func stepDetail(report *PreflightReport, name string) (string, bool) {
	for _, s := range report.Steps {
		if s.Name == name {
			return s.Detail, true
		}
	}
	return "", false
}

// TestPreflight_Executor_Skip is the #357 skip path: a container-executor
// dry-run with SkipExecutor must record the executor-probe step as a skip
// (no engine contacted) and keep the report honest the engine was not
// probed. The container engine is never reachable in CI, so without the
// gate this step would fail; the gate must turn it into a clean skip.
func TestPreflight_Executor_Skip(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	cfg := preflightTestConfig(t, srv.URL+"/v1")
	cfg.Executor = types.ExecutorConfig{Type: "container", Image: "ubuntu:26.04"}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{SkipExecutor: true})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	st, ok := stepStatus(report, "executor-probe")
	if !ok || st != PreflightSkip {
		t.Errorf("executor-probe step = %v (found=%v), want skip", st, ok)
	}
	if detail, _ := stepDetail(report, "executor-probe"); !strings.Contains(detail, "--no-probe-executor") {
		t.Errorf("executor-probe skip detail = %q, want it to name --no-probe-executor", detail)
	}
	if !report.OK {
		t.Errorf("a skipped executor probe must not fail the report; steps: %+v", report.Steps)
	}
}

// TestPreflight_Executor_LocalProbeRunsWhenNotSkipped pins that
// SkipExecutor is a container-only gate: a local executor still produces an
// executor-probe step (a skip, since the local executor exposes no Probe),
// and SkipExecutor does not suppress it with the gate reason.
func TestPreflight_Executor_LocalUnaffectedBySkip(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	cfg := preflightTestConfig(t, srv.URL+"/v1") // local executor
	report, err := Preflight(context.Background(), cfg, PreflightOptions{SkipExecutor: true})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	st, ok := stepStatus(report, "executor-probe")
	if !ok || st != PreflightSkip {
		t.Errorf("executor-probe step = %v (found=%v), want skip", st, ok)
	}
	if detail, _ := stepDetail(report, "executor-probe"); strings.Contains(detail, "--no-probe-executor") {
		t.Errorf("local executor-probe must not report the container gate reason; detail = %q", detail)
	}
}

func TestPreflight_Egress_Fail_MalformedAllowlist(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	cfg := preflightTestConfig(t, srv.URL+"/v1")
	// A malformed allowlist entry fails the egress probe deterministically
	// (a parse error, no DNS) without depending on network resolution.
	cfg.Executor = types.ExecutorConfig{
		Type:    "container",
		Image:   "ubuntu:26.04",
		Network: &types.NetworkConfig{Mode: "allowlist", Allowlist: []string{"bad host with spaces"}},
	}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	st, ok := stepStatus(report, "egress")
	if !ok || st != PreflightFail {
		t.Errorf("egress step = %v (found=%v), want fail", st, ok)
	}
}

func TestPreflight_WorkspaceExport_Skip(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	cfg := preflightTestConfig(t, srv.URL+"/v1") // no WorkspaceExportTo
	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	st, ok := stepStatus(report, "workspace-export")
	if !ok || st != PreflightSkip {
		t.Errorf("workspace-export step = %v (found=%v), want skip", st, ok)
	}
}

func TestPreflight_NilConfig(t *testing.T) {
	if _, err := Preflight(context.Background(), nil, PreflightOptions{}); err == nil {
		t.Fatal("Preflight(nil) should error")
	}
}

func TestPreflight_ShortTimeoutSurfacesAsFail(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	// A server that sleeps past a 1ns deadline so the provider probe's
	// context is already expired — the step must surface a clear fail, not
	// panic or hang.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	report, err := Preflight(context.Background(), preflightTestConfig(t, srv.URL+"/v1"), PreflightOptions{Timeout: time.Nanosecond})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if report.OK {
		t.Fatal("a deadline-exceeded probe must not report OK")
	}
}

func TestPreflight_TimeoutDefaulted(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	// A zero Timeout must fall back to DefaultPreflightTimeout rather than
	// producing an already-expired context.
	start := time.Now()
	report, err := Preflight(context.Background(), preflightTestConfig(t, srv.URL+"/v1"), PreflightOptions{Timeout: 0})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !report.OK {
		t.Fatalf("expected OK with defaulted timeout; steps: %+v", report.Steps)
	}
	if time.Since(start) > DefaultPreflightTimeout {
		t.Errorf("preflight exceeded the default timeout budget")
	}
}
