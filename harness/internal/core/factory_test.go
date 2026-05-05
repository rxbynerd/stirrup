package core

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// repoRootForTests returns the absolute repo root by walking up from
// this test file's path. Mirrors the helper in harness/cmd/stirrup/cmd/
// so tests can locate examples/ regardless of working directory.
func repoRootForTests(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	// thisFile is .../harness/internal/core/factory_test.go; walk up four
	// levels to reach the repo root.
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", "..", ".."))
}

// disableRuleOfTwo returns a RuleOfTwoConfig that overrides the Rule-of-Two
// invariant. The factory and integration tests in this file build configs
// that legitimately exercise the all-three case (default tool list +
// TEST_*_KEY APIKeyRef + allow-all/deny-side-effects) so they can verify
// factory wiring and policy behaviour. Rule-of-Two semantics are covered
// in types/runconfig_test.go; the tests here would otherwise be obscured
// by the validator rejection.
func disableRuleOfTwo() *types.RuleOfTwoConfig {
	enforce := false
	return &types.RuleOfTwoConfig{Enforce: &enforce}
}

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

// --- wrapWithCodeScanner ---

func TestWrapWithCodeScanner_NilLeavesInnerUnchanged(t *testing.T) {
	inner := edit.NewWholeFileStrategy()
	got, err := wrapWithCodeScanner(inner, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != inner {
		t.Errorf("nil cfg should return inner unchanged, got %T", got)
	}
}

func TestWrapWithCodeScanner_NoneLeavesInnerUnchanged(t *testing.T) {
	inner := edit.NewWholeFileStrategy()
	got, err := wrapWithCodeScanner(inner, &types.CodeScannerConfig{Type: "none"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != inner {
		t.Errorf("type=none should return inner unchanged, got %T", got)
	}
}

func TestWrapWithCodeScanner_PatternsWraps(t *testing.T) {
	inner := edit.NewWholeFileStrategy()
	got, err := wrapWithCodeScanner(inner, &types.CodeScannerConfig{Type: "patterns"}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == inner {
		t.Errorf("patterns scanner should wrap inner, got identity")
	}
	if _, ok := got.(*edit.ScannedStrategy); !ok {
		t.Errorf("expected *edit.ScannedStrategy, got %T", got)
	}
}

func TestWrapWithCodeScanner_UnknownTypeReturnsError(t *testing.T) {
	inner := edit.NewWholeFileStrategy()
	_, err := wrapWithCodeScanner(inner, &types.CodeScannerConfig{Type: "nope"}, nil)
	if err == nil {
		t.Fatal("expected error for unknown scanner type")
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

// --- emitRuleOfTwoEvents ---

// captureSecLogger writes to a buffer so tests can inspect the JSON-line
// stream emitted by SecurityLogger. We use the real SecurityLogger
// rather than a hand-rolled mock so the JSON shape exercised here is
// the same one downstream tooling would see.
func captureSecLogger(t *testing.T) (*security.SecurityLogger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	return security.NewSecurityLogger(buf, "test-run"), buf
}

func TestEmitRuleOfTwoEvents_AllThreeWithOverrideEmitsDisabled(t *testing.T) {
	sec, buf := captureSecLogger(t)
	enforce := false
	cfg := &types.RunConfig{
		Mode:             "execution",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"web_fetch", "run_command"}},
		DynamicContext:   map[string]string{"x": "y"},
		RuleOfTwo:        &types.RuleOfTwoConfig{Enforce: &enforce},
	}

	emitRuleOfTwoEvents(cfg, sec)

	out := buf.String()
	if !strings.Contains(out, `"event":"rule_of_two_disabled"`) {
		t.Errorf("expected rule_of_two_disabled event, got: %s", out)
	}
}

func TestEmitRuleOfTwoEvents_AllThreeWithoutOverrideStaysSilent(t *testing.T) {
	// All three flags + ask-upstream is legal without an explicit
	// override; we should NOT emit rule_of_two_disabled in that case.
	sec, buf := captureSecLogger(t)
	cfg := &types.RunConfig{
		Mode:             "research",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "ask-upstream"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"web_fetch", "run_command"}},
		DynamicContext:   map[string]string{"x": "y"},
	}

	emitRuleOfTwoEvents(cfg, sec)

	if strings.Contains(buf.String(), "rule_of_two_disabled") {
		t.Errorf("ask-upstream all-three path must not emit rule_of_two_disabled, got: %s", buf.String())
	}
}

func TestEmitRuleOfTwoEvents_TwoOfThreeEmitsWarning(t *testing.T) {
	// All three two-of-three pairs must each emit rule_of_two_warning
	// with the payload booleans set to (true, true, false) for the two
	// flags that hold and false for the third. Pre-S6 only the
	// untrusted+sensitive pair was tested, so a regression in the
	// untrusted+external or sensitive+external branches would slip
	// past CI silently.
	cases := []struct {
		name  string
		cfg   *types.RunConfig
		wantU bool
		wantS bool
		wantE bool
	}{
		{
			name: "untrusted+sensitive",
			cfg: &types.RunConfig{
				Mode:             "execution",
				Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
				PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
				Tools:            types.ToolsConfig{BuiltIn: []string{"read_file"}},
				DynamicContext:   map[string]string{"x": "y"},
			},
			wantU: true, wantS: true, wantE: false,
		},
		{
			name: "untrusted+external",
			cfg: &types.RunConfig{
				Mode: "execution",
				// APIKeyRef referencing a name without
				// key/token/secret/password and not via SSM does not
				// trip ruleOfTwoSensitiveData; use a placeholder.
				Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://CONFIG_VALUE"},
				PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
				// web_fetch = untrusted AND external; explicit BuiltIn
				// list so APIKeyRef stays the only sensitivity vector.
				Tools: types.ToolsConfig{BuiltIn: []string{"web_fetch"}},
			},
			wantU: true, wantS: false, wantE: true,
		},
		{
			name: "sensitive+external",
			cfg: &types.RunConfig{
				Mode:             "execution",
				Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
				PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
				// run_command via bridge = external comm; sensitive APIKeyRef.
				// No web_fetch / DynamicContext / MCP servers ⇒ not untrusted.
				Tools: types.ToolsConfig{BuiltIn: []string{"run_command"}},
				Executor: types.ExecutorConfig{
					Type: "container", Image: "x",
					Network: &types.NetworkConfig{Mode: "bridge"},
				},
			},
			wantU: false, wantS: true, wantE: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sec, buf := captureSecLogger(t)
			emitRuleOfTwoEvents(tc.cfg, sec)
			out := buf.String()
			if !strings.Contains(out, `"event":"rule_of_two_warning"`) {
				t.Fatalf("expected rule_of_two_warning event, got: %s", out)
			}
			// Payload field assertions. The booleans are emitted as
			// JSON true/false so a substring search is sufficient and
			// keeps this test independent of map-key ordering.
			assertPayloadBool(t, out, "untrustedInput", tc.wantU)
			assertPayloadBool(t, out, "sensitiveData", tc.wantS)
			assertPayloadBool(t, out, "externalCommunication", tc.wantE)
		})
	}
}

// assertPayloadBool checks that a JSON line in out names key with the
// expected boolean value. The SecurityLogger output uses standard
// json.Marshal so the encoding is `"key":true`/`"key":false`.
func assertPayloadBool(t *testing.T, out, key string, want bool) {
	t.Helper()
	var phrase string
	if want {
		phrase = `"` + key + `":true`
	} else {
		phrase = `"` + key + `":false`
	}
	if !strings.Contains(out, phrase) {
		t.Errorf("expected %q in payload, got: %s", phrase, out)
	}
}

// TestEmitRuleOfTwoEvents_DisabledPayloadShape extends the override
// path to assert the same payload booleans appear on rule_of_two_disabled
// events. The audit consumer gets a single shape across both events.
func TestEmitRuleOfTwoEvents_DisabledPayloadShape(t *testing.T) {
	sec, buf := captureSecLogger(t)
	enforce := false
	cfg := &types.RunConfig{
		Mode:             "execution",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"web_fetch", "run_command"}},
		DynamicContext:   map[string]string{"x": "y"},
		RuleOfTwo:        &types.RuleOfTwoConfig{Enforce: &enforce},
	}
	emitRuleOfTwoEvents(cfg, sec)
	out := buf.String()
	if !strings.Contains(out, `"event":"rule_of_two_disabled"`) {
		t.Fatalf("expected rule_of_two_disabled, got: %s", out)
	}
	assertPayloadBool(t, out, "untrustedInput", true)
	assertPayloadBool(t, out, "sensitiveData", true)
	assertPayloadBool(t, out, "externalCommunication", true)
}

