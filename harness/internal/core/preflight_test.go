package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/observability/otlp"
	"github.com/rxbynerd/stirrup/harness/internal/observability/platform"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
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
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, completionHits := metadataOnlyServer(t)
	defer srv.Close()

	report, err := Preflight(context.Background(), preflightTestConfig(t, srv.URL+"/v1"), PreflightOptions{})
	must.NoError(err)
	must.True(report.OK, "steps: %+v", report.Steps)
	must.Zero(completionHits.Load(), "a dry run must never call the completion endpoint")

	st, ok := stepStatus(report, "provider-probe:openai-compatible")
	must.True(ok, "no provider probe step")
	must.Equal(PreflightOK, st)

	st, _ = stepStatus(report, "credentials")
	must.Equal(PreflightOK, st)
}

func TestPreflight_BadCredentialFails(t *testing.T) {
	must := require.New(t)
	// The key env var is unset, so credential resolution fails during provider build.
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	report, err := Preflight(context.Background(), preflightTestConfig(t, srv.URL+"/v1"), PreflightOptions{})
	must.NoError(err)
	must.False(report.OK, "a missing credential must fail the dry run")

	st, ok := stepStatus(report, "credentials")
	must.True(ok, "no credentials step")
	must.Equal(PreflightFail, st)
}

// TestPreflight_PlatformOptionErrorFails pins that a dry run reports a
// platform option the selected profile does not accept, which
// ValidateRunConfig cannot see.
func TestPreflight_PlatformOptionErrorFails(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	for _, name := range types.ValidTraceEmitterPlatformValues() {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			config := preflightTestConfig(t, srv.URL+"/v1")
			config.TraceEmitter = types.TraceEmitterConfig{
				Type:            "otel",
				Protocol:        "http/protobuf",
				Platform:        name,
				PlatformOptions: map[string]string{"notAnOption": "x"},
			}
			report, err := Preflight(context.Background(), config, PreflightOptions{})
			must.NoError(err)
			must.False(report.OK, "an unknown platform option must fail the dry run")

			st, ok := stepStatus(report, "trace-emitter")
			must.True(ok, "no trace-emitter step in %+v", report.Steps)
			must.Equal(PreflightFail, st)

			detail, _ := stepDetail(report, "trace-emitter")
			must.Contains(detail, `does not accept option "notAnOption"`)
		})
	}
}

// TestPreflight_PlatformMissingRequiredOptionFails pins that a dry run
// reports the profile's own error for a platform selected without the
// options it requires.
func TestPreflight_PlatformMissingRequiredOptionFails(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	checked := 0
	for _, name := range types.ValidTraceEmitterPlatformValues() {
		_, resolveErr := platform.Resolve(context.Background(), name, nil, platform.Deps{})
		if resolveErr == nil || !strings.Contains(resolveErr.Error(), "requires option") {
			continue
		}
		checked++
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			config := preflightTestConfig(t, srv.URL+"/v1")
			config.TraceEmitter = types.TraceEmitterConfig{Type: "otel", Protocol: "http/protobuf", Platform: name}
			report, err := Preflight(context.Background(), config, PreflightOptions{})
			must.NoError(err)
			must.False(report.OK, "a missing required platform option must fail the dry run")

			st, ok := stepStatus(report, "trace-emitter")
			must.True(ok, "no trace-emitter step in %+v", report.Steps)
			must.Equal(PreflightFail, st)

			detail, _ := stepDetail(report, "trace-emitter")
			must.Contains(detail, resolveErr.Error())
		})
	}
	must.NotZero(checked, "no registered platform has a required option to exercise")
}

// TestPreflight_PlatformWithoutTraceEndpointFails pins that a dry run
// reports a platform that requires OTLP/HTTP but resolves no trace
// endpoint, rather than exporting to the default gRPC address over HTTP.
func TestPreflight_PlatformWithoutTraceEndpointFails(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	checked := 0
	for _, name := range types.ValidTraceEmitterPlatformValues() {
		p, err := platform.Resolve(context.Background(), name, nil, platform.Deps{})
		if err != nil || p.RequiredProtocol != otlp.ProtocolHTTPProtobuf || p.Signals[otlp.SignalTraces].Endpoint != "" {
			continue
		}
		checked++
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			config := preflightTestConfig(t, srv.URL+"/v1")
			config.TraceEmitter = types.TraceEmitterConfig{Type: "otel", Platform: name}
			report, err := Preflight(context.Background(), config, PreflightOptions{})
			must.NoError(err)
			must.False(report.OK, "a platform without a trace endpoint must fail the dry run")

			st, ok := stepStatus(report, "trace-emitter")
			must.True(ok, "no trace-emitter step in %+v", report.Steps)
			must.Equal(PreflightFail, st)

			detail, _ := stepDetail(report, "trace-emitter")
			must.Contains(detail, "has no trace endpoint")
		})
	}
	must.NotZero(checked, "no registered platform requires OTLP/HTTP without its own trace endpoint")
}

