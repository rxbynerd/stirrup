package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/hook"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// factoryRSAKey is generated lazily once per test process. The 2048-bit
// keygen is the slow part; sharing across all factory tests keeps the
// suite under a second.
var (
	factoryRSAKeyOnce sync.Once
	factoryRSAKeyPEM  string
)

func factoryTestServiceAccountPEM(t *testing.T) string {
	t.Helper()
	factoryRSAKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate RSA key: %v", err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatalf("marshal PKCS8 key: %v", err)
		}
		factoryRSAKeyPEM = string(pem.EncodeToMemory(&pem.Block{
			Type:  "PRIVATE KEY",
			Bytes: der,
		}))
	})
	return factoryRSAKeyPEM
}

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

// TestBuildEditStrategy_Empty pins the defence-in-depth behaviour of
// buildEditStrategy for callers that bypass types.ValidateRunConfig.
// The validator fills an empty EditStrategy.Type with "multi" before
// the factory is reached, so this branch should never trigger in a
// validated run — but if it does, the factory must not silently fall
// back to a strategy that exposes a different write-tool surface than
// every validated entrypoint.
func TestBuildEditStrategy_Empty(t *testing.T) {
	es := buildEditStrategy(types.EditStrategyConfig{})
	if _, ok := es.(*edit.MultiStrategy); !ok {
		t.Fatalf("expected MultiStrategy for empty type, got %T", es)
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
	if _, ok := es.(*edit.MultiStrategy); !ok {
		t.Fatalf("expected MultiStrategy for unknown type, got %T", es)
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
	v := buildVerifier(types.VerifierConfig{Type: "none"}, nil, nil)
	if _, ok := v.(*verifier.NoneVerifier); !ok {
		t.Fatalf("expected NoneVerifier, got %T", v)
	}
}

func TestBuildVerifier_Empty(t *testing.T) {
	v := buildVerifier(types.VerifierConfig{}, nil, nil)
	if _, ok := v.(*verifier.NoneVerifier); !ok {
		t.Fatalf("expected NoneVerifier for empty type, got %T", v)
	}
}

func TestBuildVerifier_TestRunner(t *testing.T) {
	v := buildVerifier(types.VerifierConfig{Type: "test-runner", Command: "go test ./..."}, nil, nil)
	if _, ok := v.(*verifier.TestRunnerVerifier); !ok {
		t.Fatalf("expected TestRunnerVerifier, got %T", v)
	}
}

func TestBuildVerifier_LLMJudge(t *testing.T) {
	v := buildVerifier(types.VerifierConfig{Type: "llm-judge", Criteria: "test criteria"}, nil, nil)
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
	}, nil, nil)
	if _, ok := v.(*verifier.CompositeVerifier); !ok {
		t.Fatalf("expected CompositeVerifier, got %T", v)
	}
}

func TestBuildVerifier_UnknownFallsBack(t *testing.T) {
	v := buildVerifier(types.VerifierConfig{Type: "nonexistent"}, nil, nil)
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
	sensitive := true
	cfg := &types.RunConfig{
		Mode:             "execution",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"web_fetch", "run_command"}},
		DynamicContext:   map[string]types.DynamicContextValue{"x": {Value: "y"}},
		SensitiveData:    &sensitive,
		RuleOfTwo:        &types.RuleOfTwoConfig{Enforce: &enforce},
	}

	emitRuleOfTwoEvents(cfg, sec, resolveRuleOfTwoArming(cfg))

	out := buf.String()
	if !strings.Contains(out, `"event":"rule_of_two_disabled"`) {
		t.Errorf("expected rule_of_two_disabled event, got: %s", out)
	}
}

func TestEmitRuleOfTwoEvents_AllThreeWithoutOverrideStaysSilent(t *testing.T) {
	// All three flags + ask-upstream is legal without an explicit
	// override; we should NOT emit rule_of_two_disabled in that case.
	sec, buf := captureSecLogger(t)
	sensitive := true
	cfg := &types.RunConfig{
		Mode:             "research",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "ask-upstream"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"web_fetch", "run_command"}},
		DynamicContext:   map[string]types.DynamicContextValue{"x": {Value: "y"}},
		SensitiveData:    &sensitive,
	}

	emitRuleOfTwoEvents(cfg, sec, resolveRuleOfTwoArming(cfg))

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
	sensitive := true
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
				DynamicContext:   map[string]types.DynamicContextValue{"x": {Value: "y"}},
				SensitiveData:    &sensitive,
			},
			wantU: true, wantS: true, wantE: false,
		},
		{
			name: "untrusted+external",
			cfg: &types.RunConfig{
				Mode:             "execution",
				Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
				PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
				// web_fetch = untrusted AND external. SensitiveData
				// unset so the sensitive leg stays false — the API key
				// reference name no longer counts.
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
				// run_command via bridge = external comm; SensitiveData
				// declares the sensitive leg explicitly. No web_fetch /
				// DynamicContext / MCP servers ⇒ not untrusted.
				Tools:         types.ToolsConfig{BuiltIn: []string{"run_command"}},
				SensitiveData: &sensitive,
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
			emitRuleOfTwoEvents(tc.cfg, sec, resolveRuleOfTwoArming(tc.cfg))
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
	sensitive := true
	cfg := &types.RunConfig{
		Mode:             "execution",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://ANTHROPIC_API_KEY"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"web_fetch", "run_command"}},
		DynamicContext:   map[string]types.DynamicContextValue{"x": {Value: "y"}},
		SensitiveData:    &sensitive,
		RuleOfTwo:        &types.RuleOfTwoConfig{Enforce: &enforce},
	}
	emitRuleOfTwoEvents(cfg, sec, resolveRuleOfTwoArming(cfg))
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
	emitRuleOfTwoEvents(cfg, sec, resolveRuleOfTwoArming(cfg))
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

// TestBuildPermissionPolicy_PolicyEngineFileReadOnce is the regression
// test for #97 B2 (CWE-367): wrapping a freshly-built policy-engine
// policy with metrics must not re-read the Cedar policy file from
// disk. The previous implementation called buildPermissionPolicy
// twice (once before metrics, once after) which loaded the file twice
// and opened a TOCTOU window between the reads. This test copies the
// starter policy to a temp file, builds the policy, removes the file,
// then wraps with metrics and asserts the wrap succeeds — proving the
// wrap path is composition-only.
func TestBuildPermissionPolicy_PolicyEngineFileReadOnce(t *testing.T) {
	starter := filepath.Join(repoRootForTests(t), "examples", "policies", "destructive-shell.cedar")
	if _, err := os.Stat(starter); err != nil {
		t.Skipf("starter policy missing at %q: %v", starter, err)
	}
	contents, err := os.ReadFile(starter)
	if err != nil {
		t.Fatalf("read starter policy: %v", err)
	}
	tempPath := filepath.Join(t.TempDir(), "test-policy.cedar")
	if err := os.WriteFile(tempPath, contents, 0o600); err != nil {
		t.Fatalf("write temp policy: %v", err)
	}

	registry := buildToolRegistry(&registryExecutor{
		caps: executor.ExecutorCapabilities{CanRead: true, CanWrite: true, CanExec: true},
	}, edit.NewWholeFileStrategy(), types.ToolsConfig{})
	rc := &types.RunConfig{
		RunID: "test-run",
		Mode:  "execution",
		PermissionPolicy: types.PermissionPolicyConfig{
			Type:       "policy-engine",
			PolicyFile: tempPath,
		},
	}

	pp, err := buildPermissionPolicy(rc, registry, nil, nil)
	if err != nil {
		t.Fatalf("buildPermissionPolicy: %v", err)
	}

	// Remove the file so any re-read attempt would hard-fail.
	if err := os.Remove(tempPath); err != nil {
		t.Fatalf("remove temp policy: %v", err)
	}

	// Wrap with metrics — must not touch disk.
	wrapped := wrapPermissionPolicyMetrics(pp, rc.PermissionPolicy, nil)
	if wrapped == nil {
		t.Fatal("wrapPermissionPolicyMetrics returned nil")
	}
	// With a nil metrics argument the wrap is a no-op and returns the
	// inner unchanged. Pass through still must not re-read the file.
	if wrapped != pp {
		t.Errorf("nil metrics wrap should return inner unchanged")
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

// --- buildHookRunner (issue #461) ---

func TestBuildHookRunner_NilConfigReturnsNoop(t *testing.T) {
	r := buildHookRunner(nil, nil, nil)
	if _, ok := r.(*hook.Noop); !ok {
		t.Fatalf("expected hook.Noop for a nil HooksConfig, got %T", r)
	}
}

func TestBuildHookRunner_EmptyConfigReturnsNoop(t *testing.T) {
	r := buildHookRunner(&types.HooksConfig{}, nil, nil)
	if _, ok := r.(*hook.Noop); !ok {
		t.Fatalf("expected hook.Noop for an empty HooksConfig, got %T", r)
	}
}

func TestBuildHookRunner_PreRunOnlyReturnsExecRunner(t *testing.T) {
	cfg := &types.HooksConfig{PreRun: []types.HookConfig{{Command: "true"}}}
	r := buildHookRunner(cfg, nil, nil)
	execRunner, ok := r.(*hook.ExecRunner)
	if !ok {
		t.Fatalf("expected *hook.ExecRunner, got %T", r)
	}
	if execRunner.Hooks != cfg {
		t.Error("ExecRunner.Hooks must be the same HooksConfig instance passed in")
	}
}

func TestBuildHookRunner_PostRunOnlyReturnsExecRunner(t *testing.T) {
	cfg := &types.HooksConfig{PostRun: []types.HookConfig{{Command: "true"}}}
	r := buildHookRunner(cfg, nil, nil)
	if _, ok := r.(*hook.ExecRunner); !ok {
		t.Fatalf("expected *hook.ExecRunner, got %T", r)
	}
}

// --- buildTraceEmitter ---

func TestBuildTraceEmitter_JSONLWithoutPath(t *testing.T) {
	te, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{Type: "jsonl"}, nil, observability.ResourceOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := te.(*trace.JSONLTraceEmitter); !ok {
		t.Fatalf("expected JSONLTraceEmitter, got %T", te)
	}
}

func TestBuildTraceEmitter_JSONLWithPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	te, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{Type: "jsonl", FilePath: path}, nil, observability.ResourceOptions{})
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
	te, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{}, nil, observability.ResourceOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := te.(*trace.JSONLTraceEmitter); !ok {
		t.Fatalf("expected JSONLTraceEmitter for empty type, got %T", te)
	}
}

func TestBuildTraceEmitter_UnsupportedType(t *testing.T) {
	_, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{Type: "datadog"}, nil, observability.ResourceOptions{})
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
	}, nil, observability.ResourceOptions{})
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

