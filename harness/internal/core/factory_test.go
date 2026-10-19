package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	pb "github.com/rxbynerd/stirrup/gen/harness/v1"
	"github.com/rxbynerd/stirrup/harness/internal/commandoutput"
	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/hook"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/observability/otlp"
	"github.com/rxbynerd/stirrup/harness/internal/observability/platform"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/transport/tlstest"
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
		require.NoError(t, err, "generate RSA key")
		der, err := x509.MarshalPKCS8PrivateKey(key)
		require.NoError(t, err, "marshal PKCS8 key")
		factoryRSAKeyPEM = string(pem.EncodeToMemory(&pem.Block{
			Type:  "PRIVATE KEY",
			Bytes: der,
		}))
	})
	return factoryRSAKeyPEM
}

// repoRootForTests returns the absolute repo root so tests can locate
// examples/ regardless of the working directory.
func repoRootForTests(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller(0) failed")
	// This file lives in harness/internal/core, three levels below the root.
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
}

// disableRuleOfTwo overrides the Rule-of-Two invariant so factory tests can
// exercise the all-three case without validator rejection; Rule-of-Two
// semantics themselves are covered in types/runconfig_test.go.
func disableRuleOfTwo() *types.RuleOfTwoConfig {
	enforce := false
	return &types.RuleOfTwoConfig{Enforce: &enforce}
}

func TestBuildRouter_StaticSelectsConfiguredModel(t *testing.T) {
	r := buildRouter(types.ModelRouterConfig{
		Type:     "static",
		Provider: "anthropic",
		Model:    "claude-sonnet-4-6",
	}, "anthropic")

	sel := r.Select(context.TODO(), router.RouterContext{})
	require.Equal(t, "anthropic", sel.Provider)
	require.Equal(t, "claude-sonnet-4-6", sel.Model)
}

func TestBuildRouter_StaticDefaultsProvider(t *testing.T) {
	r := buildRouter(types.ModelRouterConfig{
		Type:  "static",
		Model: "custom-model",
	}, "bedrock")

	sel := r.Select(context.TODO(), router.RouterContext{})
	require.Equal(t, "bedrock", sel.Provider)
}

func TestBuildRouter_PerModeSelectsModeModel(t *testing.T) {
	r := buildRouter(types.ModelRouterConfig{
		Type:       "per-mode",
		Provider:   "anthropic",
		Model:      "default-model",
		ModeModels: map[string]string{"planning": "bedrock/plan-model"},
	}, "anthropic")

	sel := r.Select(context.TODO(), router.RouterContext{Mode: "planning"})
	require.Equal(t, "bedrock", sel.Provider)
	require.Equal(t, "plan-model", sel.Model)
}

func TestBuildRouter_DynamicSelectsConfiguredProvider(t *testing.T) {
	r := buildRouter(types.ModelRouterConfig{
		Type:     "dynamic",
		Provider: "anthropic",
		Model:    "default-model",
	}, "anthropic")

	sel := r.Select(context.TODO(), router.RouterContext{Turn: 0})
	require.Equal(t, "anthropic", sel.Provider)
}

func TestBuildRouter_EmptyConfigFallsBackToDefaults(t *testing.T) {
	r := buildRouter(types.ModelRouterConfig{}, "")

	sel := r.Select(context.TODO(), router.RouterContext{})
	require.Equal(t, "anthropic", sel.Provider)
	require.Equal(t, "claude-sonnet-4-6", sel.Model)
}

func TestBuildPerModeRouter_ModeModelWithSlash(t *testing.T) {
	r := buildPerModeRouter(types.ModelRouterConfig{
		ModeModels: map[string]string{"review": "bedrock/review-model"},
	}, "anthropic")

	sel := r.Select(context.TODO(), router.RouterContext{Mode: "review"})
	require.Equal(t, "bedrock", sel.Provider)
	require.Equal(t, "review-model", sel.Model)
}

func TestBuildPerModeRouter_ModeModelWithoutSlash(t *testing.T) {
	r := buildPerModeRouter(types.ModelRouterConfig{
		ModeModels: map[string]string{"review": "review-model"},
	}, "anthropic")

	sel := r.Select(context.TODO(), router.RouterContext{Mode: "review"})
	require.Equal(t, "anthropic", sel.Provider)
	require.Equal(t, "review-model", sel.Model)
}

func TestBuildPerModeRouter_DefaultsApplied(t *testing.T) {
	r := buildPerModeRouter(types.ModelRouterConfig{}, "")

	sel := r.Select(context.TODO(), router.RouterContext{Mode: "execution"})
	require.Equal(t, "anthropic", sel.Provider)
	require.Equal(t, "claude-sonnet-4-6", sel.Model)
}

func TestBuildDynamicRouter_Defaults(t *testing.T) {
	r := buildDynamicRouter(types.ModelRouterConfig{}, "")

	sel := r.Select(context.TODO(), router.RouterContext{Turn: 0})
	require.Equal(t, "anthropic", sel.Provider)
}

func TestBuildDynamicRouter_CustomThresholds(t *testing.T) {
	r := buildDynamicRouter(types.ModelRouterConfig{
		ExpensiveTurnThreshold:  5,
		ExpensiveTokenThreshold: 10000,
		CheapModel:              "haiku",
		ExpensiveModel:          "opus",
	}, "anthropic")

	t.Run("under threshold uses cheap model", func(t *testing.T) {
		sel := r.Select(context.TODO(), router.RouterContext{Turn: 0, LastStopReason: "tool_use"})
		require.Equal(t, "haiku", sel.Model)
	})
	t.Run("over threshold uses expensive model", func(t *testing.T) {
		sel := r.Select(context.TODO(), router.RouterContext{Turn: 6})
		require.Equal(t, "opus", sel.Model)
	})
}

func mustBuildPromptBuilder(t *testing.T, config *types.RunConfig) prompt.PromptBuilder {
	t.Helper()
	pb, err := buildPromptBuilder(config)
	require.NoError(t, err, "buildPromptBuilder")
	return pb
}

func TestBuildPromptBuilder_DefaultType(t *testing.T) {
	pb := mustBuildPromptBuilder(t, &types.RunConfig{PromptBuilder: types.PromptBuilderConfig{Type: "default"}})
	require.IsType(t, (*prompt.DefaultPromptBuilder)(nil), pb)
}

func TestBuildPromptBuilder_EmptyTypeUsesDefault(t *testing.T) {
	pb := mustBuildPromptBuilder(t, &types.RunConfig{})
	require.IsType(t, (*prompt.DefaultPromptBuilder)(nil), pb)
}

func TestBuildPromptBuilder_ComposedType(t *testing.T) {
	pb := mustBuildPromptBuilder(t, &types.RunConfig{PromptBuilder: types.PromptBuilderConfig{Type: "composed"}})
	require.IsType(t, (*prompt.ComposedPromptBuilder)(nil), pb)
}

func TestBuildPromptBuilder_UnknownFallsBackToDefault(t *testing.T) {
	pb := mustBuildPromptBuilder(t, &types.RunConfig{PromptBuilder: types.PromptBuilderConfig{Type: "nonexistent"}})
	require.IsType(t, (*prompt.DefaultPromptBuilder)(nil), pb)
}

func TestBuildPromptBuilder_SystemPromptOverrideIsComposed(t *testing.T) {
	pb := mustBuildPromptBuilder(t, &types.RunConfig{
		PromptBuilder:        types.PromptBuilderConfig{Type: "default"},
		SystemPromptOverride: "Custom system prompt",
	})
	require.IsType(t, (*prompt.ComposedPromptBuilder)(nil), pb)
}

func TestBuildPromptBuilder_OperatorTemplateIsComposed(t *testing.T) {
	pb := mustBuildPromptBuilder(t, &types.RunConfig{
		Mode:          "execution",
		PromptBuilder: types.PromptBuilderConfig{Template: `Tuned.{{if eq .Tier "frontier"}} Act when ready.{{end}}`},
	})
	require.IsType(t, (*prompt.ComposedPromptBuilder)(nil), pb)
}

// A template that parses but fails at execution (unknown field) must be
// caught by the trial render at construction, not at run start.
func TestBuildPromptBuilder_OperatorTemplateExecErrorFailsFast(t *testing.T) {
	_, err := buildPromptBuilder(&types.RunConfig{
		Mode:          "execution",
		PromptBuilder: types.PromptBuilderConfig{Template: "hello {{.NoSuchField}}"},
	})
	require.Error(t, err)
}

// The override wins over an operator template. ValidateRunConfig rejects
// the combination, but the factory's precedence must still be safe if a
// caller skips validation.
func TestBuildPromptBuilder_OverrideWinsOverTemplate(t *testing.T) {
	const override = "Raw override with {{not a template}}"
	pb := mustBuildPromptBuilder(t, &types.RunConfig{
		SystemPromptOverride: override,
		PromptBuilder:        types.PromptBuilderConfig{Template: "Tuned."},
	})
	got, err := pb.Build(t.Context(), prompt.PromptContext{Mode: "execution"})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(got, override), "override not used verbatim, got %q", got)
}

func TestBuildContextStrategy_SlidingWindow(t *testing.T) {
	cs := buildContextStrategy(types.ContextStrategyConfig{Type: "sliding-window"}, nil, "", 0, nil, nil)
	require.IsType(t, (*contextpkg.SlidingWindowStrategy)(nil), cs)
}

func TestBuildContextStrategy_EmptyTypeUsesSlidingWindow(t *testing.T) {
	cs := buildContextStrategy(types.ContextStrategyConfig{}, nil, "", 0, nil, nil)
	require.IsType(t, (*contextpkg.SlidingWindowStrategy)(nil), cs)
}

func TestBuildContextStrategy_Summarise(t *testing.T) {
	cs := buildContextStrategy(types.ContextStrategyConfig{Type: "summarise"}, nil, "model", 0, nil, nil)
	require.IsType(t, (*contextpkg.SummariseStrategy)(nil), cs)
}

func TestBuildContextStrategy_OffloadToFile(t *testing.T) {
	exec, _ := executor.NewLocalExecutor(t.TempDir())
	cs := buildContextStrategy(types.ContextStrategyConfig{Type: "offload-to-file"}, nil, "", 0, exec, nil)
	require.IsType(t, (*contextpkg.OffloadToFileStrategy)(nil), cs)
}

func TestBuildContextStrategy_UnknownFallsBack(t *testing.T) {
	cs := buildContextStrategy(types.ContextStrategyConfig{Type: "nonexistent"}, nil, "", 0, nil, nil)
	require.IsType(t, (*contextpkg.SlidingWindowStrategy)(nil), cs)
}

func TestBuildEditStrategy_WholeFile(t *testing.T) {
	es := buildEditStrategy(types.EditStrategyConfig{Type: "whole-file"})
	require.IsType(t, (*edit.WholeFileStrategy)(nil), es)
}

