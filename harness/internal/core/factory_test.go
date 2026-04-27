package core

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// --- buildRouter ---

func TestBuildRouter_Static(t *testing.T) {
	r := buildRouter(types.ModelRouterConfig{
		Type:     "static",
		Provider: "anthropic",
		Model:    "claude-sonnet-4-6",
	}, "anthropic")

	sel := r.Select(context.TODO(), router.RouterContext{})
	if sel.Provider != "anthropic" || sel.Model != "claude-sonnet-4-6" {
		t.Fatalf("got %+v", sel)
	}
}

func TestBuildRouter_StaticDefaultsProvider(t *testing.T) {
	r := buildRouter(types.ModelRouterConfig{
		Type:  "static",
		Model: "custom-model",
	}, "bedrock")

	sel := r.Select(context.TODO(), router.RouterContext{})
	if sel.Provider != "bedrock" {
		t.Fatalf("expected provider bedrock, got %q", sel.Provider)
	}
}

func TestBuildRouter_PerMode(t *testing.T) {
	r := buildRouter(types.ModelRouterConfig{
		Type:       "per-mode",
		Provider:   "anthropic",
		Model:      "default-model",
		ModeModels: map[string]string{"planning": "bedrock/plan-model"},
	}, "anthropic")

	sel := r.Select(context.TODO(), router.RouterContext{Mode: "planning"})
	if sel.Provider != "bedrock" || sel.Model != "plan-model" {
		t.Fatalf("per-mode planning: got %+v", sel)
	}
}

func TestBuildRouter_Dynamic(t *testing.T) {
	r := buildRouter(types.ModelRouterConfig{
		Type:     "dynamic",
		Provider: "anthropic",
		Model:    "default-model",
	}, "anthropic")

	sel := r.Select(context.TODO(), router.RouterContext{Turn: 0})
	if sel.Provider != "anthropic" {
		t.Fatalf("dynamic: got %+v", sel)
	}
}

func TestBuildRouter_DefaultFallback(t *testing.T) {
	r := buildRouter(types.ModelRouterConfig{}, "")

	sel := r.Select(context.TODO(), router.RouterContext{})
	if sel.Provider != "anthropic" || sel.Model != "claude-sonnet-4-6" {
		t.Fatalf("default fallback: got %+v", sel)
	}
}

// --- buildPerModeRouter ---

func TestBuildPerModeRouter_ModeModelWithSlash(t *testing.T) {
	r := buildPerModeRouter(types.ModelRouterConfig{
		ModeModels: map[string]string{"review": "bedrock/review-model"},
	}, "anthropic")

	sel := r.Select(context.TODO(), router.RouterContext{Mode: "review"})
	if sel.Provider != "bedrock" || sel.Model != "review-model" {
		t.Fatalf("got %+v", sel)
	}
}

func TestBuildPerModeRouter_ModeModelWithoutSlash(t *testing.T) {
	r := buildPerModeRouter(types.ModelRouterConfig{
		ModeModels: map[string]string{"review": "review-model"},
	}, "anthropic")

	sel := r.Select(context.TODO(), router.RouterContext{Mode: "review"})
	if sel.Provider != "anthropic" || sel.Model != "review-model" {
		t.Fatalf("got %+v", sel)
	}
}

func TestBuildPerModeRouter_DefaultsApplied(t *testing.T) {
	r := buildPerModeRouter(types.ModelRouterConfig{}, "")

	sel := r.Select(context.TODO(), router.RouterContext{Mode: "execution"})
	if sel.Provider != "anthropic" || sel.Model != "claude-sonnet-4-6" {
		t.Fatalf("defaults: got %+v", sel)
	}
}

// --- buildDynamicRouter ---

func TestBuildDynamicRouter_Defaults(t *testing.T) {
	r := buildDynamicRouter(types.ModelRouterConfig{}, "")

	// Turn 0, no tokens — should get the default or cheap selection.
	sel := r.Select(context.TODO(), router.RouterContext{Turn: 0})
	if sel.Provider != "anthropic" {
		t.Fatalf("expected anthropic, got %q", sel.Provider)
	}
}