func TestBuildExecutor_K8s_MissingImage(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type:         "k8s",
		K8sNamespace: "default",
	}, nil, nil)
	if err == nil {
		t.Fatal("expected error for k8s without image")
	}
	if !strings.Contains(err.Error(), "requires image") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildExecutor_K8s_MissingNamespace(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type:  "k8s",
		Image: "busybox",
	}, nil, nil)
	if err == nil {
		t.Fatal("expected error for k8s without namespace")
	}
	if !strings.Contains(err.Error(), "requires k8sNamespace") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestBuildExecutor_K8s_TakesK8sPath confirms the "k8s" case is reached
// (not the default "unsupported" arm) when Image and K8sNamespace are
// present. Without a cluster, NewK8sExecutor fails downstream at REST
// config / Pod creation — the error must therefore be a k8s-construction
// failure, never "unsupported executor type". A bogus kubeconfig path
// forces a deterministic, cluster-free failure.
func TestBuildExecutor_K8s_TakesK8sPath(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type:          "k8s",
		Image:         "busybox",
		K8sNamespace:  "default",
		K8sKubeconfig: filepath.Join(t.TempDir(), "does-not-exist.kubeconfig"),
	}, nil, nil)
	if err == nil {
		// A success would mean a cluster was reachable; that is fine but not
		// the cluster-free case this test targets.
		t.Skip("k8s executor unexpectedly constructed (a cluster appears reachable); skipping the no-cluster assertion")
	}
	if strings.Contains(err.Error(), "unsupported executor type") {
		t.Fatalf("k8s type fell through to the default arm: %v", err)
	}
}

func TestBuildExecutor_K8sSandbox_MissingImage(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type:         "k8s-sandbox",
		K8sNamespace: "default",
	}, nil, nil)
	if err == nil {
		t.Fatal("expected error for k8s-sandbox without image")
	}
	if !strings.Contains(err.Error(), "requires image") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildExecutor_K8sSandbox_MissingNamespace(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type:  "k8s-sandbox",
		Image: "busybox",
	}, nil, nil)
	if err == nil {
		t.Fatal("expected error for k8s-sandbox without namespace")
	}
	if !strings.Contains(err.Error(), "requires k8sNamespace") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestBuildExecutor_K8sSandbox_TakesAgentSandboxPath confirms the