func TestEmitRuleOfTwoEvents_NoneOrOneStaysSilent(t *testing.T) {
	sec, buf := captureSecLogger(t)
	cfg := &types.RunConfig{
		Mode:             "execution",
		Provider:         types.ProviderConfig{Type: "anthropic"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"read_file"}},
	}
	emitRuleOfTwoEvents(cfg, sec)
	if buf.Len() > 0 {
		t.Errorf("zero-or-one flag config should emit nothing, got: %s", buf.String())
	}
}

// --- buildPermissionPolicy ---

// buildPermissionPolicyForTest is a thin wrapper that fabricates a
// minimal RunConfig from a bare PermissionPolicyConfig so the existing
// table tests don't have to spell out a full config every call.
func buildPermissionPolicyForTest(t *testing.T, cfg types.PermissionPolicyConfig, registry *tool.Registry, tp transport.Transport) permission.PermissionPolicy {
	t.Helper()
	rc := &types.RunConfig{
		RunID:            "test-run",
		Mode:             "execution",
		PermissionPolicy: cfg,
	}
	pp, err := buildPermissionPolicy(rc, registry, tp, nil)
	if err != nil {
		t.Fatalf("buildPermissionPolicy: %v", err)
	}
	return pp
}

func TestBuildPermissionPolicy_AllowAll(t *testing.T) {
	pp := buildPermissionPolicyForTest(t, types.PermissionPolicyConfig{Type: "allow-all"}, nil, nil)
	if _, ok := pp.(*permission.AllowAll); !ok {
		t.Fatalf("expected AllowAll, got %T", pp)
	}
}

