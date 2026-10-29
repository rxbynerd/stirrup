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
	"syscall"
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
	must := require.New(t)
	factoryRSAKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		must.NoError(err, "generate RSA key")
		der, err := x509.MarshalPKCS8PrivateKey(key)
		must.NoError(err, "marshal PKCS8 key")
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
	must := require.New(t)
	_, thisFile, _, ok := runtime.Caller(0)
	must.True(ok, "runtime.Caller(0) failed")
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
	must := require.New(t)
	r := buildRouter(types.ModelRouterConfig{
		Type:     "static",
		Provider: "anthropic",
		Model:    "claude-sonnet-4-6",
	}, "anthropic")

	sel := r.Select(context.TODO(), router.RouterContext{})
	must.Equal("anthropic", sel.Provider)
	must.Equal("claude-sonnet-4-6", sel.Model)
}

func TestBuildRouter_StaticDefaultsProvider(t *testing.T) {
	must := require.New(t)
	r := buildRouter(types.ModelRouterConfig{
		Type:  "static",
		Model: "custom-model",
	}, "bedrock")

	sel := r.Select(context.TODO(), router.RouterContext{})
	must.Equal("bedrock", sel.Provider)
}

func TestBuildRouter_PerModeSelectsModeModel(t *testing.T) {
	must := require.New(t)
	r := buildRouter(types.ModelRouterConfig{
		Type:       "per-mode",
		Provider:   "anthropic",
		Model:      "default-model",
		ModeModels: map[string]string{"planning": "bedrock/plan-model"},
	}, "anthropic")

	sel := r.Select(context.TODO(), router.RouterContext{Mode: "planning"})
	must.Equal("bedrock", sel.Provider)
	must.Equal("plan-model", sel.Model)
}

func TestBuildRouter_DynamicSelectsConfiguredProvider(t *testing.T) {
	must := require.New(t)
	r := buildRouter(types.ModelRouterConfig{
		Type:     "dynamic",
		Provider: "anthropic",
		Model:    "default-model",
	}, "anthropic")

	sel := r.Select(context.TODO(), router.RouterContext{Turn: 0})
	must.Equal("anthropic", sel.Provider)
}

func TestBuildRouter_EmptyConfigFallsBackToDefaults(t *testing.T) {
	must := require.New(t)
	r := buildRouter(types.ModelRouterConfig{}, "")

	sel := r.Select(context.TODO(), router.RouterContext{})
	must.Equal("anthropic", sel.Provider)
	must.Equal("claude-sonnet-4-6", sel.Model)
}

func TestBuildPerModeRouter_ModeModelWithSlash(t *testing.T) {
	must := require.New(t)
	r := buildPerModeRouter(types.ModelRouterConfig{
		ModeModels: map[string]string{"review": "bedrock/review-model"},
	}, "anthropic")

	sel := r.Select(context.TODO(), router.RouterContext{Mode: "review"})
	must.Equal("bedrock", sel.Provider)
	must.Equal("review-model", sel.Model)
}

func TestBuildPerModeRouter_ModeModelWithoutSlash(t *testing.T) {
	must := require.New(t)
	r := buildPerModeRouter(types.ModelRouterConfig{
		ModeModels: map[string]string{"review": "review-model"},
	}, "anthropic")

	sel := r.Select(context.TODO(), router.RouterContext{Mode: "review"})
	must.Equal("anthropic", sel.Provider)
	must.Equal("review-model", sel.Model)
}

func TestBuildPerModeRouter_DefaultsApplied(t *testing.T) {
	must := require.New(t)
	r := buildPerModeRouter(types.ModelRouterConfig{}, "")

	sel := r.Select(context.TODO(), router.RouterContext{Mode: "execution"})
	must.Equal("anthropic", sel.Provider)
	must.Equal("claude-sonnet-4-6", sel.Model)
}

func TestBuildDynamicRouter_Defaults(t *testing.T) {
	must := require.New(t)
	r := buildDynamicRouter(types.ModelRouterConfig{}, "")

	sel := r.Select(context.TODO(), router.RouterContext{Turn: 0})
	must.Equal("anthropic", sel.Provider)
}

func TestBuildDynamicRouter_CustomThresholds(t *testing.T) {
	r := buildDynamicRouter(types.ModelRouterConfig{
		ExpensiveTurnThreshold:  5,
		ExpensiveTokenThreshold: 10000,
		CheapModel:              "haiku",
		ExpensiveModel:          "opus",
	}, "anthropic")

	t.Run("under threshold uses cheap model", func(t *testing.T) {
		must := require.New(t)
		sel := r.Select(context.TODO(), router.RouterContext{Turn: 0, LastStopReason: "tool_use"})
		must.Equal("haiku", sel.Model)
	})
	t.Run("over threshold uses expensive model", func(t *testing.T) {
		must := require.New(t)
		sel := r.Select(context.TODO(), router.RouterContext{Turn: 6})
		must.Equal("opus", sel.Model)
	})
}

func mustBuildPromptBuilder(t *testing.T, config *types.RunConfig) prompt.PromptBuilder {
	t.Helper()
	must := require.New(t)
	pb, err := buildPromptBuilder(config)
	must.NoError(err, "buildPromptBuilder")
	return pb
}

func TestBuildPromptBuilder_DefaultType(t *testing.T) {
	must := require.New(t)
	pb := mustBuildPromptBuilder(t, &types.RunConfig{PromptBuilder: types.PromptBuilderConfig{Type: "default"}})
	must.IsType((*prompt.DefaultPromptBuilder)(nil), pb)
}

func TestBuildPromptBuilder_EmptyTypeUsesDefault(t *testing.T) {
	must := require.New(t)
	pb := mustBuildPromptBuilder(t, &types.RunConfig{})
	must.IsType((*prompt.DefaultPromptBuilder)(nil), pb)
}

func TestBuildPromptBuilder_ComposedType(t *testing.T) {
	must := require.New(t)
	pb := mustBuildPromptBuilder(t, &types.RunConfig{PromptBuilder: types.PromptBuilderConfig{Type: "composed"}})
	must.IsType((*prompt.ComposedPromptBuilder)(nil), pb)
}

func TestBuildPromptBuilder_UnknownFallsBackToDefault(t *testing.T) {
	must := require.New(t)
	pb := mustBuildPromptBuilder(t, &types.RunConfig{PromptBuilder: types.PromptBuilderConfig{Type: "nonexistent"}})
	must.IsType((*prompt.DefaultPromptBuilder)(nil), pb)
}

func TestBuildPromptBuilder_SystemPromptOverrideIsComposed(t *testing.T) {
	must := require.New(t)
	pb := mustBuildPromptBuilder(t, &types.RunConfig{
		PromptBuilder:        types.PromptBuilderConfig{Type: "default"},
		SystemPromptOverride: "Custom system prompt",
	})
	must.IsType((*prompt.ComposedPromptBuilder)(nil), pb)
}

func TestBuildPromptBuilder_OperatorTemplateIsComposed(t *testing.T) {
	must := require.New(t)
	pb := mustBuildPromptBuilder(t, &types.RunConfig{
		Mode:          "execution",
		PromptBuilder: types.PromptBuilderConfig{Template: `Tuned.{{if eq .Tier "frontier"}} Act when ready.{{end}}`},
	})
	must.IsType((*prompt.ComposedPromptBuilder)(nil), pb)
}

// A template that parses but fails at execution (unknown field) must be
// caught by the trial render at construction, not at run start.
func TestBuildPromptBuilder_OperatorTemplateExecErrorFailsFast(t *testing.T) {
	must := require.New(t)
	_, err := buildPromptBuilder(&types.RunConfig{
		Mode:          "execution",
		PromptBuilder: types.PromptBuilderConfig{Template: "hello {{.NoSuchField}}"},
	})
	must.Error(err)
}

// The override wins over an operator template. ValidateRunConfig rejects
// the combination, but the factory's precedence must still be safe if a
// caller skips validation.
func TestBuildPromptBuilder_OverrideWinsOverTemplate(t *testing.T) {
	must := require.New(t)
	const override = "Raw override with {{not a template}}"
	pb := mustBuildPromptBuilder(t, &types.RunConfig{
		SystemPromptOverride: override,
		PromptBuilder:        types.PromptBuilderConfig{Template: "Tuned."},
	})
	got, err := pb.Build(t.Context(), prompt.PromptContext{Mode: "execution"})
	must.NoError(err)
	must.True(strings.HasPrefix(got, override), "override not used verbatim, got %q", got)
}

func TestBuildContextStrategy_SlidingWindow(t *testing.T) {
	must := require.New(t)
	cs := buildContextStrategy(types.ContextStrategyConfig{Type: "sliding-window"}, nil, "", 0, nil, nil)
	must.IsType((*contextpkg.SlidingWindowStrategy)(nil), cs)
}

func TestBuildContextStrategy_EmptyTypeUsesSlidingWindow(t *testing.T) {
	must := require.New(t)
	cs := buildContextStrategy(types.ContextStrategyConfig{}, nil, "", 0, nil, nil)
	must.IsType((*contextpkg.SlidingWindowStrategy)(nil), cs)
}

func TestBuildContextStrategy_Summarise(t *testing.T) {
	must := require.New(t)
	cs := buildContextStrategy(types.ContextStrategyConfig{Type: "summarise"}, nil, "model", 0, nil, nil)
	must.IsType((*contextpkg.SummariseStrategy)(nil), cs)
}

func TestBuildContextStrategy_OffloadToFile(t *testing.T) {
	must := require.New(t)
	exec, _ := executor.NewLocalExecutor(t.TempDir())
	cs := buildContextStrategy(types.ContextStrategyConfig{Type: "offload-to-file"}, nil, "", 0, exec, nil)
	must.IsType((*contextpkg.OffloadToFileStrategy)(nil), cs)
}

func TestBuildContextStrategy_UnknownFallsBack(t *testing.T) {
	must := require.New(t)
	cs := buildContextStrategy(types.ContextStrategyConfig{Type: "nonexistent"}, nil, "", 0, nil, nil)
	must.IsType((*contextpkg.SlidingWindowStrategy)(nil), cs)
}

func TestBuildEditStrategy_WholeFile(t *testing.T) {
	must := require.New(t)
	es := buildEditStrategy(types.EditStrategyConfig{Type: "whole-file"})
	must.IsType((*edit.WholeFileStrategy)(nil), es)
}

// TestBuildEditStrategy_EmptyTypeUsesMulti pins the factory fallback for
// callers that bypass types.ValidateRunConfig, which fills an empty type
// with "multi". The fallback must match that default rather than expose a
// different write-tool surface.
func TestBuildEditStrategy_EmptyTypeUsesMulti(t *testing.T) {
	must := require.New(t)
	es := buildEditStrategy(types.EditStrategyConfig{})
	must.IsType((*edit.MultiStrategy)(nil), es)
}

func TestBuildEditStrategy_SearchReplace(t *testing.T) {
	must := require.New(t)
	es := buildEditStrategy(types.EditStrategyConfig{Type: "search-replace"})
	must.IsType((*edit.SearchReplaceStrategy)(nil), es)
}

func TestBuildEditStrategy_Udiff(t *testing.T) {
	must := require.New(t)
	es := buildEditStrategy(types.EditStrategyConfig{Type: "udiff"})
	must.IsType((*edit.UdiffStrategy)(nil), es)
}

func TestBuildEditStrategy_Multi(t *testing.T) {
	must := require.New(t)
	es := buildEditStrategy(types.EditStrategyConfig{Type: "multi"})
	must.IsType((*edit.MultiStrategy)(nil), es)
}

func TestBuildEditStrategy_CustomFuzzyThreshold(t *testing.T) {
	must := require.New(t)
	threshold := 0.95
	es := buildEditStrategy(types.EditStrategyConfig{Type: "udiff", FuzzyThreshold: &threshold})
	must.IsType((*edit.UdiffStrategy)(nil), es)
}