// "k8s-sandbox" case routes to NewAgentSandboxExecutor (not the default
// "unsupported" arm) when Image, K8sNamespace, and Network are present.
// Without a cluster, NewAgentSandboxExecutor fails downstream at REST config /
// Sandbox creation, so the error must be an agent-sandbox-construction
// failure, never "unsupported executor type". A bogus kubeconfig path forces a
// deterministic, cluster-free failure.
func TestBuildExecutor_K8sSandbox_TakesAgentSandboxPath(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type:          "k8s-sandbox",
		Image:         "busybox",
		K8sNamespace:  "default",
		Network:       &types.NetworkConfig{Mode: "none"},
		K8sKubeconfig: filepath.Join(t.TempDir(), "does-not-exist.kubeconfig"),
	}, nil, nil)
	if err == nil {
		// A success would mean a cluster was reachable; that is fine but not
		// the cluster-free case this test targets.
		t.Skip("agent sandbox executor unexpectedly constructed (a cluster appears reachable); skipping the no-cluster assertion")
	}
	if strings.Contains(err.Error(), "unsupported executor type") {
		t.Fatalf("k8s-sandbox type fell through to the default arm: %v", err)
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
	for _, name := range []string{"read_file", "list_directory", "grep_files", "find_files", "web_fetch"} {
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
	for _, name := range []string{"read_file", "list_directory", "grep_files", "find_files"} {
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
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"}, // load-bearing: whole-file registers write_file (not edit_file), expected in the approval set below
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

	ask, ok := permission.Unwrap(loop.Permissions).(*permission.AskUpstreamPolicy)
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
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
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
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
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
	if loop.Hooks == nil {
		t.Fatal("Hooks is nil")
	}
	if _, ok := loop.Hooks.(*hook.Noop); !ok {
		t.Errorf("expected hook.Noop for a config with no HooksConfig, got %T", loop.Hooks)
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

// TestBuildLoopWithTransport_HooksWiredAsExecRunner pins that a config
// carrying a non-empty HooksConfig (issue #461) wires an *hook.ExecRunner
// sharing the run's own Executor, rather than the Noop fallback.
func TestBuildLoopWithTransport_HooksWiredAsExecRunner(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-hooks-test",
		Mode:             "execution",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "test"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		RuleOfTwo:        disableRuleOfTwo(),
		MaxTurns:         2,
		Timeout:          &timeout,
		Hooks: &types.HooksConfig{
			PreRun: []types.HookConfig{{Command: "true", TimeoutSeconds: 5}},
		},
	}

	tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	loop, err := BuildLoopWithTransport(context.Background(), config, tp)
	if err != nil {
		t.Fatalf("BuildLoopWithTransport() error: %v", err)
	}
	defer func() { _ = loop.Close() }()

	execRunner, ok := loop.Hooks.(*hook.ExecRunner)
	if !ok {
		t.Fatalf("expected *hook.ExecRunner, got %T", loop.Hooks)
	}
	if execRunner.Exec != loop.Executor {
		t.Error("ExecRunner.Exec must be the run's own Executor")
	}
	if execRunner.Hooks != config.Hooks {
		t.Error("ExecRunner.Hooks must be the config's own HooksConfig instance")
	}
}

// TestBuildLoopWithTransport_EmptyEditStrategyDefaultsToMulti pins the
// full composition — ValidateRunConfig → applyEditStrategyDefault →
// buildEditStrategy → wrapWithCodeScanner — across the BuildLoopWithTransport
// seam. The two halves are exercised independently elsewhere
// (TestValidateRunConfig_EditStrategyDefaultsToMulti in
// types/runconfig_test.go and TestBuildEditStrategy_Empty above), but
// either side could regress without breaking the other: e.g. removing
// the applyEditStrategyDefault call from ValidateRunConfig while
// re-adding case "" to buildEditStrategy would leave both unit tests
// green. This test fails in that scenario because the factory's
// buildEditStrategy now returns MultiStrategy for an empty type
// (defence-in-depth) — so we need a non-multi explicit choice in the
// other tests to detect a missed default. With execution mode, the
// "patterns" CodeScanner default wraps the base strategy in a
// ScannedStrategy whose Inner() is the MultiStrategy.
func TestBuildLoopWithTransport_EmptyEditStrategyDefaultsToMulti(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	timeout := 30
	config := &types.RunConfig{
		RunID:           "factory-test-edit-default",
		Mode:            "execution",
		Prompt:          "hello",
		Provider:        types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:     types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "test"},
		PromptBuilder:   types.PromptBuilderConfig{Type: "default"},
		ContextStrategy: types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:        types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		// EditStrategy intentionally omitted: the whole point of this
		// test is the empty-type → "multi" default chain.
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

	// ValidateRunConfig should have written "multi" into the (otherwise
	// empty) EditStrategy.Type before the factory ran.
	if config.EditStrategy.Type != "multi" {
		t.Errorf("expected ValidateRunConfig to default EditStrategy.Type to \"multi\", got %q", config.EditStrategy.Type)
	}

	// Execution mode defaults CodeScanner.Type to "patterns", which
	// wraps the base strategy in a ScannedStrategy. Unwrap once before
	// asserting the inner type.
	got := loop.Edit
	if scanned, ok := got.(*edit.ScannedStrategy); ok {
		got = scanned.Inner()
	}
	if _, ok := got.(*edit.MultiStrategy); !ok {
		t.Fatalf("expected loop.Edit to resolve to *edit.MultiStrategy (possibly via ScannedStrategy wrapper), got %T", loop.Edit)
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
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
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
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
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
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"}, // load-bearing: whole-file registers write_file (not edit_file), asserted below
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
	expectedTools := []string{"read_file", "list_directory", "grep_files", "find_files", "run_command", "write_file", "web_fetch", "spawn_agent"}
	for _, name := range expectedTools {
		if loop.Tools.Resolve(name) == nil {
			t.Errorf("expected tool %q to be registered", name)
		}
	}
}

// TestBuildLoopWithTransport_TraceEmitterHeaderResolutionError pins
// the factory init failure mode introduced by gh-100. When a
// TraceEmitter.Headers value references a "secret://" target that
// cannot be resolved, observability.ResolveHeaders returns a wrapped
// error; the factory must surface this with the "resolve trace
// emitter headers" prefix and run cleanup() so transient transports
// and intermediate components don't leak. Without this test, a future
// refactor that moves or drops the error return at factory.go:238-240
// would silently fall through to a misleading "no exporter could be
// created" log line at first export. Per synthesis MF-4.
func TestBuildLoopWithTransport_TraceEmitterHeaderResolutionError(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	// Ensure the referenced env var is *not* set so EnvSecretStore
	// returns a missing-secret error.
	if err := os.Unsetenv("STIRRUP_TEST_MISSING_VAR"); err != nil {
		t.Fatalf("unset env: %v", err)
	}

	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-test-header-resolve-fail",
		Mode:             "execution",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "test"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter: types.TraceEmitterConfig{
			Type:     "otel",
			Protocol: "http/protobuf",
			Endpoint: "https://example.invalid/otlp",
			Headers: map[string]string{
				"Authorization": "secret://STIRRUP_TEST_MISSING_VAR",
			},
		},
		RuleOfTwo: disableRuleOfTwo(),
		MaxTurns:  2,
		Timeout:   &timeout,
	}

	_, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
	if err == nil {
		t.Fatal("expected error when trace emitter header resolution fails")
	}
	if !strings.Contains(err.Error(), "resolve trace emitter headers") {
		t.Fatalf("expected error to be wrapped with 'resolve trace emitter headers' prefix, got: %v", err)
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
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
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
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
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
				EditStrategy:     types.EditStrategyConfig{Type: "multi"},
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

// TestBuildProvider_Gemini exercises the gemini arm of the factory's
// provider switch end-to-end: a ProviderConfig with type="gemini"
// must resolve credentials via the credential layer (here: a service-
// account JSON pointed to by GOOGLE_APPLICATION_CREDENTIALS so the
// implicit GoogleADCSource picks it up) and produce a GeminiAdapter.
//
// Closes test-audit critical gap 1 — without this test the gemini case
// in buildProvider was reachable only via end-to-end run, not via
// targeted unit coverage.
func TestBuildProvider_Gemini(t *testing.T) {
	keyPath := writeFakeServiceAccountJSON(t, t.TempDir())
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", keyPath)

	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:        "gemini",
		GCPProject:  "test-project",
		GCPLocation: "us-central1",
	}, &stubSecretStore{secrets: map[string]string{}})
	if err != nil {
		t.Fatalf("buildProvider returned error: %v", err)
	}
	if _, ok := prov.(*provider.GeminiAdapter); !ok {
		t.Errorf("buildProvider type = %T, want *provider.GeminiAdapter", prov)
	}
}

// TestBuildProvider_GeminiNilTokenSourceErrors covers the defensive
// nil-check in the gemini arm: a credential source that resolves
// successfully but with a nil BearerToken closure must produce a clear
// factory error rather than a nil-pointer panic later in Stream().
func TestBuildProvider_GeminiNilTokenSourceErrors(t *testing.T) {
	// Force ADC to fail by pointing GOOGLE_APPLICATION_CREDENTIALS at
	// a non-existent path AND clearing the metadata-server fallback. The
	// nil-bearer path is also reachable through a malformed ADC chain —
	// using a nonexistent path keeps the test deterministic.
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "no-such-file.json"))

	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:        "gemini",
		GCPProject:  "test-project",
		GCPLocation: "us-central1",
	}, &stubSecretStore{secrets: map[string]string{}})
	if err == nil {
		t.Fatal("expected error when credential source cannot produce a Google token")
	}
	// The error should mention either credential resolution failure
	// (FindDefaultCredentials path) or the gemini-specific guard. Both
	// are acceptable shapes; the point is the factory does not return
	// a half-built adapter.
	if !strings.Contains(err.Error(), "credentials") && !strings.Contains(err.Error(), "credential") {
		t.Errorf("error should mention credentials, got: %v", err)
	}
}