func TestBuildPermissionPolicy_DenySideEffects(t *testing.T) {
	registry := buildToolRegistry(&registryExecutor{
		caps: executor.ExecutorCapabilities{CanRead: true, CanWrite: true, CanExec: true},
	}, edit.NewWholeFileStrategy(), types.ToolsConfig{})
	pp := buildPermissionPolicyForTest(t, types.PermissionPolicyConfig{Type: "deny-side-effects"}, registry, nil)
	if _, ok := pp.(*permission.DenySideEffects); !ok {
		t.Fatalf("expected DenySideEffects, got %T", pp)
	}
}

func TestBuildPermissionPolicy_AskUpstream(t *testing.T) {
	registry := buildToolRegistry(&registryExecutor{
		caps: executor.ExecutorCapabilities{CanRead: true},
	}, edit.NewWholeFileStrategy(), types.ToolsConfig{})
	tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	pp := buildPermissionPolicyForTest(t, types.PermissionPolicyConfig{Type: "ask-upstream", Timeout: 60}, registry, tp)
	if _, ok := pp.(*permission.AskUpstreamPolicy); !ok {
		t.Fatalf("expected AskUpstreamPolicy, got %T", pp)
	}
}

// TestBuildPermissionPolicy_UnknownTypeReturnsError covers S2: an
// unrecognised PermissionPolicy.Type used to fall through to allow-all,
// which silently dropped permissions on a config that ValidateRunConfig
// would have rejected on the normal path. Direct callers (tests,
// future tooling) that bypass validation must now get an explicit
// error so a misconfigured run cannot proceed under allow-all.
func TestBuildPermissionPolicy_UnknownTypeReturnsError(t *testing.T) {
	registry := buildToolRegistry(&registryExecutor{
		caps: executor.ExecutorCapabilities{CanRead: true},
	}, edit.NewWholeFileStrategy(), types.ToolsConfig{})
	rc := &types.RunConfig{
		PermissionPolicy: types.PermissionPolicyConfig{Type: "bogus"},
	}
	if _, err := buildPermissionPolicy(rc, registry, nil, nil); err == nil {
		t.Fatal("expected error for unknown permissionPolicy.type")
	}
	rc2 := &types.RunConfig{PermissionPolicy: types.PermissionPolicyConfig{}}
	if _, err := buildPermissionPolicy(rc2, registry, nil, nil); err == nil {
		t.Fatal("expected error for empty permissionPolicy.type")
	}
}