// TestBuildEditStrategy_EmptyTypeUsesMulti pins the factory fallback for
// callers that bypass types.ValidateRunConfig, which fills an empty type
// with "multi". The fallback must match that default rather than expose a
// different write-tool surface.
func TestBuildEditStrategy_EmptyTypeUsesMulti(t *testing.T) {
	es := buildEditStrategy(types.EditStrategyConfig{})
	require.IsType(t, (*edit.MultiStrategy)(nil), es)
}

func TestBuildEditStrategy_SearchReplace(t *testing.T) {
	es := buildEditStrategy(types.EditStrategyConfig{Type: "search-replace"})
	require.IsType(t, (*edit.SearchReplaceStrategy)(nil), es)
}

func TestBuildEditStrategy_Udiff(t *testing.T) {
	es := buildEditStrategy(types.EditStrategyConfig{Type: "udiff"})
	require.IsType(t, (*edit.UdiffStrategy)(nil), es)
}

func TestBuildEditStrategy_Multi(t *testing.T) {
	es := buildEditStrategy(types.EditStrategyConfig{Type: "multi"})
	require.IsType(t, (*edit.MultiStrategy)(nil), es)
}

func TestBuildEditStrategy_CustomFuzzyThreshold(t *testing.T) {
	threshold := 0.95
	es := buildEditStrategy(types.EditStrategyConfig{Type: "udiff", FuzzyThreshold: &threshold})
	require.IsType(t, (*edit.UdiffStrategy)(nil), es)
}

// buildEditStrategy must clamp a non-positive fuzzy threshold for callers that
// skip types.ValidateRunConfig. A diff that cannot match must produce a clean
// no-match result rather than panic in the udiff fuzzy search.
func TestBuildEditStrategy_OutOfRangeFuzzyThresholdClamped(t *testing.T) {
	exec, err := executor.NewLocalExecutor(t.TempDir())
	require.NoError(t, err, "NewLocalExecutor")
	require.NoError(t, exec.WriteFile(context.Background(), "file.txt", "aaaa\nbbbb\ncccc\n"), "WriteFile")

	badThreshold := 0.0
	es := buildEditStrategy(types.EditStrategyConfig{Type: "udiff", FuzzyThreshold: &badThreshold})

	diff := "--- a/file.txt\n+++ b/file.txt\n@@ -1,1 +1,1 @@\n-zzzz\n+yyyy\n"
	input, err := json.Marshal(struct {
		Path string `json:"path"`
		Diff string `json:"diff"`
	}{Path: "file.txt", Diff: diff})
	require.NoError(t, err, "marshal input")

	result, err := es.Apply(context.Background(), input, exec)
	require.NoError(t, err, "a non-matching diff is a no-match result, not an error")
	require.False(t, result.Applied, "diff cannot match aaaa/bbbb/cccc")
}

func TestBuildEditStrategy_UnknownFallsBack(t *testing.T) {
	es := buildEditStrategy(types.EditStrategyConfig{Type: "nonexistent"})
	require.IsType(t, (*edit.MultiStrategy)(nil), es)
}

func TestWrapWithCodeScanner_NilLeavesInnerUnchanged(t *testing.T) {
	inner := edit.NewWholeFileStrategy()
	got, err := wrapWithCodeScanner(inner, nil, nil)
	require.NoError(t, err)
	require.Same(t, inner, got, "nil scanner config must return the inner strategy")
}

func TestWrapWithCodeScanner_NoneLeavesInnerUnchanged(t *testing.T) {
	inner := edit.NewWholeFileStrategy()
	got, err := wrapWithCodeScanner(inner, &types.CodeScannerConfig{Type: "none"}, nil)
	require.NoError(t, err)
	require.Same(t, inner, got, "type none must return the inner strategy")
}

func TestWrapWithCodeScanner_PatternsWraps(t *testing.T) {
	inner := edit.NewWholeFileStrategy()
	got, err := wrapWithCodeScanner(inner, &types.CodeScannerConfig{Type: "patterns"}, nil)
	require.NoError(t, err)
	require.NotSame(t, inner, got, "patterns scanner must wrap the inner strategy")
	require.IsType(t, (*edit.ScannedStrategy)(nil), got)
}

func TestWrapWithCodeScanner_UnknownTypeReturnsError(t *testing.T) {
	inner := edit.NewWholeFileStrategy()
	_, err := wrapWithCodeScanner(inner, &types.CodeScannerConfig{Type: "nope"}, nil)
	require.Error(t, err)
}

func TestBuildVerifier_None(t *testing.T) {
	v := buildVerifier(types.VerifierConfig{Type: "none"}, nil, nil)
	require.IsType(t, (*verifier.NoneVerifier)(nil), v)
}

func TestBuildVerifier_EmptyTypeUsesNone(t *testing.T) {
	v := buildVerifier(types.VerifierConfig{}, nil, nil)
	require.IsType(t, (*verifier.NoneVerifier)(nil), v)
}

func TestBuildVerifier_TestRunner(t *testing.T) {
	v := buildVerifier(types.VerifierConfig{Type: "test-runner", Command: "go test ./..."}, nil, nil)
	require.IsType(t, (*verifier.TestRunnerVerifier)(nil), v)
}

func TestBuildVerifier_LLMJudge(t *testing.T) {
	v := buildVerifier(types.VerifierConfig{Type: "llm-judge", Criteria: "test criteria"}, nil, nil)
	require.IsType(t, (*verifier.LLMJudgeVerifier)(nil), v)
}

func TestBuildVerifier_Composite(t *testing.T) {
	v := buildVerifier(types.VerifierConfig{
		Type: "composite",
		Verifiers: []types.VerifierConfig{
			{Type: "none"},
			{Type: "test-runner", Command: "echo ok"},
		},
	}, nil, nil)
	require.IsType(t, (*verifier.CompositeVerifier)(nil), v)
}

func TestBuildVerifier_UnknownFallsBack(t *testing.T) {
	v := buildVerifier(types.VerifierConfig{Type: "nonexistent"}, nil, nil)
	require.IsType(t, (*verifier.NoneVerifier)(nil), v)
}

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

	require.Contains(t, buf.String(), `"event":"rule_of_two_disabled"`)
}

func TestEmitRuleOfTwoEvents_AllThreeWithoutOverrideStaysSilent(t *testing.T) {
	// ask-upstream with all three flags is allowed without an explicit
	// override, so no rule_of_two_disabled event is emitted.
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

	require.NotContains(t, buf.String(), "rule_of_two_disabled")
}

func TestEmitRuleOfTwoEvents_TwoOfThreeEmitsWarning(t *testing.T) {
	// Each two-of-three pair emits rule_of_two_warning with the payload
	// booleans true for the two flags that hold and false for the third.
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
				// web_fetch is untrusted and external. SensitiveData is
				// unset, so the sensitive leg stays false.
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
				// run_command on a bridge network is external, and
				// SensitiveData declares the sensitive leg. Without
				// web_fetch, DynamicContext or MCP servers the run is not
				// untrusted.
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
			require.Contains(t, out, `"event":"rule_of_two_warning"`)
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
	require.Contains(t, out, fmt.Sprintf(`"%s":%t`, key, want))
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
	require.Contains(t, out, `"event":"rule_of_two_disabled"`)
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
	require.Empty(t, buf.String(), "zero-or-one flag config should emit nothing")
}

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
	require.NoError(t, err)
	return pp
}

func TestBuildPermissionPolicy_AllowAll(t *testing.T) {
	pp := buildPermissionPolicyForTest(t, types.PermissionPolicyConfig{Type: "allow-all"}, nil, nil)
	require.IsType(t, (*permission.AllowAll)(nil), pp)
}

func TestBuildPermissionPolicy_DenySideEffects(t *testing.T) {
	registry := buildToolRegistry(&registryExecutor{
		caps: executor.ExecutorCapabilities{CanRead: true, CanWrite: true, CanExec: true},
	}, edit.NewWholeFileStrategy(), types.ToolsConfig{}, nil)
	pp := buildPermissionPolicyForTest(t, types.PermissionPolicyConfig{Type: "deny-side-effects"}, registry, nil)
	require.IsType(t, (*permission.DenySideEffects)(nil), pp)
}

func TestBuildPermissionPolicy_AskUpstream(t *testing.T) {
	registry := buildToolRegistry(&registryExecutor{
		caps: executor.ExecutorCapabilities{CanRead: true},
	}, edit.NewWholeFileStrategy(), types.ToolsConfig{}, nil)
	tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	pp := buildPermissionPolicyForTest(t, types.PermissionPolicyConfig{Type: "ask-upstream", Timeout: 60}, registry, tp)
	require.IsType(t, (*permission.AskUpstreamPolicy)(nil), pp)
}

// An unrecognised or empty PermissionPolicy.Type returns an error instead of
// falling through to allow-all, even when config validation was bypassed.
func TestBuildPermissionPolicy_UnknownTypeReturnsError(t *testing.T) {
	registry := buildToolRegistry(&registryExecutor{
		caps: executor.ExecutorCapabilities{CanRead: true},
	}, edit.NewWholeFileStrategy(), types.ToolsConfig{}, nil)

	t.Run("unknown type", func(t *testing.T) {
		rc := &types.RunConfig{
			PermissionPolicy: types.PermissionPolicyConfig{Type: "bogus"},
		}
		_, err := buildPermissionPolicy(rc, registry, nil, nil)
		require.Error(t, err)
	})

	t.Run("empty type", func(t *testing.T) {
		rc := &types.RunConfig{PermissionPolicy: types.PermissionPolicyConfig{}}
		_, err := buildPermissionPolicy(rc, registry, nil, nil)
		require.Error(t, err)
	})
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
	}, edit.NewWholeFileStrategy(), types.ToolsConfig{}, nil)
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
	require.NoError(t, err)
	require.IsType(t, (*permission.PolicyEnginePolicy)(nil), pp)
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
	}, edit.NewWholeFileStrategy(), types.ToolsConfig{}, nil)
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
	require.NoError(t, err)
	require.IsType(t, (*permission.PolicyEnginePolicy)(nil), pp)
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
	require.Error(t, err)
}