// buildEditStrategy must clamp a non-positive fuzzy threshold for callers that
// skip types.ValidateRunConfig. A diff that cannot match must produce a clean
// no-match result rather than panic in the udiff fuzzy search.
func TestBuildEditStrategy_OutOfRangeFuzzyThresholdClamped(t *testing.T) {
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err, "NewLocalExecutor")
	must.NoError(exec.WriteFile(context.Background(), "file.txt", "aaaa\nbbbb\ncccc\n"), "WriteFile")

	badThreshold := 0.0
	es := buildEditStrategy(types.EditStrategyConfig{Type: "udiff", FuzzyThreshold: &badThreshold})

	diff := "--- a/file.txt\n+++ b/file.txt\n@@ -1,1 +1,1 @@\n-zzzz\n+yyyy\n"
	input, err := json.Marshal(struct {
		Path string `json:"path"`
		Diff string `json:"diff"`
	}{Path: "file.txt", Diff: diff})
	must.NoError(err, "marshal input")

	result, err := es.Apply(context.Background(), input, exec)
	must.NoError(err, "a non-matching diff is a no-match result, not an error")
	must.False(result.Applied, "diff cannot match aaaa/bbbb/cccc")
}

func TestBuildEditStrategy_UnknownFallsBack(t *testing.T) {
	must := require.New(t)
	es := buildEditStrategy(types.EditStrategyConfig{Type: "nonexistent"})
	must.IsType((*edit.MultiStrategy)(nil), es)
}

func TestWrapWithCodeScanner_NilLeavesInnerUnchanged(t *testing.T) {
	must := require.New(t)
	inner := edit.NewWholeFileStrategy()
	got, err := wrapWithCodeScanner(inner, nil, nil)
	must.NoError(err)
	must.Same(inner, got, "nil scanner config must return the inner strategy")
}

func TestWrapWithCodeScanner_NoneLeavesInnerUnchanged(t *testing.T) {
	must := require.New(t)
	inner := edit.NewWholeFileStrategy()
	got, err := wrapWithCodeScanner(inner, &types.CodeScannerConfig{Type: "none"}, nil)
	must.NoError(err)
	must.Same(inner, got, "type none must return the inner strategy")
}

func TestWrapWithCodeScanner_PatternsWraps(t *testing.T) {
	must := require.New(t)
	inner := edit.NewWholeFileStrategy()
	got, err := wrapWithCodeScanner(inner, &types.CodeScannerConfig{Type: "patterns"}, nil)
	must.NoError(err)
	must.NotSame(inner, got, "patterns scanner must wrap the inner strategy")
	must.IsType((*edit.ScannedStrategy)(nil), got)
}

func TestWrapWithCodeScanner_UnknownTypeReturnsError(t *testing.T) {
	must := require.New(t)
	inner := edit.NewWholeFileStrategy()
	_, err := wrapWithCodeScanner(inner, &types.CodeScannerConfig{Type: "nope"}, nil)
	must.Error(err)
}

func TestBuildVerifier_None(t *testing.T) {
	must := require.New(t)
	v := buildVerifier(types.VerifierConfig{Type: "none"}, nil, nil, nil)
	must.IsType((*verifier.NoneVerifier)(nil), v)
}

func TestBuildVerifier_EmptyTypeUsesNone(t *testing.T) {
	must := require.New(t)
	v := buildVerifier(types.VerifierConfig{}, nil, nil, nil)
	must.IsType((*verifier.NoneVerifier)(nil), v)
}

func TestBuildVerifier_TestRunner(t *testing.T) {
	must := require.New(t)
	v := buildVerifier(types.VerifierConfig{Type: "test-runner", Command: "go test ./..."}, nil, nil, nil)
	must.IsType((*verifier.TestRunnerVerifier)(nil), v)
}

func TestBuildVerifier_LLMJudge(t *testing.T) {
	must := require.New(t)
	v := buildVerifier(types.VerifierConfig{Type: "llm-judge", Criteria: "test criteria"}, nil, nil, nil)
	must.IsType((*verifier.LLMJudgeVerifier)(nil), v)
}

func TestBuildVerifier_Composite(t *testing.T) {
	must := require.New(t)
	v := buildVerifier(types.VerifierConfig{
		Type: "composite",
		Verifiers: []types.VerifierConfig{
			{Type: "none"},
			{Type: "test-runner", Command: "echo ok"},
		},
	}, nil, nil, nil)
	must.IsType((*verifier.CompositeVerifier)(nil), v)
}

func TestBuildVerifier_UnknownFallsBack(t *testing.T) {
	must := require.New(t)
	v := buildVerifier(types.VerifierConfig{Type: "nonexistent"}, nil, nil, nil)
	must.IsType((*verifier.NoneVerifier)(nil), v)
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
	must := require.New(t)
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

	must.Contains(buf.String(), `"event":"rule_of_two_disabled"`)
}

func TestEmitRuleOfTwoEvents_AllThreeWithoutOverrideStaysSilent(t *testing.T) {
	must := require.New(t)
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

	must.NotContains(buf.String(), "rule_of_two_disabled")
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
			must := require.New(t)
			sec, buf := captureSecLogger(t)
			emitRuleOfTwoEvents(tc.cfg, sec, resolveRuleOfTwoArming(tc.cfg))
			out := buf.String()
			must.Contains(out, `"event":"rule_of_two_warning"`)
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
	must := require.New(t)
	must.Contains(out, fmt.Sprintf(`"%s":%t`, key, want))
}

// TestEmitRuleOfTwoEvents_DisabledPayloadShape extends the override
// path to assert the same payload booleans appear on rule_of_two_disabled
// events. The audit consumer gets a single shape across both events.
func TestEmitRuleOfTwoEvents_DisabledPayloadShape(t *testing.T) {
	must := require.New(t)
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
	must.Contains(out, `"event":"rule_of_two_disabled"`)
	assertPayloadBool(t, out, "untrustedInput", true)
	assertPayloadBool(t, out, "sensitiveData", true)
	assertPayloadBool(t, out, "externalCommunication", true)
}

func TestEmitRuleOfTwoEvents_NoneOrOneStaysSilent(t *testing.T) {
	must := require.New(t)
	sec, buf := captureSecLogger(t)
	cfg := &types.RunConfig{
		Mode:             "execution",
		Provider:         types.ProviderConfig{Type: "anthropic"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"read_file"}},
	}
	emitRuleOfTwoEvents(cfg, sec, resolveRuleOfTwoArming(cfg))
	must.Empty(buf.String(), "zero-or-one flag config should emit nothing")
}

// buildPermissionPolicyForTest is a thin wrapper that fabricates a
// minimal RunConfig from a bare PermissionPolicyConfig so the existing
// table tests don't have to spell out a full config every call.
func buildPermissionPolicyForTest(t *testing.T, cfg types.PermissionPolicyConfig, registry *tool.Registry, tp transport.Transport) permission.PermissionPolicy {
	t.Helper()
	must := require.New(t)
	rc := &types.RunConfig{
		RunID:            "test-run",
		Mode:             "execution",
		PermissionPolicy: cfg,
	}
	pp, err := buildPermissionPolicy(rc, registry, tp, nil)
	must.NoError(err)
	return pp
}

func TestBuildPermissionPolicy_AllowAll(t *testing.T) {
	must := require.New(t)
	pp := buildPermissionPolicyForTest(t, types.PermissionPolicyConfig{Type: "allow-all"}, nil, nil)
	must.IsType((*permission.AllowAll)(nil), pp)
}

func TestBuildPermissionPolicy_DenySideEffects(t *testing.T) {
	must := require.New(t)
	registry := buildToolRegistry(&registryExecutor{
		caps: executor.ExecutorCapabilities{CanRead: true, CanWrite: true, CanExec: true},
	}, edit.NewWholeFileStrategy(), types.ToolsConfig{}, nil)
	pp := buildPermissionPolicyForTest(t, types.PermissionPolicyConfig{Type: "deny-side-effects"}, registry, nil)
	must.IsType((*permission.DenySideEffects)(nil), pp)
}

func TestBuildPermissionPolicy_AskUpstream(t *testing.T) {
	must := require.New(t)
	registry := buildToolRegistry(&registryExecutor{
		caps: executor.ExecutorCapabilities{CanRead: true},
	}, edit.NewWholeFileStrategy(), types.ToolsConfig{}, nil)
	tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
	pp := buildPermissionPolicyForTest(t, types.PermissionPolicyConfig{Type: "ask-upstream", Timeout: 60}, registry, tp)
	must.IsType((*permission.AskUpstreamPolicy)(nil), pp)
}

// An unrecognised or empty PermissionPolicy.Type returns an error instead of
// falling through to allow-all, even when config validation was bypassed.
func TestBuildPermissionPolicy_UnknownTypeReturnsError(t *testing.T) {
	registry := buildToolRegistry(&registryExecutor{
		caps: executor.ExecutorCapabilities{CanRead: true},
	}, edit.NewWholeFileStrategy(), types.ToolsConfig{}, nil)

	t.Run("unknown type", func(t *testing.T) {
		must := require.New(t)
		rc := &types.RunConfig{
			PermissionPolicy: types.PermissionPolicyConfig{Type: "bogus"},
		}
		_, err := buildPermissionPolicy(rc, registry, nil, nil)
		must.Error(err)
	})

	t.Run("empty type", func(t *testing.T) {
		must := require.New(t)
		rc := &types.RunConfig{PermissionPolicy: types.PermissionPolicyConfig{}}
		_, err := buildPermissionPolicy(rc, registry, nil, nil)
		must.Error(err)
	})
}

// TestBuildPermissionPolicy_PolicyEngine verifies the policy-engine arm
// loads a Cedar policy file from disk and constructs a PolicyEnginePolicy
// with the configured fallback. Uses an in-tree starter policy so we
// exercise the real LoadPolicySetFromFile path.
func TestBuildPermissionPolicy_PolicyEngine(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.IsType((*permission.PolicyEnginePolicy)(nil), pp)
}

// TestBuildPermissionPolicy_PolicyEngineFallbackIsBuilt verifies the
// fallback closure threads through the same buildPermissionPolicy
// dispatch — i.e. an explicit fallback="ask-upstream" really constructs
// an AskUpstreamPolicy under the hood. This is what makes the
// no-decision path behave like a top-level ask-upstream config.
func TestBuildPermissionPolicy_PolicyEngineFallbackIsBuilt(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	must.IsType((*permission.PolicyEnginePolicy)(nil), pp)
}

// TestBuildPermissionPolicy_PolicyEngineMissingFile asserts that a
// missing policy file fails fast with a clear error.
func TestBuildPermissionPolicy_PolicyEngineMissingFile(t *testing.T) {
	must := require.New(t)
	rc := &types.RunConfig{
		RunID: "test-run",
		Mode:  "execution",
		PermissionPolicy: types.PermissionPolicyConfig{
			Type:       "policy-engine",
			PolicyFile: "/this/path/does/not/exist.cedar",
		},
	}
	_, err := buildPermissionPolicy(rc, nil, nil, nil)
	must.Error(err)
}

// Wrapping a built policy-engine policy with metrics composes the policy
// without reading the Cedar file again. The file is removed before the wrap,
// so any second read would fail.
func TestBuildPermissionPolicy_PolicyEngineFileReadOnce(t *testing.T) {
	must := require.New(t)
	starter := filepath.Join(repoRootForTests(t), "examples", "policies", "destructive-shell.cedar")
	if _, err := os.Stat(starter); err != nil {
		t.Skipf("starter policy missing at %q: %v", starter, err)
	}
	contents, err := os.ReadFile(starter)
	must.NoError(err)
	tempPath := filepath.Join(t.TempDir(), "test-policy.cedar")
	must.NoError(os.WriteFile(tempPath, contents, 0o600))

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
	must.NoError(err)

	must.NoError(os.Remove(tempPath))

	// A nil metrics argument makes the wrap a no-op that returns the inner policy.
	wrapped := wrapPermissionPolicyMetrics(pp, rc.PermissionPolicy, nil)
	must.NotNil(wrapped)
	must.Same(pp, wrapped, "nil metrics wrap should return inner unchanged")
}

func TestBuildGitStrategy_None(t *testing.T) {
	must := require.New(t)
	gs := buildGitStrategy(types.GitStrategyConfig{Type: "none"}, nil)
	must.IsType((*git.NoneGitStrategy)(nil), gs)
}

func TestBuildGitStrategy_Empty(t *testing.T) {
	must := require.New(t)
	gs := buildGitStrategy(types.GitStrategyConfig{}, nil)
	must.IsType((*git.NoneGitStrategy)(nil), gs)
}