// TestBuildPermissionPolicy_PolicyEngine verifies the policy-engine arm
// loads a Cedar policy file from disk and constructs a PolicyEnginePolicy
// with the configured fallback. Uses an in-tree starter policy so we
// exercise the real LoadPolicySetFromFile path.
func TestBuildPermissionPolicy_PolicyEngine(t *testing.T) {
	policyPath := filepath.Join(repoRootForTests(t), "examples", "policies", "destructive-shell.cedar")
	if _, err := os.Stat(policyPath); err != nil {
		t.Skipf("starter policy missing at %q: %v", policyPath, err)
	}
	registry := buildToolRegistry(&registryExecutor{
		caps: executor.ExecutorCapabilities{CanRead: true, CanWrite: true, CanExec: true},
	}, edit.NewWholeFileStrategy(), types.ToolsConfig{})
	rc := &types.RunConfig{
		RunID: "test-run",
		Mode:  "execution",
		PermissionPolicy: types.PermissionPolicyConfig{
			Type:       "policy-engine",
			PolicyFile: policyPath,
			// Omit fallback to exercise the deny-side-effects default.
		},
	}
	pp, err := buildPermissionPolicy(rc, registry, nil, nil)
	if err != nil {
		t.Fatalf("buildPermissionPolicy: %v", err)
	}
	if _, ok := pp.(*permission.PolicyEnginePolicy); !ok {
		t.Fatalf("expected PolicyEnginePolicy, got %T", pp)
	}
}

// TestBuildPermissionPolicy_PolicyEngineFallbackIsBuilt verifies the
// fallback closure threads through the same buildPermissionPolicy
// dispatch — i.e. an explicit fallback="ask-upstream" really constructs
// an AskUpstreamPolicy under the hood. This is what makes the
// no-decision path behave like a top-level ask-upstream config.
func TestBuildPermissionPolicy_PolicyEngineFallbackIsBuilt(t *testing.T) {
	policyPath := filepath.Join(repoRootForTests(t), "examples", "policies", "destructive-shell.cedar")
	if _, err := os.Stat(policyPath); err != nil {
		t.Skipf("starter policy missing at %q: %v", policyPath, err)
	}
	registry := buildToolRegistry(&registryExecutor{
		caps: executor.ExecutorCapabilities{CanRead: true, CanWrite: true, CanExec: true},
	}, edit.NewWholeFileStrategy(), types.ToolsConfig{})
	tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	rc := &types.RunConfig{
		RunID: "test-run",
		Mode:  "execution",
		PermissionPolicy: types.PermissionPolicyConfig{
			Type:       "policy-engine",
			PolicyFile: policyPath,
			Fallback:   "ask-upstream",
			Timeout:    30,
		},
	}
	pp, err := buildPermissionPolicy(rc, registry, tp, nil)
	if err != nil {
		t.Fatalf("buildPermissionPolicy: %v", err)
	}
	if _, ok := pp.(*permission.PolicyEnginePolicy); !ok {
		t.Fatalf("expected PolicyEnginePolicy, got %T", pp)
	}
}

// TestBuildPermissionPolicy_PolicyEngineMissingFile asserts that a
// missing policy file fails fast with a clear error.
func TestBuildPermissionPolicy_PolicyEngineMissingFile(t *testing.T) {
	rc := &types.RunConfig{
		RunID: "test-run",
		Mode:  "execution",
		PermissionPolicy: types.PermissionPolicyConfig{
			Type:       "policy-engine",
			PolicyFile: "/this/path/does/not/exist.cedar",
		},
	}
	_, err := buildPermissionPolicy(rc, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for missing policy file, got nil")
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
	}, nil, nil)
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
	}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := exec.(*executor.LocalExecutor); !ok {
		t.Fatalf("expected LocalExecutor for empty type, got %T", exec)
	}
}

func TestBuildExecutor_LocalDefaultsWorkspaceToCwd(t *testing.T) {
	exec, err := buildExecutor(context.Background(), types.ExecutorConfig{Type: "local"}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exec == nil {
		t.Fatal("expected non-nil executor")
	}
}

func TestBuildExecutor_API_MissingVcsBackend(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{Type: "api"}, nil, nil)
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
	}, secrets, nil)
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
	}, secrets, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := exec.(*executor.APIExecutor); !ok {
		t.Fatalf("expected APIExecutor, got %T", exec)
	}
}

func TestBuildExecutor_Container_MissingImage(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{Type: "container"}, nil, nil)
	if err == nil {
		t.Fatal("expected error for container without image")
	}
	if !strings.Contains(err.Error(), "requires image") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildExecutor_UnsupportedType(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{Type: "microvm"}, nil, nil)
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

// --- mutatingToolSet ---