// Wrapping a built policy-engine policy with metrics composes the policy
// without reading the Cedar file again. The file is removed before the wrap,
// so any second read would fail.
func TestBuildPermissionPolicy_PolicyEngineFileReadOnce(t *testing.T) {
	starter := filepath.Join(repoRootForTests(t), "examples", "policies", "destructive-shell.cedar")
	if _, err := os.Stat(starter); err != nil {
		t.Skipf("starter policy missing at %q: %v", starter, err)
	}
	contents, err := os.ReadFile(starter)
	require.NoError(t, err)
	tempPath := filepath.Join(t.TempDir(), "test-policy.cedar")
	require.NoError(t, os.WriteFile(tempPath, contents, 0o600))

	registry := buildToolRegistry(&registryExecutor{
		caps: executor.ExecutorCapabilities{CanRead: true, CanWrite: true, CanExec: true},
	}, edit.NewWholeFileStrategy(), types.ToolsConfig{}, nil)
	rc := &types.RunConfig{
		RunID: "test-run",
		Mode:  "execution",
		PermissionPolicy: types.PermissionPolicyConfig{
			Type:       "policy-engine",
			PolicyFile: tempPath,
		},
	}

	pp, err := buildPermissionPolicy(rc, registry, nil, nil)
	require.NoError(t, err)

	require.NoError(t, os.Remove(tempPath))

	// A nil metrics argument makes the wrap a no-op that returns the inner policy.
	wrapped := wrapPermissionPolicyMetrics(pp, rc.PermissionPolicy, nil)
	require.NotNil(t, wrapped)
	require.Same(t, pp, wrapped, "nil metrics wrap should return inner unchanged")
}

func TestBuildGitStrategy_None(t *testing.T) {
	gs := buildGitStrategy(types.GitStrategyConfig{Type: "none"}, nil)
	require.IsType(t, (*git.NoneGitStrategy)(nil), gs)
}

func TestBuildGitStrategy_Empty(t *testing.T) {
	gs := buildGitStrategy(types.GitStrategyConfig{}, nil)
	require.IsType(t, (*git.NoneGitStrategy)(nil), gs)
}

func TestBuildGitStrategy_Deterministic(t *testing.T) {
	exec, err := executor.NewLocalExecutor(t.TempDir())
	require.NoError(t, err)
	gs := buildGitStrategy(types.GitStrategyConfig{Type: "deterministic"}, exec)
	require.IsType(t, (*git.DeterministicGitStrategy)(nil), gs)
}

func TestBuildGitStrategy_UnknownFallsBack(t *testing.T) {
	gs := buildGitStrategy(types.GitStrategyConfig{Type: "nonexistent"}, nil)
	require.IsType(t, (*git.NoneGitStrategy)(nil), gs)
}

func TestBuildHookRunner_NilConfigReturnsNoop(t *testing.T) {
	r := buildHookRunner(nil, nil, nil)
	require.IsType(t, (*hook.Noop)(nil), r)
}

func TestBuildHookRunner_EmptyConfigReturnsNoop(t *testing.T) {
	r := buildHookRunner(&types.HooksConfig{}, nil, nil)
	require.IsType(t, (*hook.Noop)(nil), r)
}

func TestBuildHookRunner_PreRunOnlyReturnsExecRunner(t *testing.T) {
	cfg := &types.HooksConfig{PreRun: []types.HookConfig{{Command: "true"}}}
	r := buildHookRunner(cfg, nil, nil)
	execRunner, ok := r.(*hook.ExecRunner)
	require.True(t, ok, "expected *hook.ExecRunner, got %T", r)
	require.Same(t, cfg, execRunner.Hooks, "ExecRunner.Hooks must be the HooksConfig passed in")
}

func TestBuildHookRunner_PostRunOnlyReturnsExecRunner(t *testing.T) {
	cfg := &types.HooksConfig{PostRun: []types.HookConfig{{Command: "true"}}}
	r := buildHookRunner(cfg, nil, nil)
	require.IsType(t, (*hook.ExecRunner)(nil), r)
}

func TestBuildTraceEmitter_JSONLWithoutPath(t *testing.T) {
	te, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{Type: "jsonl"}, platform.Exporters{}, observability.ResourceOptions{}, false)
	require.NoError(t, err)
	require.IsType(t, (*trace.JSONLTraceEmitter)(nil), te)
}

func TestBuildTraceEmitter_JSONLWithPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	te, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{Type: "jsonl", FilePath: path}, platform.Exporters{}, observability.ResourceOptions{}, false)
	require.NoError(t, err)
	require.IsType(t, (*trace.JSONLTraceEmitter)(nil), te)
	_, err = os.Stat(path)
	require.NoError(t, err, "expected trace file to exist")
}

func TestBuildTraceEmitter_EmptyTypeDefaultsToJSONL(t *testing.T) {
	te, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{}, platform.Exporters{}, observability.ResourceOptions{}, false)
	require.NoError(t, err)
	require.IsType(t, (*trace.JSONLTraceEmitter)(nil), te)
}

func TestBuildTraceEmitter_UnsupportedType(t *testing.T) {
	_, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{Type: "datadog"}, platform.Exporters{}, observability.ResourceOptions{}, false)
	require.ErrorContains(t, err, "unsupported trace emitter type")
}

func TestResolveTelemetry_WithoutPlatformKeepsTheExplicitConfig(t *testing.T) {
	config := &types.RunConfig{
		Mode: "planning",
		TraceEmitter: types.TraceEmitterConfig{
			Type:            "otel",
			Protocol:        "http/protobuf",
			Endpoint:        "https://gw.example/otlp",
			MetricsEndpoint: "https://metrics.example/otlp",
		},
		Observability: types.ObservabilityConfig{ServiceName: "agent-a"},
	}
	headers := map[string]string{"Authorization": "Bearer t"}
	exporters, resourceOpts, err := resolveTelemetry(context.Background(), config, headers)
	require.NoError(t, err)

	t.Run("endpoints", func(t *testing.T) {
		require.Equal(t, "https://gw.example/otlp", exporters.Traces.Endpoint)
		require.Equal(t, "https://metrics.example/otlp", exporters.Metrics.Endpoint)
		require.Equal(t, "https://gw.example/otlp", exporters.Logs.Endpoint)
	})

	t.Run("trace settings", func(t *testing.T) {
		require.Equal(t, "Bearer t", exporters.Traces.Headers["Authorization"])
		require.Nil(t, exporters.Traces.HTTPClient, "trace exporter should use the SDK client")
	})

	t.Run("resource options", func(t *testing.T) {
		require.Equal(t, "agent-a", resourceOpts.ServiceName)
		require.Equal(t, "planning", resourceOpts.RunMode)
		require.Nil(t, resourceOpts.Extra)
	})
}

func TestResolveTelemetry_RejectsUnknownPlatformOption(t *testing.T) {
	config := &types.RunConfig{TraceEmitter: types.TraceEmitterConfig{
		Type:            "otel",
		Protocol:        "http/protobuf",
		Platform:        types.ValidTraceEmitterPlatformValues()[0],
		PlatformOptions: map[string]string{"notAnOption": "x"},
	}}
	_, _, err := resolveTelemetry(context.Background(), config, nil)
	require.ErrorContains(t, err, "resolve observability platform")
}

// platformTestOptions are option sets tried in turn to resolve each
// platform without naming it here.
var platformTestOptions = []map[string]string{
	{"project": "stirrup-tests"},
	{"logGroup": "/stirrup/tests"},
	{"host": "https://observability.example.com"},
}

// isolateCloudCredentials gives the AWS default chain static environment
// credentials and a region, with no shared config and no instance
// metadata lookup.
func isolateCloudCredentials(t *testing.T) {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "absent")
	for name, value := range map[string]string{
		"AWS_ACCESS_KEY_ID":                      "AKIDEXAMPLE",
		"AWS_SECRET_ACCESS_KEY":                  "secret-key",
		"AWS_SESSION_TOKEN":                      "",
		"AWS_REGION":                             "eu-west-1",
		"AWS_PROFILE":                            "",
		"AWS_CONFIG_FILE":                        missing,
		"AWS_SHARED_CREDENTIALS_FILE":            missing,
		"AWS_WEB_IDENTITY_TOKEN_FILE":            "",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI":     "",
		"AWS_EC2_METADATA_DISABLED":              "true",
	} {
		t.Setenv(name, value)
	}
}

// TestResolveTelemetry_WiresEachPlatformIntoTheBuilders pins that a real
// profile's resource attributes, span decoration and wrapped HTTP clients
// reach the inputs of the trace, metric and log builders.
func TestResolveTelemetry_WiresEachPlatformIntoTheBuilders(t *testing.T) {
	isolateCloudCredentials(t)
	var attributed, decorated, wrapped bool
	for _, name := range types.ValidTraceEmitterPlatformValues() {
		t.Run(name, func(t *testing.T) {
			config := &types.RunConfig{
				TraceEmitter:  types.TraceEmitterConfig{Type: "otel", Protocol: otlp.ProtocolHTTPProtobuf, Platform: name},
				Observability: types.ObservabilityConfig{LogsExport: types.LogsExportConfig{Type: "otlp"}},
			}
			var (
				exporters    platform.Exporters
				resourceOpts observability.ResourceOptions
				err          error
			)
			for _, opts := range platformTestOptions {
				config.TraceEmitter.PlatformOptions = opts
				if exporters, resourceOpts, err = resolveTelemetry(context.Background(), config, nil); err == nil {
					break
				}
			}
			require.NoError(t, err, "no test option set resolves this platform")

			attributed = attributed || len(exporters.ResourceAttrs) > 0
			require.Equal(t, exporters.ResourceAttrs, resourceOpts.Extra, "resource Extra should match the profile's attributes")
			opts := otelEmitterOptions(config.TraceEmitter, exporters, resourceOpts, false)
			require.Equal(t, exporters.WrapSpanExporter != nil, opts.WrapSpanExporter != nil, "the trace emitter must receive the profile's span exporter wrapper")
			require.Equal(t, exporters.ResourceAttrs, opts.Resource.Extra, "trace emitter resource Extra should match the profile's attributes")
			decorated = decorated || exporters.WrapSpanExporter != nil
			if opts.Exporter.HTTPClient == nil {
				return
			}
			wrapped = true
			for sig, s := range map[string]otlp.Settings{"traces": opts.Exporter, "metrics": exporters.Metrics, "logs": exporters.Logs} {
				require.NotNil(t, s.HTTPClient, "%s must use the profile's HTTP client", sig)
				require.Positive(t, s.HTTPClient.Timeout, "%s must use a bounded HTTP client", sig)
			}
			require.True(t, metricsEnabled(config, exporters), "a profile with a metrics endpoint must enable metric export")
		})
	}
	require.True(t, attributed && decorated && wrapped,
		"the registered platforms must exercise resource attributes (%v), span decoration (%v) and wrapped transports (%v)",
		attributed, decorated, wrapped)
}