func TestBuildDynamicRouter_CustomThresholds(t *testing.T) {
	r := buildDynamicRouter(types.ModelRouterConfig{
		ExpensiveTurnThreshold:  5,
		ExpensiveTokenThreshold: 10000,
		CheapModel:              "haiku",
		ExpensiveModel:          "opus",
	}, "anthropic")

	// Under thresholds → cheap.
	sel := r.Select(context.TODO(), router.RouterContext{Turn: 0, LastStopReason: "tool_use"})
	if sel.Model != "haiku" {
		t.Fatalf("expected haiku under threshold, got %q", sel.Model)
	}

	// Over turn threshold → expensive.
	sel = r.Select(context.TODO(), router.RouterContext{Turn: 6})
	if sel.Model != "opus" {
		t.Fatalf("expected opus over threshold, got %q", sel.Model)
	}
}

// --- buildPromptBuilder ---

func TestBuildPromptBuilder_Default(t *testing.T) {
	pb := buildPromptBuilder(types.PromptBuilderConfig{Type: "default"}, "")
	if _, ok := pb.(*prompt.DefaultPromptBuilder); !ok {
		t.Fatalf("expected DefaultPromptBuilder, got %T", pb)
	}
}

func TestBuildPromptBuilder_Empty(t *testing.T) {
	pb := buildPromptBuilder(types.PromptBuilderConfig{}, "")
	if _, ok := pb.(*prompt.DefaultPromptBuilder); !ok {
		t.Fatalf("expected DefaultPromptBuilder for empty type, got %T", pb)
	}
}

func TestBuildPromptBuilder_Composed(t *testing.T) {
	pb := buildPromptBuilder(types.PromptBuilderConfig{Type: "composed"}, "")
	if _, ok := pb.(*prompt.ComposedPromptBuilder); !ok {
		t.Fatalf("expected ComposedPromptBuilder, got %T", pb)
	}
}

func TestBuildPromptBuilder_UnknownFallsBackToDefault(t *testing.T) {
	pb := buildPromptBuilder(types.PromptBuilderConfig{Type: "nonexistent"}, "")
	if _, ok := pb.(*prompt.DefaultPromptBuilder); !ok {
		t.Fatalf("expected DefaultPromptBuilder for unknown type, got %T", pb)
	}
}

func TestBuildPromptBuilder_SystemPromptOverride(t *testing.T) {
	pb := buildPromptBuilder(types.PromptBuilderConfig{Type: "default"}, "Custom system prompt")
	if _, ok := pb.(*prompt.ComposedPromptBuilder); !ok {
		t.Fatalf("expected ComposedPromptBuilder for override, got %T", pb)
	}
}

// --- buildContextStrategy ---

func TestBuildContextStrategy_SlidingWindow(t *testing.T) {
	cs := buildContextStrategy(types.ContextStrategyConfig{Type: "sliding-window"}, nil, "", nil)
	if _, ok := cs.(*contextpkg.SlidingWindowStrategy); !ok {
		t.Fatalf("expected SlidingWindowStrategy, got %T", cs)
	}
}

func TestBuildContextStrategy_Empty(t *testing.T) {
	cs := buildContextStrategy(types.ContextStrategyConfig{}, nil, "", nil)
	if _, ok := cs.(*contextpkg.SlidingWindowStrategy); !ok {
		t.Fatalf("expected SlidingWindowStrategy for empty type, got %T", cs)
	}
}

func TestBuildContextStrategy_Summarise(t *testing.T) {
	cs := buildContextStrategy(types.ContextStrategyConfig{Type: "summarise"}, nil, "model", nil)
	if _, ok := cs.(*contextpkg.SummariseStrategy); !ok {
		t.Fatalf("expected SummariseStrategy, got %T", cs)
	}
}

func TestBuildContextStrategy_OffloadToFile(t *testing.T) {
	exec, _ := executor.NewLocalExecutor(t.TempDir())
	cs := buildContextStrategy(types.ContextStrategyConfig{Type: "offload-to-file"}, nil, "", exec)
	if _, ok := cs.(*contextpkg.OffloadToFileStrategy); !ok {
		t.Fatalf("expected OffloadToFileStrategy, got %T", cs)
	}
}

