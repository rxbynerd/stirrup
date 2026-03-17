package core

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/mcp"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/tool/builtins"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// BuildLoop constructs an AgenticLoop from a RunConfig. It validates the config,
// resolves secrets, and instantiates all components. This is the composition root.
func BuildLoop(ctx context.Context, config *types.RunConfig) (*AgenticLoop, error) {
	// Validate RunConfig security invariants.
	if err := types.ValidateRunConfig(config); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	// Secret store for resolving credential references. AutoSecretStore routes
	// to SSM for "secret://ssm:///..." refs, falling back to env/file otherwise.
	secrets, err := security.NewAutoSecretStore(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("build secret store: %w", err)
	}

	// 1. Provider adapter.
	prov, err := buildProvider(ctx, config.Provider, secrets)
	if err != nil {
		return nil, fmt.Errorf("build provider: %w", err)
	}

	// 2. Model router.
	rtr := buildRouter(config.ModelRouter)

	// 3. Prompt builder.
	pb := buildPromptBuilder(config.PromptBuilder)

	// 4. Executor (built early because context strategy may need it).
	exec, err := buildExecutor(config.Executor)
	if err != nil {
		return nil, fmt.Errorf("build executor: %w", err)
	}

	// 5. Context strategy.
	cs := buildContextStrategy(config.ContextStrategy, prov, config.ModelRouter.Model, exec)

	// 6. Tool registry.
	registry := buildToolRegistry(exec)

	// 6b. MCP tool discovery — connect to remote MCP servers and register
	// their tools into the registry alongside the built-in tools.
	if len(config.Tools.MCPServers) > 0 {
		mcpClient := mcp.NewClient(registry, nil)
		for _, srv := range config.Tools.MCPServers {
			if err := mcpClient.Connect(ctx, srv, secrets); err != nil {
				return nil, fmt.Errorf("connect MCP server %q: %w", srv.Name, err)
			}
		}
	}

	// 7. Edit strategy.
	es := buildEditStrategy(config.EditStrategy)

	// 8. Verifier.
	v := buildVerifier(config.Verifier)

	// 9. Transport (built before permission policy since ask-upstream needs it).
	tp, err := buildTransport(ctx, config.Transport)
	if err != nil {
		return nil, fmt.Errorf("build transport: %w", err)
	}

	// 10. Permission policy.
	pp := buildPermissionPolicy(config.PermissionPolicy, registry, tp)

	// 11. Git strategy.
	gs := buildGitStrategy(config.GitStrategy)

	// 12. Trace emitter.
	te, err := buildTraceEmitter(config.TraceEmitter)
	if err != nil {
		return nil, fmt.Errorf("build trace emitter: %w", err)
	}

	// 13. Security logger (writes to stderr).
	secLogger := security.NewSecurityLogger(os.Stderr, config.RunID)

	// Wire security logger into executor if it supports it.
	switch e := exec.(type) {
	case *executor.LocalExecutor:
		e.Security = secLogger
	case *executor.ContainerExecutor:
		e.Security = secLogger
	}

	return &AgenticLoop{
		Provider:    prov,
		Router:      rtr,
		Prompt:      pb,
		Context:     cs,
		Tools:       registry,
		Executor:    exec,
		Edit:        es,
		Verifier:    v,
		Permissions: pp,
		Git:         gs,
		Transport:   tp,
		Trace:       te,
		Security:    secLogger,
	}, nil
}

func buildProvider(ctx context.Context, cfg types.ProviderConfig, secrets security.SecretStore) (provider.ProviderAdapter, error) {
	switch cfg.Type {
	case "anthropic":
		apiKey, err := secrets.Resolve(ctx, cfg.APIKeyRef)
		if err != nil {
			return nil, fmt.Errorf("resolve API key: %w", err)
		}
		return provider.NewAnthropicAdapter(apiKey), nil
	case "openai-compatible":
		apiKey, err := secrets.Resolve(ctx, cfg.APIKeyRef)
		if err != nil {
			return nil, fmt.Errorf("resolve API key: %w", err)
		}
		return provider.NewOpenAICompatibleAdapter(apiKey, cfg.BaseURL), nil
	case "bedrock":
		return provider.NewBedrockAdapter(cfg.Region, cfg.Profile)
	default:
		return nil, fmt.Errorf("unsupported provider type: %q (supported: anthropic, bedrock, openai-compatible)", cfg.Type)
	}
}