func TestBuildGitStrategy_Deterministic(t *testing.T) {
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err)
	gs := buildGitStrategy(types.GitStrategyConfig{Type: "deterministic"}, exec)
	must.IsType((*git.DeterministicGitStrategy)(nil), gs)
}

func TestBuildGitStrategy_UnknownFallsBack(t *testing.T) {
	must := require.New(t)
	gs := buildGitStrategy(types.GitStrategyConfig{Type: "nonexistent"}, nil)
	must.IsType((*git.NoneGitStrategy)(nil), gs)
}

func TestBuildHookRunner_NilConfigReturnsNoop(t *testing.T) {
	must := require.New(t)
	r := buildHookRunner(nil, nil, nil)
	must.IsType((*hook.Noop)(nil), r)
}

func TestBuildHookRunner_EmptyConfigReturnsNoop(t *testing.T) {
	must := require.New(t)
	r := buildHookRunner(&types.HooksConfig{}, nil, nil)
	must.IsType((*hook.Noop)(nil), r)
}

func TestBuildHookRunner_PreRunOnlyReturnsExecRunner(t *testing.T) {
	must := require.New(t)
	cfg := &types.HooksConfig{PreRun: []types.HookConfig{{Command: "true"}}}
	r := buildHookRunner(cfg, nil, nil)
	execRunner, ok := r.(*hook.ExecRunner)
	must.True(ok, "expected *hook.ExecRunner, got %T", r)
	must.Same(cfg, execRunner.Hooks, "ExecRunner.Hooks must be the HooksConfig passed in")
}

func TestBuildHookRunner_PostRunOnlyReturnsExecRunner(t *testing.T) {
	must := require.New(t)
	cfg := &types.HooksConfig{PostRun: []types.HookConfig{{Command: "true"}}}
	r := buildHookRunner(cfg, nil, nil)
	must.IsType((*hook.ExecRunner)(nil), r)
}

func TestBuildTraceEmitter_JSONLWithoutPath(t *testing.T) {
	must := require.New(t)
	te, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{Type: "jsonl"}, platform.Exporters{}, observability.ResourceOptions{}, false)
	must.NoError(err)
	must.IsType((*trace.JSONLTraceEmitter)(nil), te)
}

func TestBuildTraceEmitter_JSONLWithPath(t *testing.T) {
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	te, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{Type: "jsonl", FilePath: path}, platform.Exporters{}, observability.ResourceOptions{}, false)
	must.NoError(err)
	must.IsType((*trace.JSONLTraceEmitter)(nil), te)
	_, err = os.Stat(path)
	must.NoError(err, "expected trace file to exist")
}

func TestBuildTraceEmitter_EmptyTypeDefaultsToJSONL(t *testing.T) {
	must := require.New(t)
	te, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{}, platform.Exporters{}, observability.ResourceOptions{}, false)
	must.NoError(err)
	must.IsType((*trace.JSONLTraceEmitter)(nil), te)
}

func TestBuildTraceEmitter_UnsupportedType(t *testing.T) {
	must := require.New(t)
	_, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{Type: "datadog"}, platform.Exporters{}, observability.ResourceOptions{}, false)
	must.ErrorContains(err, "unsupported trace emitter type")
}

func TestResolveTelemetry_WithoutPlatformKeepsTheExplicitConfig(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)

	t.Run("endpoints", func(t *testing.T) {
		must := require.New(t)
		must.Equal("https://gw.example/otlp", exporters.Traces.Endpoint)
		must.Equal("https://metrics.example/otlp", exporters.Metrics.Endpoint)
		must.Equal("https://gw.example/otlp", exporters.Logs.Endpoint)
	})

	t.Run("trace settings", func(t *testing.T) {
		must := require.New(t)
		must.Equal("Bearer t", exporters.Traces.Headers["Authorization"])
		must.Nil(exporters.Traces.HTTPClient, "trace exporter should use the SDK client")
	})

	t.Run("resource options", func(t *testing.T) {
		must := require.New(t)
		must.Equal("agent-a", resourceOpts.ServiceName)
		must.Equal("planning", resourceOpts.RunMode)
		must.Nil(resourceOpts.Extra)
	})
}

func TestResolveTelemetry_RejectsUnknownPlatformOption(t *testing.T) {
	must := require.New(t)
	config := &types.RunConfig{TraceEmitter: types.TraceEmitterConfig{
		Type:            "otel",
		Protocol:        "http/protobuf",
		Platform:        types.ValidTraceEmitterPlatformValues()[0],
		PlatformOptions: map[string]string{"notAnOption": "x"},
	}}
	_, _, err := resolveTelemetry(context.Background(), config, nil)
	must.ErrorContains(err, "resolve observability platform")
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
	must := require.New(t)
	isolateCloudCredentials(t)
	var attributed, decorated, wrapped bool
	for _, name := range types.ValidTraceEmitterPlatformValues() {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
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
			must.NoError(err, "no test option set resolves this platform")

			attributed = attributed || len(exporters.ResourceAttrs) > 0
			must.Equal(exporters.ResourceAttrs, resourceOpts.Extra, "resource Extra should match the profile's attributes")
			opts := otelEmitterOptions(config.TraceEmitter, exporters, resourceOpts, false)
			must.Equal(exporters.WrapSpanExporter != nil, opts.WrapSpanExporter != nil, "the trace emitter must receive the profile's span exporter wrapper")
			must.Equal(exporters.ResourceAttrs, opts.Resource.Extra, "trace emitter resource Extra should match the profile's attributes")
			decorated = decorated || exporters.WrapSpanExporter != nil
			if opts.Exporter.HTTPClient == nil {
				return
			}
			wrapped = true
			for sig, s := range map[string]otlp.Settings{"traces": opts.Exporter, "metrics": exporters.Metrics, "logs": exporters.Logs} {
				must.NotNil(s.HTTPClient, "%s must use the profile's HTTP client", sig)
				must.Positive(s.HTTPClient.Timeout, "%s must use a bounded HTTP client", sig)
			}
			must.True(metricsEnabled(config, exporters), "a profile with a metrics endpoint must enable metric export")
		})
	}
	must.True(attributed && decorated && wrapped,
		"the registered platforms must exercise resource attributes (%v), span decoration (%v) and wrapped transports (%v)",
		attributed, decorated, wrapped)
}

func TestLogPlatformExporters_NamesEndpointsNotHeaders(t *testing.T) {
	must := require.New(t)
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
	must.Contains(out, "platform=example")
	must.Contains(out, "traces=https://traces.example")
	must.Contains(out, "metrics=https://metrics.example")
	must.Contains(out, "OTEL_EXPORTER_OTLP environment")
	must.NotContains(out, "header-value-must-not-appear", "log line must not leak a header value")

	buf.Reset()
	logPlatformExporters(logger, config, platform.Exporters{})
	must.Empty(buf.String(), "no platform must log nothing")
}

func TestBuildTraceEmitter_JSONLBadPath(t *testing.T) {
	must := require.New(t)
	_, err := buildTraceEmitter(context.Background(), types.TraceEmitterConfig{
		Type:     "jsonl",
		FilePath: "/nonexistent/deeply/nested/dir/trace.jsonl",
	}, platform.Exporters{}, observability.ResourceOptions{}, false)
	must.Error(err)
}

func TestBuildExecutor_Local(t *testing.T) {
	must := require.New(t)
	workspace := t.TempDir()
	exec, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type:      "local",
		Workspace: workspace,
	}, nil, nil, nil, executor.Lifecycle{})
	must.NoError(err)
	must.IsType((*executor.LocalExecutor)(nil), exec)
}

func TestBuildExecutor_EmptyTypeDefaultsToLocal(t *testing.T) {
	must := require.New(t)
	exec, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Workspace: t.TempDir(),
	}, nil, nil, nil, executor.Lifecycle{})
	must.NoError(err)
	must.IsType((*executor.LocalExecutor)(nil), exec)
}

func TestBuildExecutor_LocalDefaultsWorkspaceToCwd(t *testing.T) {
	must := require.New(t)
	exec, err := buildExecutor(context.Background(), types.ExecutorConfig{Type: "local"}, nil, nil, nil, executor.Lifecycle{})
	must.NoError(err)
	must.NotNil(exec)
}

func TestBuildExecutor_API_MissingVcsBackend(t *testing.T) {
	must := require.New(t)
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{Type: "api"}, nil, nil, nil, executor.Lifecycle{})
	must.ErrorContains(err, "requires vcsBackend")
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
			must := require.New(t)
			secrets := &stubSecretStore{secrets: map[string]string{"secret://token": "tok"}}
			exec, err := buildExecutor(context.Background(), types.ExecutorConfig{
				Type:       "api",
				VcsBackend: &tt.backend,
			}, secrets, nil, nil, executor.Lifecycle{})

			if tt.wantErr != "" {
				must.Error(err, "got executor %T", exec)
				must.ErrorContains(err, tt.wantErr)
				return
			}
			must.NoError(err)
			must.Equal(tt.wantExec, fmt.Sprintf("%T", exec))
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
			must := require.New(t)
			exec, err := buildExecutor(context.Background(), types.ExecutorConfig{
				Type: "api",
				VcsBackend: &types.VcsBackendConfig{
					Type: backend.backendType,
					Repo: backend.repo,
					Ref:  "main",
				},
			}, &stubSecretStore{}, nil, nil, executor.Lifecycle{})
			must.NoError(err)
			must.Equal(backend.wantExec, fmt.Sprintf("%T", exec))
		})
	}
}

// The error for an unresolvable apiKeyRef names the config field that holds it.
func TestBuildExecutor_API_SecretResolutionFailureNamesTheField(t *testing.T) {
	must := require.New(t)
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type: "api",
		VcsBackend: &types.VcsBackendConfig{
			Type:      "github",
			APIKeyRef: "secret://missing",
			Repo:      "owner/repo",
		},
	}, &stubSecretStore{}, nil, nil, executor.Lifecycle{})
	must.ErrorContains(err, "executor.vcsBackend.apiKeyRef")
}

func TestBuildExecutor_Container_MissingImage(t *testing.T) {
	must := require.New(t)
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{Type: "container"}, nil, nil, nil, executor.Lifecycle{})
	must.ErrorContains(err, "requires image")
}

func TestBuildExecutor_None_HasNoCapabilities(t *testing.T) {
	must := require.New(t)
	exec, err := buildExecutor(context.Background(), types.ExecutorConfig{Type: "none"}, nil, nil, nil, executor.Lifecycle{})
	must.NoError(err)
	must.IsType((*executor.NoneExecutor)(nil), exec)

	caps := exec.Capabilities()
	must.False(caps.CanRead, "CanRead")
	must.False(caps.CanWrite, "CanWrite")
	must.False(caps.CanExec, "CanExec")
	must.False(caps.CanNetwork, "CanNetwork")
}

func TestBuildExecutor_UnsupportedType(t *testing.T) {
	must := require.New(t)
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{Type: "microvm"}, nil, nil, nil, executor.Lifecycle{})
	must.ErrorContains(err, "unsupported executor type")
}

func TestBuildExecutor_K8s_MissingImage(t *testing.T) {
	must := require.New(t)
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type:         "k8s",
		K8sNamespace: "default",
	}, nil, nil, nil, executor.Lifecycle{})
	must.ErrorContains(err, "requires image")
}

func TestBuildExecutor_K8s_MissingNamespace(t *testing.T) {
	must := require.New(t)
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type:  "k8s",
		Image: "busybox",
	}, nil, nil, nil, executor.Lifecycle{})
	must.ErrorContains(err, "requires k8sNamespace")
}

// TestBuildExecutor_K8s_TakesK8sPath confirms the "k8s" case is reached
// (not the default "unsupported" arm) when Image and K8sNamespace are
// present. Without a cluster, NewK8sExecutor fails downstream at REST
// config / Pod creation — the error must therefore be a k8s-construction
// failure, never "unsupported executor type". A bogus kubeconfig path
// forces a deterministic, cluster-free failure.
func TestBuildExecutor_K8s_TakesK8sPath(t *testing.T) {
	must := require.New(t)
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
	must.NotContains(err.Error(), "unsupported executor type", "k8s type fell through to the default arm")
}

