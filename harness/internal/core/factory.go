package core

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

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

	// Secret store for resolving credential references.
	secrets := security.NewEnvSecretStore()

	// 1. Provider adapter.
	prov, err := buildProvider(ctx, config.Provider, secrets)
	if err != nil {
		return nil, fmt.Errorf("build provider: %w", err)
	}

	// 2. Model router.
	rtr := buildRouter(config.ModelRouter)

	// 3. Prompt builder.
	pb := buildPromptBuilder(config.PromptBuilder)

	// 4. Context strategy.
	cs := buildContextStrategy(config.ContextStrategy)

	// 6. Executor.
	exec, err := buildExecutor(config.Executor)
	if err != nil {
		return nil, fmt.Errorf("build executor: %w", err)
	}

	// 5. Tool registry.
	registry := buildToolRegistry(exec)

	// 7. Edit strategy.
	es := buildEditStrategy(config.EditStrategy)

	// 8. Verifier.
	v := buildVerifier(config.Verifier)

	// 9. Permission policy.
	pp := buildPermissionPolicy(config.PermissionPolicy, registry)

	// 10. Transport.
	tp := buildTransport()

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
	if le, ok := exec.(*executor.LocalExecutor); ok {
		le.Security = secLogger
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
	default:
		return nil, fmt.Errorf("unsupported provider type: %q (Phase 1 supports: anthropic)", cfg.Type)
	}
}

func buildRouter(cfg types.ModelRouterConfig) router.ModelRouter {
	switch cfg.Type {
	case "static":
		return router.NewStaticRouter(cfg.Provider, cfg.Model)
	default:
		// Default to static with claude-sonnet-4-6.
		return router.NewStaticRouter("anthropic", "claude-sonnet-4-6")
	}
}

func buildPromptBuilder(cfg types.PromptBuilderConfig) prompt.PromptBuilder {
	switch cfg.Type {
	case "default", "":
		return prompt.NewDefaultPromptBuilder()
	default:
		return prompt.NewDefaultPromptBuilder()
	}
}

func buildContextStrategy(cfg types.ContextStrategyConfig) contextpkg.ContextStrategy {
	switch cfg.Type {
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
	default:
		return nil, fmt.Errorf("unsupported executor type: %q (Phase 1 supports: local)", cfg.Type)
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
	default:
		return edit.NewWholeFileStrategy()
	}
}

func buildVerifier(cfg types.VerifierConfig) verifier.Verifier {
	switch cfg.Type {
	case "none", "":
		return verifier.NewNoneVerifier()
	default:
		return verifier.NewNoneVerifier()
	}
}

func buildPermissionPolicy(cfg types.PermissionPolicyConfig, registry *tool.Registry) permission.PermissionPolicy {
	switch cfg.Type {
	case "allow-all":
		return permission.NewAllowAll()
	case "deny-side-effects":
		// Build the set of side-effecting tool names from the registry.
		sideEffecting := make(map[string]bool)
		for _, td := range registry.List() {
			t := registry.Resolve(td.Name)
			if t != nil && t.SideEffects {
				sideEffecting[td.Name] = true
			}
		}
		return permission.NewDenySideEffects(sideEffecting)
	default:
		return permission.NewAllowAll()
	}
}

func buildTransport() transport.Transport {
	return transport.NewStdioTransport(os.Stdout, os.Stdin)
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