func TestMutatingToolSet(t *testing.T) {
	exec, _ := executor.NewLocalExecutor(t.TempDir())
	registry := buildToolRegistry(exec, edit.NewWholeFileStrategy(), types.ToolsConfig{})

	mutating := mutatingToolSet(registry)

	// write_file and run_command mutate the workspace.
	if !mutating["write_file"] {
		t.Fatal("write_file should be workspace-mutating")
	}
	if !mutating["run_command"] {
		t.Fatal("run_command should be workspace-mutating")
	}
	// Read-only tools are not mutating. (spawn_agent is registered
	// after the loop is built and is therefore not in this set; see
	// BuildLoopWithTransport.)
	for _, name := range []string{"read_file", "list_directory", "search_files", "web_fetch"} {
		if mutating[name] {
			t.Errorf("%s should not be workspace-mutating", name)
		}
	}
}

// --- approvalRequiredToolSet ---

func TestApprovalRequiredToolSet(t *testing.T) {
	exec, _ := executor.NewLocalExecutor(t.TempDir())
	registry := buildToolRegistry(exec, edit.NewWholeFileStrategy(), types.ToolsConfig{})

	approval := approvalRequiredToolSet(registry)

	// All tools that touch the world require approval: writes, shell, network.
	for _, name := range []string{"write_file", "run_command", "web_fetch"} {
		if !approval[name] {
			t.Errorf("%s should require approval", name)
		}
	}
	// Pure-read tools never require approval.
	for _, name := range []string{"read_file", "list_directory", "search_files"} {
		if approval[name] {
			t.Errorf("%s should not require approval", name)
		}
	}
	// spawn_agent is registered post-loop-construction in factory.go (see
	// BuildLoopWithTransport). Its absence here is load-bearing: if it
	// ever appears in this set, the AddApprovalTool refresh has become
	// redundant and the post-registration step in the factory should
	// be removed.
	if approval["spawn_agent"] {
		t.Error("spawn_agent should not be in approvalRequiredToolSet — it is added post-loop-construction in BuildLoopWithTransport")
	}
}