func TestPreflight_CredentialHintNamesAPIKeyRefOnProviderMismatch(t *testing.T) {
	must := require.New(t)
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	hintFor := func(cfg *types.RunConfig) string {
		t.Helper()
		report, err := Preflight(context.Background(), cfg, PreflightOptions{})
		must.NoError(err)
		for _, s := range report.Steps {
			if s.Name == "credentials" {
				return s.Hint
			}
		}
		must.FailNow("no credentials step in report")
		return ""
	}

	cfg := preflightTestConfig(t, srv.URL+"/v1")
	cfg.Provider.APIKeyRef = "secret://ANTHROPIC_API_KEY"
	t.Setenv("ANTHROPIC_API_KEY", "")
	hint := hintFor(cfg)
	for _, want := range []string{"--api-key-ref", "openai-compatible", "secret://ANTHROPIC_API_KEY"} {
		must.Contains(hint, want)
	}

	cfg = preflightTestConfig(t, srv.URL+"/v1")
	cfg.Provider.APIKeyRef = "secret://TEST_PREFLIGHT_UNSET"
	must.NotContains(hintFor(cfg), "--api-key-ref", "a non-Anthropic key reference should keep the generic hint")
}

func TestPreflight_SkipProvider_ReportsSkippedProbe(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	report, err := Preflight(context.Background(), preflightTestConfig(t, srv.URL+"/v1"), PreflightOptions{SkipProvider: true})
	must.NoError(err)

	st, ok := stepStatus(report, "provider-probe:openai-compatible")
	must.True(ok, "no provider probe step")
	must.Equal(PreflightSkip, st)
	must.True(report.OK, "a skipped probe must not fail the report; steps: %+v", report.Steps)
}

func TestPreflight_ProviderProbeFailure_FailsReport(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	report, err := Preflight(context.Background(), preflightTestConfig(t, srv.URL+"/v1"), PreflightOptions{})
	must.NoError(err)
	must.False(report.OK, "a 401 from the provider probe must fail the report")

	st, _ := stepStatus(report, "provider-probe:openai-compatible")
	must.Equal(PreflightFail, st)
}

func TestPreflight_ValidationFailureStopsEarly(t *testing.T) {
	must := require.New(t)
	cfg := preflightTestConfig(t, "http://127.0.0.1:1/v1")
	cfg.MaxTurns = 0 // invalid

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)
	must.False(report.OK, "an invalid config must fail the report")

	st, _ := stepStatus(report, "config-validation")
	must.Equal(PreflightFail, st)

	// Validation failure short-circuits, so no later steps run.
	_, ok := stepStatus(report, "credentials")
	must.False(ok, "credentials step should not run after a validation failure")
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

func TestPreflight_MCP_SkipMCPReportsSkippedProbe(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()
	mcpSrv := mcpProbeServer(t, false)
	defer mcpSrv.Close()

	cfg := preflightTestConfig(t, srv.URL+"/v1")
	cfg.Tools.MCPServers = []types.MCPServerConfig{{Name: "docs", URI: mcpSrv.URL}}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{SkipMCP: true})
	must.NoError(err)

	st, ok := stepStatus(report, "mcp:docs")
	must.True(ok, "no mcp:docs step")
	must.Equal(PreflightSkip, st)
}

func TestPreflight_MCP_ReachableServerPasses(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()
	mcpSrv := mcpProbeServer(t, false)
	defer mcpSrv.Close()

	cfg := preflightTestConfig(t, srv.URL+"/v1")
	cfg.Tools.MCPServers = []types.MCPServerConfig{{Name: "docs", URI: mcpSrv.URL}}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	st, ok := stepStatus(report, "mcp:docs")
	must.True(ok, "no mcp:docs step; steps: %+v", report.Steps)
	must.Equal(PreflightOK, st, "steps: %+v", report.Steps)
}

func TestPreflight_MCP_FailingServerFailsReport(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()
	mcpSrv := mcpProbeServer(t, true)
	defer mcpSrv.Close()

	cfg := preflightTestConfig(t, srv.URL+"/v1")
	cfg.Tools.MCPServers = []types.MCPServerConfig{{Name: "docs", URI: mcpSrv.URL}}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)
	must.False(report.OK, "a failing MCP probe must fail the report")

	st, _ := stepStatus(report, "mcp:docs")
	must.Equal(PreflightFail, st)
}