func TestBuildContextStrategy_UnknownFallsBack(t *testing.T) {
	cs := buildContextStrategy(types.ContextStrategyConfig{Type: "nonexistent"}, nil, "", nil)
	if _, ok := cs.(*contextpkg.SlidingWindowStrategy); !ok {
		t.Fatalf("expected SlidingWindowStrategy for unknown type, got %T", cs)
	}
}

// --- buildEditStrategy ---

func TestBuildEditStrategy_WholeFile(t *testing.T) {
	es := buildEditStrategy(types.EditStrategyConfig{Type: "whole-file"})
	if _, ok := es.(*edit.WholeFileStrategy); !ok {
		t.Fatalf("expected WholeFileStrategy, got %T", es)
	}
}

func TestBuildEditStrategy_Empty(t *testing.T) {
	es := buildEditStrategy(types.EditStrategyConfig{})
	if _, ok := es.(*edit.WholeFileStrategy); !ok {
		t.Fatalf("expected WholeFileStrategy for empty type, got %T", es)
	}
}

func TestBuildEditStrategy_SearchReplace(t *testing.T) {
	es := buildEditStrategy(types.EditStrategyConfig{Type: "search-replace"})
	if _, ok := es.(*edit.SearchReplaceStrategy); !ok {
		t.Fatalf("expected SearchReplaceStrategy, got %T", es)
	}
}

func TestBuildEditStrategy_Udiff(t *testing.T) {
	es := buildEditStrategy(types.EditStrategyConfig{Type: "udiff"})
	if _, ok := es.(*edit.UdiffStrategy); !ok {
		t.Fatalf("expected UdiffStrategy, got %T", es)
	}
}

func TestBuildEditStrategy_Multi(t *testing.T) {
	es := buildEditStrategy(types.EditStrategyConfig{Type: "multi"})
	if _, ok := es.(*edit.MultiStrategy); !ok {
		t.Fatalf("expected MultiStrategy, got %T", es)
	}
}

func TestBuildEditStrategy_CustomFuzzyThreshold(t *testing.T) {
	threshold := 0.95
	es := buildEditStrategy(types.EditStrategyConfig{Type: "udiff", FuzzyThreshold: &threshold})
	if _, ok := es.(*edit.UdiffStrategy); !ok {
		t.Fatalf("expected UdiffStrategy with custom threshold, got %T", es)
	}
}

func TestBuildEditStrategy_UnknownFallsBack(t *testing.T) {
	es := buildEditStrategy(types.EditStrategyConfig{Type: "nonexistent"})
	if _, ok := es.(*edit.WholeFileStrategy); !ok {
		t.Fatalf("expected WholeFileStrategy for unknown type, got %T", es)
	}
}

// --- buildVerifier ---

func TestBuildVerifier_None(t *testing.T) {
	v := buildVerifier(types.VerifierConfig{Type: "none"}, nil)
	if _, ok := v.(*verifier.NoneVerifier); !ok {
		t.Fatalf("expected NoneVerifier, got %T", v)
	}
}

func TestBuildVerifier_Empty(t *testing.T) {
	v := buildVerifier(types.VerifierConfig{}, nil)
	if _, ok := v.(*verifier.NoneVerifier); !ok {
		t.Fatalf("expected NoneVerifier for empty type, got %T", v)
	}
}

func TestBuildVerifier_TestRunner(t *testing.T) {
	v := buildVerifier(types.VerifierConfig{Type: "test-runner", Command: "go test ./..."}, nil)
	if _, ok := v.(*verifier.TestRunnerVerifier); !ok {
		t.Fatalf("expected TestRunnerVerifier, got %T", v)
	}
}

func TestBuildVerifier_LLMJudge(t *testing.T) {
	v := buildVerifier(types.VerifierConfig{Type: "llm-judge", Criteria: "test criteria"}, nil)
	if _, ok := v.(*verifier.LLMJudgeVerifier); !ok {
		t.Fatalf("expected LLMJudgeVerifier, got %T", v)
	}
}

func TestBuildVerifier_Composite(t *testing.T) {
	v := buildVerifier(types.VerifierConfig{
		Type: "composite",
		Verifiers: []types.VerifierConfig{
			{Type: "none"},
			{Type: "test-runner", Command: "echo ok"},
		},
	}, nil)
	if _, ok := v.(*verifier.CompositeVerifier); !ok {
		t.Fatalf("expected CompositeVerifier, got %T", v)
	}
}