func TestLogPlatformExporters_NamesEndpointsNotHeaders(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	config := &types.RunConfig{
		TraceEmitter:  types.TraceEmitterConfig{Type: "otel"},
		Observability: types.ObservabilityConfig{LogsExport: types.LogsExportConfig{Type: "otlp"}},
	}
	headers := map[string]string{"Authorization": "Bearer header-value-must-not-appear"}
	logPlatformExporters(logger, config, platform.Exporters{
		Platform: "example",
		Traces:   testOTLPSettings("https://traces.example", headers),
		Metrics:  testOTLPSettings("https://metrics.example", headers),
		Logs:     testOTLPSettings("", headers),
	})
	out := buf.String()
	require.Contains(t, out, "platform=example")
	require.Contains(t, out, "traces=https://traces.example")
	require.Contains(t, out, "metrics=https://metrics.example")
	require.Contains(t, out, "OTEL_EXPORTER_OTLP environment")
	require.NotContains(t, out, "header-value-must-not-appear", "log line must not leak a header value")

	buf.Reset()
	logPlatformExporters(logger, config, platform.Exporters{})
	require.Empty(t, buf.String(), "no platform must log nothing")
}

func TestBuildTraceEmitter_JSONLBadPath(t *testing.T) {
	_, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{
		Type:     "jsonl",
		FilePath: "/nonexistent/deeply/nested/dir/trace.jsonl",
	}, platform.Exporters{}, observability.ResourceOptions{}, false)
	require.Error(t, err)
}

func TestBuildExecutor_Local(t *testing.T) {
	workspace := t.TempDir()
	exec, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type:      "local",
		Workspace: workspace,
	}, nil, nil, nil, executor.Lifecycle{})
	require.NoError(t, err)
	require.IsType(t, (*executor.LocalExecutor)(nil), exec)
}

func TestBuildExecutor_EmptyTypeDefaultsToLocal(t *testing.T) {
	exec, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Workspace: t.TempDir(),
	}, nil, nil, nil, executor.Lifecycle{})
	require.NoError(t, err)
	require.IsType(t, (*executor.LocalExecutor)(nil), exec)
}

func TestBuildExecutor_LocalDefaultsWorkspaceToCwd(t *testing.T) {
	exec, err := buildExecutor(context.Background(), types.ExecutorConfig{Type: "local"}, nil, nil, nil, executor.Lifecycle{})
	require.NoError(t, err)
	require.NotNil(t, exec)
}

func TestBuildExecutor_API_MissingVcsBackend(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{Type: "api"}, nil, nil, nil, executor.Lifecycle{})
	require.ErrorContains(t, err, "requires vcsBackend")
}

// TestBuildExecutor_API_DispatchesOnBackendType pins that vcsBackend.type
// selects the forge client and that an unrecognised type is rejected: a
// silent GitHub fallback reads a same-named repository from the wrong forge.
func TestBuildExecutor_API_DispatchesOnBackendType(t *testing.T) {
	tests := []struct {
		name     string
		backend  types.VcsBackendConfig
		wantExec string
		wantErr  string
	}{
		{
			name:     "github",
			backend:  types.VcsBackendConfig{Type: "github", APIKeyRef: "secret://token", Repo: "owner/repo", Ref: "main"},
			wantExec: "*executor.APIExecutor",
		},
		{
			name:     "gitlab",
			backend:  types.VcsBackendConfig{Type: "gitlab", APIKeyRef: "secret://token", Repo: "group/project", Ref: "main"},
			wantExec: "*executor.GitLabExecutor",
		},
		{
			name:     "gitlab subgroup path",
			backend:  types.VcsBackendConfig{Type: "gitlab", APIKeyRef: "secret://token", Repo: "group/subgroup/project"},
			wantExec: "*executor.GitLabExecutor",
		},
		{
			name:    "github rejects a repo without an owner",
			backend: types.VcsBackendConfig{Type: "github", APIKeyRef: "secret://token", Repo: "invalid-no-slash"},
			wantErr: "expected 'owner/repo'",
		},
		{
			name:    "gitlab rejects a bare project name",
			backend: types.VcsBackendConfig{Type: "gitlab", APIKeyRef: "secret://token", Repo: "project"},
			wantErr: "expected a project path",
		},
		{
			name:    "unknown backend type",
			backend: types.VcsBackendConfig{Type: "bitbucket", APIKeyRef: "secret://token", Repo: "owner/repo"},
			wantErr: "unsupported executor.vcsBackend.type",
		},
		{
			name:    "unset backend type",
			backend: types.VcsBackendConfig{APIKeyRef: "secret://token", Repo: "owner/repo"},
			wantErr: "unsupported executor.vcsBackend.type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secrets := &stubSecretStore{secrets: map[string]string{"secret://token": "tok"}}
			exec, err := buildExecutor(context.Background(), types.ExecutorConfig{
				Type:       "api",
				VcsBackend: &tt.backend,
			}, secrets, nil, nil, executor.Lifecycle{})

			if tt.wantErr != "" {
				require.Error(t, err, "got executor %T", exec)
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantExec, fmt.Sprintf("%T", exec))
		})
	}
}

// TestBuildExecutor_API_EmptyAPIKeyRefSkipsSecretResolution pins
// unauthenticated public-repo access: an empty apiKeyRef must not reach the
// secret store, which rejects a reference with no "secret://" scheme. The
// store here resolves nothing, so any lookup fails the build.
func TestBuildExecutor_API_EmptyAPIKeyRefSkipsSecretResolution(t *testing.T) {
	for _, backend := range []struct {
		backendType string
		repo        string
		wantExec    string
	}{
		{backendType: "github", repo: "owner/repo", wantExec: "*executor.APIExecutor"},
		{backendType: "gitlab", repo: "group/project", wantExec: "*executor.GitLabExecutor"},
	} {
		t.Run(backend.backendType, func(t *testing.T) {
			exec, err := buildExecutor(context.Background(), types.ExecutorConfig{
				Type: "api",
				VcsBackend: &types.VcsBackendConfig{
					Type: backend.backendType,
					Repo: backend.repo,
					Ref:  "main",
				},
			}, &stubSecretStore{}, nil, nil, executor.Lifecycle{})
			require.NoError(t, err)
			require.Equal(t, backend.wantExec, fmt.Sprintf("%T", exec))
		})
	}
}

// The error for an unresolvable apiKeyRef names the config field that holds it.
func TestBuildExecutor_API_SecretResolutionFailureNamesTheField(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type: "api",
		VcsBackend: &types.VcsBackendConfig{
			Type:      "github",
			APIKeyRef: "secret://missing",
			Repo:      "owner/repo",
		},
	}, &stubSecretStore{}, nil, nil, executor.Lifecycle{})
	require.ErrorContains(t, err, "executor.vcsBackend.apiKeyRef")
}

func TestBuildExecutor_Container_MissingImage(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{Type: "container"}, nil, nil, nil, executor.Lifecycle{})
	require.ErrorContains(t, err, "requires image")
}

func TestBuildExecutor_None_HasNoCapabilities(t *testing.T) {
	exec, err := buildExecutor(context.Background(), types.ExecutorConfig{Type: "none"}, nil, nil, nil, executor.Lifecycle{})
	require.NoError(t, err)
	require.IsType(t, (*executor.NoneExecutor)(nil), exec)

	caps := exec.Capabilities()
	require.False(t, caps.CanRead, "CanRead")
	require.False(t, caps.CanWrite, "CanWrite")
	require.False(t, caps.CanExec, "CanExec")
	require.False(t, caps.CanNetwork, "CanNetwork")
}

func TestBuildExecutor_UnsupportedType(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{Type: "microvm"}, nil, nil, nil, executor.Lifecycle{})
	require.ErrorContains(t, err, "unsupported executor type")
}

func TestBuildExecutor_K8s_MissingImage(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type:         "k8s",
		K8sNamespace: "default",
	}, nil, nil, nil, executor.Lifecycle{})
	require.ErrorContains(t, err, "requires image")
}

func TestBuildExecutor_K8s_MissingNamespace(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type:  "k8s",
		Image: "busybox",
	}, nil, nil, nil, executor.Lifecycle{})
	require.ErrorContains(t, err, "requires k8sNamespace")
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
	}, nil, nil, nil, executor.Lifecycle{})
	if err == nil {
		// A success would mean a cluster was reachable; that is fine but not
		// the cluster-free case this test targets.
		t.Skip("k8s executor unexpectedly constructed (a cluster appears reachable); skipping the no-cluster assertion")
	}
	require.NotContains(t, err.Error(), "unsupported executor type", "k8s type fell through to the default arm")
}

func TestBuildExecutor_K8sSandbox_MissingImage(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type:         "k8s-sandbox",
		K8sNamespace: "default",
	}, nil, nil, nil, executor.Lifecycle{})
	require.ErrorContains(t, err, "requires image")
}

func TestBuildExecutor_K8sSandbox_MissingNamespace(t *testing.T) {
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type:  "k8s-sandbox",
		Image: "busybox",
	}, nil, nil, nil, executor.Lifecycle{})
	require.ErrorContains(t, err, "requires k8sNamespace")
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
	}, nil, nil, nil, executor.Lifecycle{})
	if err == nil {
		// A success would mean a cluster was reachable; that is fine but not
		// the cluster-free case this test targets.
		t.Skip("agent sandbox executor unexpectedly constructed (a cluster appears reachable); skipping the no-cluster assertion")
	}
	require.NotContains(t, err.Error(), "unsupported executor type", "k8s-sandbox type fell through to the default arm")
}

func TestBuildTransport_StdioReturnsTransport(t *testing.T) {
	tp, err := buildTransport(context.Background(), types.TransportConfig{Type: "stdio"}, nil)
	require.NoError(t, err)
	require.NotNil(t, tp)
}

func TestBuildTransport_EmptyDefaultsToStdio(t *testing.T) {
	tp, err := buildTransport(context.Background(), types.TransportConfig{}, nil)
	require.NoError(t, err)
	require.NotNil(t, tp)
}

func TestBuildTransport_GRPCMissingAddress(t *testing.T) {
	_, err := buildTransport(context.Background(), types.TransportConfig{Type: "grpc"}, nil)
	require.ErrorContains(t, err, "requires an address")
}

func TestBuildTransport_UnsupportedType(t *testing.T) {
	_, err := buildTransport(context.Background(), types.TransportConfig{Type: "websocket"}, nil)
	require.ErrorContains(t, err, "unsupported transport type")
}

func TestBuildTransport_TLSRequiresGRPC(t *testing.T) {
	creds := credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13})
	tests := []struct {
		name string
		typ  string
	}{
		{name: "stdio", typ: "stdio"},
		{name: "empty type", typ: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildTransport(context.Background(), types.TransportConfig{Type: tt.typ}, creds)
			require.ErrorContains(t, err, "require the grpc transport")
		})
	}
}