func TestBuildExecutor_K8sSandbox_MissingImage(t *testing.T) {
	must := require.New(t)
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type:         "k8s-sandbox",
		K8sNamespace: "default",
	}, nil, nil, nil, executor.Lifecycle{})
	must.ErrorContains(err, "requires image")
}

func TestBuildExecutor_K8sSandbox_MissingNamespace(t *testing.T) {
	must := require.New(t)
	_, err := buildExecutor(context.Background(), types.ExecutorConfig{
		Type:  "k8s-sandbox",
		Image: "busybox",
	}, nil, nil, nil, executor.Lifecycle{})
	must.ErrorContains(err, "requires k8sNamespace")
}

// TestBuildExecutor_K8sSandbox_TakesAgentSandboxPath confirms the
// "k8s-sandbox" case routes to NewAgentSandboxExecutor (not the default
// "unsupported" arm) when Image, K8sNamespace, and Network are present.
// Without a cluster, NewAgentSandboxExecutor fails downstream at REST config /
// Sandbox creation, so the error must be an agent-sandbox-construction
// failure, never "unsupported executor type". A bogus kubeconfig path forces a
// deterministic, cluster-free failure.
func TestBuildExecutor_K8sSandbox_TakesAgentSandboxPath(t *testing.T) {
	must := require.New(t)
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
	must.NotContains(err.Error(), "unsupported executor type", "k8s-sandbox type fell through to the default arm")
}

func TestBuildTransport_StdioReturnsTransport(t *testing.T) {
	must := require.New(t)
	tp, err := buildTransport(context.Background(), types.TransportConfig{Type: "stdio"}, nil)
	must.NoError(err)
	must.NotNil(tp)
}

func TestBuildTransport_EmptyDefaultsToStdio(t *testing.T) {
	must := require.New(t)
	tp, err := buildTransport(context.Background(), types.TransportConfig{}, nil)
	must.NoError(err)
	must.NotNil(tp)
}

func TestBuildTransport_GRPCMissingAddress(t *testing.T) {
	must := require.New(t)
	_, err := buildTransport(context.Background(), types.TransportConfig{Type: "grpc"}, nil)
	must.ErrorContains(err, "requires an address")
}

func TestBuildTransport_UnsupportedType(t *testing.T) {
	must := require.New(t)
	_, err := buildTransport(context.Background(), types.TransportConfig{Type: "websocket"}, nil)
	must.ErrorContains(err, "unsupported transport type")
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
			must := require.New(t)
			_, err := buildTransport(context.Background(), types.TransportConfig{Type: tt.typ}, creds)
			must.ErrorContains(err, "require the grpc transport")
		})
	}
}

func TestBuildTransport_GRPCUsesTLSCredentials(t *testing.T) {
	must := require.New(t)
	pki := tlstest.New(t)
	creds, err := transport.TLSConfig{
		CAFile:   pki.CAFile,
		CertFile: pki.ClientCertFile,
		KeyFile:  pki.ClientKeyFile,
	}.Credentials()
	must.NoError(err, "Credentials")

	serve := func(serverTLS *tls.Config) string {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		must.NoError(err, "net.Listen")
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
	must.NoError(err, "buildTransport over mutual TLS")
	_ = tp.Close()

	tp, err = buildTransport(ctx, types.TransportConfig{Type: "grpc", Address: serve(nil)}, creds)
	if err == nil {
		_ = tp.Close()
	}
	must.Error(err, "buildTransport with TLS credentials connected to a plaintext server")
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
			must := require.New(t)
			must.Equal(tt.want, parseLogLevel(tt.input))
		})
	}
}

func TestToolEnabled_EmptyListEnablesAll(t *testing.T) {
	must := require.New(t)
	must.True(toolEnabled(nil, "read_file"), "nil list")
	must.True(toolEnabled([]string{}, "read_file"), "empty slice")
}

func TestToolEnabled_ExplicitListEnablesOnlyListed(t *testing.T) {
	must := require.New(t)
	enabled := []string{"read_file", "run_command"}
	must.True(toolEnabled(enabled, "read_file"))
	must.False(toolEnabled(enabled, "write_file"))
}

func TestEditToolEnabled_EmptyListEnablesAll(t *testing.T) {
	must := require.New(t)
	must.True(editToolEnabled(nil, "write_file"))
}

func TestEditToolEnabled_DirectMatch(t *testing.T) {
	must := require.New(t)
	must.True(editToolEnabled([]string{"edit_file"}, "edit_file"))
}

func TestEditToolEnabled_AliasMatch(t *testing.T) {
	must := require.New(t)
	// "write_file" is an alias for edit tools.
	must.True(editToolEnabled([]string{"write_file"}, "edit_file"), "write_file alias")
	must.True(editToolEnabled([]string{"search_replace"}, "edit_file"), "search_replace alias")
	must.True(editToolEnabled([]string{"apply_diff"}, "edit_file"), "apply_diff alias")
}

func TestEditToolEnabled_NoMatch(t *testing.T) {
	must := require.New(t)
	must.False(editToolEnabled([]string{"read_file"}, "edit_file"))
}

func TestMutatingToolSet_ClassifiesWorkspaceTools(t *testing.T) {
	must := require.New(t)
	exec, _ := executor.NewLocalExecutor(t.TempDir())
	registry := buildToolRegistry(exec, edit.NewWholeFileStrategy(), types.ToolsConfig{}, nil)

	mutating := mutatingToolSet(registry)

	// write_file and run_command mutate the workspace.
	must.True(mutating["write_file"], "write_file should be workspace-mutating")
	must.True(mutating["run_command"], "run_command should be workspace-mutating")
	// Read-only tools are not mutating. (spawn_agent is registered
	// after the loop is built and is therefore not in this set; see
	// BuildLoopWithTransport.)
	for _, name := range []string{"read_file", "list_directory", "grep_files", "find_files", "web_fetch"} {
		must.False(mutating[name], "%s should not be workspace-mutating", name)
	}
}

func TestApprovalRequiredToolSet(t *testing.T) {
	must := require.New(t)
	exec, _ := executor.NewLocalExecutor(t.TempDir())
	registry := buildToolRegistry(exec, edit.NewWholeFileStrategy(), types.ToolsConfig{}, nil)

	approval := approvalRequiredToolSet(registry)

	// All tools that touch the world require approval: writes, shell, network.
	for _, name := range []string{"write_file", "run_command", "web_fetch"} {
		must.True(approval[name], "%s should require approval", name)
	}
	// Pure-read tools never require approval.
	for _, name := range []string{"read_file", "list_directory", "grep_files", "find_files"} {
		must.False(approval[name], "%s should not require approval", name)
	}
	// spawn_agent is registered post-loop-construction in factory.go (see
	// BuildLoopWithTransport). Its absence here is load-bearing: if it
	// ever appears in this set, the AddApprovalTool refresh has become
	// redundant and the post-registration step in the factory should
	// be removed.
	must.False(approval["spawn_agent"], "spawn_agent is added after loop construction in BuildLoopWithTransport")
}

// TestBuildToolRegistry_DefaultReadOnlyIncludesGitTools checks that
// buildToolRegistry registers the git tools for the default read-only tool
// list, the configuration a bare read-only run uses.
func TestBuildToolRegistry_DefaultReadOnlyIncludesGitTools(t *testing.T) {
	must := require.New(t)
	exec, err := executor.NewLocalExecutor(t.TempDir())
	must.NoError(err, "new local executor")
	registry := buildToolRegistry(exec, edit.NewWholeFileStrategy(), types.ToolsConfig{
		BuiltIn: types.DefaultReadOnlyBuiltInTools(),
	}, nil)

	registered := make(map[string]bool)
	for _, def := range registry.List() {
		registered[def.Name] = true
	}

	for _, name := range []string{"git_status", "git_changed_files", "git_diff", "git_show"} {
		must.True(registered[name], "buildToolRegistry did not register %q for a default read-only tool list", name)
	}
}

// TestBuildToolRegistry_NoneExecutorRegistersOnlyUngatedTools checks that a
// none executor, whose capabilities are all false, drops every
// capability-gated tool from the registry while web_fetch, which is not
// gated, still registers. ValidateRunConfig rejects these tool names for a
// real run; this test calls buildToolRegistry directly to pin the gate itself.
func TestBuildToolRegistry_NoneExecutorRegistersOnlyUngatedTools(t *testing.T) {
	must := require.New(t)
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
		must.False(registered[name], "buildToolRegistry registered %q for the none executor, which has no capability to back it", name)
	}
	must.True(registered["web_fetch"], "web_fetch is not capability-gated")
}

// TestBuildToolRegistry_GitStatusExecutes checks that a git tool resolved
// from the live registry runs against a real repository, not only that its
// name is registered.
func TestBuildToolRegistry_GitStatusExecutes(t *testing.T) {
	must := require.New(t)
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	must.NoError(err, "eval symlinks")
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
		must.NoError(err, "git %v failed:\n%s", args, out)
	}
	runGitCmd("init", "-q")
	runGitCmd("config", "user.email", "test@example.com")
	runGitCmd("config", "user.name", "test")

	exec, err := executor.NewLocalExecutor(dir)
	must.NoError(err, "new local executor")
	registry := buildToolRegistry(exec, edit.NewWholeFileStrategy(), types.ToolsConfig{
		BuiltIn: types.DefaultReadOnlyBuiltInTools(),
	}, nil)

	gitStatus := registry.Resolve("git_status")
	must.NotNil(gitStatus, "git_status not resolvable from the live registry")
	res, err := gitStatus.StructuredHandler(context.Background(), json.RawMessage(`{}`))
	must.NoError(err, "git_status execution failed")
	must.Contains(res.Text, "working tree clean")
}

// TestBuildLoopWithTransport_AskUpstreamIncludesSpawnAgent asserts the
// post-registration refresh in BuildLoopWithTransport: spawn_agent must
// appear in the AskUpstreamPolicy approval set even though it is
// registered after the policy is built. Without the refresh, spawn_agent
// calls would bypass the control plane approval gate.
func TestBuildLoopWithTransport_AskUpstreamIncludesSpawnAgent(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err, "BuildLoopWithTransport()")
	defer func() { _ = loop.Close() }()

	ask, ok := permission.Unwrap(loop.Permissions).(*permission.AskUpstreamPolicy)
	must.True(ok, "expected AskUpstreamPolicy, got %T", loop.Permissions)

	must.Subset(ask.ApprovalToolNames(), []string{"write_file", "run_command", "web_fetch", "spawn_agent"})
}

func TestBuildLoopWithTransport_InvalidConfigReturnsError(t *testing.T) {
	must := require.New(t)
	_, err := BuildLoopWithTransport(context.Background(), &types.RunConfig{
		// Missing provider type — validation will fail.
		MaxTurns: 5,
	}, nil)
	must.ErrorContains(err, "config validation")
}

// TestFactory_ConfigValidationFailed_EmitsSecurityEvent asserts that the
// SecurityLogger constructed inside BuildLoopWithTransport emits a
// config_validation_failed event when ValidateRunConfig rejects the config.
// We capture os.Stderr (where the logger writes) for the duration of the
// call and assert the event reaches the buffer.
func TestFactory_ConfigValidationFailed_EmitsSecurityEvent(t *testing.T) {
	must := require.New(t)
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	must.NoError(err, "os.Pipe")
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

	must.ErrorContains(buildErr, "config validation")

	captured, readErr := io.ReadAll(r)
	must.NoError(readErr, "read captured stderr")
	must.Contains(string(captured), `"event":"config_validation_failed"`, "captured stderr")
}

