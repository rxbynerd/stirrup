package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/credential"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/mcp"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
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
// Transport is built from config.Transport; use BuildLoopWithTransport to inject
// a pre-established transport (e.g. from the K8s job entrypoint).
func BuildLoop(ctx context.Context, config *types.RunConfig) (*AgenticLoop, error) {
	return BuildLoopWithTransport(ctx, config, nil)
}

// BuildLoopWithTransport is like BuildLoop but accepts an optional pre-built
// Transport. When tp is non-nil it is used directly, skipping buildTransport.
// This allows the K8s job binary to reuse its already-connected gRPC stream.
func BuildLoopWithTransport(ctx context.Context, config *types.RunConfig, tp transport.Transport) (*AgenticLoop, error) {
	var ownedClosers []io.Closer
	emitReady := tp == nil
	cleanup := func() {
		for i := len(ownedClosers) - 1; i >= 0; i-- {
			_ = ownedClosers[i].Close()
		}
	}

	// Construct the security logger before config validation so we can emit
	// a config_validation_failed event when the invariants check fails.
	// Only runID and an io.Writer are needed at this point; the metric
	// counter is wired further down once Metrics is available.
	secLogger := security.NewSecurityLogger(os.Stderr, config.RunID)

	// Validate RunConfig security invariants.
	if err := types.ValidateRunConfig(config); err != nil {
		secLogger.ConfigValidationFailed([]string{err.Error()})
		return nil, fmt.Errorf("config validation: %w", err)
	}

	// Secret store for resolving credential references. AutoSecretStore routes
	// to SSM for "secret://ssm:///..." refs, falling back to env/file otherwise.
	secrets, err := security.NewAutoSecretStore(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("build secret store: %w", err)
	}

	// 1. Provider adapters.
	prov, providers, err := buildProviders(ctx, config, secrets)
	if err != nil {
		return nil, fmt.Errorf("build providers: %w", err)
	}

	// 2. Model router.
	rtr := buildRouter(config.ModelRouter, config.Provider.Type)

	// 3. Prompt builder.
	pb := buildPromptBuilder(config.PromptBuilder, config.SystemPromptOverride)

	// 4. Executor (built early because context strategy may need it).
	exec, err := buildExecutor(ctx, config.Executor, secrets)
	if err != nil {
		return nil, fmt.Errorf("build executor: %w", err)
	}
	if closer, ok := exec.(io.Closer); ok {
		ownedClosers = append(ownedClosers, closer)
	}

	// 5. Context strategy.
	cs := buildContextStrategy(config.ContextStrategy, prov, config.ModelRouter.Model, exec)

	// 6. Tool registry.
	es := buildEditStrategy(config.EditStrategy)
	registry := buildToolRegistry(exec, es, config.Tools)

	// secLogger was constructed above (before ValidateRunConfig) so it can
	// emit config_validation_failed before we know whether we have a
	// MeterProvider. Wire it into the structured logger here so log-side
	// secret redactions also produce SecretRedactedInOutput events; the
	// metric counter is set once Metrics is available further below.
	//
	// Build logger early so MCP connection warnings go through the ScrubHandler.
	logLevel := parseLogLevel(config.LogLevel)
	logger := observability.NewLoggerWithSecurity(config.RunID, logLevel, os.Stderr, secLogger)

	// 6b. MCP tool discovery — connect to remote MCP servers and register
	// their tools into the registry alongside the built-in tools.
	// Connection failures are non-fatal: the server's tools are skipped
	// so the harness can still operate with its built-in tools.
	if len(config.Tools.MCPServers) > 0 {
		mcpClient := mcp.NewClient(registry, nil)
		ownedClosers = append(ownedClosers, mcpClient)
		for _, srv := range config.Tools.MCPServers {
			if err := mcpClient.Connect(ctx, srv, secrets); err != nil {
				logger.Warn("MCP server unavailable, skipping its tools", "server", srv.Name, "error", err)
			}
		}
	}

	// 7. Verifier.
	v := buildVerifier(config.Verifier, prov)

	// 9. Transport — use the injected one if provided, otherwise build from config.
	if tp == nil {
		tp, err = buildTransport(ctx, config.Transport)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("build transport: %w", err)
		}
		ownedClosers = append(ownedClosers, tp)
	}

	// Wire the security logger into the transport so Emit can fire
	// SecretRedactedInOutput events whenever the scrubber redacts a value.
	switch t := tp.(type) {
	case *transport.StdioTransport:
		t.Security = secLogger
	case *transport.GRPCTransport:
		t.Security = secLogger
	}

	// 10. Permission policy.
	pp := buildPermissionPolicy(config.PermissionPolicy, registry, tp)

	// 11. Git strategy.
	gs := buildGitStrategy(config.GitStrategy)

	// 12. Trace emitter.
	te, err := buildTraceEmitter(ctx, config.TraceEmitter)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("build trace emitter: %w", err)
	}
	if closer, ok := te.(io.Closer); ok {
		ownedClosers = append(ownedClosers, closer)
	}

	// 13. OTel metrics.
	var metrics *observability.Metrics
	metricsEndpoint := config.TraceEmitter.MetricsEndpoint
	if metricsEndpoint == "" {
		metricsEndpoint = config.TraceEmitter.Endpoint
	}
	if config.TraceEmitter.Type == "otel" && metricsEndpoint != "" {
		metrics, err = observability.NewMetrics(ctx, metricsEndpoint)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("build metrics: %w", err)
		}
		ownedClosers = append(ownedClosers, metrics)
	} else {
		metrics = observability.NewNoopMetrics()
	}

	// 14. Wire the SecurityEvents counter into the security logger so every
	// Emit increments OTel metrics with an "event" attribute. The
	// EventCounter interface is satisfied by metric.Int64Counter, which is
	// the concrete type of metrics.SecurityEvents.
	if metrics != nil {
		secLogger.SetEventCounter(metrics.SecurityEvents)
	}

	// Wire security logger into executor if it supports it.
	switch e := exec.(type) {
	case *executor.LocalExecutor:
		e.Security = secLogger
	case *executor.ContainerExecutor:
		e.Security = secLogger
	}

	// Extract tracer for deeper span instrumentation.
	var tracer oteltrace.Tracer
	if otelEmitter, ok := te.(*trace.OTelTraceEmitter); ok {
		tracer = otelEmitter.Tracer()
	} else {
		tracer = noop.NewTracerProvider().Tracer("")
	}

	// Set tracer + metrics on provider adapters for HTTP-level instrumentation.
	for _, p := range providers {
		switch pa := p.(type) {
		case *provider.AnthropicAdapter:
			pa.Tracer = tracer
			pa.Metrics = metrics
		case *provider.OpenAICompatibleAdapter:
			pa.Tracer = tracer
			pa.Metrics = metrics
		case *provider.OpenAIResponsesAdapter:
			pa.Tracer = tracer
			pa.Metrics = metrics
		case *provider.BedrockAdapter:
			pa.Tracer = tracer
			pa.Metrics = metrics
		}
	}

	loop := &AgenticLoop{
		Provider:     prov,
		Providers:    providers,
		Router:       rtr,
		Prompt:       pb,
		Context:      cs,
		Tools:        registry,
		Executor:     exec,
		Edit:         es,
		Verifier:     v,
		Permissions:  pp,
		Git:          gs,
		Transport:    tp,
		Trace:        te,
		Tracer:       tracer,
		Metrics:      metrics,
		Security:     secLogger,
		Logger:       logger,
		emitReady:    emitReady,
		ownedClosers: ownedClosers,
	}

	// Register spawn_agent after loop construction. The tool needs a
	// reference to the loop (chicken-and-egg), so we close over the loop
	// pointer here. The spawner closure captures the loop and config so
	// the tool can call SpawnSubAgent without a circular import.
	if toolEnabled(config.Tools.BuiltIn, "spawn_agent") {
		spawner := func(ctx context.Context, prompt, mode string, maxTurns int) (json.RawMessage, error) {
			result, err := SpawnSubAgent(ctx, loop, config, SubAgentConfig{
				Prompt:   prompt,
				Mode:     mode,
				MaxTurns: maxTurns,
			})
			if err != nil {
				return nil, err
			}
			return json.Marshal(result)
		}
		registry.Register(builtins.SpawnAgentTool(spawner))

		// The ask-upstream policy snapshots the approval-required tool
		// set at construction time, but spawn_agent is registered
		// after the policy is built. Refresh it here so spawn_agent
		// calls are gated by the control plane rather than silently
		// auto-allowed. (See TestApprovalRequiredToolSet which asserts
		// the load-bearing absence of spawn_agent in the unrefreshed
		// set.)
		if ask, ok := pp.(*permission.AskUpstreamPolicy); ok {
			ask.AddApprovalTool("spawn_agent")
		}
	}

	return loop, nil
}