func TestBuildTransport_GRPCUsesTLSCredentials(t *testing.T) {
	pki := tlstest.New(t)
	creds, err := transport.TLSConfig{
		CAFile:   pki.CAFile,
		CertFile: pki.ClientCertFile,
		KeyFile:  pki.ClientKeyFile,
	}.Credentials()
	require.NoError(t, err, "Credentials")

	serve := func(serverTLS *tls.Config) string {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err, "net.Listen")
		var opts []grpc.ServerOption
		if serverTLS != nil {
			opts = append(opts, grpc.Creds(credentials.NewTLS(serverTLS)))
		}
		gs := grpc.NewServer(opts...)
		pb.RegisterHarnessServiceServer(gs, &pb.UnimplementedHarnessServiceServer{})
		go func() { _ = gs.Serve(lis) }()
		t.Cleanup(gs.Stop)
		return lis.Addr().String()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tp, err := buildTransport(ctx, types.TransportConfig{Type: "grpc", Address: serve(pki.ServerTLSConfig(true))}, creds)
	require.NoError(t, err, "buildTransport over mutual TLS")
	_ = tp.Close()

	tp, err = buildTransport(ctx, types.TransportConfig{Type: "grpc", Address: serve(nil)}, creds)
	if err == nil {
		_ = tp.Close()
	}
	require.Error(t, err, "buildTransport with TLS credentials connected to a plaintext server")
}

func TestParseLogLevel_MapsNamesToLevels(t *testing.T) {
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
			require.Equal(t, tt.want, parseLogLevel(tt.input))
		})
	}
}

func TestToolEnabled_EmptyListEnablesAll(t *testing.T) {
	require.True(t, toolEnabled(nil, "read_file"), "nil list")
	require.True(t, toolEnabled([]string{}, "read_file"), "empty slice")
}

func TestToolEnabled_ExplicitListEnablesOnlyListed(t *testing.T) {
	enabled := []string{"read_file", "run_command"}
	require.True(t, toolEnabled(enabled, "read_file"))
	require.False(t, toolEnabled(enabled, "write_file"))
}

func TestEditToolEnabled_EmptyListEnablesAll(t *testing.T) {
	require.True(t, editToolEnabled(nil, "write_file"))
}

func TestEditToolEnabled_DirectMatch(t *testing.T) {
	require.True(t, editToolEnabled([]string{"edit_file"}, "edit_file"))
}

func TestEditToolEnabled_AliasMatch(t *testing.T) {
	// "write_file" is an alias for edit tools.
	require.True(t, editToolEnabled([]string{"write_file"}, "edit_file"), "write_file alias")
	require.True(t, editToolEnabled([]string{"search_replace"}, "edit_file"), "search_replace alias")
	require.True(t, editToolEnabled([]string{"apply_diff"}, "edit_file"), "apply_diff alias")
}

func TestEditToolEnabled_NoMatch(t *testing.T) {
	require.False(t, editToolEnabled([]string{"read_file"}, "edit_file"))
}

func TestMutatingToolSet_ClassifiesWorkspaceTools(t *testing.T) {
	exec, _ := executor.NewLocalExecutor(t.TempDir())
	registry := buildToolRegistry(exec, edit.NewWholeFileStrategy(), types.ToolsConfig{}, nil)

	mutating := mutatingToolSet(registry)

	// write_file and run_command mutate the workspace.
	require.True(t, mutating["write_file"], "write_file should be workspace-mutating")
	require.True(t, mutating["run_command"], "run_command should be workspace-mutating")
	// Read-only tools are not mutating. (spawn_agent is registered
	// after the loop is built and is therefore not in this set; see
	// BuildLoopWithTransport.)
	for _, name := range []string{"read_file", "list_directory", "grep_files", "find_files", "web_fetch"} {
		require.False(t, mutating[name], "%s should not be workspace-mutating", name)
	}
}

func TestApprovalRequiredToolSet(t *testing.T) {
	exec, _ := executor.NewLocalExecutor(t.TempDir())
	registry := buildToolRegistry(exec, edit.NewWholeFileStrategy(), types.ToolsConfig{}, nil)

	approval := approvalRequiredToolSet(registry)

	// All tools that touch the world require approval: writes, shell, network.
	for _, name := range []string{"write_file", "run_command", "web_fetch"} {
		require.True(t, approval[name], "%s should require approval", name)
	}
	// Pure-read tools never require approval.
	for _, name := range []string{"read_file", "list_directory", "grep_files", "find_files"} {
		require.False(t, approval[name], "%s should not require approval", name)
	}
	// spawn_agent is registered post-loop-construction in factory.go (see
	// BuildLoopWithTransport). Its absence here is load-bearing: if it
	// ever appears in this set, the AddApprovalTool refresh has become
	// redundant and the post-registration step in the factory should
	// be removed.
	require.False(t, approval["spawn_agent"], "spawn_agent is added after loop construction in BuildLoopWithTransport")
}

// TestBuildToolRegistry_DefaultReadOnlyIncludesGitTools checks that
// buildToolRegistry registers the git tools for the default read-only tool
// list, the configuration a bare read-only run uses.
func TestBuildToolRegistry_DefaultReadOnlyIncludesGitTools(t *testing.T) {
	exec, err := executor.NewLocalExecutor(t.TempDir())
	require.NoError(t, err, "new local executor")
	registry := buildToolRegistry(exec, edit.NewWholeFileStrategy(), types.ToolsConfig{
		BuiltIn: types.DefaultReadOnlyBuiltInTools(),
	}, nil)

	registered := make(map[string]bool)
	for _, def := range registry.List() {
		registered[def.Name] = true
	}

	for _, name := range []string{"git_status", "git_changed_files", "git_diff", "git_show"} {
		require.True(t, registered[name], "buildToolRegistry did not register %q for a default read-only tool list", name)
	}
}

// TestBuildToolRegistry_NoneExecutorRegistersOnlyUngatedTools checks that a
// none executor, whose capabilities are all false, drops every
// capability-gated tool from the registry while web_fetch, which is not
// gated, still registers. ValidateRunConfig rejects these tool names for a
// real run; this test calls buildToolRegistry directly to pin the gate itself.
func TestBuildToolRegistry_NoneExecutorRegistersOnlyUngatedTools(t *testing.T) {
	exec := executor.NewNoneExecutor()
	registry := buildToolRegistry(exec, edit.NewWholeFileStrategy(), types.ToolsConfig{
		BuiltIn: []string{
			"read_file", "list_directory", "grep_files", "find_files",
			"git_status", "git_changed_files", "git_diff", "git_show",
			"run_command", "write_file", "web_fetch",
		},
	}, nil)

	registered := make(map[string]bool)
	for _, def := range registry.List() {
		registered[def.Name] = true
	}

	for _, name := range []string{
		"read_file", "list_directory", "grep_files", "find_files",
		"git_status", "git_changed_files", "git_diff", "git_show",
		"run_command", "write_file",
	} {
		require.False(t, registered[name], "buildToolRegistry registered %q for the none executor, which has no capability to back it", name)
	}
	require.True(t, registered["web_fetch"], "web_fetch is not capability-gated")
}

// TestBuildToolRegistry_GitStatusExecutes checks that a git tool resolved
// from the live registry runs against a real repository, not only that its
// name is registered.
func TestBuildToolRegistry_GitStatusExecutes(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err, "eval symlinks")
	dir = resolved

	runGitCmd := func(args ...string) {
		t.Helper()
		c := osexec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		out, err := c.CombinedOutput()
		require.NoError(t, err, "git %v failed:\n%s", args, out)
	}
	runGitCmd("init", "-q")
	runGitCmd("config", "user.email", "test@example.com")
	runGitCmd("config", "user.name", "test")

	exec, err := executor.NewLocalExecutor(dir)
	require.NoError(t, err, "new local executor")
	registry := buildToolRegistry(exec, edit.NewWholeFileStrategy(), types.ToolsConfig{
		BuiltIn: types.DefaultReadOnlyBuiltInTools(),
	}, nil)

	gitStatus := registry.Resolve("git_status")
	require.NotNil(t, gitStatus, "git_status not resolvable from the live registry")
	res, err := gitStatus.StructuredHandler(context.Background(), json.RawMessage(`{}`))
	require.NoError(t, err, "git_status execution failed")
	require.Contains(t, res.Text, "working tree clean")
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
	require.NoError(t, err, "BuildLoopWithTransport()")
	defer func() { _ = loop.Close() }()

	ask, ok := permission.Unwrap(loop.Permissions).(*permission.AskUpstreamPolicy)
	require.True(t, ok, "expected AskUpstreamPolicy, got %T", loop.Permissions)

	require.Subset(t, ask.ApprovalToolNames(), []string{"write_file", "run_command", "web_fetch", "spawn_agent"})
}

func TestBuildLoopWithTransport_InvalidConfigReturnsError(t *testing.T) {
	_, err := BuildLoopWithTransport(context.Background(), &types.RunConfig{
		// Missing provider type — validation will fail.
		MaxTurns: 5,
	}, nil)
	require.ErrorContains(t, err, "config validation")
}

// TestFactory_ConfigValidationFailed_EmitsSecurityEvent asserts that the
// SecurityLogger constructed inside BuildLoopWithTransport emits a
// config_validation_failed event when ValidateRunConfig rejects the config.
// We capture os.Stderr (where the logger writes) for the duration of the
// call and assert the event reaches the buffer.
func TestFactory_ConfigValidationFailed_EmitsSecurityEvent(t *testing.T) {
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err, "os.Pipe")
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

	require.ErrorContains(t, buildErr, "config validation")

	captured, readErr := io.ReadAll(r)
	require.NoError(t, readErr, "read captured stderr")
	require.Contains(t, string(captured), `"event":"config_validation_failed"`, "captured stderr")
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
	require.NoError(t, err, "BuildLoopWithTransport()")
	defer func() { _ = loop.Close() }()

	require.NotNil(t, loop.Provider, "loop.Provider")
	require.NotNil(t, loop.Router, "loop.Router")
	require.NotNil(t, loop.Prompt, "loop.Prompt")
	require.NotNil(t, loop.Context, "loop.Context")
	require.NotNil(t, loop.Tools, "loop.Tools")
	require.NotNil(t, loop.Executor, "loop.Executor")
	require.NotNil(t, loop.Edit, "loop.Edit")
	require.NotNil(t, loop.Verifier, "loop.Verifier")
	require.NotNil(t, loop.Permissions, "loop.Permissions")
	require.NotNil(t, loop.Git, "loop.Git")
	require.NotNil(t, loop.Hooks, "loop.Hooks")
	_, isNoop := loop.Hooks.(*hook.Noop)
	require.True(t, isNoop, "expected hook.Noop for a config with no HooksConfig, got %T", loop.Hooks)
	require.NotNil(t, loop.Transport, "loop.Transport")
	require.NotNil(t, loop.Trace, "loop.Trace")
	require.NotNil(t, loop.Tracer, "loop.Tracer")
	require.NotNil(t, loop.Metrics, "loop.Metrics")
	require.NotNil(t, loop.Security, "loop.Security")
	require.NotNil(t, loop.Logger, "loop.Logger")
	require.NotNil(t, loop.ContextWindows, "loop.ContextWindows")
	got := loop.ContextWindows.Resolve("openai-compatible", "gpt-4o").ContextWindow
	require.Equal(t, 128_000, got, "gpt-4o context window from the model registry")
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
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()

	execRunner, ok := loop.Hooks.(*hook.ExecRunner)
	require.True(t, ok, "expected *hook.ExecRunner, got %T", loop.Hooks)
	require.Equal(t, loop.Executor, execRunner.Exec, "ExecRunner.Exec must be the run's own Executor")
	require.Same(t, config.Hooks, execRunner.Hooks, "ExecRunner.Hooks must be the config's own HooksConfig instance")
}