func TestBuildLoopWithTransport_MinimalValidConfig(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err, "BuildLoopWithTransport()")
	defer func() { _ = loop.Close() }()

	must.NotNil(loop.Provider, "loop.Provider")
	must.NotNil(loop.Router, "loop.Router")
	must.NotNil(loop.Prompt, "loop.Prompt")
	must.NotNil(loop.Context, "loop.Context")
	must.NotNil(loop.Tools, "loop.Tools")
	must.NotNil(loop.Executor, "loop.Executor")
	must.NotNil(loop.Edit, "loop.Edit")
	must.NotNil(loop.Verifier, "loop.Verifier")
	must.NotNil(loop.Permissions, "loop.Permissions")
	must.NotNil(loop.Git, "loop.Git")
	must.NotNil(loop.Hooks, "loop.Hooks")
	_, isNoop := loop.Hooks.(*hook.Noop)
	must.True(isNoop, "expected hook.Noop for a config with no HooksConfig, got %T", loop.Hooks)
	must.NotNil(loop.Transport, "loop.Transport")
	must.NotNil(loop.Trace, "loop.Trace")
	must.NotNil(loop.Tracer, "loop.Tracer")
	must.NotNil(loop.Metrics, "loop.Metrics")
	must.NotNil(loop.Security, "loop.Security")
	must.NotNil(loop.Logger, "loop.Logger")
	must.NotNil(loop.ContextWindows, "loop.ContextWindows")
	got := loop.ContextWindows.Resolve(quirks.Target{ProviderType: "openai-compatible", Model: "gpt-4o"}).ContextWindow
	must.Equal(128_000, got, "gpt-4o context window from the model registry")
}

// TestBuildLoopWithTransport_HooksWiredAsExecRunner pins that a config
// carrying a non-empty HooksConfig (issue #461) wires an *hook.ExecRunner
// sharing the run's own Executor, rather than the Noop fallback.
func TestBuildLoopWithTransport_HooksWiredAsExecRunner(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	execRunner, ok := loop.Hooks.(*hook.ExecRunner)
	must.True(ok, "expected *hook.ExecRunner, got %T", loop.Hooks)
	must.Equal(loop.Executor, execRunner.Exec, "ExecRunner.Exec must be the run's own Executor")
	must.Same(config.Hooks, execRunner.Hooks, "ExecRunner.Hooks must be the config's own HooksConfig instance")
}

func TestBuildLoopWithTransport_HeartbeatIntervalFromConfig(t *testing.T) {
	for name, tc := range map[string]struct {
		seconds int
		want    time.Duration
	}{
		"unset selects the default": {0, 30 * time.Second},
		"configured value":          {45, 45 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			t.Setenv("TEST_OPENAI_KEY", "test-key")
			server := newOpenAIServer(t, nil, nil, nil)
			defer server.Close()

			timeout := 30
			config := &types.RunConfig{
				RunID:            "factory-heartbeat-test",
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
				HeartbeatSeconds: tc.seconds,
			}

			tp := transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{})
			loop, err := BuildLoopWithTransport(context.Background(), config, tp)
			must.NoError(err)
			defer func() { _ = loop.Close() }()

			must.Equal(tc.want, loop.HeartbeatInterval)
		})
	}
}

// An empty edit strategy type must resolve to "multi" across the whole factory
// path, not only in ValidateRunConfig or buildEditStrategy alone. The
// execution-mode code scanner wraps the strategy, so the test unwraps it.
func TestBuildLoopWithTransport_EmptyEditStrategyDefaultsToMulti(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	must.Equal("multi", config.EditStrategy.Type, "ValidateRunConfig should default EditStrategy.Type")

	got := loop.Edit
	if scanned, ok := got.(*edit.ScannedStrategy); ok {
		got = scanned.Inner()
	}
	must.IsType((*edit.MultiStrategy)(nil), got, "loop.Edit should resolve to *edit.MultiStrategy, possibly via ScannedStrategy")
}

func TestBuildLoopWithTransport_InjectedTransportSkipsBuild(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	must.Same(injected, loop.Transport, "injected transport should be used")
	must.False(loop.emitReady, "emitReady should be false when transport is injected")
}

func TestBuildLoopWithTransport_NoopMetricsWhenNotOTel(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	must.NotNil(loop.Metrics, "Metrics should be no-op instruments, not nil, when the trace emitter is jsonl")
}

func TestBuildLoopWithTransport_AllToolsRegisteredByDefault(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	// With no tools.builtIn filter, all tools should be registered.
	expectedTools := []string{"read_file", "list_directory", "grep_files", "find_files", "run_command", "write_file", "web_fetch", "spawn_agent"}
	for _, name := range expectedTools {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			must.NotNil(loop.Tools.Resolve(name), "tool should be registered")
		})
	}
}

// An unresolvable secret in the trace emitter headers must fail the factory
// with a "resolve trace emitter headers" error.
func TestBuildLoopWithTransport_TraceEmitterHeaderResolutionError(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	server := newOpenAIServer(t, nil, nil, nil)
	defer server.Close()

	// Ensure the referenced env var is *not* set so EnvSecretStore
	// returns a missing-secret error.
	must.NoError(os.Unsetenv("STIRRUP_TEST_MISSING_VAR"), "unset env")

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
	must.Error(err, "expected error when trace emitter header resolution fails")
	must.ErrorContains(err, "resolve trace emitter headers")
}

func TestBuildLoopWithTransport_SecretResolutionFailure(t *testing.T) {
	must := require.New(t)
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
	must.ErrorContains(err, "build providers")
}

// Research mode is read-only but must register web_fetch and let it pass the
// deny-side-effects permission policy.
func TestBuildLoopWithTransport_ResearchModeAllowsWebFetch(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	wf := loop.Tools.Resolve("web_fetch")
	must.NotNil(wf, "web_fetch should be registered in research mode")

	result, err := loop.Permissions.Check(context.Background(), wf.Definition(), nil)
	must.NoError(err, "permission check")
	must.True(result.Allowed, "research mode permission policy denied web_fetch: %q", result.Reason)
}

// Every read-only mode must register web_fetch and allow it through its
// permission policy.
func TestBuildLoopWithTransport_ReadOnlyModesAllowWebFetch(t *testing.T) {
	modes := []string{"research", "review", "toil", "planning"}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
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
			must.NoError(err)
			defer func() { _ = loop.Close() }()

			wf := loop.Tools.Resolve("web_fetch")
			must.NotNil(wf, "web_fetch should be registered")

			result, err := loop.Permissions.Check(context.Background(), wf.Definition(), nil)
			must.NoError(err, "permission check")
			must.True(result.Allowed, "permission policy denied web_fetch: %q", result.Reason)
		})
	}
}

func TestBuildProvider_OpenAIResponses(t *testing.T) {
	must := require.New(t)
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:      "openai-responses",
		APIKeyRef: "secret://OPENAI_KEY",
		BaseURL:   "https://api.openai.com/v1",
	}, secrets)
	must.NoError(err)
	_, ok := prov.(*provider.OpenAIResponsesAdapter)
	must.True(ok, "buildProvider type = %T, want *provider.OpenAIResponsesAdapter", prov)
}

// TestBuildProvider_OpenAIResponsesAzureFields checks that APIKeyHeader and
// QueryParams from the ProviderConfig reach the wire through buildProvider.
// The gRPC transport test in harness/internal/transport/grpc_test.go covers the
// proto passthrough for these fields.
func TestBuildProvider_OpenAIResponsesAzureFields(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Equal("AZURE-KEY", r.Header.Get("api-key"), "api-key header")
		check.Empty(r.Header.Get("Authorization"), "Authorization header")
		check.Equal("/openai/v1/responses", r.URL.Path, "URL.Path")
		check.Equal("preview", r.URL.Query().Get("api-version"), "api-version")
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
	must.NoError(err)

	ch, err := prov.Stream(context.Background(), types.StreamParams{Model: "gpt-4o", MaxTokens: 16})
	must.NoError(err, "Stream()")
	for range ch { //nolint:revive // drain stream so the goroutine completes
	}
}

// TestBuildProvider_OpenAICompatibleAzureFields is the Chat Completions
// counterpart of TestBuildProvider_OpenAIResponsesAzureFields.
func TestBuildProvider_OpenAICompatibleAzureFields(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		check.Equal("AZURE-KEY", r.Header.Get("api-key"), "api-key header")
		check.Empty(r.Header.Get("Authorization"), "Authorization header")
		check.Equal("/openai/v1/chat/completions", r.URL.Path, "URL.Path")
		check.Equal("preview", r.URL.Query().Get("api-version"), "api-version")
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
	must.NoError(err)

	ch, err := prov.Stream(context.Background(), types.StreamParams{Model: "gpt-4o", MaxTokens: 16})
	must.NoError(err, "Stream()")
	for range ch { //nolint:revive // drain stream so the goroutine completes
	}
}

// TestBuildProvider_Gemini checks that the gemini provider type builds a
// GeminiAdapter. The service-account key is found through
// GOOGLE_APPLICATION_CREDENTIALS by the implicit ADC credential source.
func TestBuildProvider_Gemini(t *testing.T) {
	must := require.New(t)
	keyPath := writeFakeServiceAccountJSON(t, t.TempDir())
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", keyPath)

	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:        "gemini",
		GCPProject:  "test-project",
		GCPLocation: "us-central1",
	}, &stubSecretStore{secrets: map[string]string{}})
	must.NoError(err)
	_, ok := prov.(*provider.GeminiAdapter)
	must.True(ok, "buildProvider type = %T, want *provider.GeminiAdapter", prov)
}

// TestBuildProvider_GeminiMissingCredentialsErrors checks that the gemini arm
// returns an error, not a partly built adapter, when no credential can be
// loaded. A GOOGLE_APPLICATION_CREDENTIALS path that does not exist makes ADC
// fail deterministically.
func TestBuildProvider_GeminiMissingCredentialsErrors(t *testing.T) {
	must := require.New(t)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", filepath.Join(t.TempDir(), "no-such-file.json"))

	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:        "gemini",
		GCPProject:  "test-project",
		GCPLocation: "us-central1",
	}, &stubSecretStore{secrets: map[string]string{}})
	must.Error(err, "credential source cannot produce a Google token")
	must.ErrorContains(err, "credential", "error should mention credentials")
}

// TestBuildProvider_OpenAICompatibleWithRetryConfig checks that the provider
// retry settings are copied onto the OpenAICompatibleAdapter's RetryPolicy.
func TestBuildProvider_OpenAICompatibleWithRetryConfig(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)
	adapter, ok := prov.(*provider.OpenAICompatibleAdapter)
	must.True(ok, "buildProvider type = %T, want *provider.OpenAICompatibleAdapter", prov)
	check.Equal(5, adapter.RetryPolicy.MaxAttempts, "RetryPolicy.MaxAttempts")
	check.Equal(200*time.Millisecond, adapter.RetryPolicy.InitialDelay, "RetryPolicy.InitialDelay")
	check.Equal(10*time.Second, adapter.RetryPolicy.MaxDelay, "RetryPolicy.MaxDelay")
	check.Equal(60*time.Second, adapter.RetryPolicy.WallClockBudget, "RetryPolicy.WallClockBudget")
}

// TestBuildProvider_AnthropicWithRetryConfig checks that the provider retry
// settings reach the default anthropic adapter, not only openai-compatible.
func TestBuildProvider_AnthropicWithRetryConfig(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.NoError(err)
	adapter, ok := prov.(*provider.AnthropicAdapter)
	must.True(ok, "buildProvider type = %T, want *provider.AnthropicAdapter", prov)
	check.Equal(5, adapter.RetryPolicy.MaxAttempts, "RetryPolicy.MaxAttempts")
	check.Equal(200*time.Millisecond, adapter.RetryPolicy.InitialDelay, "RetryPolicy.InitialDelay")
	check.Equal(10*time.Second, adapter.RetryPolicy.MaxDelay, "RetryPolicy.MaxDelay")
	check.Equal(60*time.Second, adapter.RetryPolicy.WallClockBudget, "RetryPolicy.WallClockBudget")
}

// The retry settings in ProviderConfig.Retry reach the RetryPolicy of the
// adapter built for each provider type.
func TestBuildProvider_GeminiWithRetryConfig(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	adapter, ok := prov.(*provider.GeminiAdapter)
	must.True(ok, "buildProvider type = %T, want *provider.GeminiAdapter", prov)
	must.Equal(5, adapter.RetryPolicy.MaxAttempts)
}

func TestBuildProvider_OpenAIResponsesWithRetryConfig(t *testing.T) {
	must := require.New(t)
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:      "openai-responses",
		APIKeyRef: "secret://OPENAI_KEY",
		Retry: &types.ProviderRetryConfig{
			MaxAttempts: 5,
			MaxDelayMs:  10000,
		},
	}, secrets)
	must.NoError(err)
	adapter, ok := prov.(*provider.OpenAIResponsesAdapter)
	must.True(ok, "buildProvider type = %T, want *provider.OpenAIResponsesAdapter", prov)
	must.Equal(5, adapter.RetryPolicy.MaxAttempts)
}