func buildProviders(ctx context.Context, config *types.RunConfig, secrets security.SecretStore) (provider.ProviderAdapter, map[string]provider.ProviderAdapter, error) {
	defaultProvider, err := buildProvider(ctx, config.Provider, secrets)
	if err != nil {
		return nil, nil, err
	}

	providers := make(map[string]provider.ProviderAdapter, len(config.Providers)+1)
	providers[config.Provider.Type] = defaultProvider
	for name, cfg := range config.Providers {
		providerAdapter, err := buildProvider(ctx, cfg, secrets)
		if err != nil {
			return nil, nil, fmt.Errorf("build provider %q: %w", name, err)
		}
		providers[name] = providerAdapter
	}

	return defaultProvider, providers, nil
}

func buildProvider(ctx context.Context, cfg types.ProviderConfig, secrets security.SecretStore) (provider.ProviderAdapter, error) {
	src, err := credential.BuildSource(cfg, secrets)
	if err != nil {
		return nil, fmt.Errorf("build credential source: %w", err)
	}
	cred, err := src.Resolve(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve credentials: %w", err)
	}

	switch cfg.Type {
	case "anthropic":
		return provider.NewAnthropicAdapter(cred.BearerToken), nil
	case "openai-compatible":
		auth := provider.OpenAIAuthConfig{
			APIKeyHeader: cfg.APIKeyHeader,
			QueryParams:  cfg.QueryParams,
		}
		return provider.NewOpenAICompatibleAdapter(cred.BearerToken, cfg.BaseURL, auth), nil
	case "openai-responses":
		auth := provider.OpenAIAuthConfig{
			APIKeyHeader: cfg.APIKeyHeader,
			QueryParams:  cfg.QueryParams,
		}
		return provider.NewOpenAIResponsesAdapter(cred.BearerToken, cfg.BaseURL, auth), nil
	case "bedrock":
		return provider.NewBedrockAdapter(cfg.Region, cfg.Profile, cred.AWSCredentials)
	default:
		return nil, fmt.Errorf("unsupported provider type: %q (supported: anthropic, bedrock, openai-compatible, openai-responses)", cfg.Type)
	}
}