// TestBuildProvider_OpenAICompatibleWithRetryConfig asserts the factory
// propagates a non-nil ProviderRetryConfig through to the resulting
// OpenAICompatibleAdapter's RetryPolicy. Without this, a future change
// that hard-codes a zero RetryPolicy in `buildProvider` would silently
// disable the retry path for openai-compatible runs.
func TestBuildProvider_OpenAICompatibleWithRetryConfig(t *testing.T) {
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:      "openai-compatible",
		APIKeyRef: "secret://OPENAI_KEY",
		BaseURL:   "https://api.openai.com/v1",
		Retry: &types.ProviderRetryConfig{
			MaxAttempts:       5,
			InitialDelayMs:    200,
			MaxDelayMs:        10000,
			WallClockBudgetMs: 60000,
		},
	}, secrets)
	if err != nil {
		t.Fatalf("buildProvider returned error: %v", err)
	}
	adapter, ok := prov.(*provider.OpenAICompatibleAdapter)
	if !ok {
		t.Fatalf("buildProvider type = %T, want *provider.OpenAICompatibleAdapter", prov)
	}
	if got, want := adapter.RetryPolicy.MaxAttempts, 5; got != want {
		t.Errorf("RetryPolicy.MaxAttempts = %d, want %d", got, want)
	}
	if got, want := adapter.RetryPolicy.InitialDelay, 200*time.Millisecond; got != want {
		t.Errorf("RetryPolicy.InitialDelay = %v, want %v", got, want)
	}
	if got, want := adapter.RetryPolicy.MaxDelay, 10*time.Second; got != want {
		t.Errorf("RetryPolicy.MaxDelay = %v, want %v", got, want)
	}
	if got, want := adapter.RetryPolicy.WallClockBudget, 60*time.Second; got != want {
		t.Errorf("RetryPolicy.WallClockBudget = %v, want %v", got, want)
	}
}

// TestBuildProvider_OpenAICompatibleWithCompatProfile_ZAI pins the
// factory's resolveCompatProfile dispatch for the openai-compatible
// + CompatProfile="zai-glm" combination. Without this test the
// production injection path is uncovered (the zai package's own
// test mounts the rule on a hand-rolled registry rather than going
// through the factory), so a regression that drops the
// resolveCompatProfile call from buildProvider would not be caught.
//
// The assertion uses the adapter's exported Registry field to
// resolve the GLM model end-to-end: the registry returned by the
// factory must include zai.CompatRules(), so a glm-4-plus resolution
// produces TokenFieldMaxTokens and the tool_stream extra body field,
// and a glm-4.7 resolution additionally produces the thinking-family
// quirks (reasoning_content replay + the thinking extra body).
func TestBuildProvider_OpenAICompatibleWithCompatProfile_ZAI(t *testing.T) {
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:          "openai-compatible",
		APIKeyRef:     "secret://OPENAI_KEY",
		BaseURL:       "https://api.z.ai/api/coding/paas/v4",
		CompatProfile: "zai-glm",
	}, secrets)
	if err != nil {
		t.Fatalf("buildProvider returned error: %v", err)
	}
	adapter, ok := prov.(*provider.OpenAICompatibleAdapter)
	if !ok {
		t.Fatalf("buildProvider type = %T, want *provider.OpenAICompatibleAdapter", prov)
	}
	if adapter.Registry == nil {
		t.Fatal("adapter.Registry is nil; factory should have injected a registry containing the Z.ai compat rule")
	}
	q := adapter.Registry.Resolve("openai-compatible", "glm-4-plus")
	if got, want := q.BehaviourFlags.OpenAI.TokenField, quirks.TokenFieldMaxTokens; got != want {
		t.Errorf("glm-4-plus resolution: TokenField = %v, want %v (Z.ai compat rule did not fire — check resolveCompatProfile dispatch)", got, want)
	}
	v, ok := q.BehaviourFlags.OpenAI.ExtraBodyFields["tool_stream"]
	if !ok {
		t.Errorf("glm-4-plus resolution: ExtraBodyFields[tool_stream] missing; Z.ai rule should set it")
	} else if b, isBool := v.(bool); !isBool || !b {
		t.Errorf("glm-4-plus resolution: ExtraBodyFields[tool_stream] = %v (type %T), want true", v, v)
	}

	// A thinking-family model (glm-4.7) must additionally resolve the
	// reasoning_content replay field and the thinking extra body through
	// the same factory-built registry — proving resolveCompatProfile
	// returns the full CompatRules() slice, not just the base rule.
	qt := adapter.Registry.Resolve("openai-compatible", "glm-4.7")
	hasReasoning := false
	for _, p := range qt.ReplayFields {
		if p == "reasoning_content" {
			hasReasoning = true
			break
		}
	}
	if !hasReasoning {
		t.Errorf("glm-4.7 resolution: reasoning_content not in ReplayFields %v (thinking-family rule did not fire via factory)", qt.ReplayFields)
	}
	thinking, ok := qt.BehaviourFlags.OpenAI.ExtraBodyFields["thinking"]
	if !ok {
		t.Fatalf("glm-4.7 resolution: ExtraBodyFields[thinking] missing; thinking-family rule should set it")
	}
	tm, ok := thinking.(map[string]any)
	if !ok || len(tm) != 1 || tm["type"] != "enabled" {
		t.Errorf("glm-4.7 resolution: thinking = %#v, want map[string]any{\"type\":\"enabled\"}", thinking)
	}
}

// TestBuildProvider_OpenAICompatibleWithUnknownCompatProfile_Errors
// pins the defence-in-depth arm in resolveCompatProfile. The validator
// at types/runconfig.go rejects unknown compatProfile values at
// startup, but the factory still has a switch that returns an error
// for an unrecognised name (in case a non-CLI caller bypasses the
// validator). This test exercises that arm so a regression replacing
// it with a silent fallthrough is caught.
func TestBuildProvider_OpenAICompatibleWithUnknownCompatProfile_Errors(t *testing.T) {
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:          "openai-compatible",
		APIKeyRef:     "secret://OPENAI_KEY",
		BaseURL:       "https://api.example.com/v1",
		CompatProfile: "not-a-real-profile",
	}, secrets)
	if err == nil {
		t.Fatal("buildProvider with unknown compatProfile returned nil error; expected dispatch failure")
	}
	if !strings.Contains(err.Error(), "not-a-real-profile") {
		t.Errorf("error %q does not name the bad profile; operator could not locate the misconfiguration", err.Error())
	}
}