// An empty edit strategy type must resolve to "multi" across the whole factory
// path, not only in ValidateRunConfig or buildEditStrategy alone. The
// execution-mode code scanner wraps the strategy, so the test unwraps it.
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
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()

	require.Equal(t, "multi", config.EditStrategy.Type, "ValidateRunConfig should default EditStrategy.Type")

	got := loop.Edit
	if scanned, ok := got.(*edit.ScannedStrategy); ok {
		got = scanned.Inner()
	}
	require.IsType(t, (*edit.MultiStrategy)(nil), got, "loop.Edit should resolve to *edit.MultiStrategy, possibly via ScannedStrategy")
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

	// The grpc transport would fail without an address, so injecting one must skip buildTransport.
	injected := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	loop, err := BuildLoopWithTransport(context.Background(), config, injected)
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()

	require.Same(t, injected, loop.Transport, "injected transport should be used")
	require.False(t, loop.emitReady, "emitReady should be false when transport is injected")
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
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()

	require.NotNil(t, loop.Metrics, "Metrics should be no-op instruments, not nil, when the trace emitter is jsonl")
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
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()

	// With no tools.builtIn filter, all tools should be registered.
	expectedTools := []string{"read_file", "list_directory", "grep_files", "find_files", "run_command", "write_file", "web_fetch", "spawn_agent"}
	for _, name := range expectedTools {
		t.Run(name, func(t *testing.T) {
			require.NotNil(t, loop.Tools.Resolve(name), "tool should be registered")
		})
	}
}

// An unresolvable secret in the trace emitter headers must fail the factory
// with a "resolve trace emitter headers" error.
func TestBuildLoopWithTransport_TraceEmitterHeaderResolutionError(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	// Ensure the referenced env var is *not* set so EnvSecretStore
	// returns a missing-secret error.
	require.NoError(t, os.Unsetenv("STIRRUP_TEST_MISSING_VAR"), "unset env")

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
	require.Error(t, err, "expected error when trace emitter header resolution fails")
	require.ErrorContains(t, err, "resolve trace emitter headers")
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
	require.ErrorContains(t, err, "build providers")
}

// Research mode is read-only but must register web_fetch and let it pass the
// deny-side-effects permission policy.
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
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()

	wf := loop.Tools.Resolve("web_fetch")
	require.NotNil(t, wf, "web_fetch should be registered in research mode")

	result, err := loop.Permissions.Check(context.Background(), wf.Definition(), nil)
	require.NoError(t, err, "permission check")
	require.True(t, result.Allowed, "research mode permission policy denied web_fetch: %q", result.Reason)
}

// Every read-only mode must register web_fetch and allow it through its
// permission policy.
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
			require.NoError(t, err)
			defer func() { _ = loop.Close() }()

			wf := loop.Tools.Resolve("web_fetch")
			require.NotNil(t, wf, "web_fetch should be registered")

			result, err := loop.Permissions.Check(context.Background(), wf.Definition(), nil)
			require.NoError(t, err, "permission check")
			require.True(t, result.Allowed, "permission policy denied web_fetch: %q", result.Reason)
		})
	}
}

func TestBuildProvider_OpenAIResponses(t *testing.T) {
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:      "openai-responses",
		APIKeyRef: "secret://OPENAI_KEY",
		BaseURL:   "https://api.openai.com/v1",
	}, secrets)
	require.NoError(t, err)
	_, ok := prov.(*provider.OpenAIResponsesAdapter)
	require.True(t, ok, "buildProvider type = %T, want *provider.OpenAIResponsesAdapter", prov)
}

// TestBuildProvider_OpenAIResponsesAzureFields checks that APIKeyHeader and
// QueryParams from the ProviderConfig reach the wire through buildProvider.
// The gRPC transport test in harness/internal/transport/grpc_test.go covers the
// proto passthrough for these fields.
func TestBuildProvider_OpenAIResponsesAzureFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "AZURE-KEY", r.Header.Get("api-key"), "api-key header")
		assert.Empty(t, r.Header.Get("Authorization"), "Authorization header")
		assert.Equal(t, "/openai/v1/responses", r.URL.Path, "URL.Path")
		assert.Equal(t, "preview", r.URL.Query().Get("api-version"), "api-version")
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
	require.NoError(t, err)

	ch, err := prov.Stream(context.Background(), types.StreamParams{Model: "gpt-4o", MaxTokens: 16})
	require.NoError(t, err, "Stream()")
	for range ch { //nolint:revive // drain stream so the goroutine completes
	}
}

// TestBuildProvider_OpenAICompatibleAzureFields is the Chat Completions
// counterpart of TestBuildProvider_OpenAIResponsesAzureFields.
func TestBuildProvider_OpenAICompatibleAzureFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "AZURE-KEY", r.Header.Get("api-key"), "api-key header")
		assert.Empty(t, r.Header.Get("Authorization"), "Authorization header")
		assert.Equal(t, "/openai/v1/chat/completions", r.URL.Path, "URL.Path")
		assert.Equal(t, "preview", r.URL.Query().Get("api-version"), "api-version")
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
	require.NoError(t, err)

	ch, err := prov.Stream(context.Background(), types.StreamParams{Model: "gpt-4o", MaxTokens: 16})
	require.NoError(t, err, "Stream()")
	for range ch { //nolint:revive // drain stream so the goroutine completes
	}
}

// TestBuildProvider_Gemini checks that the gemini provider type builds a
// GeminiAdapter. The service-account key is found through
// GOOGLE_APPLICATION_CREDENTIALS by the implicit ADC credential source.
func TestBuildProvider_Gemini(t *testing.T) {
	keyPath := writeFakeServiceAccountJSON(t, t.TempDir())
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", keyPath)

	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:        "gemini",
		GCPProject:  "test-project",
		GCPLocation: "us-central1",
	}, &stubSecretStore{secrets: map[string]string{}})
	require.NoError(t, err)
	_, ok := prov.(*provider.GeminiAdapter)
	require.True(t, ok, "buildProvider type = %T, want *provider.GeminiAdapter", prov)
}

// TestBuildProvider_GeminiMissingCredentialsErrors checks that the gemini arm
// returns an error, not a partly built adapter, when no credential can be
// loaded. A GOOGLE_APPLICATION_CREDENTIALS path that does not exist makes ADC
// fail deterministically.
func TestBuildProvider_GeminiMissingCredentialsErrors(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "no-such-file.json"))

	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:        "gemini",
		GCPProject:  "test-project",
		GCPLocation: "us-central1",
	}, &stubSecretStore{secrets: map[string]string{}})
	require.Error(t, err, "credential source cannot produce a Google token")
	require.ErrorContains(t, err, "credential", "error should mention credentials")
}

// TestBuildProvider_OpenAICompatibleWithRetryConfig checks that the provider
// retry settings are copied onto the OpenAICompatibleAdapter's RetryPolicy.
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
	require.NoError(t, err)
	adapter, ok := prov.(*provider.OpenAICompatibleAdapter)
	require.True(t, ok, "buildProvider type = %T, want *provider.OpenAICompatibleAdapter", prov)
	assert.Equal(t, 5, adapter.RetryPolicy.MaxAttempts, "RetryPolicy.MaxAttempts")
	assert.Equal(t, 200*time.Millisecond, adapter.RetryPolicy.InitialDelay, "RetryPolicy.InitialDelay")
	assert.Equal(t, 10*time.Second, adapter.RetryPolicy.MaxDelay, "RetryPolicy.MaxDelay")
	assert.Equal(t, 60*time.Second, adapter.RetryPolicy.WallClockBudget, "RetryPolicy.WallClockBudget")
}

// TestBuildProvider_AnthropicWithRetryConfig checks that the provider retry
// settings reach the default anthropic adapter, not only openai-compatible.
func TestBuildProvider_AnthropicWithRetryConfig(t *testing.T) {
	secrets := &stubSecretStore{secrets: map[string]string{"secret://ANTHROPIC_API_KEY": "sk-ant-api03-fake"}}
	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:      "anthropic",
		APIKeyRef: "secret://ANTHROPIC_API_KEY",
		Retry: &types.ProviderRetryConfig{
			MaxAttempts:       5,
			InitialDelayMs:    200,
			MaxDelayMs:        10000,
			WallClockBudgetMs: 60000,
		},
	}, secrets)
	require.NoError(t, err)
	adapter, ok := prov.(*provider.AnthropicAdapter)
	require.True(t, ok, "buildProvider type = %T, want *provider.AnthropicAdapter", prov)
	assert.Equal(t, 5, adapter.RetryPolicy.MaxAttempts, "RetryPolicy.MaxAttempts")
	assert.Equal(t, 200*time.Millisecond, adapter.RetryPolicy.InitialDelay, "RetryPolicy.InitialDelay")
	assert.Equal(t, 10*time.Second, adapter.RetryPolicy.MaxDelay, "RetryPolicy.MaxDelay")
	assert.Equal(t, 60*time.Second, adapter.RetryPolicy.WallClockBudget, "RetryPolicy.WallClockBudget")
}

// The retry settings in ProviderConfig.Retry reach the RetryPolicy of the
// adapter built for each provider type.
func TestBuildProvider_GeminiWithRetryConfig(t *testing.T) {
	keyPath := writeFakeServiceAccountJSON(t, t.TempDir())
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", keyPath)

	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:        "gemini",
		GCPProject:  "test-project",
		GCPLocation: "us-central1",
		Retry: &types.ProviderRetryConfig{
			MaxAttempts: 5,
			MaxDelayMs:  10000,
		},
	}, &stubSecretStore{secrets: map[string]string{}})
	require.NoError(t, err)
	adapter, ok := prov.(*provider.GeminiAdapter)
	require.True(t, ok, "buildProvider type = %T, want *provider.GeminiAdapter", prov)
	require.Equal(t, 5, adapter.RetryPolicy.MaxAttempts)
}