// TestBuildLoopWithTransport_AskUpstreamIncludesSpawnAgent asserts the
// post-registration refresh in BuildLoopWithTransport: spawn_agent must
// appear in the AskUpstreamPolicy approval set even though it is
// registered after the policy is built. Without the refresh, spawn_agent
// calls would bypass the control plane approval gate.
func TestBuildLoopWithTransport_AskUpstreamIncludesSpawnAgent(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-test-ask-upstream",
		Mode:             "execution",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "test"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "ask-upstream", Timeout: 60},
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

	ask, ok := loop.Permissions.(*permission.AskUpstreamPolicy)
	if !ok {
		t.Fatalf("expected AskUpstreamPolicy, got %T", loop.Permissions)
	}

	approval := make(map[string]bool)
	for _, name := range ask.ApprovalToolNames() {
		approval[name] = true
	}
	for _, name := range []string{"write_file", "run_command", "web_fetch", "spawn_agent"} {
		if !approval[name] {
			t.Errorf("ask-upstream policy missing %q in approval set; got %v", name, approval)
		}
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

// TestFactory_ConfigValidationFailed_EmitsSecurityEvent asserts that the
// SecurityLogger constructed inside BuildLoopWithTransport emits a
// config_validation_failed event when ValidateRunConfig rejects the config.
// We capture os.Stderr (where the logger writes) for the duration of the
// call and assert the event reaches the buffer.
func TestFactory_ConfigValidationFailed_EmitsSecurityEvent(t *testing.T) {
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() {
		os.Stderr = origStderr
	})

	// MaxTurns of 1000 exceeds the validator's hard cap of 100.
	config := &types.RunConfig{
		RunID:            "factory-validation-fail",
		Mode:             "execution",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://TEST"},
		ModelRouter:      types.ModelRouterConfig{Type: "static"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		MaxTurns:         1000, // exceeds the validator's cap
	}

	_, buildErr := BuildLoopWithTransport(context.Background(), config, nil)
	// Close the writer so the read side returns EOF cleanly.
	_ = w.Close()
	os.Stderr = origStderr

	if buildErr == nil {
		t.Fatal("expected error from BuildLoopWithTransport")
	}
	if !strings.Contains(buildErr.Error(), "config validation") {
		t.Fatalf("unexpected error: %v", buildErr)
	}

	captured, readErr := io.ReadAll(r)
	if readErr != nil {
		t.Fatalf("read captured stderr: %v", readErr)
	}
	if !strings.Contains(string(captured), `"event":"config_validation_failed"`) {
		t.Errorf("expected config_validation_failed event in captured stderr, got: %s", string(captured))
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
		RuleOfTwo:        disableRuleOfTwo(),
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
		RuleOfTwo:        disableRuleOfTwo(),
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
		RuleOfTwo:        disableRuleOfTwo(),
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
		RuleOfTwo:        disableRuleOfTwo(),
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
		RuleOfTwo:        disableRuleOfTwo(),
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

// TestBuildLoopWithTransport_ResearchModeAllowsWebFetch is the WP1
// regression: research mode (a read-only mode) must enable web_fetch
// and the deny-side-effects policy must allow it through. Before WP1
// the conflated SideEffects flag caused the same policy to reject
// web_fetch as a "side effect", breaking documented research-mode
// semantics.
func TestBuildLoopWithTransport_ResearchModeAllowsWebFetch(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-test-research",
		Mode:             "research",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "test"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		Tools:            types.ToolsConfig{BuiltIn: types.DefaultReadOnlyBuiltInTools()},
		RuleOfTwo:        disableRuleOfTwo(),
		MaxTurns:         2,
		Timeout:          &timeout,
	}

	tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	loop, err := BuildLoopWithTransport(context.Background(), config, tp)
	if err != nil {
		t.Fatalf("BuildLoopWithTransport() error: %v", err)
	}
	defer func() { _ = loop.Close() }()

	// web_fetch must be registered in research mode.
	wf := loop.Tools.Resolve("web_fetch")
	if wf == nil {
		t.Fatal("expected web_fetch to be registered in research mode")
	}

	// And the resulting permission policy must let web_fetch through.
	result, err := loop.Permissions.Check(context.Background(), wf.Definition(), nil)
	if err != nil {
		t.Fatalf("permission check returned error: %v", err)
	}
	if !result.Allowed {
		t.Fatalf("research mode permission policy denied web_fetch: %q", result.Reason)
	}
}

// TestBuildLoopWithTransport_ReadOnlyModesAllowWebFetch is the WP1
// regression covering all four read-only modes (research, review, toil,
// planning). Each must enable web_fetch and have a permission policy
// that allows it through. Without table coverage, a future change that
// special-cases just one mode would silently break the others.
func TestBuildLoopWithTransport_ReadOnlyModesAllowWebFetch(t *testing.T) {
	modes := []string{"research", "review", "toil", "planning"}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("TEST_OPENAI_KEY", "test-key")
			server := newOpenAIServer(t, nil, nil, nil)
			defer server.Close()

			timeout := 30
			config := &types.RunConfig{
				RunID:            "factory-test-readonly-" + mode,
				Mode:             mode,
				Prompt:           "hello",
				Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
				ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "test"},
				PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
				ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
				Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
				EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
				Verifier:         types.VerifierConfig{Type: "none"},
				PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
				GitStrategy:      types.GitStrategyConfig{Type: "none"},
				TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
				Tools:            types.ToolsConfig{BuiltIn: types.DefaultReadOnlyBuiltInTools()},
				RuleOfTwo:        disableRuleOfTwo(),
				MaxTurns:         2,
				Timeout:          &timeout,
			}

			tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
			loop, err := BuildLoopWithTransport(context.Background(), config, tp)
			if err != nil {
				t.Fatalf("BuildLoopWithTransport(mode=%q) error: %v", mode, err)
			}
			defer func() { _ = loop.Close() }()

			wf := loop.Tools.Resolve("web_fetch")
			if wf == nil {
				t.Fatalf("expected web_fetch to be registered in mode %q", mode)
			}

			result, err := loop.Permissions.Check(context.Background(), wf.Definition(), nil)
			if err != nil {
				t.Fatalf("permission check returned error in mode %q: %v", mode, err)
			}
			if !result.Allowed {
				t.Fatalf("mode %q permission policy denied web_fetch: %q", mode, result.Reason)
			}
		})
	}
}

// --- buildProvider ---

func TestBuildProvider_OpenAIResponses(t *testing.T) {
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:      "openai-responses",
		APIKeyRef: "secret://OPENAI_KEY",
		BaseURL:   "https://api.openai.com/v1",
	}, secrets)
	if err != nil {
		t.Fatalf("buildProvider returned error: %v", err)
	}
	if _, ok := prov.(*provider.OpenAIResponsesAdapter); !ok {
		t.Errorf("buildProvider type = %T, want *provider.OpenAIResponsesAdapter", prov)
	}
}