// TestBuildLoopWithTransport_OpenAICompatibleAdapterHasLogger asserts that
// BuildLoopWithTransport injects a non-nil Logger into the
// OpenAICompatibleAdapter, so DoWithRetry's warn output runs through the
// factory's ScrubHandler-backed logger rather than the slog.Default
// fallback. A future deletion of the `pa.Logger = logger` line in the
// factory would silently regress the scrub invariant for retry warnings;
// this test surfaces that regression at the assembly seam.
func TestBuildLoopWithTransport_OpenAICompatibleAdapterHasLogger(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-test-retry-logger",
		Mode:             "execution",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "test"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
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

	adapter, ok := unwrapNormalizer(loop.Provider).(*provider.OpenAICompatibleAdapter)
	if !ok {
		t.Fatalf("loop.Provider (after unwrap) type = %T, want *provider.OpenAICompatibleAdapter", unwrapNormalizer(loop.Provider))
	}
	if adapter.Logger == nil {
		t.Error("OpenAICompatibleAdapter.Logger is nil; factory should inject the ScrubHandler-backed logger so retry warnings get scrubbed")
	}
}

// writeFakeServiceAccountJSON produces a service-account JSON keyfile
// good enough to pass google.JWTConfigFromJSON's static parse — it is
// not signable against real Google IAM, which is fine for factory
// tests that never call Token().
func writeFakeServiceAccountJSON(t *testing.T, dir string) string {
	t.Helper()
	// Generating a fresh RSA key for each test would be slow; share one
	// across the package via the helper below.
	pemKey := factoryTestServiceAccountPEM(t)
	doc := map[string]any{
		"type":                        "service_account",
		"project_id":                  "test-project",
		"private_key_id":              "abc123",
		"private_key":                 pemKey,
		"client_email":                "test@test-project.iam.gserviceaccount.com",
		"client_id":                   "1234567890",
		"token_uri":                   "https://oauth2.googleapis.com/token",
		"auth_uri":                    "https://accounts.google.com/o/oauth2/auth",
		"auth_provider_x509_cert_url": "https://www.googleapis.com/oauth2/v1/certs",
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal sa json: %v", err)
	}
	path := filepath.Join(dir, "key.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write sa json: %v", err)
	}
	return path
}

// TestBuildProvider_OpenAICompatibleAzureWIF verifies that a RunConfig
// with provider.type=openai-compatible + credential.type=azure-workload-identity
// builds an *OpenAICompatibleAdapter without error. The adapter's
// bearer closure is wired through to the AzureWorkloadIdentitySource
// (which only reaches Entra lazily at first BearerToken call); the
// factory itself must not block on network IO during Resolve.
//
// We point the file token source at a local file rather than a
// projected k8s volume so the test stays self-contained.
func TestBuildProvider_OpenAICompatibleAzureWIF(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "azure-identity-token")
	if err := os.WriteFile(tokenPath, []byte("eyJ.fake.jwt"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:    "openai-compatible",
		BaseURL: "https://example.openai.azure.com/openai/v1",
		Credential: &types.CredentialConfig{
			Type:          "azure-workload-identity",
			AzureTenantID: "11111111-1111-1111-1111-111111111111",
			AzureClientID: "22222222-2222-2222-2222-222222222222",
			TokenSource: &types.TokenSourceConfig{
				Type: "file",
				Path: tokenPath,
			},
		},
	}, &stubSecretStore{secrets: map[string]string{}})
	if err != nil {
		t.Fatalf("buildProvider returned error: %v", err)
	}
	if _, ok := prov.(*provider.OpenAICompatibleAdapter); !ok {
		t.Errorf("buildProvider type = %T, want *provider.OpenAICompatibleAdapter", prov)
	}
}

// TestBuildProvider_OpenAIResponsesAzureWIF mirrors
// TestBuildProvider_OpenAICompatibleAzureWIF for the Responses adapter.
// Both adapters share the same bearer-closure plumbing
// (factory.go::buildProvider), so the two together pin the seam
// between credential.AzureWorkloadIdentitySource and the OpenAI
// adapter family.
func TestBuildProvider_OpenAIResponsesAzureWIF(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "azure-identity-token")
	if err := os.WriteFile(tokenPath, []byte("eyJ.fake.jwt"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:    "openai-responses",
		BaseURL: "https://example.openai.azure.com/openai/v1",
		Credential: &types.CredentialConfig{
			Type:          "azure-workload-identity",
			AzureTenantID: "11111111-1111-1111-1111-111111111111",
			AzureClientID: "22222222-2222-2222-2222-222222222222",
			TokenSource: &types.TokenSourceConfig{
				Type: "file",
				Path: tokenPath,
			},
		},
	}, &stubSecretStore{secrets: map[string]string{}})
	if err != nil {
		t.Fatalf("buildProvider returned error: %v", err)
	}
	if _, ok := prov.(*provider.OpenAIResponsesAdapter); !ok {
		t.Errorf("buildProvider type = %T, want *provider.OpenAIResponsesAdapter", prov)
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

// TestResourceOptionsFromConfig pins the three branches of
// resourceOptionsFromConfig in isolation. The full factory exercises the
// populated and empty branches indirectly, but the nil-config guard is
// structurally unreachable from BuildLoopWithTransport (which validates
// for nil before calling) and is easy to remove as "dead code" in a future
// refactor — this test documents that the guard is load-bearing for any
// caller that constructs ResourceOptions outside the agentic loop (eval
// runners, ad-hoc tools).
func TestResourceOptionsFromConfig(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		got := resourceOptionsFromConfig(nil)
		if got != (observability.ResourceOptions{}) {
			t.Errorf("nil config: got %+v, want zero ResourceOptions", got)
		}
	})

	t.Run("empty observability with mode", func(t *testing.T) {
		got := resourceOptionsFromConfig(&types.RunConfig{Mode: "execution"})
		want := observability.ResourceOptions{RunMode: "execution"}
		if got != want {
			t.Errorf("empty observability: got %+v, want %+v", got, want)
		}
	})

	t.Run("populated observability and mode", func(t *testing.T) {
		got := resourceOptionsFromConfig(&types.RunConfig{
			Mode: "planning",
			Observability: types.ObservabilityConfig{
				Environment:      "prod",
				ServiceNamespace: "eval",
			},
		})
		want := observability.ResourceOptions{
			Environment:      "prod",
			ServiceNamespace: "eval",
			RunMode:          "planning",
		}
		if got != want {
			t.Errorf("populated config: got %+v, want %+v", got, want)
		}
	})
}

// TestBuildProvider_AnthropicNilBearerErrors guards the defensive
// nil-bearer check at the top of the anthropic arm in buildProvider.
// A credential source that resolves cleanly but produces a nil
// BearerToken closure (e.g. AWSDefaultSource — its Resolved carries
// only AWSCredentials) must fail the factory with a clear error
// rather than yielding a half-built adapter that nil-panics on the
// first Stream() call.
//
// Sourced from a config with credential.type=aws-default piped into a
// non-AWS provider, which is the canonical mis-wiring shape the guard
// exists to catch.
func TestBuildProvider_AnthropicNilBearerErrors(t *testing.T) {
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:       "anthropic",
		Credential: &types.CredentialConfig{Type: "aws-default"},
	}, &stubSecretStore{secrets: map[string]string{}})
	if err == nil {
		t.Fatal("expected error when anthropic is wired to a non-bearer credential source")
	}
	if !strings.Contains(err.Error(), "bearer credential") {
		t.Errorf("error should mention bearer credential, got: %v", err)
	}
}