func buildRouter(cfg types.ModelRouterConfig) router.ModelRouter {
	switch cfg.Type {
	case "static":
		return router.NewStaticRouter(cfg.Provider, cfg.Model)
	case "per-mode":
		return buildPerModeRouter(cfg)
	case "dynamic":
		return buildDynamicRouter(cfg)
	default:
		// Default to static with claude-sonnet-4-6.
		return router.NewStaticRouter("anthropic", "claude-sonnet-4-6")
	}
}

// buildPerModeRouter constructs a PerModeRouter from the config. Each entry in
// ModeModels is "provider/model"; if the slash is absent, the default provider
// is used with the value treated as the model name.
func buildPerModeRouter(cfg types.ModelRouterConfig) *router.PerModeRouter {
	defaultProvider := cfg.Provider
	if defaultProvider == "" {
		defaultProvider = "anthropic"
	}
	defaultModel := cfg.Model
	if defaultModel == "" {
		defaultModel = "claude-sonnet-4-6"
	}
	defaultSel := router.ModelSelection{Provider: defaultProvider, Model: defaultModel}

	modeMap := make(map[string]router.ModelSelection, len(cfg.ModeModels))
	for mode, spec := range cfg.ModeModels {
		if p, m, ok := strings.Cut(spec, "/"); ok {
			modeMap[mode] = router.ModelSelection{Provider: p, Model: m}
		} else {
			// No slash: use default provider with the given model name.
			modeMap[mode] = router.ModelSelection{Provider: defaultProvider, Model: spec}
		}
	}

	return router.NewPerModeRouter(defaultSel, modeMap)
}

// buildDynamicRouter constructs a DynamicRouter from the config, applying
// sensible defaults for any fields not explicitly set.
func buildDynamicRouter(cfg types.ModelRouterConfig) *router.DynamicRouter {
	defaultProvider := cfg.Provider
	if defaultProvider == "" {
		defaultProvider = "anthropic"
	}
	defaultModel := cfg.Model
	if defaultModel == "" {
		defaultModel = "claude-sonnet-4-6"
	}

	cheapProvider := cfg.CheapProvider
	if cheapProvider == "" {
		cheapProvider = "anthropic"
	}
	cheapModel := cfg.CheapModel
	if cheapModel == "" {
		cheapModel = "claude-haiku-4-5-20251001"
	}

	expensiveProvider := cfg.ExpensiveProvider
	if expensiveProvider == "" {
		expensiveProvider = "anthropic"
	}
	expensiveModel := cfg.ExpensiveModel
	if expensiveModel == "" {
		expensiveModel = "claude-sonnet-4-6"
	}

	turnThreshold := cfg.ExpensiveTurnThreshold
	if turnThreshold == 0 {
		turnThreshold = 10
	}

	tokenThreshold := cfg.ExpensiveTokenThreshold
	if tokenThreshold == 0 {
		tokenThreshold = 50000
	}

	cheapStopReasons := cfg.CheapStopReasons
	if len(cheapStopReasons) == 0 {
		cheapStopReasons = []string{"tool_use"}
	}

	return router.NewDynamicRouter(router.DynamicRouterConfig{
		DefaultSelection:        router.ModelSelection{Provider: defaultProvider, Model: defaultModel},
		CheapSelection:          router.ModelSelection{Provider: cheapProvider, Model: cheapModel},
		ExpensiveSelection:      router.ModelSelection{Provider: expensiveProvider, Model: expensiveModel},
		ExpensiveTurnThreshold:  turnThreshold,
		ExpensiveTokenThreshold: tokenThreshold,
		CheapStopReasons:        cheapStopReasons,
	})
}

func buildPromptBuilder(cfg types.PromptBuilderConfig) prompt.PromptBuilder {
	switch cfg.Type {
	case "composed":
		return prompt.NewComposedPromptBuilder(
			prompt.WithFragments(prompt.DefaultComposedFragments()...),
		)
	case "default", "":
		return prompt.NewDefaultPromptBuilder()
	default:
		return prompt.NewDefaultPromptBuilder()
	}
}

func buildContextStrategy(cfg types.ContextStrategyConfig, prov provider.ProviderAdapter, model string, exec executor.Executor) contextpkg.ContextStrategy {
	switch cfg.Type {
	case "summarise":
		return contextpkg.NewSummariseStrategy(prov, model)
	case "offload-to-file":
		return contextpkg.NewOffloadToFileStrategy(exec)
	case "sliding-window", "":
		return contextpkg.NewSlidingWindowStrategy()
	default:
		return contextpkg.NewSlidingWindowStrategy()
	}
}