func TestBuildProvider_OpenAIResponsesWithRetryConfig(t *testing.T) {
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:      "openai-responses",
		APIKeyRef: "secret://OPENAI_KEY",
		Retry: &types.ProviderRetryConfig{
			MaxAttempts: 5,
			MaxDelayMs:  10000,
		},
	}, secrets)
	require.NoError(t, err)
	adapter, ok := prov.(*provider.OpenAIResponsesAdapter)
	require.True(t, ok, "buildProvider type = %T, want *provider.OpenAIResponsesAdapter", prov)
	require.Equal(t, 5, adapter.RetryPolicy.MaxAttempts)
}

// The factory wires the Z.ai compat rules into the adapter's registry, so GLM
// model IDs resolve through them.
func TestBuildProvider_OpenAICompatibleWithCompatProfile_ZAI(t *testing.T) {
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:          "openai-compatible",
		APIKeyRef:     "secret://OPENAI_KEY",
		BaseURL:       "https://api.z.ai/api/coding/paas/v4",
		CompatProfile: "zai-glm",
	}, secrets)
	require.NoError(t, err)
	adapter, ok := prov.(*provider.OpenAICompatibleAdapter)
	require.True(t, ok, "buildProvider type = %T, want *provider.OpenAICompatibleAdapter", prov)
	require.NotNil(t, adapter.Registry, "factory should inject a registry containing the Z.ai compat rule")

	t.Run("glm-4-plus maps to max_tokens and tool_stream", func(t *testing.T) {
		q := adapter.Registry.Resolve("openai-compatible", "glm-4-plus")
		require.Equal(t, quirks.TokenFieldMaxTokens, q.BehaviourFlags.OpenAI.TokenField,
			"Z.ai compat rule did not fire; check resolveCompatProfile dispatch")
		require.Contains(t, q.BehaviourFlags.OpenAI.ExtraBodyFields, "tool_stream")
		require.Equal(t, true, q.BehaviourFlags.OpenAI.ExtraBodyFields["tool_stream"])
	})

	// The thinking-family assertions prove the factory keeps the full
	// CompatRules() slice, not only the base rule.
	t.Run("glm-4.7 enables the thinking family", func(t *testing.T) {
		q := adapter.Registry.Resolve("openai-compatible", "glm-4.7")
		require.Contains(t, q.ReplayFields, "reasoning_content",
			"thinking-family rule did not fire via factory")
		require.Contains(t, q.BehaviourFlags.OpenAI.ExtraBodyFields, "thinking")
		require.Equal(t, map[string]any{"type": "enabled"}, q.BehaviourFlags.OpenAI.ExtraBodyFields["thinking"])
	})
}

// An unknown compatProfile fails the build even when RunConfig validation is
// bypassed.
func TestBuildProvider_OpenAICompatibleWithUnknownCompatProfile_Errors(t *testing.T) {
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:          "openai-compatible",
		APIKeyRef:     "secret://OPENAI_KEY",
		BaseURL:       "https://api.example.com/v1",
		CompatProfile: "not-a-real-profile",
	}, secrets)
	require.ErrorContains(t, err, "not-a-real-profile")
}

// BuildLoopWithTransport injects the scrub-backed logger into the
// OpenAI-compatible adapter, so retry warnings pass through secret scrubbing.
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
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()

	inner := unwrapNormalizer(loop.Provider)
	adapter, ok := inner.(*provider.OpenAICompatibleAdapter)
	require.True(t, ok, "loop.Provider (after unwrap) type = %T, want *provider.OpenAICompatibleAdapter", inner)
	require.NotNil(t, adapter.Logger, "factory should inject the scrub-backed logger")
}

// writeFakeServiceAccountJSON produces a service-account JSON keyfile
// good enough to pass google.JWTConfigFromJSON's static parse. It is not
// signable against real Google IAM, which is fine for factory tests that
// never call Token().
func writeFakeServiceAccountJSON(t *testing.T, dir string) string {
	t.Helper()
	// Generating a fresh RSA key for each test would be slow, so the key is
	// shared across the package.
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
	require.NoError(t, err, "marshal sa json")
	path := filepath.Join(dir, "key.json")
	require.NoError(t, os.WriteFile(path, data, 0o600), "write sa json")
	return path
}

// The factory builds an OpenAI-compatible adapter from a file-backed Azure
// workload identity without contacting Entra. The bearer closure only
// reaches Entra on the first BearerToken call.
func TestBuildProvider_OpenAICompatibleAzureWIF(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "azure-identity-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("eyJ.fake.jwt"), 0o600))

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
	require.NoError(t, err)
	_, ok := prov.(*provider.OpenAICompatibleAdapter)
	require.True(t, ok, "buildProvider type = %T, want *provider.OpenAICompatibleAdapter", prov)
}

// TestBuildProvider_OpenAIResponsesAzureWIF mirrors
// TestBuildProvider_OpenAICompatibleAzureWIF for the Responses adapter.
// Both adapters share the bearer-closure plumbing in factory.go::buildProvider.
func TestBuildProvider_OpenAIResponsesAzureWIF(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "azure-identity-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("eyJ.fake.jwt"), 0o600))

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
	require.NoError(t, err)
	_, ok := prov.(*provider.OpenAIResponsesAdapter)
	require.True(t, ok, "buildProvider type = %T, want *provider.OpenAIResponsesAdapter", prov)
}

func TestBuildProvider_OpenAICompatibleWithAPIKey(t *testing.T) {
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:      "openai-compatible",
		APIKeyRef: "secret://OPENAI_KEY",
	}, secrets)
	require.NoError(t, err)
	_, ok := prov.(*provider.OpenAICompatibleAdapter)
	require.True(t, ok, "buildProvider type = %T, want *provider.OpenAICompatibleAdapter", prov)
}

func TestBuildProvider_UnknownTypeMentionsResponses(t *testing.T) {
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:      "nonsense",
		APIKeyRef: "secret://OPENAI_KEY",
	}, secrets)
	require.ErrorContains(t, err, "openai-responses")
}

// resourceOptionsFromConfig maps a nil config, an empty observability block,
// and a populated observability block. The nil case matters for callers
// outside BuildLoopWithTransport, such as eval runners.
func TestResourceOptionsFromConfig_MapsObservabilityAndMode(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		got := resourceOptionsFromConfig(nil)
		require.Equal(t, observability.ResourceOptions{}, got)
	})

	t.Run("empty observability with mode", func(t *testing.T) {
		got := resourceOptionsFromConfig(&types.RunConfig{Mode: "execution"})
		require.Equal(t, observability.ResourceOptions{RunMode: "execution"}, got)
	})

	t.Run("populated observability and mode", func(t *testing.T) {
		got := resourceOptionsFromConfig(&types.RunConfig{
			Mode: "planning",
			Observability: types.ObservabilityConfig{
				ServiceName:      "agent-a",
				Environment:      "prod",
				ServiceNamespace: "eval",
			},
		})
		require.Equal(t, observability.ResourceOptions{
			ServiceName:      "agent-a",
			Environment:      "prod",
			ServiceNamespace: "eval",
			RunMode:          "planning",
		}, got)
	})
}

// The anthropic arm of buildProvider rejects a credential source that has no
// bearer closure, instead of building an adapter that panics on Stream.
func TestBuildProvider_AnthropicNilBearerErrors(t *testing.T) {
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:       "anthropic",
		Credential: &types.CredentialConfig{Type: "aws-default"},
	}, &stubSecretStore{secrets: map[string]string{}})
	require.ErrorContains(t, err, "bearer credential")
}

// TestBuildProvider_OpenAICompatibleNilBearerErrors mirrors the
// anthropic case for the openai-compatible arm.
func TestBuildProvider_OpenAICompatibleNilBearerErrors(t *testing.T) {
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:       "openai-compatible",
		Credential: &types.CredentialConfig{Type: "aws-default"},
	}, &stubSecretStore{secrets: map[string]string{}})
	require.ErrorContains(t, err, "bearer credential")
}

// The factory routes an anthropic-wif credential through credential.BuildSource
// and into NewAnthropicAdapter. The OAuth exchange itself is covered by the
// credential package's tests.
func TestBuildProvider_AnthropicWIF(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "jwt")
	require.NoError(t, os.WriteFile(tokenPath, []byte("eyJ.fake.jwt"), 0o600))

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
	require.NoError(t, err)
	adapter, ok := prov.(*provider.AnthropicAdapter)
	require.True(t, ok, "buildProvider type = %T, want *provider.AnthropicAdapter", prov)
	// The adapter must send the WIF access token as Authorization: Bearer;
	// x-api-key gets a 401 from Anthropic.
	require.Equal(t, provider.AuthModeBearer, adapter.AuthMode())
}

// The static API key path uses x-api-key rather than a Bearer header.
func TestBuildProvider_AnthropicStaticKeyUsesAPIKeyMode(t *testing.T) {
	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:      "anthropic",
		APIKeyRef: "secret://ANTHROPIC_API_KEY",
	}, &stubSecretStore{secrets: map[string]string{"secret://ANTHROPIC_API_KEY": "sk-ant-api03-fake"}})
	require.NoError(t, err)
	adapter, ok := prov.(*provider.AnthropicAdapter)
	require.True(t, ok, "buildProvider type = %T, want *provider.AnthropicAdapter", prov)
	require.Equal(t, provider.AuthModeAPIKey, adapter.AuthMode())
}

// credential.BuildSource rejects an anthropic-wif credential without a
// tokenSource, even when RunConfig validation is bypassed.
func TestBuildProvider_AnthropicWIFMissingTokenSourceErrors(t *testing.T) {
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type: "anthropic",
		Credential: &types.CredentialConfig{
			Type:             "anthropic-wif",
			FederationRuleID: "fdrl_example",
			OrganizationID:   "550e8400-e29b-41d4-a716-446655440000",
			ServiceAccountID: "svac_example",
		},
	}, &stubSecretStore{secrets: map[string]string{}})
	require.ErrorContains(t, err, "tokenSource")
}

// TestBuildProvider_OpenAIResponsesNilBearerErrors mirrors the
// anthropic case for the openai-responses arm.
func TestBuildProvider_OpenAIResponsesNilBearerErrors(t *testing.T) {
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:       "openai-responses",
		Credential: &types.CredentialConfig{Type: "aws-default"},
	}, &stubSecretStore{secrets: map[string]string{}})
	require.ErrorContains(t, err, "bearer credential")
}

// intPtr returns a pointer to v for one-off literal pointer fields.
func intPtr(v int) *int { return &v }

// unwrapNormalizer returns the adapter inside the outermost NormalizingAdapter
// that the factory applies, so assertions can target the concrete adapter type.
func unwrapNormalizer(p provider.ProviderAdapter) provider.ProviderAdapter {
	if n, ok := p.(*provider.NormalizingAdapter); ok {
		return n.Unwrap()
	}
	return p
}