// TestBuildProvider_OpenAIResponsesAzureFields is the factory-level
// regression guard for issue #48: a RunConfig with APIKeyHeader / QueryParams
// populated must produce an adapter that, when invoked, sends the configured
// header and URL. The Stream call uses an httptest.Server stand-in so we can
// inspect the live HTTP request without depending on the live OpenAI API.
//
// Mirrors the wire-level round-trip test in
// harness/internal/transport/grpc_test.go (which only confirms proto field
// passthrough), closing the loop end-to-end: gRPC carries the fields →
// factory propagates them into the adapter → adapter applies them on the
// wire.
func TestBuildProvider_OpenAIResponsesAzureFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("api-key"), "AZURE-KEY"; got != want {
			t.Errorf("api-key header = %q, want %q", got, want)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization header should be empty, got %q", got)
		}
		if got, want := r.URL.Path, "/openai/v1/responses"; got != want {
			t.Errorf("URL.Path = %q, want %q", got, want)
		}
		if got, want := r.URL.Query().Get("api-version"), "preview"; got != want {
			t.Errorf("api-version = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"response\":{\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	t.Cleanup(srv.Close)

	secrets := &stubSecretStore{secrets: map[string]string{"secret://AZURE_KEY": "AZURE-KEY"}}
	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:         "openai-responses",
		APIKeyRef:    "secret://AZURE_KEY",
		BaseURL:      srv.URL + "/openai/v1",
		APIKeyHeader: "api-key",
		QueryParams:  map[string]string{"api-version": "preview"},
	}, secrets)
	if err != nil {
		t.Fatalf("buildProvider returned error: %v", err)
	}

	ch, err := prov.Stream(context.Background(), types.StreamParams{Model: "gpt-4o", MaxTokens: 16})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	for range ch { //nolint:revive // drain stream so the goroutine completes
	}
}

// TestBuildProvider_OpenAICompatibleAzureFields is the same factory-level
// regression guard for the Chat Completions adapter — see
// TestBuildProvider_OpenAIResponsesAzureFields for context.
func TestBuildProvider_OpenAICompatibleAzureFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("api-key"), "AZURE-KEY"; got != want {
			t.Errorf("api-key header = %q, want %q", got, want)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization header should be empty, got %q", got)
		}
		if got, want := r.URL.Path, "/openai/v1/chat/completions"; got != want {
			t.Errorf("URL.Path = %q, want %q", got, want)
		}
		if got, want := r.URL.Query().Get("api-version"), "preview"; got != want {
			t.Errorf("api-version = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	secrets := &stubSecretStore{secrets: map[string]string{"secret://AZURE_KEY": "AZURE-KEY"}}
	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:         "openai-compatible",
		APIKeyRef:    "secret://AZURE_KEY",
		BaseURL:      srv.URL + "/openai/v1",
		APIKeyHeader: "api-key",
		QueryParams:  map[string]string{"api-version": "preview"},
	}, secrets)
	if err != nil {
		t.Fatalf("buildProvider returned error: %v", err)
	}

	ch, err := prov.Stream(context.Background(), types.StreamParams{Model: "gpt-4o", MaxTokens: 16})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}
	for range ch { //nolint:revive // drain stream so the goroutine completes
	}
}

func TestBuildProvider_OpenAICompatibleStillWorks(t *testing.T) {
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:      "openai-compatible",
		APIKeyRef: "secret://OPENAI_KEY",
	}, secrets)
	if err != nil {
		t.Fatalf("buildProvider returned error: %v", err)
	}
	if _, ok := prov.(*provider.OpenAICompatibleAdapter); !ok {
		t.Errorf("buildProvider type = %T, want *provider.OpenAICompatibleAdapter", prov)
	}
}

func TestBuildProvider_UnknownTypeMentionsResponses(t *testing.T) {
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:      "nonsense",
		APIKeyRef: "secret://OPENAI_KEY",
	}, secrets)
	if err == nil {
		t.Fatal("expected error for unknown provider type")
	}
	if !strings.Contains(err.Error(), "openai-responses") {
		t.Errorf("error message should advertise openai-responses, got: %v", err)
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