func TestPreflight_MCP_DetailIsScrubbedAndBounded(t *testing.T) {
	const secret = "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()
	mcpSrv := leakyMCPServer(t, secret)

	for _, tc := range []struct {
		name     string
		optional bool
	}{
		{"required", false},
		{"optional", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cfg := preflightTestConfig(t, srv.URL+"/v1")
			cfg.Tools.MCPServers = []types.MCPServerConfig{{Name: "docs", URI: mcpSrv.URL, Optional: tc.optional}}

			report, err := Preflight(context.Background(), cfg, PreflightOptions{})
			must.NoError(err)

			detail, _ := stepDetail(report, "mcp:docs")
			must.NotEmpty(detail, "no detail on the mcp:docs step; steps: %+v", report.Steps)
			must.NotContains(detail, secret, "detail leaked a secret")
			must.LessOrEqual(len(detail), maxDegradedErrorBytes+len("... [truncated by harness]"),
				"detail should be bounded near %d bytes", maxDegradedErrorBytes)
		})
	}
}

func TestPreflight_MCP_OptionalUnreachableWarns(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()
	mcpSrv := mcpProbeServer(t, true)
	defer mcpSrv.Close()

	cfg := preflightTestConfig(t, srv.URL+"/v1")
	cfg.Tools.MCPServers = []types.MCPServerConfig{{Name: "docs", URI: mcpSrv.URL, Optional: true}}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)
	must.True(report.OK, "an unreachable optional MCP server must not fail the report; steps: %+v", report.Steps)

	st, _ := stepStatus(report, "mcp:docs")
	must.Equal(PreflightWarn, st)
}

func TestPreflight_MCP_MissingAllowedTool(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()
	mcpSrv := mcpToolsServer(t, "search")

	for _, tc := range []struct {
		name     string
		optional bool
		wantOK   bool
		want     PreflightStatus
	}{
		{"required", false, false, PreflightFail},
		{"optional", true, true, PreflightWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cfg := preflightTestConfig(t, srv.URL+"/v1")
			cfg.Tools.MCPServers = []types.MCPServerConfig{{
				Name:         "docs",
				URI:          mcpSrv.URL,
				AllowedTools: []string{"search", "fetch"},
				Optional:     tc.optional,
			}}

			report, err := Preflight(context.Background(), cfg, PreflightOptions{})
			must.NoError(err)
			must.Equal(tc.wantOK, report.OK)

			st, _ := stepStatus(report, "mcp:docs")
			must.Equal(tc.want, st)

			detail, _ := stepDetail(report, "mcp:docs")
			must.Contains(detail, "fetch", "step detail should name the missing tool")
		})
	}
}

func TestPreflight_Egress_SkipEgressReportsSkippedStep(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	st, ok := stepStatus(report, "egress")
	must.True(ok, "no egress step")
	must.Equal(PreflightSkip, st)
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

// TestPreflight_Executor_Skip pins that a container-executor dry-run with
// SkipExecutor records the executor-probe step as a skip (no engine
// contacted); the container engine is never reachable in CI, so without
// the gate this step would fail.
func TestPreflight_Executor_SkipExecutorReportsSkippedProbe(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	cfg := preflightTestConfig(t, srv.URL+"/v1")
	cfg.Executor = types.ExecutorConfig{Type: "container", Image: "ubuntu:26.04"}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{SkipExecutor: true})
	must.NoError(err)

	st, ok := stepStatus(report, "executor-probe")
	must.True(ok, "no executor-probe step")
	must.Equal(PreflightSkip, st)

	detail, _ := stepDetail(report, "executor-probe")
	must.Contains(detail, "--no-probe-executor")
	must.True(report.OK, "a skipped executor probe must not fail the report; steps: %+v", report.Steps)
}

func TestPreflight_GitStrategy_NoneSkipsGitProbe(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	report, err := Preflight(context.Background(), preflightTestConfig(t, srv.URL+"/v1"), PreflightOptions{})
	must.NoError(err)

	st, ok := stepStatus(report, "git-strategy")
	must.True(ok, "no git-strategy step")
	must.Equal(PreflightOK, st)

	st, ok = stepStatus(report, "git-strategy-probe")
	must.True(ok, "no git-strategy-probe step")
	must.Equal(PreflightSkip, st, "the none strategy should skip the git probe")
}