func TestBuildVerifier_UnknownFallsBack(t *testing.T) {
	v := buildVerifier(types.VerifierConfig{Type: "nonexistent"}, nil)
	if _, ok := v.(*verifier.NoneVerifier); !ok {
		t.Fatalf("expected NoneVerifier for unknown type, got %T", v)
	}
}

// --- buildPermissionPolicy ---

func TestBuildPermissionPolicy_AllowAll(t *testing.T) {
	pp := buildPermissionPolicy(types.PermissionPolicyConfig{Type: "allow-all"}, nil, nil)
	if _, ok := pp.(*permission.AllowAll); !ok {
		t.Fatalf("expected AllowAll, got %T", pp)
	}
}

func TestBuildPermissionPolicy_DenySideEffects(t *testing.T) {
	registry := buildToolRegistry(&registryExecutor{
		caps: executor.ExecutorCapabilities{CanRead: true, CanWrite: true, CanExec: true},
	}, edit.NewWholeFileStrategy(), types.ToolsConfig{})
	pp := buildPermissionPolicy(types.PermissionPolicyConfig{Type: "deny-side-effects"}, registry, nil)
	if _, ok := pp.(*permission.DenySideEffects); !ok {
		t.Fatalf("expected DenySideEffects, got %T", pp)
	}
}

func TestBuildPermissionPolicy_AskUpstream(t *testing.T) {
	registry := buildToolRegistry(&registryExecutor{
		caps: executor.ExecutorCapabilities{CanRead: true},
	}, edit.NewWholeFileStrategy(), types.ToolsConfig{})
	tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	pp := buildPermissionPolicy(types.PermissionPolicyConfig{Type: "ask-upstream", Timeout: 60}, registry, tp)
	if _, ok := pp.(*permission.AskUpstreamPolicy); !ok {
		t.Fatalf("expected AskUpstreamPolicy, got %T", pp)
	}
}

func TestBuildPermissionPolicy_DefaultFallback(t *testing.T) {
	pp := buildPermissionPolicy(types.PermissionPolicyConfig{}, nil, nil)
	if _, ok := pp.(*permission.AllowAll); !ok {
		t.Fatalf("expected AllowAll for empty type, got %T", pp)
	}
}

// --- buildGitStrategy ---

func TestBuildGitStrategy_None(t *testing.T) {
	gs := buildGitStrategy(types.GitStrategyConfig{Type: "none"})
	if _, ok := gs.(*git.NoneGitStrategy); !ok {
		t.Fatalf("expected NoneGitStrategy, got %T", gs)
	}
}

func TestBuildGitStrategy_Empty(t *testing.T) {
	gs := buildGitStrategy(types.GitStrategyConfig{})
	if _, ok := gs.(*git.NoneGitStrategy); !ok {
		t.Fatalf("expected NoneGitStrategy for empty type, got %T", gs)
	}
}

func TestBuildGitStrategy_Deterministic(t *testing.T) {
	gs := buildGitStrategy(types.GitStrategyConfig{Type: "deterministic"})
	if _, ok := gs.(*git.DeterministicGitStrategy); !ok {
		t.Fatalf("expected DeterministicGitStrategy, got %T", gs)
	}
}

func TestBuildGitStrategy_UnknownFallsBack(t *testing.T) {
	gs := buildGitStrategy(types.GitStrategyConfig{Type: "nonexistent"})
	if _, ok := gs.(*git.NoneGitStrategy); !ok {
		t.Fatalf("expected NoneGitStrategy for unknown type, got %T", gs)
	}
}

// --- buildTraceEmitter ---

func TestBuildTraceEmitter_JSONLWithoutPath(t *testing.T) {
	te, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{Type: "jsonl"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := te.(*trace.JSONLTraceEmitter); !ok {
		t.Fatalf("expected JSONLTraceEmitter, got %T", te)
	}
}

func TestBuildTraceEmitter_JSONLWithPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	te, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{Type: "jsonl", FilePath: path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := te.(*trace.JSONLTraceEmitter); !ok {
		t.Fatalf("expected JSONLTraceEmitter, got %T", te)
	}
	// Verify the file was created.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected trace file to exist: %v", err)
	}
}