func buildRouter(cfg types.ModelRouterConfig, defaultProvider string) router.ModelRouter {
	switch cfg.Type {
	case "static":
		providerName := cfg.Provider
		if providerName == "" {
			providerName = defaultProvider
		}
		return router.NewStaticRouter(providerName, cfg.Model)
	case "per-mode":
		return buildPerModeRouter(cfg, defaultProvider)
	case "dynamic":
		return buildDynamicRouter(cfg, defaultProvider)
	default:
		if defaultProvider == "" {
			defaultProvider = "anthropic"
		}
		return router.NewStaticRouter(defaultProvider, "claude-sonnet-4-6")
	}
}

// buildPerModeRouter constructs a PerModeRouter from the config. Each entry in
// ModeModels is "provider/model"; if the slash is absent, the default provider
// is used with the value treated as the model name.
func buildPerModeRouter(cfg types.ModelRouterConfig, fallbackProvider string) *router.PerModeRouter {
	defaultProvider := cfg.Provider
	if defaultProvider == "" {
		defaultProvider = fallbackProvider
	}
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
func buildDynamicRouter(cfg types.ModelRouterConfig, fallbackProvider string) *router.DynamicRouter {
	defaultProvider := cfg.Provider
	if defaultProvider == "" {
		defaultProvider = fallbackProvider
	}
	if defaultProvider == "" {
		defaultProvider = "anthropic"
	}
	defaultModel := cfg.Model
	if defaultModel == "" {
		defaultModel = "claude-sonnet-4-6"
	}

	cheapProvider := cfg.CheapProvider
	if cheapProvider == "" {
		cheapProvider = defaultProvider
	}
	cheapModel := cfg.CheapModel
	if cheapModel == "" {
		cheapModel = "claude-haiku-4-5-20251001"
	}

	expensiveProvider := cfg.ExpensiveProvider
	if expensiveProvider == "" {
		expensiveProvider = defaultProvider
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

func buildPromptBuilder(cfg types.PromptBuilderConfig, systemPromptOverride string) prompt.PromptBuilder {
	if systemPromptOverride != "" {
		return prompt.NewOverridePromptBuilder(systemPromptOverride)
	}
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

func buildExecutor(ctx context.Context, cfg types.ExecutorConfig, secrets security.SecretStore) (executor.Executor, error) {
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
	case "api":
		if cfg.VcsBackend == nil {
			return nil, fmt.Errorf("api executor requires vcsBackend configuration")
		}
		token, err := secrets.Resolve(ctx, cfg.VcsBackend.APIKeyRef)
		if err != nil {
			return nil, fmt.Errorf("resolve VCS API key: %w", err)
		}
		parts := strings.SplitN(cfg.VcsBackend.Repo, "/", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid repo format %q, expected 'owner/repo'", cfg.VcsBackend.Repo)
		}
		return executor.NewAPIExecutor(token, parts[0], parts[1], cfg.VcsBackend.Ref), nil
	default:
		return nil, fmt.Errorf("unsupported executor type: %q (supported: local, container, api)", cfg.Type)
	}
}

func buildToolRegistry(exec executor.Executor, es edit.EditStrategy, cfg types.ToolsConfig) *tool.Registry {
	registry := tool.NewRegistry()
	caps := exec.Capabilities()
	if toolEnabled(cfg.BuiltIn, "read_file") && caps.CanRead {
		registry.Register(builtins.ReadFileTool(exec))
	}
	if toolEnabled(cfg.BuiltIn, "list_directory") && caps.CanRead {
		registry.Register(builtins.ListDirectoryTool(exec))
	}
	if toolEnabled(cfg.BuiltIn, "search_files") && caps.CanExec {
		registry.Register(builtins.SearchFilesTool(exec))
	}
	if toolEnabled(cfg.BuiltIn, "run_command") && caps.CanExec {
		registry.Register(builtins.RunCommandTool(exec))
	}
	if toolEnabled(cfg.BuiltIn, "web_fetch") {
		registry.Register(builtins.WebFetchTool())
	}
	if editToolEnabled(cfg.BuiltIn, es.ToolDefinition().Name) && caps.CanWrite {
		registry.Register(editStrategyTool(es, exec))
	}
	return registry
}

func toolEnabled(enabled []string, name string) bool {
	if len(enabled) == 0 {
		return true
	}
	for _, candidate := range enabled {
		if candidate == name {
			return true
		}
	}
	return false
}

func editToolEnabled(enabled []string, actualName string) bool {
	if len(enabled) == 0 {
		return true
	}
	editAliases := map[string]bool{
		"write_file":     true,
		"search_replace": true,
		"apply_diff":     true,
	}
	for _, candidate := range enabled {
		if candidate == actualName || editAliases[candidate] {
			return true
		}
	}
	return false
}

func editStrategyTool(es edit.EditStrategy, exec executor.Executor) *tool.Tool {
	definition := es.ToolDefinition()
	return &tool.Tool{
		Name:              definition.Name,
		Description:       definition.Description,
		InputSchema:       definition.InputSchema,
		WorkspaceMutating: true,
		RequiresApproval:  true,
		Handler: func(ctx context.Context, input json.RawMessage) (string, error) {
			result, err := es.Apply(ctx, input, exec)
			if err != nil {
				return "", err
			}
			if result == nil {
				return "", fmt.Errorf("edit strategy returned no result")
			}
			if !result.Applied {
				if result.Error == "" {
					return "", fmt.Errorf("edit was not applied")
				}
				return "", fmt.Errorf("%s", result.Error)
			}
			if result.Diff != "" {
				return result.Diff, nil
			}
			return fmt.Sprintf("Successfully edited %s", result.Path), nil
		},
	}
}

func buildEditStrategy(cfg types.EditStrategyConfig) edit.EditStrategy {
	fuzzyThreshold := 0.80
	if cfg.FuzzyThreshold != nil {
		fuzzyThreshold = *cfg.FuzzyThreshold
	}

	switch cfg.Type {
	case "whole-file", "":
		return edit.NewWholeFileStrategy()
	case "search-replace":
		return edit.NewSearchReplaceStrategy()
	case "udiff":
		return edit.NewUdiffStrategy(fuzzyThreshold)
	case "multi":
		return edit.NewMultiStrategy(fuzzyThreshold)
	default:
		return edit.NewWholeFileStrategy()
	}
}

func buildVerifier(cfg types.VerifierConfig, prov provider.ProviderAdapter) verifier.Verifier {
	switch cfg.Type {
	case "composite":
		subs := make([]verifier.Verifier, len(cfg.Verifiers))
		for i, sub := range cfg.Verifiers {
			subs[i] = buildVerifier(sub, prov)
		}
		return verifier.NewCompositeVerifier(subs)
	case "llm-judge":
		model := cfg.Model
		if model == "" {
			model = "claude-haiku-4-5-20251001"
		}
		return verifier.NewLLMJudgeVerifier(prov, model, cfg.Criteria)
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
		// DenySideEffects rejects only tools that mutate workspace
		// state. Tools whose only sensitivity is "operator should
		// approve" (web_fetch, spawn_agent) are still allowed —
		// research-mode users explicitly enable them.
		return permission.NewDenySideEffects(mutatingToolSet(registry))
	case "ask-upstream":
		// AskUpstreamPolicy prompts on tools whose RequiresApproval
		// flag is set. This includes mutating tools but also covers
		// non-mutating-but-sensitive tools.
		timeout := time.Duration(cfg.Timeout) * time.Second
		return permission.NewAskUpstreamPolicy(tp, approvalRequiredToolSet(registry), timeout)
	default:
		return permission.NewAllowAll()
	}
}

// mutatingToolSet returns the names of registered tools that mutate
// workspace state. DenySideEffects denies exactly this set.
func mutatingToolSet(registry *tool.Registry) map[string]bool {
	out := make(map[string]bool)
	for _, td := range registry.List() {
		t := registry.Resolve(td.Name)
		if t != nil && t.WorkspaceMutating {
			out[td.Name] = true
		}
	}
	return out
}

// approvalRequiredToolSet returns the names of registered tools that
// require upstream operator approval before being executed.
// AskUpstreamPolicy prompts on exactly this set.
func approvalRequiredToolSet(registry *tool.Registry) map[string]bool {
	out := make(map[string]bool)
	for _, td := range registry.List() {
		t := registry.Resolve(td.Name)
		if t != nil && t.RequiresApproval {
			out[td.Name] = true
		}
	}
	return out
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

func buildTraceEmitter(ctx context.Context, cfg types.TraceEmitterConfig) (trace.TraceEmitter, error) {
	switch cfg.Type {
	case "otel":
		endpoint := cfg.Endpoint
		if endpoint == "" {
			endpoint = "localhost:4317"
		}
		return trace.NewOTelTraceEmitter(ctx, endpoint)
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
		return nil, fmt.Errorf("unsupported trace emitter type: %q (supported: jsonl, otel)", cfg.Type)
	}
}

// parseLogLevel converts a log level string to slog.Level.
// Defaults to slog.LevelInfo for unrecognised values.
func parseLogLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