// TestBuildProvider_OpenAICompatibleNilBearerErrors mirrors the
// anthropic case for the openai-compatible arm.
func TestBuildProvider_OpenAICompatibleNilBearerErrors(t *testing.T) {
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:       "openai-compatible",
		Credential: &types.CredentialConfig{Type: "aws-default"},
	}, &stubSecretStore{secrets: map[string]string{}})
	if err == nil {
		t.Fatal("expected error when openai-compatible is wired to a non-bearer credential source")
	}
	if !strings.Contains(err.Error(), "bearer credential") {
		t.Errorf("error should mention bearer credential, got: %v", err)
	}
}

// TestBuildProvider_AnthropicWIF exercises the anthropic-wif arm of
// the factory's credential switch end-to-end: a ProviderConfig with
// type="anthropic" and credential.type="anthropic-wif" must construct
// an AnthropicWIFSource (via credential.BuildSource), wire its bearer
// closure into NewAnthropicAdapter, and produce a working adapter.
//
// The test does NOT exercise the OAuth token exchange itself — that
// is exhaustively covered by anthropic_wif_test.go in the credential
// package. The role here is the factory wiring: that the four
// federation IDs reach the source constructor and that the resulting
// adapter is the right type. Mirrors TestBuildProvider_Gemini in
// scope and shape.
func TestBuildProvider_AnthropicWIF(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "jwt")
	if err := os.WriteFile(tokenPath, []byte("eyJ.fake.jwt"), 0o600); err != nil {
		t.Fatalf("write jwt: %v", err)
	}

	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type: "anthropic",
		Credential: &types.CredentialConfig{
			Type:             "anthropic-wif",
			FederationRuleID: "fdrl_example",
			OrganizationID:   "550e8400-e29b-41d4-a716-446655440000",
			ServiceAccountID: "svac_example",
			WorkspaceID:      "default",
			TokenSource: &types.TokenSourceConfig{
				Type: "file",
				Path: tokenPath,
			},
		},
	}, &stubSecretStore{secrets: map[string]string{}})
	if err != nil {
		t.Fatalf("buildProvider returned error: %v", err)
	}
	adapter, ok := prov.(*provider.AnthropicAdapter)
	if !ok {
		t.Errorf("buildProvider type = %T, want *provider.AnthropicAdapter", prov)
		return
	}
	// BLOCKING B2 (issue #117): the factory must wire AuthModeBearer
	// when credential.type=anthropic-wif, otherwise the adapter sends
	// the WIF OAuth access token via x-api-key and Anthropic returns
	// 401 on every /v1/messages request.
	if got := adapter.AuthMode(); got != provider.AuthModeBearer {
		t.Errorf("AuthMode = %v, want AuthModeBearer (WIF requires Authorization: Bearer)", got)
	}
}

// TestBuildProvider_AnthropicStaticKeyUsesAPIKeyMode pins the factory's
// header-mode default for the static API key code path. Static keys
// (sk-ant-api03-...) ride x-api-key, not Authorization: Bearer; the
// adapter would still work for static keys via Bearer, but the symmetry
// matters as a regression guard against a future change to
// buildProvider that flips the default.
func TestBuildProvider_AnthropicStaticKeyUsesAPIKeyMode(t *testing.T) {
	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:      "anthropic",
		APIKeyRef: "secret://ANTHROPIC_API_KEY",
	}, &stubSecretStore{secrets: map[string]string{"secret://ANTHROPIC_API_KEY": "sk-ant-api03-fake"}})
	if err != nil {
		t.Fatalf("buildProvider returned error: %v", err)
	}
	adapter, ok := prov.(*provider.AnthropicAdapter)
	if !ok {
		t.Fatalf("buildProvider type = %T, want *provider.AnthropicAdapter", prov)
	}
	if got := adapter.AuthMode(); got != provider.AuthModeAPIKey {
		t.Errorf("AuthMode = %v, want AuthModeAPIKey for static-key path", got)
	}
}

// TestBuildProvider_AnthropicWIFMissingTokenSourceErrors guards the
// belt-and-braces field check inside the anthropic-wif arm of
// credential.BuildSource. Reciprocal validation in
// types.validateCredentialConfig already enforces the same shape at
// config-load time; this confirms BuildSource is self-contained for
// callers that bypass full RunConfig validation.
func TestBuildProvider_AnthropicWIFMissingTokenSourceErrors(t *testing.T) {
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type: "anthropic",
		Credential: &types.CredentialConfig{
			Type:             "anthropic-wif",
			FederationRuleID: "fdrl_example",
			OrganizationID:   "550e8400-e29b-41d4-a716-446655440000",
			ServiceAccountID: "svac_example",
			// TokenSource omitted — must surface as a clear error.
		},
	}, &stubSecretStore{secrets: map[string]string{}})
	if err == nil {
		t.Fatal("expected error when anthropic-wif is missing tokenSource")
	}
	if !strings.Contains(err.Error(), "tokenSource") {
		t.Errorf("error should mention tokenSource, got: %v", err)
	}
}

// TestBuildProvider_OpenAIResponsesNilBearerErrors mirrors the
// anthropic case for the openai-responses arm.
func TestBuildProvider_OpenAIResponsesNilBearerErrors(t *testing.T) {
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:       "openai-responses",
		Credential: &types.CredentialConfig{Type: "aws-default"},
	}, &stubSecretStore{secrets: map[string]string{}})
	if err == nil {
		t.Fatal("expected error when openai-responses is wired to a non-bearer credential source")
	}
	if !strings.Contains(err.Error(), "bearer credential") {
		t.Errorf("error should mention bearer credential, got: %v", err)
	}
}

// --- BatchAdapter wiring (phase 2 / #135) ---

// intPtr returns a pointer to v. Local helper to avoid pulling in a
// dependency just for a one-off literal pointer.
func intPtr(v int) *int { return &v }

// unwrapNormalizer peels off the *provider.NormalizingAdapter the
// factory now applies as the outermost wrap (#223), returning the
// inner adapter. Tests that assert on a specific concrete adapter
// type (BatchAdapter, OpenAICompatibleAdapter, …) use this so the
// assertion still describes the meaningful structural invariant
// (batch wrapping is present, logger is wired, …) without being
// coupled to the normalizer's existence.
func unwrapNormalizer(p provider.ProviderAdapter) provider.ProviderAdapter {
	if n, ok := p.(*provider.NormalizingAdapter); ok {
		return n.Unwrap()
	}
	return p
}