func TestBuildTraceEmitter_EmptyTypeDefaultsToJSONL(t *testing.T) {
	te, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := te.(*trace.JSONLTraceEmitter); !ok {
		t.Fatalf("expected JSONLTraceEmitter for empty type, got %T", te)
	}
}

func TestBuildTraceEmitter_UnsupportedType(t *testing.T) {
	_, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{Type: "datadog"})
	if err == nil {
		t.Fatal("expected error for unsupported type")
	}
	if !strings.Contains(err.Error(), "unsupported trace emitter type") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildTraceEmitter_JSONLBadPath(t *testing.T) {
	_, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{
		Type:     "jsonl",
		FilePath: "/nonexistent/deeply/nested/dir/trace.jsonl",
	})
	if err == nil {
		t.Fatal("expected error for bad trace file path")
	}
}

// --- buildExecutor ---

func TestBuildExecutor_Local(t *testing.T) {
	workspace := t.TempDir()
	exec, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type:      "local",
		Workspace: workspace,
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := exec.(*executor.LocalExecutor); !ok {
		t.Fatalf("expected LocalExecutor, got %T", exec)
	}
}

func TestBuildExecutor_EmptyTypeDefaultsToLocal(t *testing.T) {
	exec, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Workspace: t.TempDir(),
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := exec.(*executor.LocalExecutor); !ok {
		t.Fatalf("expected LocalExecutor for empty type, got %T", exec)
	}
}

func TestBuildExecutor_LocalDefaultsWorkspaceToCwd(t *testing.T) {
	exec, err := buildExecutor(context.Background(), types.ExecutorConfig{Type: "local"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exec == nil {
		t.Fatal("expected non-nil executor")
	}
}

func TestBuildExecutor_API_MissingVcsBackend(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{Type: "api"}, nil)
	if err == nil {
		t.Fatal("expected error for api without vcsBackend")
	}
	if !strings.Contains(err.Error(), "requires vcsBackend") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildExecutor_API_BadRepoFormat(t *testing.T) {
	secrets := &stubSecretStore{secrets: map[string]string{"secret://token": "tok"}}
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type: "api",
		VcsBackend: &types.VcsBackendConfig{
			APIKeyRef: "secret://token",
			Repo:      "invalid-no-slash",
			Ref:       "main",
		},
	}, secrets)
	if err == nil {
		t.Fatal("expected error for bad repo format")
	}
	if !strings.Contains(err.Error(), "expected 'owner/repo'") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildExecutor_API_ValidConfig(t *testing.T) {
	secrets := &stubSecretStore{secrets: map[string]string{"secret://token": "tok"}}
	exec, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type: "api",
		VcsBackend: &types.VcsBackendConfig{
			APIKeyRef: "secret://token",
			Repo:      "owner/repo",
			Ref:       "main",
		},
	}, secrets)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := exec.(*executor.APIExecutor); !ok {
		t.Fatalf("expected APIExecutor, got %T", exec)
	}
}

func TestBuildExecutor_Container_MissingImage(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{Type: "container"}, nil)
	if err == nil {
		t.Fatal("expected error for container without image")
	}
	if !strings.Contains(err.Error(), "requires image") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildExecutor_UnsupportedType(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{Type: "microvm"}, nil)
	if err == nil {
		t.Fatal("expected error for unsupported type")
	}
	if !strings.Contains(err.Error(), "unsupported executor type") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- buildTransport ---