func TestPreflight_GitStrategy_DeterministicProbesGitInExecutor(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	cfg := preflightTestConfig(t, srv.URL+"/v1")
	cfg.GitStrategy = types.GitStrategyConfig{Type: "deterministic"}
	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	st, ok := stepStatus(report, "git-strategy-probe")
	must.True(ok, "no git-strategy-probe step; steps: %+v", report.Steps)
	must.Equal(PreflightOK, st, "steps: %+v", report.Steps)

	detail, _ := stepDetail(report, "git-strategy-probe")
	must.Contains(detail, "inside the executor")
}

// TestPreflight_GitStrategy_MissingGitFails leaves the executor's PATH with
// a shell but no git, which is what a sandbox image without git looks like
// to the probe.
func TestPreflight_GitStrategy_MissingGitFails(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not found: %v", err)
	}
	bin := t.TempDir()
	must.NoError(os.Symlink(sh, filepath.Join(bin, "sh")), "symlink sh")
	t.Setenv("PATH", bin)

	cfg := preflightTestConfig(t, srv.URL+"/v1")
	cfg.GitStrategy = types.GitStrategyConfig{Type: "deterministic"}
	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	st, ok := stepStatus(report, "git-strategy-probe")
	must.True(ok, "no git-strategy-probe step; steps: %+v", report.Steps)
	must.Equal(PreflightFail, st)

	detail, _ := stepDetail(report, "git-strategy-probe")
	must.Contains(detail, "git is not available in the sandbox")
	must.False(report.OK, "a missing git binary must fail the dry-run")
}

func TestPreflight_GitStrategy_ContainerNotProbed(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	cfg := preflightTestConfig(t, srv.URL+"/v1")
	cfg.Executor = types.ExecutorConfig{Type: "container", Image: "ubuntu:26.04"}
	cfg.GitStrategy = types.GitStrategyConfig{Type: "deterministic"}
	report, err := Preflight(context.Background(), cfg, PreflightOptions{SkipExecutor: true})
	must.NoError(err)

	st, ok := stepStatus(report, "git-strategy-probe")
	must.True(ok, "no git-strategy-probe step")
	must.Equal(PreflightSkip, st)

	detail, _ := stepDetail(report, "git-strategy-probe")
	must.Contains(detail, "sandbox image", "the sandbox image should be reported as not probed")
	must.True(report.OK, "an unprobed container git must not fail the report; steps: %+v", report.Steps)
}

// TestPreflight_Executor_LocalUnaffectedBySkip pins that SkipExecutor is
// a container-only gate: a local executor still produces an
// executor-probe step (a skip, since it exposes no Probe), without the
// container gate reason.
func TestPreflight_Executor_LocalUnaffectedBySkip(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	cfg := preflightTestConfig(t, srv.URL+"/v1") // local executor
	report, err := Preflight(context.Background(), cfg, PreflightOptions{SkipExecutor: true})
	must.NoError(err)

	st, ok := stepStatus(report, "executor-probe")
	must.True(ok, "no executor-probe step")
	must.Equal(PreflightSkip, st)

	detail, _ := stepDetail(report, "executor-probe")
	must.NotContains(detail, "--no-probe-executor", "a local executor must not report the container gate reason")
}

// TestPreflight_Executor_NoneConstructsAndSkipsProbe pins that the none
// executor needs no special-case arm in preflight.go: it flows through the
// same construct-then-probe path as local/api, so "executor" reports a
// successful construction and "executor-probe" records a skip (NoneExecutor
// implements no Probe).
func TestPreflight_Executor_NoneConstructsAndSkipsProbe(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	cfg := preflightTestConfig(t, srv.URL+"/v1")
	cfg.Executor = types.ExecutorConfig{Type: "none"}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	st, ok := stepStatus(report, "executor")
	must.True(ok, "executor step missing")
	must.Equal(PreflightOK, st)

	detail, _ := stepDetail(report, "executor")
	must.Contains(detail, "none executor constructed")

	probeSt, ok := stepStatus(report, "executor-probe")
	must.True(ok, "executor-probe step missing")
	must.Equal(PreflightSkip, probeSt, "NoneExecutor has no Probe")
}

func TestPreflight_Egress_MalformedAllowlistFails(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	st, ok := stepStatus(report, "egress")
	must.True(ok, "egress step missing")
	must.Equal(PreflightFail, st)
}

func TestPreflight_WorkspaceExport_SkipsWithoutExportTarget(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	cfg := preflightTestConfig(t, srv.URL+"/v1") // no WorkspaceExportTo
	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)

	st, ok := stepStatus(report, "workspace-export")
	must.True(ok, "workspace-export step missing")
	must.Equal(PreflightSkip, st)
}

