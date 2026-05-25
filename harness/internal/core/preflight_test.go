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