// TestBuildLoopWithTransport_BatchAdapterWiredWhenEnabled asserts the
// gRPC + batch.enabled wiring path in the factory: the top-level provider
// and the entry in the providers map are both replaced with a
// *provider.BatchAdapter. Without the map replacement, model-router
// lookups by provider type would silently bypass batching.
func TestBuildLoopWithTransport_BatchAdapterWiredWhenEnabled(t *testing.T) {
	t.Setenv("TEST_ANTHROPIC_KEY", "test-key")

	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-test-batch",
		Mode:             "planning",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://TEST_ANTHROPIC_KEY"},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "claude-sonnet-4-6"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		Transport:        types.TransportConfig{Type: "grpc"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		Tools:            types.ToolsConfig{BuiltIn: types.DefaultReadOnlyBuiltInTools()},
		RuleOfTwo:        disableRuleOfTwo(),
		MaxTurns:         2,
		Timeout:          &timeout,
	}
	config.Provider.Batch = &types.BatchProviderConfig{
		Enabled:               true,
		MaxWaitSeconds:        intPtr(86400),
		AllowInteractiveModes: true,
	}

	// Inject a transport so buildTransport (gRPC) does not try to dial.
	injected := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	loop, err := BuildLoopWithTransport(context.Background(), config, injected)
	if err != nil {
		t.Fatalf("BuildLoopWithTransport: %v", err)
	}
	defer func() { _ = loop.Close() }()

	ba, ok := unwrapNormalizer(loop.Provider).(*provider.BatchAdapter)
	if !ok {
		t.Fatalf("loop.Provider (after unwrap): got %T, want *provider.BatchAdapter", unwrapNormalizer(loop.Provider))
	}
	mapped, ok := loop.Providers[config.Provider.Type]
	if !ok {
		t.Fatalf("loop.Providers[%q] missing", config.Provider.Type)
	}
	if unwrapNormalizer(mapped) != ba {
		t.Errorf("loop.Providers[%q]: %T %p, want same *BatchAdapter %p", config.Provider.Type, mapped, mapped, ba)
	}
}

// TestBuildLoopWithTransport_BatchAdapterWiredOnStdio asserts the
// phase-4 polling client wiring: transport=stdio with
// HarnessSidePolling=true now wraps the streaming adapter in a
// *BatchAdapter (backed by *harnessPollingBatchClient — the BatchClient
// concrete type is unexported so we only assert the outer wrapper).
// The providers map is updated in lockstep so the model router does
// not bypass batching.
func TestBuildLoopWithTransport_BatchAdapterWiredOnStdio(t *testing.T) {
	t.Setenv("TEST_ANTHROPIC_KEY", "test-key")

	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-test-batch-stdio",
		Mode:             "planning",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://TEST_ANTHROPIC_KEY"},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "claude-sonnet-4-6"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		Transport:        types.TransportConfig{Type: "stdio"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		Tools:            types.ToolsConfig{BuiltIn: types.DefaultReadOnlyBuiltInTools()},
		RuleOfTwo:        disableRuleOfTwo(),
		MaxTurns:         2,
		Timeout:          &timeout,
	}
	config.Provider.Batch = &types.BatchProviderConfig{
		Enabled:               true,
		MaxWaitSeconds:        intPtr(86400),
		HarnessSidePolling:    true,
		AllowInteractiveModes: true,
	}

	injected := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	loop, err := BuildLoopWithTransport(context.Background(), config, injected)
	if err != nil {
		t.Fatalf("BuildLoopWithTransport: %v", err)
	}
	defer func() { _ = loop.Close() }()

	ba, ok := unwrapNormalizer(loop.Provider).(*provider.BatchAdapter)
	if !ok {
		t.Fatalf("loop.Provider (after unwrap): got %T, want *provider.BatchAdapter", unwrapNormalizer(loop.Provider))
	}
	mapped := loop.Providers[config.Provider.Type]
	if unwrapNormalizer(mapped) != ba {
		t.Errorf("loop.Providers[%q]: %T %p, want same *BatchAdapter %p", config.Provider.Type, mapped, mapped, ba)
	}
}

// TestBuildLoopWithTransport_BatchOnStdioAcceptsOpenAI pins the phase-6
// (#139) relaxation of the stdio-batch dispatch: openai-compatible and
// openai-responses are now valid provider types alongside anthropic.
// The factory must build the loop successfully (the BatchAdapter +
// harnessPollingBatchClient wiring is exercised by the
// harness/internal/provider tests).
func TestBuildLoopWithTransport_BatchOnStdioAcceptsOpenAI(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")

	for _, provType := range []string{"openai-compatible", "openai-responses"} {
		t.Run(provType, func(t *testing.T) {
			timeout := 30
			config := &types.RunConfig{
				RunID:            "factory-test-batch-stdio-" + provType,
				Mode:             "planning",
				Prompt:           "hello",
				Provider:         types.ProviderConfig{Type: provType, APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: "https://api.openai.com/v1"},
				ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: provType, Model: "gpt-4o"},
				PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
				ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
				Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
				EditStrategy:     types.EditStrategyConfig{Type: "multi"},
				Verifier:         types.VerifierConfig{Type: "none"},
				PermissionPolicy: types.PermissionPolicyConfig{Type: "deny-side-effects"},
				GitStrategy:      types.GitStrategyConfig{Type: "none"},
				Transport:        types.TransportConfig{Type: "stdio"},
				TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
				Tools:            types.ToolsConfig{BuiltIn: types.DefaultReadOnlyBuiltInTools()},
				RuleOfTwo:        disableRuleOfTwo(),
				MaxTurns:         2,
				Timeout:          &timeout,
			}
			config.Provider.Batch = &types.BatchProviderConfig{
				Enabled:               true,
				MaxWaitSeconds:        intPtr(86400),
				HarnessSidePolling:    true,
				AllowInteractiveModes: true,
			}

			injected := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
			loop, err := BuildLoopWithTransport(context.Background(), config, injected)
			if err != nil {
				t.Fatalf("BuildLoopWithTransport: %v", err)
			}
			if loop == nil {
				t.Fatal("expected non-nil loop")
			}

			// loop.Provider must be the BatchAdapter wrapper for the
			// stdio batch path (unwrapped from the outer NormalizingAdapter
			// added by #223); the providers map entry for this provider
			// type must point at the same wrapper so a model-router that
			// picks the default provider by type routes through batching
			// rather than bypassing it.
			ba, ok := unwrapNormalizer(loop.Provider).(*provider.BatchAdapter)
			if !ok {
				t.Fatalf("loop.Provider (after unwrap): got %T, want *provider.BatchAdapter", unwrapNormalizer(loop.Provider))
			}
			mapped, ok := loop.Providers[config.Provider.Type]
			if !ok {
				t.Fatalf("loop.Providers[%q] missing", config.Provider.Type)
			}
			if unwrapNormalizer(mapped) != ba {
				t.Errorf("loop.Providers[%q]: %T %p, want same *BatchAdapter %p", config.Provider.Type, mapped, mapped, ba)
			}
		})
	}
}

// --- normalizer wrap pinned at loop seam (#223) ---