func TestPreflight_NilConfigErrors(t *testing.T) {
	must := require.New(t)
	_, err := Preflight(context.Background(), nil, PreflightOptions{})
	must.Error(err)
}

func TestPreflight_ShortTimeoutSurfacesAsFail(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.False(report.OK, "a deadline-exceeded probe must not report OK")
}

func TestPreflight_ZeroTimeoutUsesDefault(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	// A zero Timeout must fall back to DefaultPreflightTimeout rather than
	// producing an already-expired context.
	start := time.Now()
	report, err := Preflight(context.Background(), preflightTestConfig(t, srv.URL+"/v1"), PreflightOptions{Timeout: 0})
	must.NoError(err)
	must.True(report.OK, "expected OK with defaulted timeout; steps: %+v", report.Steps)
	must.LessOrEqual(time.Since(start), DefaultPreflightTimeout, "preflight exceeded the default timeout budget")
}

// TestPreflight_LintWarningsReachTheDryRunReport pins the reason
// permissionPolicyDetail exists. Preflight builds its SecurityLogger over
// io.Discard, so the policy_lint audit events a real run emits go
// nowhere here; without the step detail, an operator who accepted a
// finding with @stirrupLintIgnore would see a clean dry run and no trace
// of the risk they accepted.
func TestPreflight_LintWarningsReachTheDryRunReport(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	policyFile := filepath.Join(t.TempDir(), "accepted.cedar")
	policy := `@stirrupLintIgnore("wildcard-host-pattern")
permit (principal, action == Action::"tool:web_fetch", resource == Tool::"web_fetch")
    when { context.input.url like "https://*.github.com/*" };`
	must.NoError(os.WriteFile(policyFile, []byte(policy), 0o600))

	cfg := preflightTestConfig(t, srv.URL+"/v1")
	cfg.PermissionPolicy = types.PermissionPolicyConfig{Type: "policy-engine", PolicyFile: policyFile}

	report, err := Preflight(context.Background(), cfg, PreflightOptions{})
	must.NoError(err)
	must.True(report.OK, "an accepted finding must not fail the dry run; steps: %+v", report.Steps)

	detail, _ := stepDetail(report, "permission-policy")
	must.Contains(detail, permission.LintRuleWildcardHostPattern)
	must.Contains(detail, "warning")
}

func TestCredentialsHint_MentionsFlagOnlyOnProviderMismatch(t *testing.T) {
	const anthropicRef = "secret://ANTHROPIC_API_KEY"
	tests := []struct {
		name         string
		providerType string
		apiKeyRef    string
		wantMismatch bool
	}{
		{"anthropic provider with the Anthropic ref", "anthropic", anthropicRef, false},
		{"empty provider type with the Anthropic ref", "", anthropicRef, false},
		{"non-anthropic provider with the Anthropic ref", "openai-responses", anthropicRef, true},
		{"non-anthropic provider with its own ref", "openai-responses", "secret://OPENAI_API_KEY", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cfg := &types.RunConfig{Provider: types.ProviderConfig{Type: tc.providerType, APIKeyRef: tc.apiKeyRef}}
			hint := credentialsHint(cfg)
			must.Equal(tc.wantMismatch, strings.Contains(hint, "--api-key-ref"), "credentialsHint = %q", hint)
		})
	}
}

func TestPreflight_EmptyAPIKeyRef_FailsCredentialsOnDefaultEndpoint(t *testing.T) {
	must := require.New(t)
	config := preflightTestConfig(t, "")
	config.Provider.APIKeyRef = ""

	report, err := Preflight(context.Background(), config, PreflightOptions{})
	must.NoError(err)
	must.False(report.OK, "a keyless provider on api.openai.com must fail the dry run")

	idx := slices.IndexFunc(report.Steps, func(s PreflightStep) bool { return s.Name == "credentials" })
	must.GreaterOrEqual(idx, 0, "no credentials step")
	must.Equal(PreflightFail, report.Steps[idx].Status)
	must.Contains(report.Steps[idx].Detail, "requires apiKeyRef")
}

func TestPreflight_EmptyAPIKeyRef_PassesCredentialsOnCustomEndpoint(t *testing.T) {
	must := require.New(t)
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()
	config := preflightTestConfig(t, srv.URL+"/v1")
	config.Provider.APIKeyRef = ""

	report, err := Preflight(context.Background(), config, PreflightOptions{})
	must.NoError(err)

	st, ok := stepStatus(report, "credentials")
	must.True(ok, "no credentials step")
	must.Equal(PreflightOK, st)
}