func TestBuildProvider_OpenAIAdaptersCarryTheEffectiveEndpointProfile(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  types.ProviderConfig
		want string
	}{
		{"responses default endpoint", types.ProviderConfig{Type: "openai-responses"}, types.EndpointProfileOpenAI},
		{"responses derived generic", types.ProviderConfig{Type: "openai-responses", BaseURL: "https://gateway.example.com/v1"}, types.EndpointProfileGeneric},
		{"responses declared openai relay", types.ProviderConfig{Type: "openai-responses", BaseURL: "https://relay.example.com/v1", EndpointProfile: types.EndpointProfileOpenAI}, types.EndpointProfileOpenAI},
		{"compatible declared azure", types.ProviderConfig{Type: "openai-compatible", BaseURL: "https://example.openai.azure.com/openai/v1", EndpointProfile: types.EndpointProfileAzureOpenAI}, types.EndpointProfileAzureOpenAI},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
			cfg := tc.cfg
			cfg.APIKeyRef = "secret://OPENAI_KEY"
			prov, err := buildProvider(context.Background(), cfg, secrets)
			must.NoError(err)
			switch adapter := prov.(type) {
			case *provider.OpenAIResponsesAdapter:
				must.Equal(tc.want, adapter.EndpointProfile)
			case *provider.OpenAICompatibleAdapter:
				must.Equal(tc.want, adapter.EndpointProfile)
			default:
				must.Failf("unexpected adapter", "buildProvider type = %T", prov)
			}
		})
	}
}

// The factory wires the Z.ai compat rules into the adapter's registry, so GLM
// model IDs resolve through them.
func TestBuildProvider_OpenAICompatibleWithCompatProfile_ZAI(t *testing.T) {
	must := require.New(t)
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:          "openai-compatible",
		APIKeyRef:     "secret://OPENAI_KEY",
		BaseURL:       "https://api.z.ai/api/coding/paas/v4",
		CompatProfile: "zai-glm",
	}, secrets)
	must.NoError(err)
	adapter, ok := prov.(*provider.OpenAICompatibleAdapter)
	must.True(ok, "buildProvider type = %T, want *provider.OpenAICompatibleAdapter", prov)
	must.NotNil(adapter.Registry, "factory should inject a registry containing the Z.ai compat rule")

	t.Run("glm-4-plus maps to max_tokens and tool_stream", func(t *testing.T) {
		must := require.New(t)
		q := adapter.Registry.Resolve(quirks.Target{ProviderType: "openai-compatible", Model: "glm-4-plus"})
		must.Equal(quirks.TokenFieldMaxTokens, q.BehaviourFlags.OpenAI.TokenField,
			"Z.ai compat rule did not fire; check resolveCompatProfile dispatch")
		must.Contains(q.BehaviourFlags.OpenAI.ExtraBodyFields, "tool_stream")
		must.Equal(true, q.BehaviourFlags.OpenAI.ExtraBodyFields["tool_stream"])
	})

	// The thinking-family assertions prove the factory keeps the full
	// CompatRules() slice, not only the base rule.
	t.Run("glm-4.7 enables the thinking family", func(t *testing.T) {
		must := require.New(t)
		q := adapter.Registry.Resolve(quirks.Target{ProviderType: "openai-compatible", Model: "glm-4.7"})
		must.Contains(q.ReplayFields, "reasoning_content",
			"thinking-family rule did not fire via factory")
		must.Contains(q.BehaviourFlags.OpenAI.ExtraBodyFields, "thinking")
		must.Equal(map[string]any{"type": "enabled"}, q.BehaviourFlags.OpenAI.ExtraBodyFields["thinking"])
	})
}

// An unknown compatProfile fails the build even when RunConfig validation is
// bypassed.
func TestBuildProvider_OpenAICompatibleWithUnknownCompatProfile_Errors(t *testing.T) {
	must := require.New(t)
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:          "openai-compatible",
		APIKeyRef:     "secret://OPENAI_KEY",
		BaseURL:       "https://api.example.com/v1",
		CompatProfile: "not-a-real-profile",
	}, secrets)
	must.ErrorContains(err, "not-a-real-profile")
}

func TestWarnCedarWithAliasedProfile_WarnsOnlyForCedarWithAliasingProfile(t *testing.T) {
	tests := []struct {
		name     string
		policy   string
		profile  string
		wantWarn bool
	}{
		{"cedar with coding-classic", "policy-engine", "coding-classic", true},
		{"cedar with the empty profile", "policy-engine", "", false},
		{"cedar with the default profile", "policy-engine", "default", false},
		{"allow-all with coding-classic", "allow-all", "coding-classic", false},
		{"deny-side-effects with coding-classic", "deny-side-effects", "coding-classic", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			check := assert.New(t)
			var logs bytes.Buffer
			config := &types.RunConfig{
				PermissionPolicy: types.PermissionPolicyConfig{Type: tc.policy},
				Tools:            types.ToolsConfig{Profile: tc.profile},
			}
			warnCedarWithAliasedProfile(config, slog.New(slog.NewTextHandler(&logs, nil)))
			out := logs.String()
			if !tc.wantWarn {
				must.Empty(out)
				return
			}
			must.Equal(1, strings.Count(out, "level=WARN"), out)
			check.Contains(out, "internal tool IDs")
			check.Contains(out, "profile=coding-classic")
		})
	}
}

func TestResolveCompatProfile_ResolvesEveryValidProfile(t *testing.T) {
	for _, name := range types.ValidCompatProfileValues() {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			_, err := resolveCompatProfile(name)
			must.NoError(err)
		})
	}
}

func TestResolveCompatProfile_UnknownNamesSupportedProfiles(t *testing.T) {
	must := require.New(t)
	_, err := resolveCompatProfile("not-a-real-profile")
	must.Error(err)
	must.ErrorContains(err, "not-a-real-profile")
	for _, name := range types.ValidCompatProfileValues() {
		must.ErrorContains(err, name)
	}
}

// BuildLoopWithTransport injects the scrub-backed logger into the
// OpenAI-compatible adapter, so retry warnings pass through secret scrubbing.
func TestBuildLoopWithTransport_OpenAICompatibleAdapterHasLogger(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	inner := unwrapNormalizer(loop.Provider)
	adapter, ok := inner.(*provider.OpenAICompatibleAdapter)
	must.True(ok, "loop.Provider (after unwrap) type = %T, want *provider.OpenAICompatibleAdapter", inner)
	must.NotNil(adapter.Logger, "factory should inject the scrub-backed logger")
}

// writeFakeServiceAccountJSON produces a service-account JSON keyfile
// good enough to pass google.JWTConfigFromJSON's static parse. It is not
// signable against real Google IAM, which is fine for factory tests that
// never call Token().
func writeFakeServiceAccountJSON(t *testing.T, dir string) string {
	t.Helper()
	must := require.New(t)
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
	must.NoError(err, "marshal sa json")
	path := filepath.Join(dir, "key.json")
	must.NoError(os.WriteFile(path, data, 0o600), "write sa json")
	return path
}

// The factory builds an OpenAI-compatible adapter from a file-backed Azure
// workload identity without contacting Entra. The bearer closure only
// reaches Entra on the first BearerToken call.
func TestBuildProvider_OpenAICompatibleAzureWIF(t *testing.T) {
	must := require.New(t)
	tokenPath := filepath.Join(t.TempDir(), "azure-identity-token")
	must.NoError(os.WriteFile(tokenPath, []byte("eyJ.fake.jwt"), 0o600))

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
	must.NoError(err)
	_, ok := prov.(*provider.OpenAICompatibleAdapter)
	must.True(ok, "buildProvider type = %T, want *provider.OpenAICompatibleAdapter", prov)
}

// TestBuildProvider_OpenAIResponsesAzureWIF mirrors
// TestBuildProvider_OpenAICompatibleAzureWIF for the Responses adapter.
// Both adapters share the bearer-closure plumbing in factory.go::buildProvider.
func TestBuildProvider_OpenAIResponsesAzureWIF(t *testing.T) {
	must := require.New(t)
	tokenPath := filepath.Join(t.TempDir(), "azure-identity-token")
	must.NoError(os.WriteFile(tokenPath, []byte("eyJ.fake.jwt"), 0o600))

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
	must.NoError(err)
	_, ok := prov.(*provider.OpenAIResponsesAdapter)
	must.True(ok, "buildProvider type = %T, want *provider.OpenAIResponsesAdapter", prov)
}

func TestBuildProvider_OpenAICompatibleWithAPIKey(t *testing.T) {
	must := require.New(t)
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:      "openai-compatible",
		APIKeyRef: "secret://OPENAI_KEY",
	}, secrets)
	must.NoError(err)
	_, ok := prov.(*provider.OpenAICompatibleAdapter)
	must.True(ok, "buildProvider type = %T, want *provider.OpenAICompatibleAdapter", prov)
}

func TestBuildProvider_UnknownTypeMentionsResponses(t *testing.T) {
	must := require.New(t)
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:      "nonsense",
		APIKeyRef: "secret://OPENAI_KEY",
	}, secrets)
	must.ErrorContains(err, "openai-responses")
}

// resourceOptionsFromConfig maps a nil config, an empty observability block,
// and a populated observability block. The nil case matters for callers
// outside BuildLoopWithTransport, such as eval runners.
func TestResourceOptionsFromConfig_MapsObservabilityAndMode(t *testing.T) {
	t.Run("nil config", func(t *testing.T) {
		must := require.New(t)
		got := resourceOptionsFromConfig(nil)
		must.Equal(observability.ResourceOptions{}, got)
	})

	t.Run("empty observability with mode", func(t *testing.T) {
		must := require.New(t)
		got := resourceOptionsFromConfig(&types.RunConfig{Mode: "execution"})
		must.Equal(observability.ResourceOptions{RunMode: "execution"}, got)
	})

	t.Run("populated observability and mode", func(t *testing.T) {
		must := require.New(t)
		got := resourceOptionsFromConfig(&types.RunConfig{
			Mode: "planning",
			Observability: types.ObservabilityConfig{
				ServiceName:      "agent-a",
				Environment:      "prod",
				ServiceNamespace: "eval",
			},
		})
		must.Equal(observability.ResourceOptions{
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
	must := require.New(t)
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:       "anthropic",
		Credential: &types.CredentialConfig{Type: "aws-default"},
	}, &stubSecretStore{secrets: map[string]string{}})
	must.ErrorContains(err, "bearer credential")
}

// TestBuildProvider_OpenAICompatibleNilBearerErrors mirrors the
// anthropic case for the openai-compatible arm.
func TestBuildProvider_OpenAICompatibleNilBearerErrors(t *testing.T) {
	must := require.New(t)
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:       "openai-compatible",
		Credential: &types.CredentialConfig{Type: "aws-default"},
	}, &stubSecretStore{secrets: map[string]string{}})
	must.ErrorContains(err, "bearer credential")
}

// The factory routes an anthropic-wif credential through credential.BuildSource
// and into NewAnthropicAdapter. The OAuth exchange itself is covered by the
// credential package's tests.
func TestBuildProvider_AnthropicWIF(t *testing.T) {
	must := require.New(t)
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "jwt")
	must.NoError(os.WriteFile(tokenPath, []byte("eyJ.fake.jwt"), 0o600))

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
	must.NoError(err)
	adapter, ok := prov.(*provider.AnthropicAdapter)
	must.True(ok, "buildProvider type = %T, want *provider.AnthropicAdapter", prov)
	// The adapter must send the WIF access token as Authorization: Bearer;
	// x-api-key gets a 401 from Anthropic.
	must.Equal(provider.AuthModeBearer, adapter.AuthMode())
}