func TestBuildTransport_Stdio(t *testing.T) {
	tp, err := buildTransport(context.Background(), types.TransportConfig{Type: "stdio"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tp == nil {
		t.Fatal("expected non-nil transport")
	}
}

func TestBuildTransport_EmptyDefaultsToStdio(t *testing.T) {
	tp, err := buildTransport(context.Background(), types.TransportConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tp == nil {
		t.Fatal("expected non-nil transport")
	}
}

func TestBuildTransport_GRPCMissingAddress(t *testing.T) {
	_, err := buildTransport(context.Background(), types.TransportConfig{Type: "grpc"})
	if err == nil {
		t.Fatal("expected error for gRPC without address")
	}
	if !strings.Contains(err.Error(), "requires an address") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildTransport_UnsupportedType(t *testing.T) {
	_, err := buildTransport(context.Background(), types.TransportConfig{Type: "websocket"})
	if err == nil {
		t.Fatal("expected error for unsupported type")
	}
	if !strings.Contains(err.Error(), "unsupported transport type") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- parseLogLevel ---

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"ERROR", slog.LevelError},
		{"", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
		{"trace", slog.LevelInfo},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseLogLevel(tt.input)
			if got != tt.want {
				t.Fatalf("parseLogLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// --- toolEnabled ---

func TestToolEnabled_EmptyListEnablesAll(t *testing.T) {
	if !toolEnabled(nil, "read_file") {
		t.Fatal("empty list should enable all tools")
	}
	if !toolEnabled([]string{}, "read_file") {
		t.Fatal("empty slice should enable all tools")
	}
}

func TestToolEnabled_ExplicitList(t *testing.T) {
	enabled := []string{"read_file", "run_command"}
	if !toolEnabled(enabled, "read_file") {
		t.Fatal("read_file should be enabled")
	}
	if toolEnabled(enabled, "write_file") {
		t.Fatal("write_file should not be enabled")
	}
}

// --- editToolEnabled ---

func TestEditToolEnabled_EmptyListEnablesAll(t *testing.T) {
	if !editToolEnabled(nil, "write_file") {
		t.Fatal("empty list should enable all edit tools")
	}
}

func TestEditToolEnabled_DirectMatch(t *testing.T) {
	if !editToolEnabled([]string{"edit_file"}, "edit_file") {
		t.Fatal("direct match should enable the tool")
	}
}

func TestEditToolEnabled_AliasMatch(t *testing.T) {
	// "write_file" is an alias for edit tools.
	if !editToolEnabled([]string{"write_file"}, "edit_file") {
		t.Fatal("write_file alias should enable edit tools")
	}
	if !editToolEnabled([]string{"search_replace"}, "edit_file") {
		t.Fatal("search_replace alias should enable edit tools")
	}
	if !editToolEnabled([]string{"apply_diff"}, "edit_file") {
		t.Fatal("apply_diff alias should enable edit tools")
	}
}

func TestEditToolEnabled_NoMatch(t *testing.T) {
	if editToolEnabled([]string{"read_file"}, "edit_file") {
		t.Fatal("read_file should not enable edit tools")
	}
}

// --- sideEffectingToolSet ---

func TestSideEffectingToolSet(t *testing.T) {
	exec, _ := executor.NewLocalExecutor(t.TempDir())
	registry := buildToolRegistry(exec, edit.NewWholeFileStrategy(), types.ToolsConfig{})

	sideEffecting := sideEffectingToolSet(registry)

	// write_file and run_command are workspace-mutating and require approval.
	if !sideEffecting["write_file"] {
		t.Fatal("write_file should be in the gating set")
	}
	if !sideEffecting["run_command"] {
		t.Fatal("run_command should be in the gating set")
	}
	// read_file does not need gating.
	if sideEffecting["read_file"] {
		t.Fatal("read_file should not be in the gating set")
	}
}

// --- BuildLoopWithTransport integration ---

func TestBuildLoopWithTransport_InvalidConfigReturnsError(t *testing.T) {
	_, err := BuildLoopWithTransport(context.Background(), &types.RunConfig{
		// Missing provider type — validation will fail.
		MaxTurns: 5,
	}, nil)
	if err == nil {
		t.Fatal("expected error for invalid config")
	}
	if !strings.Contains(err.Error(), "config validation") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildLoopWithTransport_MinimalValidConfig(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-test",
		Mode:             "execution",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "test"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		MaxTurns:         2,
		Timeout:          &timeout,
	}

	tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	loop, err := BuildLoopWithTransport(context.Background(), config, tp)
	if err != nil {
		t.Fatalf("BuildLoopWithTransport() error: %v", err)
	}
	defer func() { _ = loop.Close() }()

	// Verify all components were wired.
	if loop.Provider == nil {
		t.Fatal("Provider is nil")
	}
	if loop.Router == nil {
		t.Fatal("Router is nil")
	}
	if loop.Prompt == nil {
		t.Fatal("Prompt is nil")
	}
	if loop.Context == nil {
		t.Fatal("Context is nil")
	}
	if loop.Tools == nil {
		t.Fatal("Tools is nil")
	}
	if loop.Executor == nil {
		t.Fatal("Executor is nil")
	}
	if loop.Edit == nil {
		t.Fatal("Edit is nil")
	}
	if loop.Verifier == nil {
		t.Fatal("Verifier is nil")
	}
	if loop.Permissions == nil {
		t.Fatal("Permissions is nil")
	}
	if loop.Git == nil {
		t.Fatal("Git is nil")
	}
	if loop.Transport == nil {
		t.Fatal("Transport is nil")
	}
	if loop.Trace == nil {
		t.Fatal("Trace is nil")
	}
	if loop.Tracer == nil {
		t.Fatal("Tracer is nil")
	}
	if loop.Metrics == nil {
		t.Fatal("Metrics is nil")
	}
	if loop.Security == nil {
		t.Fatal("Security is nil")
	}
	if loop.Logger == nil {
		t.Fatal("Logger is nil")
	}
}

func TestBuildLoopWithTransport_InjectedTransportSkipsBuild(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-test-tp",
		Mode:             "execution",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "test"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		Transport:        types.TransportConfig{Type: "grpc"}, // would fail without address
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		MaxTurns:         2,
		Timeout:          &timeout,
	}

	// Inject a transport so buildTransport is skipped (gRPC would fail without address).
	injected := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	loop, err := BuildLoopWithTransport(context.Background(), config, injected)
	if err != nil {
		t.Fatalf("BuildLoopWithTransport() error: %v", err)
	}
	defer func() { _ = loop.Close() }()

	if loop.Transport != injected {
		t.Fatal("expected injected transport to be used")
	}
	// emitReady should be false when transport is injected.
	if loop.emitReady {
		t.Fatal("expected emitReady=false when transport is injected")
	}
}

func TestBuildLoopWithTransport_NoopMetricsWhenNotOTel(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-test-noop-metrics",
		Mode:             "execution",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "test"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		MaxTurns:         2,
		Timeout:          &timeout,
	}

	tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	loop, err := BuildLoopWithTransport(context.Background(), config, tp)
	if err != nil {
		t.Fatalf("BuildLoopWithTransport() error: %v", err)
	}
	defer func() { _ = loop.Close() }()

	// Metrics should be noop (non-nil, but no-op instruments) when trace type is jsonl.
	if loop.Metrics == nil {
		t.Fatal("Metrics should be noop, not nil")
	}
}

func TestBuildLoopWithTransport_AllToolsRegisteredByDefault(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-test-tools",
		Mode:             "execution",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "test"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		MaxTurns:         2,
		Timeout:          &timeout,
	}

	tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	loop, err := BuildLoopWithTransport(context.Background(), config, tp)
	if err != nil {
		t.Fatalf("BuildLoopWithTransport() error: %v", err)
	}
	defer func() { _ = loop.Close() }()

	// With no tools.builtIn filter, all tools should be registered.
	expectedTools := []string{"read_file", "list_directory", "search_files", "run_command", "write_file", "web_fetch", "spawn_agent"}
	for _, name := range expectedTools {
		if loop.Tools.Resolve(name) == nil {
			t.Errorf("expected tool %q to be registered", name)
		}
	}
}

func TestBuildLoopWithTransport_SecretResolutionFailure(t *testing.T) {
	// Don't set the env var — secret resolution will fail.
	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-test-secret-fail",
		Mode:             "execution",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://MISSING_KEY"},
		ModelRouter:      types.ModelRouterConfig{Type: "static"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		MaxTurns:         2,
		Timeout:          &timeout,
	}

	_, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
	if err == nil {
		t.Fatal("expected error when secret resolution fails")
	}
	if !strings.Contains(err.Error(), "build providers") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- stubSecretStore ---

type stubSecretStore struct {
	secrets map[string]string
}

func (s *stubSecretStore) Resolve(_ context.Context, ref string) (string, error) {
	v, ok := s.secrets[ref]
	if !ok {
		return "", os.ErrNotExist
	}
	return v, nil
}