// With batch.enabled, the top-level provider and the providers-map entry are
// both wrapped in a *provider.BatchAdapter. Model-router lookups by provider
// type would otherwise bypass batching.
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
		MaxWaitSeconds:        intPtr(30),
		AllowInteractiveModes: true,
	}

	// Inject a transport so buildTransport (gRPC) does not try to dial.
	injected := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	loop, err := BuildLoopWithTransport(context.Background(), config, injected)
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()

	inner := unwrapNormalizer(loop.Provider)
	ba, ok := inner.(*provider.BatchAdapter)
	require.True(t, ok, "loop.Provider (after unwrap) type = %T, want *provider.BatchAdapter", inner)
	mapped, ok := loop.Providers[config.Provider.Type]
	require.True(t, ok, "loop.Providers[%q] missing", config.Provider.Type)
	require.Same(t, ba, unwrapNormalizer(mapped))
}

// With harness-side polling on stdio, the provider is wrapped in a
// *BatchAdapter. The BatchClient concrete type is unexported, so only the
// outer wrapper is asserted. The providers map must match the top-level
// provider so model-router lookups do not bypass batching.
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
		MaxWaitSeconds:        intPtr(30),
		HarnessSidePolling:    true,
		AllowInteractiveModes: true,
	}

	injected := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	loop, err := BuildLoopWithTransport(context.Background(), config, injected)
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()

	inner := unwrapNormalizer(loop.Provider)
	ba, ok := inner.(*provider.BatchAdapter)
	require.True(t, ok, "loop.Provider (after unwrap) type = %T, want *provider.BatchAdapter", inner)
	mapped := loop.Providers[config.Provider.Type]
	require.Same(t, ba, unwrapNormalizer(mapped))
}

// The stdio batch path accepts openai-compatible and openai-responses
// provider types, not only anthropic.
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
				MaxWaitSeconds:        intPtr(30),
				HarnessSidePolling:    true,
				AllowInteractiveModes: true,
			}

			injected := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
			loop, err := BuildLoopWithTransport(context.Background(), config, injected)
			require.NoError(t, err)
			require.NotNil(t, loop)

			// The outer NormalizingAdapter wraps the BatchAdapter. The providers
			// map entry must be that same BatchAdapter so that routing by
			// provider type goes through batching.
			ba, ok := unwrapNormalizer(loop.Provider).(*provider.BatchAdapter)
			require.True(t, ok, "loop.Provider after unwrap is %T, want *provider.BatchAdapter", unwrapNormalizer(loop.Provider))
			require.Contains(t, loop.Providers, config.Provider.Type)
			require.Same(t, ba, unwrapNormalizer(loop.Providers[config.Provider.Type]))
		})
	}
}

// The provider a Gemini loop calls is the NormalizingAdapter wrapping a
// GeminiAdapter, and the providers map returns that same wrapper.
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
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()

	// The loop itself must hold the NormalizingAdapter, not merely have one
	// somewhere in its chain.
	n, ok := loop.Provider.(*provider.NormalizingAdapter)
	require.True(t, ok, "loop.Provider is %T, want outermost *provider.NormalizingAdapter", loop.Provider)
	require.IsType(t, (*provider.GeminiAdapter)(nil), n.Unwrap())
	require.Contains(t, loop.Providers, config.Provider.Type)
	require.Same(t, loop.Provider, loop.Providers[config.Provider.Type])
}

// The provider a Bedrock loop calls is the NormalizingAdapter wrapping a
// BedrockAdapter. The AWS SDK client is built lazily, so the build does
// not need real AWS credentials.
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
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()

	n, ok := loop.Provider.(*provider.NormalizingAdapter)
	require.True(t, ok, "loop.Provider is %T, want outermost *provider.NormalizingAdapter", loop.Provider)
	require.IsType(t, (*provider.BedrockAdapter)(nil), n.Unwrap())
	require.Contains(t, loop.Providers, config.Provider.Type)
	require.Same(t, loop.Provider, loop.Providers[config.Provider.Type])
}

// BuildLoopWithTransport must inject a non-nil Logger into the
// BedrockAdapter, so the tool-choice downgrade warning goes through the
// scrubbing logger that carries run and trace correlation.
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
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()

	adapter, ok := unwrapNormalizer(loop.Provider).(*provider.BedrockAdapter)
	require.True(t, ok, "loop.Provider after unwrap is %T, want *provider.BedrockAdapter", unwrapNormalizer(loop.Provider))
	require.NotNil(t, adapter.Logger, "factory must inject the ScrubHandler-backed logger")
}

// BuildLoopWithTransport must inject a non-nil Logger into the
// AnthropicAdapter, so the warning for caller temperature suppressed by
// model quirks goes through the scrubbing logger that carries run and
// trace correlation.
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
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()

	adapter, ok := unwrapNormalizer(loop.Provider).(*provider.AnthropicAdapter)
	require.True(t, ok, "loop.Provider after unwrap is %T, want *provider.AnthropicAdapter", unwrapNormalizer(loop.Provider))
	require.NotNil(t, adapter.Logger, "factory must inject the ScrubHandler-backed logger")
}

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

// BuildLoopWithTransport must inject a non-nil Logger into the
// OpenAIResponsesAdapter, so its quirks debug log and tool-choice
// downgrade warning go through the scrubbing logger that carries run and
// trace correlation.
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
	require.NoError(t, err)
	defer func() { _ = loop.Close() }()

	adapter, ok := unwrapNormalizer(loop.Provider).(*provider.OpenAIResponsesAdapter)
	require.True(t, ok, "loop.Provider after unwrap is %T, want *provider.OpenAIResponsesAdapter", unwrapNormalizer(loop.Provider))
	require.NotNil(t, adapter.Logger, "factory must inject the ScrubHandler-backed logger")
}

func TestBuildToolRegistry_CommandOutputReaderFollowsStore(t *testing.T) {
	store, err := commandoutput.New(commandoutput.Options{RunID: "registry-test"})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	execAll := &registryExecutor{caps: executor.ExecutorCapabilities{CanRead: true, CanWrite: true, CanExec: true}}

	cases := []struct {
		name       string
		builtIn    []string
		store      *commandoutput.Store
		wantRun    bool
		wantReader bool
	}{
		{"default tools with store", nil, store, true, true},
		{"default tools without store", nil, nil, true, false},
		{"explicit run_command allowlist gains the companion reader", []string{"run_command"}, store, true, true},
		{"standalone reader for replay", []string{"read_command_output"}, store, false, true},
		{"reader listed without a store registers nothing", []string{"read_command_output"}, nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry := buildToolRegistry(execAll, edit.NewWholeFileStrategy(), types.ToolsConfig{BuiltIn: tc.builtIn}, tc.store)
			require.Equal(t, tc.wantRun, registry.Resolve("run_command") != nil, "run_command registration")
			require.Equal(t, tc.wantReader, registry.Resolve("read_command_output") != nil, "read_command_output registration")
		})
	}
}

func TestBuildCommandOutputStore_ConstructionFollowsConfig(t *testing.T) {
	disabled := false
	cases := []struct {
		name    string
		mutate  func(*types.RunConfig)
		wantNil bool
	}{
		{"default builds a store", func(*types.RunConfig) {}, false},
		{"enabled=false skips construction", func(c *types.RunConfig) { c.Tools.CommandOutput.Enabled = &disabled }, true},
		{"tool list without command tools skips construction", func(c *types.RunConfig) {
			c.Tools.BuiltIn = []string{"read_file", "grep_files"}
		}, true},
		{"explicit run_command builds a store", func(c *types.RunConfig) {
			c.Tools.BuiltIn = []string{"run_command"}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &types.RunConfig{RunID: "gating-test"}
			tc.mutate(cfg)
			store, err := buildCommandOutputStore(context.Background(), cfg)
			require.NoError(t, err)
			if store != nil {
				defer func() { _ = store.Close() }()
			}
			if tc.wantNil {
				require.Nil(t, store)
			} else {
				require.NotNil(t, store)
			}
		})
	}
}

func testOTLPSettings(endpoint string, headers map[string]string) otlp.Settings {
	return otlp.Settings{Protocol: otlp.ProtocolHTTPProtobuf, Endpoint: endpoint, Headers: headers}
}

func TestSandboxLifecycle_MaxLifetime(t *testing.T) {
	timeout := func(s int) *int { return &s }

	t.Run("run timeout plus margin", func(t *testing.T) {
		life := sandboxLifecycle(&types.RunConfig{RunID: "run-1", Timeout: timeout(600)}, 0)
		require.Equal(t, 10*time.Minute+executor.SandboxGCMargin, life.MaxLifetime)
		require.Equal(t, "run-1", life.RunID)
	})

	t.Run("longer session budget wins", func(t *testing.T) {
		life := sandboxLifecycle(&types.RunConfig{Timeout: timeout(600)}, 5*time.Hour)
		require.Equal(t, 5*time.Hour+executor.SandboxGCMargin, life.MaxLifetime)
	})

	t.Run("budget shorter than the timeout is raised to it", func(t *testing.T) {
		life := sandboxLifecycle(&types.RunConfig{Timeout: timeout(600)}, time.Minute)
		require.Equal(t, 10*time.Minute+executor.SandboxGCMargin, life.MaxLifetime)
	})

	t.Run("no timeout and no budget stays unbounded", func(t *testing.T) {
		life := sandboxLifecycle(&types.RunConfig{}, 0)
		require.Zero(t, life.MaxLifetime)
	})

	t.Run("postRun hook budget extends the run timeout", func(t *testing.T) {
		cfg := &types.RunConfig{Timeout: timeout(600), Hooks: &types.HooksConfig{PostRun: []types.HookConfig{
			{Command: "true", TimeoutSeconds: 1800},
			{Command: "true", TimeoutSeconds: 60},
		}}}
		want := 10*time.Minute + postHookBudget(cfg.Hooks) + executor.SandboxGCMargin
		require.Equal(t, want, sandboxLifecycle(cfg, 0).MaxLifetime)
	})

	t.Run("postRun hook budget extends the session budget", func(t *testing.T) {
		cfg := &types.RunConfig{Timeout: timeout(600), Hooks: &types.HooksConfig{PostRun: []types.HookConfig{
			{Command: "true", TimeoutSeconds: 300},
		}}}
		want := 5*time.Hour + (300+30)*time.Second + executor.SandboxGCMargin
		require.Equal(t, want, sandboxLifecycle(cfg, 5*time.Hour).MaxLifetime)
	})

	t.Run("hooks without postRun entries leave the span unchanged", func(t *testing.T) {
		cfg := &types.RunConfig{Timeout: timeout(600), Hooks: &types.HooksConfig{PreRun: []types.HookConfig{
			{Command: "true", TimeoutSeconds: 900},
		}}}
		require.Equal(t, 10*time.Minute+executor.SandboxGCMargin, sandboxLifecycle(cfg, 0).MaxLifetime)
	})
}