// The static API key path uses x-api-key rather than a Bearer header.
func TestBuildProvider_AnthropicStaticKeyUsesAPIKeyMode(t *testing.T) {
	must := require.New(t)
	prov, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:      "anthropic",
		APIKeyRef: "secret://ANTHROPIC_API_KEY",
	}, &stubSecretStore{secrets: map[string]string{"secret://ANTHROPIC_API_KEY": "sk-ant-api03-fake"}})
	must.NoError(err)
	adapter, ok := prov.(*provider.AnthropicAdapter)
	must.True(ok, "buildProvider type = %T, want *provider.AnthropicAdapter", prov)
	must.Equal(provider.AuthModeAPIKey, adapter.AuthMode())
}

// credential.BuildSource rejects an anthropic-wif credential without a
// tokenSource, even when RunConfig validation is bypassed.
func TestBuildProvider_AnthropicWIFMissingTokenSourceErrors(t *testing.T) {
	must := require.New(t)
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type: "anthropic",
		Credential: &types.CredentialConfig{
			Type:             "anthropic-wif",
			FederationRuleID: "fdrl_example",
			OrganizationID:   "550e8400-e29b-41d4-a716-446655440000",
			ServiceAccountID: "svac_example",
		},
	}, &stubSecretStore{secrets: map[string]string{}})
	must.ErrorContains(err, "tokenSource")
}

// TestBuildProvider_OpenAIResponsesNilBearerErrors mirrors the
// anthropic case for the openai-responses arm.
func TestBuildProvider_OpenAIResponsesNilBearerErrors(t *testing.T) {
	must := require.New(t)
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:       "openai-responses",
		Credential: &types.CredentialConfig{Type: "aws-default"},
	}, &stubSecretStore{secrets: map[string]string{}})
	must.ErrorContains(err, "bearer credential")
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
	must := require.New(t)
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
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	inner := unwrapNormalizer(loop.Provider)
	ba, ok := inner.(*provider.BatchAdapter)
	must.True(ok, "loop.Provider (after unwrap) type = %T, want *provider.BatchAdapter", inner)
	mapped, ok := loop.Providers[config.Provider.Type]
	must.True(ok, "loop.Providers[%q] missing", config.Provider.Type)
	must.Same(ba, unwrapNormalizer(mapped))
}

// With harness-side polling on stdio, the provider is wrapped in a
// *BatchAdapter. The BatchClient concrete type is unexported, so only the
// outer wrapper is asserted. The providers map must match the top-level
// provider so model-router lookups do not bypass batching.
func TestBuildLoopWithTransport_BatchAdapterWiredOnStdio(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	inner := unwrapNormalizer(loop.Provider)
	ba, ok := inner.(*provider.BatchAdapter)
	must.True(ok, "loop.Provider (after unwrap) type = %T, want *provider.BatchAdapter", inner)
	mapped := loop.Providers[config.Provider.Type]
	must.Same(ba, unwrapNormalizer(mapped))
}

// The stdio batch path accepts openai-compatible and openai-responses
// provider types, not only anthropic.
func TestBuildLoopWithTransport_BatchOnStdioAcceptsOpenAI(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")

	for _, provType := range []string{"openai-compatible", "openai-responses"} {
		t.Run(provType, func(t *testing.T) {
			must := require.New(t)
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
			must.NoError(err)
			must.NotNil(loop)

			// The outer NormalizingAdapter wraps the BatchAdapter. The providers
			// map entry must be that same BatchAdapter so that routing by
			// provider type goes through batching.
			ba, ok := unwrapNormalizer(loop.Provider).(*provider.BatchAdapter)
			must.True(ok, "loop.Provider after unwrap is %T, want *provider.BatchAdapter", unwrapNormalizer(loop.Provider))
			must.Contains(loop.Providers, config.Provider.Type)
			must.Same(ba, unwrapNormalizer(loop.Providers[config.Provider.Type]))
		})
	}
}

// hostedSearchCapture records every request body sent to a fake OpenAI
// API and fails each request, so a stream or batch submit ends after
// sending its body.
type hostedSearchCapture struct {
	mu     sync.Mutex
	bodies map[string][]string
}