// TestBuildLoopWithTransport_NormalizingAdapterWrapsGeminiProvider
// asserts the outermost wrap on a Gemini-built loop is the
// NormalizingAdapter. Existing Gemini factory tests call buildProvider
// directly, which returns _before_ the step 14c wrap in
// BuildLoopWithTransport; a refactor that dropped the Gemini wrap
// branch would not have been caught by them. This test reaches the
// loop seam end-to-end so the invariant is pinned where it matters —
// the only ProviderAdapter the loop ever calls Stream on.
func TestBuildLoopWithTransport_NormalizingAdapterWrapsGeminiProvider(t *testing.T) {
	keyPath := writeFakeServiceAccountJSON(t, t.TempDir())
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", keyPath)

	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-test-gemini-normalizer",
		Mode:             "planning",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "gemini", GCPProject: "test-project", GCPLocation: "us-central1"},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "gemini", Model: "gemini-2.5-pro"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
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
		t.Fatalf("BuildLoopWithTransport: %v", err)
	}
	defer func() { _ = loop.Close() }()

	// Outermost-wrap assertion: type-assert directly to
	// *provider.NormalizingAdapter rather than going through
	// unwrapNormalizer. The synthesis brief calls this out — the
	// invariant is "the loop sees the normalizer", not merely "a
	// normalizer exists somewhere in the chain".
	n, ok := loop.Provider.(*provider.NormalizingAdapter)
	if !ok {
		t.Fatalf("loop.Provider type = %T, want *provider.NormalizingAdapter (outermost wrap for gemini)", loop.Provider)
	}
	if _, ok := n.Unwrap().(*provider.GeminiAdapter); !ok {
		t.Errorf("unwrapped inner type = %T, want *provider.GeminiAdapter", n.Unwrap())
	}
	mapped, ok := loop.Providers[config.Provider.Type]
	if !ok {
		t.Fatalf("loop.Providers[%q] missing", config.Provider.Type)
	}
	if mapped != loop.Provider {
		t.Errorf("loop.Providers[%q] is not the same wrapper as loop.Provider (pointer identity broken)", config.Provider.Type)
	}
}

// TestBuildLoopWithTransport_NormalizingAdapterWrapsBedrockProvider
// asserts the outermost wrap on a Bedrock-built loop is the
// NormalizingAdapter. Bedrock's AWS SDK config loader is lazy: it
// constructs the client without hitting the network, so the factory
// build succeeds even without real AWS credentials — sufficient to
// pin step 14c's wrap for the Bedrock branch.
func TestBuildLoopWithTransport_NormalizingAdapterWrapsBedrockProvider(t *testing.T) {
	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-test-bedrock-normalizer",
		Mode:             "planning",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "bedrock", Region: "us-east-1"},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "bedrock", Model: "anthropic.claude-3-5-sonnet-20241022-v2:0"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
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
		t.Fatalf("BuildLoopWithTransport: %v", err)
	}
	defer func() { _ = loop.Close() }()

	n, ok := loop.Provider.(*provider.NormalizingAdapter)
	if !ok {
		t.Fatalf("loop.Provider type = %T, want *provider.NormalizingAdapter (outermost wrap for bedrock)", loop.Provider)
	}
	if _, ok := n.Unwrap().(*provider.BedrockAdapter); !ok {
		t.Errorf("unwrapped inner type = %T, want *provider.BedrockAdapter", n.Unwrap())
	}
	mapped, ok := loop.Providers[config.Provider.Type]
	if !ok {
		t.Fatalf("loop.Providers[%q] missing", config.Provider.Type)
	}
	if mapped != loop.Provider {
		t.Errorf("loop.Providers[%q] is not the same wrapper as loop.Provider (pointer identity broken)", config.Provider.Type)
	}
}

// TestBuildLoopWithTransport_BedrockAdapterHasLogger asserts that
// BuildLoopWithTransport injects a non-nil Logger into the BedrockAdapter,
// so its tool-choice downgrade warn runs through the factory's
// ScrubHandler-backed logger (carrying run/trace correlation) rather than
// the slog.Default fallback. A future deletion of the `pa.Logger = logger`
// line in the *provider.BedrockAdapter factory arm would silently regress
// the scrub and correlation invariants for that warn; this test surfaces
// the regression at the assembly seam.
func TestBuildLoopWithTransport_BedrockAdapterHasLogger(t *testing.T) {
	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-test-bedrock-logger",
		Mode:             "planning",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "bedrock", Region: "us-east-1"},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "bedrock", Model: "anthropic.claude-3-5-sonnet-20241022-v2:0"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
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
		t.Fatalf("BuildLoopWithTransport: %v", err)
	}
	defer func() { _ = loop.Close() }()

	adapter, ok := unwrapNormalizer(loop.Provider).(*provider.BedrockAdapter)
	if !ok {
		t.Fatalf("loop.Provider (after unwrap) type = %T, want *provider.BedrockAdapter", unwrapNormalizer(loop.Provider))
	}
	if adapter.Logger == nil {
		t.Error("BedrockAdapter.Logger is nil; factory should inject the ScrubHandler-backed logger so the tool-choice downgrade warn keeps run/trace correlation and scrubbing")
	}
}

// TestBuildLoopWithTransport_AnthropicAdapterHasLogger asserts that
// BuildLoopWithTransport injects a non-nil Logger into the
// AnthropicAdapter, so the "quirks suppressed caller temperature" warn
// (fired for Claude Opus 4.7+, Sonnet 5, and Fable 5/Mythos 5) runs
// through the factory's ScrubHandler-backed logger rather than the
// slog.Default fallback. A future deletion of the `pa.Logger = logger`
// line in the *provider.AnthropicAdapter factory arm would silently
// regress the scrub and correlation invariants for that warn; this test
// surfaces that regression at the assembly seam, mirroring the sibling
// OpenAI-compatible and Bedrock tests above.
func TestBuildLoopWithTransport_AnthropicAdapterHasLogger(t *testing.T) {
	t.Setenv("TEST_ANTHROPIC_KEY", "test-key")

	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-test-anthropic-logger",
		Mode:             "planning",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://TEST_ANTHROPIC_KEY"},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "claude-sonnet-5"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
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
		t.Fatalf("BuildLoopWithTransport: %v", err)
	}
	defer func() { _ = loop.Close() }()

	adapter, ok := unwrapNormalizer(loop.Provider).(*provider.AnthropicAdapter)
	if !ok {
		t.Fatalf("loop.Provider (after unwrap) type = %T, want *provider.AnthropicAdapter", unwrapNormalizer(loop.Provider))
	}
	if adapter.Logger == nil {
		t.Error("AnthropicAdapter.Logger is nil; factory should inject the ScrubHandler-backed logger so the suppressed-temperature warn keeps run/trace correlation and scrubbing")
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

// TestBuildLoopWithTransport_OpenAIResponsesAdapterHasLogger asserts that
// BuildLoopWithTransport injects a non-nil Logger into the
// OpenAIResponsesAdapter, so its quirks-resolution debug log and
// tool-choice downgrade warn run through the factory's ScrubHandler-backed
// logger rather than the slog.Default fallback. A future deletion of the
// `pa.Logger = logger` line in the *provider.OpenAIResponsesAdapter factory
// arm would silently regress the scrub and correlation invariants for that
// output; this test surfaces the regression at the assembly seam, mirroring
// the Bedrock and OpenAICompatible equivalents.
func TestBuildLoopWithTransport_OpenAIResponsesAdapterHasLogger(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-test-responses-logger",
		Mode:             "execution",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "openai-responses", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-responses", Model: "test"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window"},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: t.TempDir()},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
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

	adapter, ok := unwrapNormalizer(loop.Provider).(*provider.OpenAIResponsesAdapter)
	if !ok {
		t.Fatalf("loop.Provider (after unwrap) type = %T, want *provider.OpenAIResponsesAdapter", unwrapNormalizer(loop.Provider))
	}
	if adapter.Logger == nil {
		t.Error("OpenAIResponsesAdapter.Logger is nil; factory should inject the ScrubHandler-backed logger so the quirks debug log and tool-choice downgrade warn keep run/trace correlation and scrubbing")
	}
}