func buildExecutor(cfg types.ExecutorConfig) (executor.Executor, error) {
	switch cfg.Type {
	case "local", "":
		workspace := cfg.Workspace
		if workspace == "" {
			var err error
			workspace, err = os.Getwd()
			if err != nil {
				return nil, fmt.Errorf("get working directory: %w", err)
			}
		}
		return executor.NewLocalExecutor(workspace)
	case "container":
		if cfg.Image == "" {
			return nil, fmt.Errorf("container executor requires image")
		}
		workspace := cfg.Workspace
		if workspace == "" {
			var err error
			workspace, err = os.Getwd()
			if err != nil {
				return nil, fmt.Errorf("get working directory: %w", err)
			}
		}
		return executor.NewContainerExecutor(executor.ContainerExecutorConfig{
			Image:     cfg.Image,
			HostDir:   workspace,
			Network:   cfg.Network,
			Resources: cfg.Resources,
		})
	default:
		return nil, fmt.Errorf("unsupported executor type: %q (supported: local, container)", cfg.Type)
	}
}

func buildToolRegistry(exec executor.Executor) *tool.Registry {
	registry := tool.NewRegistry()
	builtins.RegisterBuiltins(registry, exec)
	return registry
}

func buildEditStrategy(cfg types.EditStrategyConfig) edit.EditStrategy {
	switch cfg.Type {
	case "whole-file", "":
		return edit.NewWholeFileStrategy()
	case "search-replace":
		return edit.NewSearchReplaceStrategy()
	case "udiff":
		return edit.NewUdiffStrategy()
	default:
		return edit.NewWholeFileStrategy()
	}
}

func buildVerifier(cfg types.VerifierConfig) verifier.Verifier {
	switch cfg.Type {
	case "composite":
		subs := make([]verifier.Verifier, len(cfg.Verifiers))
		for i, sub := range cfg.Verifiers {
			subs[i] = buildVerifier(sub)
		}
		return verifier.NewCompositeVerifier(subs)
	case "test-runner":
		timeout := time.Duration(cfg.Timeout) * time.Second
		return verifier.NewTestRunnerVerifier(cfg.Command, timeout)
	case "none", "":
		return verifier.NewNoneVerifier()
	default:
		return verifier.NewNoneVerifier()
	}
}

func buildPermissionPolicy(cfg types.PermissionPolicyConfig, registry *tool.Registry, tp transport.Transport) permission.PermissionPolicy {
	switch cfg.Type {
	case "allow-all":
		return permission.NewAllowAll()
	case "deny-side-effects":
		// Build the set of side-effecting tool names from the registry.
		sideEffecting := sideEffectingToolSet(registry)
		return permission.NewDenySideEffects(sideEffecting)
	case "ask-upstream":
		sideEffecting := sideEffectingToolSet(registry)
		return permission.NewAskUpstreamPolicy(tp, sideEffecting)
	default:
		return permission.NewAllowAll()
	}
}

// sideEffectingToolSet builds a set of tool names that have side effects
// from the tool registry.
func sideEffectingToolSet(registry *tool.Registry) map[string]bool {
	sideEffecting := make(map[string]bool)
	for _, td := range registry.List() {
		t := registry.Resolve(td.Name)
		if t != nil && t.SideEffects {
			sideEffecting[td.Name] = true
		}
	}
	return sideEffecting
}

func buildTransport(ctx context.Context, cfg types.TransportConfig) (transport.Transport, error) {
	switch cfg.Type {
	case "grpc":
		if cfg.Address == "" {
			return nil, fmt.Errorf("gRPC transport requires an address")
		}
		return transport.NewGRPCTransport(ctx, cfg.Address)
	case "stdio", "":
		return transport.NewStdioTransport(os.Stdout, os.Stdin), nil
	default:
		return nil, fmt.Errorf("unsupported transport type: %q (supported: stdio, grpc)", cfg.Type)
	}
}

func buildGitStrategy(cfg types.GitStrategyConfig) git.GitStrategy {
	switch cfg.Type {
	case "deterministic":
		return git.NewDeterministicGitStrategy()
	case "none", "":
		return git.NewNoneGitStrategy()
	default:
		return git.NewNoneGitStrategy()
	}
}

func buildTraceEmitter(cfg types.TraceEmitterConfig) (trace.TraceEmitter, error) {
	switch cfg.Type {
	case "jsonl", "":
		var w io.Writer
		if cfg.FilePath != "" {
			f, err := os.OpenFile(cfg.FilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return nil, fmt.Errorf("open trace file: %w", err)
			}
			w = f
		} else {
			// Write to a discard buffer if no path specified.
			w = &bytes.Buffer{}
		}
		return trace.NewJSONLTraceEmitter(w), nil
	default:
		return nil, fmt.Errorf("unsupported trace emitter type: %q (Phase 1 supports: jsonl)", cfg.Type)
	}
}