func newHostedSearchCapture(t *testing.T) (*hostedSearchCapture, string) {
	t.Helper()
	c := &hostedSearchCapture{bodies: map[string][]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies[r.URL.Path] = append(c.bodies[r.URL.Path], string(body))
		c.mu.Unlock()
		http.Error(w, `{"error":{"message":"captured"}}`, http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)
	return c, srv.URL + "/v1"
}

func (c *hostedSearchCapture) last(path string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b := c.bodies[path]; len(b) > 0 {
		return b[len(b)-1]
	}
	return ""
}

func hostedSearchStreamParams() types.StreamParams {
	return types.StreamParams{
		Model:      "gpt-5.5",
		MaxTokens:  256,
		ToolSearch: true,
		Tools: []types.ToolDefinition{
			{Name: "read_file", Description: "Read a file", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)},
			{Name: "mcp_jira_create_issue", Description: "Create a Jira issue", InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`), Deferred: true},
		},
		Messages: []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "file a ticket"}}}},
	}
}

// streamOnce sends one request through p. The capture server fails every
// request, so the error, returned or streamed, is expected and ignored.
func streamOnce(t *testing.T, p provider.ProviderAdapter, params types.StreamParams) {
	t.Helper()
	ch, err := p.Stream(t.Context(), params)
	if err != nil {
		return
	}
	for range ch {
	}
}

// buildResponsesLoopForCapture builds a loop for an openai-responses
// provider at baseURL, batched over harness-side polling when batch is set.
func buildResponsesLoopForCapture(t *testing.T, baseURL, endpointProfile string, batch bool) *AgenticLoop {
	t.Helper()
	must := require.New(t)
	timeout := 30
	config := &types.RunConfig{
		RunID:            "factory-test-hosted-search",
		Mode:             "planning",
		Prompt:           "hello",
		Provider:         types.ProviderConfig{Type: "openai-responses", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: baseURL, EndpointProfile: endpointProfile},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-responses", Model: "gpt-5.5"},
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
	if batch {
		config.Provider.Batch = &types.BatchProviderConfig{
			Enabled:               true,
			MaxWaitSeconds:        intPtr(30),
			HarnessSidePolling:    true,
			AllowInteractiveModes: true,
		}
	}
	loop, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
	must.NoError(err)
	t.Cleanup(func() { _ = loop.Close() })
	return loop
}

func TestBuildLoopWithTransport_BatchHostedSearchMatchesStreaming(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")
	const hostedEntry = `{"type":"tool_search"}`
	for _, tc := range []struct {
		name            string
		endpointProfile string
		wantHosted      bool
	}{
		{"declared openai on a custom baseUrl", types.EndpointProfileOpenAI, true},
		{"derived generic", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			capture, baseURL := newHostedSearchCapture(t)

			streaming := buildResponsesLoopForCapture(t, baseURL, tc.endpointProfile, false)
			streamOnce(t, unwrapNormalizer(streaming.Provider), hostedSearchStreamParams())
			streamed := capture.last("/v1/responses")
			must.NotEmpty(streamed, "the streaming adapter sent no request")

			batched := buildResponsesLoopForCapture(t, baseURL, tc.endpointProfile, true)
			ba, ok := unwrapNormalizer(batched.Provider).(*provider.BatchAdapter)
			must.True(ok, "loop.Provider after unwrap is %T, want *provider.BatchAdapter", unwrapNormalizer(batched.Provider))
			streamOnce(t, ba, hostedSearchStreamParams())
			uploaded := capture.last("/v1/files")
			must.NotEmpty(uploaded, "the batch adapter uploaded no input file")

			must.Equal(tc.wantHosted, strings.Contains(streamed, hostedEntry), "streaming body:\n%s", streamed)
			must.Equal(tc.wantHosted, strings.Contains(uploaded, hostedEntry), "batch input file:\n%s", uploaded)
		})
	}
}

// The provider a Gemini loop calls is the NormalizingAdapter wrapping a
// GeminiAdapter, and the providers map returns that same wrapper.
func TestBuildLoopWithTransport_NormalizingAdapterWrapsGeminiProvider(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	// The loop itself must hold the NormalizingAdapter, not merely have one
	// somewhere in its chain.
	n, ok := loop.Provider.(*provider.NormalizingAdapter)
	must.True(ok, "loop.Provider is %T, want outermost *provider.NormalizingAdapter", loop.Provider)
	must.IsType((*provider.GeminiAdapter)(nil), n.Unwrap())
	must.Contains(loop.Providers, config.Provider.Type)
	must.Same(loop.Provider, loop.Providers[config.Provider.Type])
}

// The provider a Bedrock loop calls is the NormalizingAdapter wrapping a
// BedrockAdapter. The AWS SDK client is built lazily, so the build does
// not need real AWS credentials.
func TestBuildLoopWithTransport_NormalizingAdapterWrapsBedrockProvider(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	n, ok := loop.Provider.(*provider.NormalizingAdapter)
	must.True(ok, "loop.Provider is %T, want outermost *provider.NormalizingAdapter", loop.Provider)
	must.IsType((*provider.BedrockAdapter)(nil), n.Unwrap())
	must.Contains(loop.Providers, config.Provider.Type)
	must.Same(loop.Provider, loop.Providers[config.Provider.Type])
}

// BuildLoopWithTransport must inject a non-nil Logger into the
// BedrockAdapter, so the tool-choice downgrade warning goes through the
// scrubbing logger that carries run and trace correlation.
func TestBuildLoopWithTransport_BedrockAdapterHasLogger(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	adapter, ok := unwrapNormalizer(loop.Provider).(*provider.BedrockAdapter)
	must.True(ok, "loop.Provider after unwrap is %T, want *provider.BedrockAdapter", unwrapNormalizer(loop.Provider))
	must.NotNil(adapter.Logger, "factory must inject the ScrubHandler-backed logger")
}

// BuildLoopWithTransport must inject a non-nil Logger into the
// AnthropicAdapter, so the warning for caller temperature suppressed by
// model quirks goes through the scrubbing logger that carries run and
// trace correlation.
func TestBuildLoopWithTransport_AnthropicAdapterHasLogger(t *testing.T) {
	must := require.New(t)
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
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	adapter, ok := unwrapNormalizer(loop.Provider).(*provider.AnthropicAdapter)
	must.True(ok, "loop.Provider after unwrap is %T, want *provider.AnthropicAdapter", unwrapNormalizer(loop.Provider))
	must.NotNil(adapter.Logger, "factory must inject the ScrubHandler-backed logger")
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
	must := require.New(t)
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
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	adapter, ok := unwrapNormalizer(loop.Provider).(*provider.OpenAIResponsesAdapter)
	must.True(ok, "loop.Provider after unwrap is %T, want *provider.OpenAIResponsesAdapter", unwrapNormalizer(loop.Provider))
	must.NotNil(adapter.Logger, "factory must inject the ScrubHandler-backed logger")
}

func TestBuildToolRegistry_CommandOutputReaderFollowsStore(t *testing.T) {
	must := require.New(t)
	store, err := commandoutput.New(commandoutput.Options{RunID: "registry-test"})
	must.NoError(err)
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
			must := require.New(t)
			registry := buildToolRegistry(execAll, edit.NewWholeFileStrategy(), types.ToolsConfig{BuiltIn: tc.builtIn}, tc.store)
			must.Equal(tc.wantRun, registry.Resolve("run_command") != nil, "run_command registration")
			must.Equal(tc.wantReader, registry.Resolve("read_command_output") != nil, "read_command_output registration")
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
			must := require.New(t)
			cfg := &types.RunConfig{RunID: "gating-test"}
			tc.mutate(cfg)
			store, err := buildCommandOutputStore(context.Background(), cfg)
			must.NoError(err)
			if store != nil {
				defer func() { _ = store.Close() }()
			}
			if tc.wantNil {
				must.Nil(store)
			} else {
				must.NotNil(store)
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
		must := require.New(t)
		life := sandboxLifecycle(&types.RunConfig{RunID: "run-1", Timeout: timeout(600)}, 0)
		must.Equal(10*time.Minute+executor.SandboxGCMargin, life.MaxLifetime)
		must.Equal("run-1", life.RunID)
	})

	t.Run("longer session budget wins", func(t *testing.T) {
		must := require.New(t)
		life := sandboxLifecycle(&types.RunConfig{Timeout: timeout(600)}, 5*time.Hour)
		must.Equal(5*time.Hour+executor.SandboxGCMargin, life.MaxLifetime)
	})

	t.Run("budget shorter than the timeout is raised to it", func(t *testing.T) {
		must := require.New(t)
		life := sandboxLifecycle(&types.RunConfig{Timeout: timeout(600)}, time.Minute)
		must.Equal(10*time.Minute+executor.SandboxGCMargin, life.MaxLifetime)
	})

	t.Run("no timeout and no budget stays unbounded", func(t *testing.T) {
		must := require.New(t)
		life := sandboxLifecycle(&types.RunConfig{}, 0)
		must.Zero(life.MaxLifetime)
	})

	t.Run("postRun hook budget extends the run timeout", func(t *testing.T) {
		must := require.New(t)
		cfg := &types.RunConfig{Timeout: timeout(600), Hooks: &types.HooksConfig{PostRun: []types.HookConfig{
			{Command: "true", TimeoutSeconds: 1800},
			{Command: "true", TimeoutSeconds: 60},
		}}}
		want := 10*time.Minute + postHookBudget(cfg.Hooks) + executor.SandboxGCMargin
		must.Equal(want, sandboxLifecycle(cfg, 0).MaxLifetime)
	})

	t.Run("postRun hook budget extends the session budget", func(t *testing.T) {
		must := require.New(t)
		cfg := &types.RunConfig{Timeout: timeout(600), Hooks: &types.HooksConfig{PostRun: []types.HookConfig{
			{Command: "true", TimeoutSeconds: 300},
		}}}
		want := 5*time.Hour + (300+30)*time.Second + executor.SandboxGCMargin
		must.Equal(want, sandboxLifecycle(cfg, 5*time.Hour).MaxLifetime)
	})

	t.Run("hooks without postRun entries leave the span unchanged", func(t *testing.T) {
		must := require.New(t)
		cfg := &types.RunConfig{Timeout: timeout(600), Hooks: &types.HooksConfig{PreRun: []types.HookConfig{
			{Command: "true", TimeoutSeconds: 900},
		}}}
		must.Equal(10*time.Minute+executor.SandboxGCMargin, sandboxLifecycle(cfg, 0).MaxLifetime)
	})
}

// An empty apiKeyRef reaches buildProvider from --api-key-ref="", from a
// static credential block that names no key, and from the gRPC path, which
// applies no CLI key default.
func TestBuildProvider_EmptyAPIKeyRef_RejectedForFixedOrDefaultEndpoint(t *testing.T) {
	static := &types.CredentialConfig{Type: "static"}
	cases := []struct {
		name string
		cfg  types.ProviderConfig
	}{
		{"anthropic without a credential block", types.ProviderConfig{Type: "anthropic"}},
		{"anthropic with a static credential block", types.ProviderConfig{Type: "anthropic", Credential: static}},
		{"openai-compatible on the default endpoint", types.ProviderConfig{Type: "openai-compatible"}},
		{"openai-responses on the explicit default endpoint", types.ProviderConfig{Type: "openai-responses", BaseURL: "https://api.openai.com/v1"}},
		{"openai-responses with a static credential block", types.ProviderConfig{Type: "openai-responses", Credential: static}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := buildProvider(context.Background(), tc.cfg, &stubSecretStore{secrets: map[string]string{}})
			must.ErrorContains(err, "provider "+tc.cfg.Type+" requires apiKeyRef")
		})
	}
}

func TestBuildProvider_EmptyAPIKeyRef_AllowedForCustomOpenAIEndpoint(t *testing.T) {
	cases := []types.ProviderConfig{
		{Type: "openai-compatible", BaseURL: "http://localhost:1234/v1"},
		{Type: "openai-responses", BaseURL: "http://localhost:1234/v1", Credential: &types.CredentialConfig{Type: "static"}},
	}
	for _, cfg := range cases {
		t.Run(cfg.Type, func(t *testing.T) {
			must := require.New(t)
			prov, err := buildProvider(context.Background(), cfg, &stubSecretStore{secrets: map[string]string{}})
			must.NoError(err)
			must.NotNil(prov)
		})
	}
}

func TestBuildProviders_EmptyAPIKeyRef_RejectedInProvidersMap(t *testing.T) {
	must := require.New(t)
	secrets := &stubSecretStore{secrets: map[string]string{"secret://OPENAI_KEY": "sk-test"}}
	_, _, err := buildProviders(context.Background(), &types.RunConfig{
		Provider:  types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://OPENAI_KEY"},
		Providers: map[string]types.ProviderConfig{"claude": {Type: "anthropic"}},
	}, secrets)
	must.ErrorContains(err, `build provider "claude"`)
	must.ErrorContains(err, "provider anthropic requires apiKeyRef")
}

// writeReplayRecording writes a JSONL trace holding the given turn_record
// lines and returns its path.
func writeReplayRecording(t *testing.T, lines ...string) string {
	t.Helper()
	must := require.New(t)
	path := filepath.Join(t.TempDir(), "recording.jsonl")
	must.NoError(os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	return path
}

const (
	replayToolTurn = `{"kind":"turn_record","turn":1,"modelOutput":[{"type":"tool_use","id":"c1","name":"read_file","input":{"path":"a.txt"}}]}`
	replayTextTurn = `{"kind":"turn_record","turn":2,"modelOutput":[{"type":"text","text":"done"}]}`
)

// TestBuildProvider_Replay_RejectsIncompleteRecording pins that a recording
// the reader would have to drop a line from is refused outright, since a
// dropped line would shift every later turn onto the wrong request.
func TestBuildProvider_Replay_RejectsIncompleteRecording(t *testing.T) {
	oversized := `{"kind":"turn_record","turn":2,"modelOutput":[{"type":"text","text":"` + strings.Repeat("x", 4*1024*1024) + `"}]}`
	cases := []struct {
		name      string
		lines     []string
		errSubstr string
	}{
		{name: "malformed middle line", lines: []string{replayToolTurn, `{"kind":"turn_record",`, replayTextTurn}, errSubstr: "1 malformed line(s)"},
		{name: "oversized middle line", lines: []string{replayToolTurn, oversized, replayTextTurn}, errSubstr: "line(s) over"},
		{name: "only malformed lines", lines: []string{"not json", "{"}, errSubstr: "malformed line(s)"},
		{name: "only sub-agent turns", lines: []string{`{"kind":"turn_record","turn":1,"runId":"child","parentRunId":"parent","modelOutput":[{"type":"text","text":"x"}]}`}, errSubstr: "holds only sub-agent turns"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			path := writeReplayRecording(t, tc.lines...)
			_, err := buildProvider(context.Background(), types.ProviderConfig{Type: "replay", ReplayFile: path}, &stubSecretStore{})
			must.ErrorContains(err, tc.errSubstr)
		})
	}
}

// TestBuildProvider_Replay_RejectsNonRegularOrOversizedFile pins that the
// loader returns promptly with an error for a path that is not a bounded
// regular file, since the path can arrive from a control plane.
func TestBuildProvider_Replay_RejectsNonRegularOrOversizedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFOs and /dev/null are POSIX")
	}
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	require.New(t).NoError(syscall.Mkfifo(fifo, 0o600))
	huge := filepath.Join(dir, "huge.jsonl")
	require.New(t).NoError(os.WriteFile(huge, nil, 0o600))
	require.New(t).NoError(os.Truncate(huge, maxReplayFileBytes+1))

	cases := []struct {
		name      string
		path      string
		errSubstr string
	}{
		{name: "directory", path: dir, errSubstr: "not a regular file"},
		{name: "fifo", path: fifo, errSubstr: "not a regular file"},
		{name: "character device", path: "/dev/null", errSubstr: "not a regular file"},
		{name: "over the size cap", path: huge, errSubstr: "the cap is"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			done := make(chan error, 1)
			go func() {
				_, err := buildProvider(context.Background(), types.ProviderConfig{Type: "replay", ReplayFile: tc.path}, &stubSecretStore{})
				done <- err
			}()
			select {
			case err := <-done:
				must.ErrorContains(err, tc.errSubstr)
			case <-time.After(5 * time.Second):
				must.FailNow("buildProvider did not return for " + tc.name)
			}
		})
	}
}

// TestBuildProviders_Replay_NamedEntryServed pins that a replay entry in
// the providers map is built like the default one and routable by name.
func TestBuildProviders_Replay_NamedEntryServed(t *testing.T) {
	must := require.New(t)
	path := writeReplayRecording(t, replayToolTurn, replayTextTurn)
	cfg := &types.RunConfig{
		Provider:  types.ProviderConfig{Type: "replay", ReplayFile: path},
		Providers: map[string]types.ProviderConfig{"recorded": {Type: "replay", ReplayFile: path}},
	}
	_, providers, err := buildProviders(context.Background(), cfg, &stubSecretStore{})
	must.NoError(err)
	named, ok := providers["recorded"].(*provider.ReplayProvider)
	must.True(ok, "providers[recorded] type = %T", providers["recorded"])
	must.Equal(2, named.Len())
}

func TestBuildProvider_Replay_ServesRecordedTurns(t *testing.T) {
	must := require.New(t)
	path := writeReplayRecording(t,
		`{"kind":"turn_record","turn":1,"modelOutput":[{"type":"tool_use","id":"c1","name":"read_file","input":{"path":"a.txt"}}]}`,
		`{"kind":"turn_record","turn":2,"modelOutput":[{"type":"text","text":"done"}]}`,
	)
	prov, err := buildProvider(context.Background(), types.ProviderConfig{Type: "replay", ReplayFile: path}, &stubSecretStore{})
	must.NoError(err)
	replay, ok := prov.(*provider.ReplayProvider)
	must.True(ok, "buildProvider type = %T, want *provider.ReplayProvider", prov)
	must.Equal(2, replay.Len())
}

func TestBuildProvider_Replay_RejectsMissingFile(t *testing.T) {
	must := require.New(t)
	_, err := buildProvider(context.Background(), types.ProviderConfig{
		Type:       "replay",
		ReplayFile: filepath.Join(t.TempDir(), "absent.jsonl"),
	}, &stubSecretStore{})
	must.ErrorContains(err, "open replay file")
}

func TestBuildProvider_Replay_RejectsRecordingWithoutTurns(t *testing.T) {
	must := require.New(t)
	path := writeReplayRecording(t, `{"kind":"run_started","runId":"r1"}`)
	_, err := buildProvider(context.Background(), types.ProviderConfig{Type: "replay", ReplayFile: path}, &stubSecretStore{})
	must.ErrorContains(err, "holds no turn_record event")
}

// TestBuildLoop_ReplayProvider_RunsRecordedTurns drives a RunConfig that
// selects the replay provider through the factory and the full agentic
// loop: the recorded edit_file call lands on the seeded workspace and the
// run completes with no credential and no network.
func TestBuildLoop_ReplayProvider_RunsRecordedTurns(t *testing.T) {
	must := require.New(t)
	workspace := t.TempDir()
	target := filepath.Join(workspace, "greeting.txt")
	must.NoError(os.WriteFile(target, []byte("hello old world\n"), 0o644))
	recording := writeReplayRecording(t,
		`{"kind":"turn_record","turn":1,"modelOutput":[{"type":"tool_use","id":"c1","name":"edit_file","input":{"path":"greeting.txt","operation":"replace","old_string":"old","new_string":"new"}}]}`,
		`{"kind":"turn_record","turn":2,"modelOutput":[{"type":"text","text":"Updated."}]}`,
	)

	timeout := 30
	enforce := false
	config := &types.RunConfig{
		RunID:            "replay-provider-loop",
		Mode:             "execution",
		Prompt:           "Change old to new.",
		Provider:         types.ProviderConfig{Type: "replay", ReplayFile: recording},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Model: "replay"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: workspace},
		EditStrategy:     types.EditStrategyConfig{Type: "multi"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		RuleOfTwo:        &types.RuleOfTwoConfig{Enforce: &enforce},
		MaxTurns:         4,
		Timeout:          &timeout,
	}
	must.NoError(types.ValidateRunConfig(config))

	loop, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
	must.NoError(err)
	defer func() { _ = loop.Close() }()

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)
	got, err := os.ReadFile(target)
	must.NoError(err)
	must.Equal("hello new world\n", string(got))
	must.Len(runTrace.ToolCalls, 1)
	must.Equal("edit_file", runTrace.ToolCalls[0].Name)
}
