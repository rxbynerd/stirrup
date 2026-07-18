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
	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/hook"
	"github.com/rxbynerd/stirrup/harness/internal/mcp"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/provider/compat/zai"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/ruleoftwo"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/security/codescanner"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/tool/builtins"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// BuildLoop constructs an AgenticLoop from a RunConfig: it validates the
// config, resolves secrets, and instantiates all components. This is the
// composition root.
func BuildLoop(ctx context.Context, config *types.RunConfig) (*AgenticLoop, error) {
	return BuildLoopWithTransport(ctx, config, nil)
}

// BuildLoopWithTransport is like BuildLoop but accepts an optional
// pre-built Transport; a non-nil tp is used directly, skipping
// buildTransport, so a caller (e.g. the K8s job binary) can reuse an
// already-connected transport.
func BuildLoopWithTransport(ctx context.Context, config *types.RunConfig, tp transport.Transport) (*AgenticLoop, error) {
	var ownedClosers []io.Closer
	emitReady := tp == nil
	cleanup := func() {
		for i := len(ownedClosers) - 1; i >= 0; i-- {
			_ = ownedClosers[i].Close()
		}
	}

	// secLogger is constructed before ValidateRunConfig so a validation
	// failure can still emit config_validation_failed.
	secLogger := security.NewSecurityLogger(os.Stderr, config.RunID)

	if err := types.ValidateRunConfig(config); err != nil {
		secLogger.ConfigValidationFailed([]string{err.Error()})
		return nil, fmt.Errorf("config validation: %w", err)
	}

	// Rule-of-Two arming/audit; see docs/safety-rings.md. The arming
	// decision feeds both the audit event here and the monitor built
	// in step 10b below.
	ruleOfTwoArmingState := resolveRuleOfTwoArming(config)
	emitRuleOfTwoEvents(config, secLogger, ruleOfTwoArmingState)

	// AutoSecretStore routes "secret://ssm:///..." refs to SSM, falling
	// back to env/file otherwise.
	secrets, err := security.NewAutoSecretStore(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("build secret store: %w", err)
	}

	// 1. Model router. Needs only the provider TYPE (a string), not the
	// constructed adapter, so it precedes buildComponents.
	rtr := buildRouter(config.ModelRouter, config.Provider.Type)

	// 2. Prompt builder.
	pb, err := buildPromptBuilder(config)
	if err != nil {
		return nil, fmt.Errorf("build prompt builder: %w", err)
	}

	// 3. Executor (built early because context strategy may need it).
	// secLogger is threaded into the container executor so its in-process
	// egress proxy can emit egress_allowed/egress_blocked events.
	exec, err := buildExecutor(ctx, config.Executor, secrets, secLogger)
	if err != nil {
		return nil, fmt.Errorf("build executor: %w", err)
	}
	if closer, ok := exec.(io.Closer); ok {
		ownedClosers = append(ownedClosers, closer)
	}

	// 4. Tool registry. ValidateRunConfig fills CodeScanner with a
	// per-mode default so it's never nil here, but defend in depth
	// against a caller that bypasses the validator.
	es := buildEditStrategy(config.EditStrategy)
	es, err = wrapWithCodeScanner(es, config.CodeScanner, secLogger)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("build code scanner: %w", err)
	}
	registry := buildToolRegistry(exec, es, config.Tools)

	// resourceOpts and resolvedHeaders are shared by the log, trace, and
	// metric exporters below so all three signals carry a consistent
	// resource identity and authenticate identically; computed once here
	// rather than re-resolved per signal.
	resourceOpts := resourceOptionsFromConfig(config)
	resolvedHeaders, err := observability.ResolveHeaders(ctx, secrets, config.TraceEmitter.Headers)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("resolve trace emitter headers: %w", err)
	}

	// Optional second log sink alongside stderr, opted into via
	// observability.logsExport.type == "otlp"; falls back to the trace
	// emitter's endpoint so one --otel-endpoint covers all three signals.
	var logExportHandler slog.Handler
	if config.Observability.LogsExport.Type == "otlp" {
		logEndpoint := config.Observability.LogsExport.Endpoint
		if logEndpoint == "" {
			logEndpoint = config.TraceEmitter.Endpoint
		}
		logExporter, handler, lerr := observability.NewLogExporter(ctx, logEndpoint, resolvedHeaders, resourceOpts)
		if lerr != nil {
			cleanup()
			return nil, fmt.Errorf("build log exporter: %w", lerr)
		}
		logExportHandler = handler
		ownedClosers = append(ownedClosers, logExporter)
	}

	// Wiring secLogger here means log-side secret redactions also produce
	// SecretRedactedInOutput events; built early so MCP connection
	// warnings below go through the ScrubHandler.
	logLevel := parseLogLevel(config.LogLevel)
	logger := observability.NewLoggerWithExport(config.RunID, logLevel, os.Stderr, secLogger, logExportHandler)
	if config.SessionName != "" {
		// Reassigned (not shadowed) so the label propagates into
		// AgenticLoop.Logger below.
		logger = logger.With("sessionName", config.SessionName)
	}

	// 5. MCP tool discovery. Connection failures are non-fatal: the
	// server's tools are skipped so the harness still runs with its
	// built-in tools. mcpClient is retained so Metrics can be
	// field-injected once the run's metrics instance exists below.
	var mcpClient *mcp.Client
	if len(config.Tools.MCPServers) > 0 {
		mcpClient = mcp.NewClient(registry, nil)
		// Metrics is field-injected later, once the run's metrics instance exists.
		mcpClient.Logger = logger
		ownedClosers = append(ownedClosers, mcpClient)
		for _, srv := range config.Tools.MCPServers {
			if err := mcpClient.Connect(ctx, srv, secrets); err != nil {
				logger.Warn("MCP server unavailable, skipping its tools", "server", srv.Name, "error", err)
			}
		}
	}

	// 6. Transport — use the injected one if provided, otherwise build
	// from config; built before buildComponents because the ask-upstream
	// permission policy needs it.
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

	// 7. Probe-eligible components (providers, permission policy, trace
	// emitter) via the shared buildComponents path — also used by
	// Preflight; see docs/architecture.md. A nil sink here means no
	// per-component construction steps are emitted.
	components, err := buildComponents(ctx, config, secrets, secLogger, registry, tp, executorBuildResult{exec: exec}, resolvedHeaders, resourceOpts, nil)
	if err != nil {
		cleanup()
		// buildComponents already prefixes the failing component.
		return nil, err
	}
	prov := components.defaultProvider
	providers := components.providers
	pp := components.permissionPolicy
	te := components.traceEmitter
	if closer, ok := te.(io.Closer); ok {
		ownedClosers = append(ownedClosers, closer)
	}

	// 8. Context strategy. Built after buildComponents because the
	// "summarise" strategy needs the default provider adapter.
	cs := buildContextStrategy(config.ContextStrategy, prov, config.ModelRouter.Model, exec)

	// 9. Verifier. Construction is deferred to step 13, once metrics
	// exists — buildVerifier wraps in a metric-recorder when metrics is
	// non-nil, so building it twice would discard the first build.
	var v verifier.Verifier

	// 10. GuardRail, built after providers so cloud-judge can reuse the
	// default ProviderAdapter.
	gr, err := buildGuardRail(config.GuardRail, providers, prov)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("build guardrail: %w", err)
	}

	// 10b. Rule-of-Two runtime monitor from the arming decision above.
	rot := buildRuleOfTwoMonitor(ruleOfTwoArmingState)

	// 11. Git strategy.
	gs := buildGitStrategy(config.GitStrategy)

	// 11b. Lifecycle hook runner; shares the run's Executor so hooks run
	// under the same sandbox and egress posture as every tool call.
	hooksRunner := buildHookRunner(config.Hooks, exec, logger)

	// 13. OTel metrics.
	var metrics *observability.Metrics
	metricsEndpoint := config.TraceEmitter.MetricsEndpoint
	if metricsEndpoint == "" {
		metricsEndpoint = config.TraceEmitter.Endpoint
	}
	if config.TraceEmitter.Type == "otel" && metricsEndpoint != "" {
		metrics, err = observability.NewMetrics(ctx, metricsEndpoint, config.TraceEmitter.Protocol, resolvedHeaders, resourceOpts)
		if err != nil {
			cleanup()
			return nil, fmt.Errorf("build metrics: %w", err)
		}
		ownedClosers = append(ownedClosers, metrics)
	} else {
		metrics = observability.NewNoopMetrics()
	}

	// 14. Wire the SecurityEvents counter so every Emit increments OTel
	// metrics tagged by "event".
	if metrics != nil {
		secLogger.SetEventCounter(metrics.SecurityEvents)
	}

	// Field-inject Metrics into the MCP client (built before metrics
	// existed, so it can't take it at construction). Nil mcpClient is a
	// no-op.
	if mcpClient != nil {
		mcpClient.Metrics = metrics
	}

	// Field-inject Metrics into the edit strategy. The base strategy may
	// be a *MultiStrategy and/or wrapped in a *ScannedStrategy — walk the
	// outer wrapper first, then unwrap to reach an inner *MultiStrategy.
	wireEditMetrics(es, metrics)

	// Build the verifier now that metrics is available so each Verify
	// call records stirrup.verifier.runs / stirrup.verifier.duration_ms
	// with the appropriate type label. Construction is deferred to here
	// (rather than step 7) so the metric-recorder wrapping centralised
	// in buildVerifier sees a non-nil metrics on the only call site.
	v = buildVerifier(config.Verifier, prov, metrics)

	// Applied before the metrics wrapper so gate denials still land in
	// stirrup.permission.decisions under the policy label (true
	// attribution rides stirrup.ruleoftwo.actions from the gate itself).
	pp = wrapRuleOfTwoGate(pp, rot, ruleOfTwoArmingState, registry, config, tp, metrics)

	// Composition-only: does not re-construct the policy, so a
	// policy-engine's Cedar file is loaded exactly once.
	pp = wrapPermissionPolicyMetrics(pp, config.PermissionPolicy, metrics)

	cs = wrapContextStrategy(cs, config.ContextStrategy, metrics)

	// K8sExecutor is intentionally absent: its Security emitter is set
	// at construction (buildExecutor), not re-wired here.
	switch e := exec.(type) {
	case *executor.LocalExecutor:
		e.Security = secLogger
	case *executor.ContainerExecutor:
		e.Security = secLogger
	}

	var tracer oteltrace.Tracer
	if otelEmitter, ok := te.(*trace.OTelTraceEmitter); ok {
		tracer = otelEmitter.Tracer()
	} else {
		tracer = noop.NewTracerProvider().Tracer("")
	}

	for _, p := range providers {
		switch pa := p.(type) {
		case *provider.AnthropicAdapter:
			pa.Tracer = tracer
			pa.Metrics = metrics
			pa.Logger = logger
		case *provider.OpenAICompatibleAdapter:
			pa.Tracer = tracer
			pa.Metrics = metrics
			pa.Logger = logger
		case *provider.OpenAIResponsesAdapter:
			pa.Tracer = tracer
			pa.Metrics = metrics
			pa.Logger = logger
		case *provider.BedrockAdapter:
			pa.Tracer = tracer
			pa.Metrics = metrics
			pa.Logger = logger
		case *provider.GeminiAdapter:
			pa.Tracer = tracer
			pa.Metrics = metrics
			pa.Logger = logger
		}
	}

	// 14b. Optional BatchAdapter wrapping; see docs/batch.md. Only the
	// top-level provider is wrapped — entries in config.Providers are
	// streaming-only in v1. The streaming inner is retained so
	// cfg.FallbackOnTimeout can delegate to it without a second build.
	if config.Provider.Batch != nil && config.Provider.Batch.Enabled {
		// Defence-in-depth: ValidateRunConfig fills this default when
		// batch.enabled is true, for callers that bypass the validator.
		maxWaitSec := 86_400
		if config.Provider.Batch.MaxWaitSeconds != nil {
			maxWaitSec = *config.Provider.Batch.MaxWaitSeconds
		}
		maxWait := time.Duration(maxWaitSec) * time.Second

		var batchClient provider.BatchClient
		switch config.Transport.Type {
		case "grpc":
			batchClient = provider.NewControlPlaneBatchClient(tp, maxWait, config.Provider.Batch.CancelBundleOnRunCancel)
		case "stdio":
			// Bedrock and Gemini are out of scope (validBatchProviderTypes
			// rejects them in ValidateRunConfig); this dispatch mirrors
			// that closed set so a misconfigured run fails at build time.
			switch config.Provider.Type {
			case "anthropic", "openai-compatible", "openai-responses":
				// supported
			default:
				cleanup()
				return nil, fmt.Errorf(
					"batch with transport=stdio does not support provider type %q",
					config.Provider.Type,
				)
			}
			// Rebuilt here (rather than captured from buildProviders)
			// because the polling client needs the Source itself so
			// each poll can re-resolve rotating credentials.
			credSrc, err := credential.BuildSource(config.Provider, secrets)
			if err != nil {
				cleanup()
				return nil, fmt.Errorf("build batch credential source: %w", err)
			}
			batchClient = provider.NewHarnessPollingBatchClient(provider.HarnessBatchClientOptions{
				ProviderType: config.Provider.Type,
				APIKeyRef:    config.Provider.APIKeyRef,
				CredSource:   credSrc,
				BaseURL:      config.Provider.BaseURL,
				APIKeyHeader: config.Provider.APIKeyHeader,
				MaxWait:      maxWait,
				Logger:       logger,
			})
		default:
			// Defence in depth: validateBatchConfig already rejects any
			// transport that isn't grpc or stdio.
			cleanup()
			return nil, fmt.Errorf("batch is not supported for transport type %q", config.Transport.Type)
		}

		batchAdapter := provider.NewBatchAdapter(prov, batchClient, config.Provider.Batch, config.Provider.Type, config.RunID)
		// Thread the streaming inner adapter's quirks registry through so
		// the batch marshal path matches the streaming wire shape,
		// including any compat-profile extras.
		if compatible, ok := prov.(*provider.OpenAICompatibleAdapter); ok {
			batchAdapter.Registry = compatible.Registry
		}
		prov = batchAdapter
		// Replace the map entry so model-router lookups route to the
		// batched wrapper rather than the raw streaming adapter.
		providers[config.Provider.Type] = prov
	}

	// 14c. Wrap every loop-facing provider adapter with the tool-name
	// normalizer, applied outermost (after batch/fallback wraps) so the
	// invariant "provider never sees an invalid tool name" holds for any
	// MCP server or operator-defined tool.
	prov = provider.NewNormalizingAdapter(prov, config.Provider.Type)
	wrappedProviders := make(map[string]provider.ProviderAdapter, len(providers))
	for name, p := range providers {
		if name == config.Provider.Type {
			// Reuse the exact wrapper just built above so identity is
			// preserved across loop.Provider and loop.Providers[default]
			// — some call sites compare by pointer.
			wrappedProviders[name] = prov
			continue
		}
		// The map key may differ from the declared Type discriminator;
		// fall back to the key when no config entry is found.
		providerType := name
		if cfg, ok := config.Providers[name]; ok {
			providerType = cfg.Type
		}
		wrappedProviders[name] = provider.NewNormalizingAdapter(p, providerType)
	}
	providers = wrappedProviders

	// Tool-choice escalation policy: off by default (nil) unless the
	// operator opts in via RunConfig.ToolChoiceEscalation.
	escalation := buildEscalationPolicy(config.EffectiveToolChoiceEscalationMaxRetries(), prov)

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
		GuardRail:    gr,
		RuleOfTwo:    rot,
		Escalation:   escalation,
		Hooks:        hooksRunner,
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

		// The ask-upstream policy snapshots the approval-required tool set
		// at construction time, before spawn_agent is registered; refresh
		// it here so spawn_agent is gated rather than auto-allowed. See
		// TestApprovalRequiredToolSet.
		addApprovalTool(pp, "spawn_agent")
	}

	// Apply the toolset-profile presentation last, after every tool
	// (built-ins, MCP, spawn_agent) is registered, so the alias mapping
	// covers the complete set. The presenter wraps only the loop's
	// List/Resolve seam — permission policy and dispatch keep operating
	// on the raw registry and internal tool IDs. ValidateRunConfig
	// already validated the profile name, so a false here is a
	// build-time bug worth failing loudly on.
	profile, ok := tool.ProfileFor(config.Tools.Profile)
	if !ok {
		cleanup()
		return nil, fmt.Errorf("tools.profile %q has no presentation table", config.Tools.Profile)
	}
	presenter, err := tool.NewPresenter(registry, profile)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("build tool profile presenter: %w", err)
	}
	loop.Tools = presenter
	loop.ToolProfile = profile

	return loop, nil
}

// buildEscalationPolicy constructs the tool-choice escalation policy
// injected into the loop. maxRetries <= 0 returns nil (off by default);
// the resolver is quirks.DefaultRegistry() since tool-choice support is
// a cross-provider capability no compat profile overrides. The _
// provider argument is reserved for a future per-provider registry.
func buildEscalationPolicy(maxRetries int, _ provider.ProviderAdapter) EscalationPolicy {
	if maxRetries <= 0 {
		return nil
	}
	return newDefaultEscalationPolicy(maxRetries, quirks.DefaultRegistry())
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

	// Single point where RunConfig's retry config becomes a provider-level
	// RetryPolicy; RetryPolicyFromConfig handles a nil cfg.Retry
	// defensively for callers that bypass ValidateRunConfig.
	retry := provider.RetryPolicyFromConfig(cfg.Retry)

	switch cfg.Type {
	case "anthropic":
		if cred.BearerToken == nil {
			return nil, fmt.Errorf("anthropic provider requires a bearer credential but the credential source produced none")
		}
		// Static API keys (sk-ant-api03-...) ride x-api-key; WIF OAuth
		// access tokens (sk-ant-oat01-...) require Authorization: Bearer.
		authMode := provider.AuthModeAPIKey
		if cfg.Credential != nil && cfg.Credential.Type == "anthropic-wif" {
			authMode = provider.AuthModeBearer
		}
		adapter := provider.NewAnthropicAdapter(cred.BearerToken, authMode)
		adapter.RetryPolicy = retry
		return adapter, nil
	case "openai-compatible":
		if cred.BearerToken == nil {
			return nil, fmt.Errorf("openai-compatible provider requires a bearer credential but the credential source produced none")
		}
		auth := provider.OpenAIAuthConfig{
			APIKeyHeader: cfg.APIKeyHeader,
			QueryParams:  cfg.QueryParams,
		}
		adapter := provider.NewOpenAICompatibleAdapter(cred.BearerToken, cfg.BaseURL, auth, retry)
		// Compat rules are appended after BuiltinRules so their
		// specificity ordering wins against any overlapping glob.
		if cfg.CompatProfile != "" {
			extra, err := resolveCompatProfile(cfg.CompatProfile)
			if err != nil {
				return nil, fmt.Errorf("resolve compat profile: %w", err)
			}
			// BuiltinRules() returns a fresh slice each call, so this
			// cannot mutate the shared catalogue other adapters use.
			rules := append(quirks.BuiltinRules(), extra...)
			adapter.Registry = quirks.NewRegistry(rules)
		}
		return adapter, nil
	case "openai-responses":
		if cred.BearerToken == nil {
			return nil, fmt.Errorf("openai-responses provider requires a bearer credential but the credential source produced none")
		}
		auth := provider.OpenAIAuthConfig{
			APIKeyHeader: cfg.APIKeyHeader,
			QueryParams:  cfg.QueryParams,
		}
		adapter := provider.NewOpenAIResponsesAdapter(cred.BearerToken, cfg.BaseURL, auth)
		adapter.RetryPolicy = retry
		return adapter, nil
	case "bedrock":
		return provider.NewBedrockAdapter(cfg.Region, cfg.Profile, cred.AWSCredentials, retry)
	case "gemini":
		if cred.BearerToken == nil {
			return nil, fmt.Errorf("gemini provider requires GCP credentials but the credential source produced none")
		}
		// No compat profiles for Gemini in v1; registry defaults to DefaultRegistry().
		adapter := provider.NewGeminiAdapter(cred.BearerToken, cfg.GCPProject, cfg.GCPLocation, cfg.GeminiSafetySettings)
		adapter.RetryPolicy = retry
		return adapter, nil
	default:
		return nil, fmt.Errorf("unsupported provider type: %q (supported: anthropic, bedrock, gemini, openai-compatible, openai-responses)", cfg.Type)
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
		return router.NewStaticRouter(defaultProvider, types.DefaultModel)
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
		defaultModel = types.DefaultModel
	}
	defaultSel := router.ModelSelection{Provider: defaultProvider, Model: defaultModel}

	modeMap := make(map[string]router.ModelSelection, len(cfg.ModeModels))
	for mode, spec := range cfg.ModeModels {
		if p, m, ok := strings.Cut(spec, "/"); ok {
			modeMap[mode] = router.ModelSelection{Provider: p, Model: m}
		} else {
			// No slash: use the default provider.
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
		defaultModel = types.DefaultModel
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
		expensiveModel = types.DefaultModel
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

// buildPromptBuilder selects the prompt preamble source. Precedence:
// systemPromptOverride (raw, never template-parsed), then an operator
// template (promptBuilder.template), then the embedded mode templates.
// ValidateRunConfig rejects the override combined with either template
// field, so the precedence never silently discards operator input. The
// operator template is trial-rendered against the run's resolved prompt
// model and mode — the exact render the loop will perform — so template
// execution errors fail at construction.
func buildPromptBuilder(config *types.RunConfig) (prompt.PromptBuilder, error) {
	if config.SystemPromptOverride != "" {
		return prompt.NewOverridePromptBuilder(config.SystemPromptOverride), nil
	}
	if tmpl := config.PromptBuilder.Template; tmpl != "" {
		return prompt.NewTemplatePromptBuilder(tmpl, prompt.TemplateData{
			Model: config.EffectivePromptModel(),
			Mode:  config.Mode,
		})
	}
	switch config.PromptBuilder.Type {
	case "composed":
		return prompt.NewComposedPromptBuilder(
			prompt.WithFragments(prompt.DefaultComposedFragments()...),
		), nil
	case "default", "":
		return prompt.NewDefaultPromptBuilder(), nil
	default:
		return prompt.NewDefaultPromptBuilder(), nil
	}
}

// wrapContextStrategy wraps the constructed ContextStrategy with a
// metric recorder using the configured strategy name as the label. An
// empty cfg.Type maps to "sliding-window" (the default constructor
// branch), matching the behaviour of buildContextStrategy.
func wrapContextStrategy(cs contextpkg.ContextStrategy, cfg types.ContextStrategyConfig, metrics *observability.Metrics) contextpkg.ContextStrategy {
	if metrics == nil || cs == nil {
		return cs
	}
	name := cfg.Type
	if name == "" {
		name = "sliding-window"
	}
	return contextpkg.NewMetricRecorder(cs, metrics, name)
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

func buildExecutor(ctx context.Context, cfg types.ExecutorConfig, secrets security.SecretStore, secLogger *security.SecurityLogger) (executor.Executor, error) {
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
		return executor.NewContainerExecutorWithContext(ctx, executor.ContainerExecutorConfig{
			Image:             cfg.Image,
			HostDir:           workspace,
			Network:           cfg.Network,
			Resources:         cfg.Resources,
			Runtime:           cfg.Runtime,
			RegistryAllowlist: cfg.RegistryAllowlist,
			EgressSecurity:    secLogger,
		})
	case "k8s", "k8s-sandbox":
		// Both types share the K8s* config surface and differ only in
		// how the sandbox Pod is created: "k8s" manages the Pod
		// directly, "k8s-sandbox" provisions it via the Agent Sandbox
		// CRD (gVisor-only). The guards below are defence-in-depth for
		// callers that bypass ValidateRunConfig.
		if cfg.Image == "" {
			return nil, fmt.Errorf("%s executor requires image", cfg.Type)
		}
		if cfg.K8sNamespace == "" {
			return nil, fmt.Errorf("%s executor requires k8sNamespace", cfg.Type)
		}
		k8sCfg := executor.K8sExecutorConfig{
			Image:              cfg.Image,
			Namespace:          cfg.K8sNamespace,
			Kubeconfig:         cfg.K8sKubeconfig,
			NodeSelector:       cfg.K8sNodeSelector,
			RuntimeClassName:   cfg.Runtime,
			ServiceAccountName: cfg.K8sServiceAccount,
			Resources:          cfg.Resources,
			Network:            cfg.Network,
			EgressProxyURL:     cfg.K8sEgressProxyURL,
			Security:           secLogger,
		}
		if cfg.Type == "k8s-sandbox" {
			return executor.NewAgentSandboxExecutor(ctx, k8sCfg)
		}
		return executor.NewK8sExecutor(ctx, k8sCfg)
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
	case "none":
		return executor.NewNoneExecutor(), nil
	default:
		return nil, fmt.Errorf("unsupported executor type: %q (supported: local, container, k8s, k8s-sandbox, api, none)", cfg.Type)
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
	// find_files is pure Go and never shells out; grep_files's native
	// walker only needs read access (its ripgrep fast path checks
	// CanExec internally). Gating on CanRead means a read-only sandboxed
	// executor still gets working content/name search.
	if toolEnabled(cfg.BuiltIn, "grep_files") && caps.CanRead {
		registry.Register(builtins.GrepFilesTool(exec))
	}
	if toolEnabled(cfg.BuiltIn, "find_files") && caps.CanRead {
		registry.Register(builtins.FindFilesTool(exec))
	}
	// The four git_* tools are gated on CanRead (they're read-only) even
	// though every call shells out via exec.Exec: on a CanRead-but-not-
	// CanExec executor they still register but fail with a clear "git is
	// not available" error rather than silently disappearing from the
	// tool list the model was told to expect.
	if toolEnabled(cfg.BuiltIn, "git_status") && caps.CanRead {
		registry.Register(builtins.GitStatusTool(exec))
	}
	if toolEnabled(cfg.BuiltIn, "git_changed_files") && caps.CanRead {
		registry.Register(builtins.GitChangedFilesTool(exec))
	}
	if toolEnabled(cfg.BuiltIn, "git_diff") && caps.CanRead {
		registry.Register(builtins.GitDiffTool(exec))
	}
	if toolEnabled(cfg.BuiltIn, "git_show") && caps.CanRead {
		registry.Register(builtins.GitShowTool(exec))
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
	// Carry the strategy's worked example (#222) onto the registered tool so
	// Definition() folds it into the schema where the provider supports it.
	// The strategy owns the example next to its description; nil Presentation
	// (strategies without an example) leaves InputExamples unset.
	var inputExamples []json.RawMessage
	if definition.Presentation != nil {
		inputExamples = definition.Presentation.InputExamples
	}
	return &tool.Tool{
		Name:              definition.Name,
		Description:       definition.Description,
		InputSchema:       definition.InputSchema,
		InputExamples:     inputExamples,
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

// defaultRuleOfTwoGuardCriteria is the guard-criteria set used when
// ruleOfTwo.runtime.guardCriteria is unset: the two built-in criteria
// an LLM guard is expected to name when it spots sensitive content.
var defaultRuleOfTwoGuardCriteria = []string{"sensitive_data", "pii"}

// defaultRuleOfTwoAction is the documented onDetect default, enforced
// via the permission gate (block-external) when the arming matrix arms
// the run.
const defaultRuleOfTwoAction = "block-external"

// ruleOfTwoArming is the factory's arming decision for the Rule-of-Two
// runtime monitor, computed once per run from the static Rule-of-Two
// state and the operator's ruleOfTwo.runtime block. Default arming is
// deliberately factory behaviour, not config mutation — the validator
// never injects a Runtime block, so the Redact()-persisted config
// reflects exactly what the operator declared.
type ruleOfTwoArming struct {
	armed           bool
	enforcing       bool
	action          string
	criteria        []string
	classifier      string
	staticSensitive bool
}

// resolveRuleOfTwoArming computes the arming matrix (u = holdsUntrusted,
// e = canCommExternal, s = static sensitive declaration). See "When the
// classifier arms" in docs/safety-rings.md for the full decision table.
func resolveRuleOfTwoArming(config *types.RunConfig) ruleOfTwoArming {
	if config == nil {
		return ruleOfTwoArming{}
	}
	classifier := ""
	action := defaultRuleOfTwoAction
	criteria := defaultRuleOfTwoGuardCriteria
	enforceOverridden := false
	if config.RuleOfTwo != nil {
		enforceOverridden = config.RuleOfTwo.Enforce != nil && !*config.RuleOfTwo.Enforce
		if rt := config.RuleOfTwo.Runtime; rt != nil {
			classifier = rt.Classifier
			if rt.OnDetect != "" {
				action = rt.OnDetect
			}
			if len(rt.GuardCriteria) > 0 {
				criteria = rt.GuardCriteria
			}
		}
	}
	if classifier == "none" {
		return ruleOfTwoArming{}
	}
	u, s, e := types.RuleOfTwoState(config)
	switch {
	case u && e:
		enforcing := !s && config.PermissionPolicy.Type != "ask-upstream" && !enforceOverridden
		return ruleOfTwoArming{
			armed:           true,
			enforcing:       enforcing,
			action:          action,
			criteria:        criteria,
			classifier:      "patterns",
			staticSensitive: s,
		}
	case classifier == "patterns":
		// staticSensitive stays false here even if sensitiveData was
		// declared: a pre-tripped latch would skip scanning and yield
		// zero detection telemetry, defeating an explicit request for
		// pattern scanning. It's only meaningful on the enforcing
		// u&&e path above.
		return ruleOfTwoArming{
			armed:           true,
			enforcing:       false,
			action:          action,
			criteria:        criteria,
			classifier:      "patterns",
			staticSensitive: false,
		}
	default:
		return ruleOfTwoArming{}
	}
}

// buildRuleOfTwoMonitor maps an arming decision onto a Monitor. Noop
// for unarmed runs so the loop's call sites stay unconditional.
func buildRuleOfTwoMonitor(arming ruleOfTwoArming) ruleoftwo.Monitor {
	if !arming.armed {
		return ruleoftwo.NewNoop()
	}
	return ruleoftwo.NewPatternMonitor(arming.enforcing, arming.action, arming.criteria, arming.staticSensitive)
}

// emitRuleOfTwoEvents records the Rule-of-Two security audit events at
// run start (rule_of_two_runtime_armed, rule_of_two_disabled,
// rule_of_two_warning); see docs/safety-rings.md.
func emitRuleOfTwoEvents(config *types.RunConfig, sec *security.SecurityLogger, arming ruleOfTwoArming) {
	if sec == nil || config == nil {
		return
	}
	if arming.armed {
		effectiveAction := arming.action
		if !arming.enforcing {
			effectiveAction = "warn"
		}
		sec.Emit("info", "rule_of_two_runtime_armed", map[string]any{
			"classifier": arming.classifier,
			"onDetect":   effectiveAction,
			"enforcing":  arming.enforcing,
		})
	}
	u, s, e := types.RuleOfTwoState(config)

	if u && s && e {
		// All three hold only via ask-upstream or an explicit override;
		// only the override case is interesting for audit.
		if config.RuleOfTwo != nil && config.RuleOfTwo.Enforce != nil && !*config.RuleOfTwo.Enforce {
			sec.Emit("warn", "rule_of_two_disabled", map[string]any{
				"reason":                "operator override via RuleOfTwo.Enforce: false",
				"untrustedInput":        u,
				"sensitiveData":         s,
				"externalCommunication": e,
			})
		}
		return
	}

	// Exactly two hold: structural warning so reviewers can spot drift.
	count := 0
	if u {
		count++
	}
	if s {
		count++
	}
	if e {
		count++
	}
	if count == 2 {
		sec.Emit("warn", "rule_of_two_warning", map[string]any{
			"untrustedInput":        u,
			"sensitiveData":         s,
			"externalCommunication": e,
		})
	}
}

// wrapWithCodeScanner builds a CodeScanner from cfg and wraps inner with a
// ScannedStrategy when scanning is enabled. A nil cfg, an empty Type, or
// Type=="none" short-circuits and returns inner unchanged so the no-scan
// path has zero overhead. The supplied emitter receives code_scan_warning
// events on warn findings; pass nil to disable security event emission
// (warnings still log via slog).
func wrapWithCodeScanner(inner edit.EditStrategy, cfg *types.CodeScannerConfig, emitter edit.SecurityEventEmitter) (edit.EditStrategy, error) {
	if cfg == nil || cfg.Type == "" || cfg.Type == "none" {
		return inner, nil
	}
	scanner, err := codescanner.New(cfg)
	if err != nil {
		return nil, err
	}
	return edit.NewScannedStrategy(inner, scanner, cfg, emitter), nil
}

// addApprovalTool routes an approval-tool registration to the
// underlying *AskUpstreamPolicy, walking through any metric-recorder
// wrapper via permission.Unwrap. Returns true when the registration
// landed on an ask-upstream policy.
func addApprovalTool(pp permission.PermissionPolicy, name string) bool {
	if ask, ok := permission.Unwrap(pp).(*permission.AskUpstreamPolicy); ok {
		ask.AddApprovalTool(name)
		return true
	}
	return false
}

// wireEditMetrics field-injects metrics into a *MultiStrategy or
// *ScannedStrategy(*MultiStrategy) chain. Direct strategies (whole-file,
// search-replace, udiff, ScannedStrategy(direct)) carry no per-attempt
// metric of their own — the edit_file tool path covers them at the loop
// level — so the only writable target here is *MultiStrategy. Walking
// through the ScannedStrategy wrapper means scanned + multi runs are
// instrumented end-to-end without changing public APIs.
func wireEditMetrics(es edit.EditStrategy, metrics *observability.Metrics) {
	if metrics == nil || es == nil {
		return
	}
	// Always wire Scanned wrapper first so codescanner metrics fire,
	// then recurse into its inner strategy for Multi.
	if scanned, ok := es.(*edit.ScannedStrategy); ok {
		scanned.Metrics = metrics
		wireEditMetrics(scanned.Inner(), metrics)
		return
	}
	if multi, ok := es.(*edit.MultiStrategy); ok {
		multi.Metrics = metrics
	}
}

// buildEditStrategy maps the (already-defaulted) EditStrategyConfig to
// a concrete EditStrategy. types.ValidateRunConfig is the single
// normalization point that fills an empty Type with "multi", so the
// empty-string case is never expected here; the default branch routes
// to MultiStrategy as a defence-in-depth fallback for callers that
// bypass validation entirely.
func buildEditStrategy(cfg types.EditStrategyConfig) edit.EditStrategy {
	fuzzyThreshold := 0.80
	if cfg.FuzzyThreshold != nil {
		fuzzyThreshold = *cfg.FuzzyThreshold
	}
	// A caller that bypasses ValidateRunConfig can reach here with a
	// value <= 0, which defeats udiff's fuzzy-match sentinel check
	// (findFuzzyMatch's zero value passes an unguarded `>= 0` comparison)
	// and panics on a negative slice index. Clamp instead.
	if fuzzyThreshold <= 0 || fuzzyThreshold > 1 {
		slog.Default().Warn("edit strategy fuzzyThreshold out of range; falling back to default",
			slog.Float64("attempted_fuzzy_threshold", fuzzyThreshold),
			slog.Float64("selected_fuzzy_threshold", 0.80),
			slog.String("hint", "route the RunConfig through types.ValidateRunConfig to reject out-of-range values"),
		)
		fuzzyThreshold = 0.80
	}

	switch cfg.Type {
	case "whole-file":
		return edit.NewWholeFileStrategy()
	case "search-replace":
		return edit.NewSearchReplaceStrategy()
	case "udiff":
		return edit.NewUdiffStrategy(fuzzyThreshold)
	case "multi":
		return edit.NewMultiStrategy(fuzzyThreshold)
	default:
		// Reached only by callers that bypass ValidateRunConfig. Uses
		// slog.Default() rather than a threaded logger: this call site
		// precedes structured-logger construction.
		slog.Default().Warn("unknown edit strategy type; falling back to multi",
			slog.String("attempted_type", cfg.Type),
			slog.String("selected_type", "multi"),
			slog.String("hint", "route the RunConfig through types.ValidateRunConfig to normalize EditStrategy.Type"),
		)
		return edit.NewMultiStrategy(fuzzyThreshold)
	}
}

// buildVerifier constructs a Verifier from cfg. Each leaf (and the
// composite at every level) is wrapped with verifier.NewMetricRecorder
// when metrics is non-nil; metrics=nil skips wrapping (used before the
// run's Metrics instance exists, ahead of a later rebuild).
func buildVerifier(cfg types.VerifierConfig, prov provider.ProviderAdapter, metrics *observability.Metrics) verifier.Verifier {
	switch cfg.Type {
	case "composite":
		subs := make([]verifier.Verifier, len(cfg.Verifiers))
		for i, sub := range cfg.Verifiers {
			subs[i] = buildVerifier(sub, prov, metrics)
		}
		return verifier.NewMetricRecorder(verifier.NewCompositeVerifier(subs), metrics, "composite")
	case "llm-judge":
		model := cfg.Model
		if model == "" {
			model = "claude-haiku-4-5-20251001"
		}
		return verifier.NewMetricRecorder(verifier.NewLLMJudgeVerifier(prov, model, cfg.Criteria), metrics, "llm-judge")
	case "test-runner":
		timeout := time.Duration(cfg.Timeout) * time.Second
		return verifier.NewMetricRecorder(verifier.NewTestRunnerVerifier(cfg.Command, timeout), metrics, "test-runner")
	case "none", "":
		return verifier.NewMetricRecorder(verifier.NewNoneVerifier(), metrics, "none")
	default:
		return verifier.NewMetricRecorder(verifier.NewNoneVerifier(), metrics, "none")
	}
}

// buildGuardRail constructs the operator-configured GuardRail. A nil cfg
// or an explicit "none" type returns the package-level Noop so the
// loop's call sites can be unconditional. Cloud-judge reuses the default
// provider adapter; granite-guardian builds its own HTTP client. The
// outer Phases gate (when non-empty) is applied after the inner guard
// is built so a misconfigured PhaseGated does not silently bypass the
// guard at non-listed phases.
func buildGuardRail(cfg *types.GuardRailConfig, providers map[string]provider.ProviderAdapter, defaultProvider provider.ProviderAdapter) (guard.GuardRail, error) {
	if cfg == nil || cfg.Type == "" || cfg.Type == "none" {
		return guard.NewNoop(), nil
	}
	return buildGuardRailNode(cfg, providers, defaultProvider)
}

// buildGuardRailNode is the recursive worker for buildGuardRail. It
// rejects "composite" containing another "composite" implicitly by
// relying on ValidateRunConfig (which forbids composite-of-composite at
// config validation time) — but we still defend in depth by returning
// an error for any unsupported type so a non-CLI caller bypassing
// validation gets a clear diagnostic instead of a silent allow.
func buildGuardRailNode(cfg *types.GuardRailConfig, providers map[string]provider.ProviderAdapter, defaultProvider provider.ProviderAdapter) (guard.GuardRail, error) {
	switch cfg.Type {
	case "none":
		return guard.NewNoop(), nil
	case "granite-guardian":
		// FailOpen lives at the GuardRailConfig level (consulted via
		// guardFailOpen() in the loop). The adapter does not read it.
		gg, err := guard.NewGraniteGuardian(guard.GraniteGuardianConfig{
			Endpoint:       cfg.Endpoint,
			Model:          cfg.Model,
			Criteria:       cfg.Criteria,
			CustomCriteria: cfg.CustomCriteria,
			Threshold:      cfg.Threshold,
			Think:          cfg.Think != nil && *cfg.Think,
			Timeout:        time.Duration(cfg.TimeoutMs) * time.Millisecond,
			MinChunkChars:  cfg.MinChunkChars,
		})
		if err != nil {
			return nil, err
		}
		return wrapWithPhases(gg, cfg.Phases), nil
	case "cloud-judge":
		// v1: always use the default provider. A future revision could
		// route to a named provider in `providers` based on a future
		// GuardRailConfig.Provider field.
		_ = providers
		// FailOpen lives at the GuardRailConfig level (consulted via
		// guardFailOpen() in the loop). The adapter does not read it.
		cj, err := guard.NewCloudJudge(guard.CloudJudgeConfig{
			Provider: defaultProvider,
			Model:    cfg.Model,
			Timeout:  time.Duration(cfg.TimeoutMs) * time.Millisecond,
		})
		if err != nil {
			return nil, err
		}
		return wrapWithPhases(cj, cfg.Phases), nil
	case "composite":
		guards := make([]guard.GuardRail, 0, len(cfg.Stages))
		for i := range cfg.Stages {
			stage, err := buildGuardRailNode(&cfg.Stages[i], providers, defaultProvider)
			if err != nil {
				return nil, fmt.Errorf("composite stage %d: %w", i, err)
			}
			guards = append(guards, stage)
		}
		seq := &guard.Sequential{Guards: guards, ID: "composite"}
		return wrapWithPhases(seq, cfg.Phases), nil
	default:
		return nil, fmt.Errorf("unsupported guardRail.type %q", cfg.Type)
	}
}

// wrapWithPhases applies a PhaseGated wrapper when phases is non-empty.
// An empty phases slice means "the guard runs on every phase" and
// returns the inner guard unchanged — this matches the operator-friendly
// reading of GuardRailConfig.Phases (default = all three) and avoids
// the PhaseGated trap where an empty Phases slice silently disables
// the guard.
func wrapWithPhases(g guard.GuardRail, phases []string) guard.GuardRail {
	if len(phases) == 0 {
		return g
	}
	parsed := make([]guard.Phase, 0, len(phases))
	for _, p := range phases {
		parsed = append(parsed, guard.Phase(p))
	}
	return &guard.PhaseGated{Phases: parsed, Inner: g}
}

// buildPermissionPolicy constructs the configured PermissionPolicy. The
// returned policy is raw — callers wanting metric instrumentation
// compose it through wrapPermissionPolicyMetrics — so the policy-engine
// arm's Cedar file is read exactly once rather than re-read on a second
// build call, which previously opened a TOCTOU window on
// workspace-relative paths (CWE-367).
//
// The FallbackBuilder closure re-routes a policy-engine's declared
// fallback type through this same switch, so e.g. fallback="ask-upstream"
// behaves identically to top-level ask-upstream when Cedar abstains.
func buildPermissionPolicy(config *types.RunConfig, registry *tool.Registry, tp transport.Transport, secLogger *security.SecurityLogger) (permission.PermissionPolicy, error) {
	cfg := config.PermissionPolicy
	switch cfg.Type {
	case "allow-all":
		return permission.NewAllowAll(), nil
	case "deny-side-effects":
		// DenySideEffects rejects only tools that mutate workspace
		// state. Tools whose only sensitivity is "operator should
		// approve" (web_fetch, spawn_agent) are still allowed —
		// research-mode users explicitly enable them.
		return permission.NewDenySideEffects(mutatingToolSet(registry)), nil
	case "ask-upstream":
		// AskUpstreamPolicy prompts on tools whose RequiresApproval
		// flag is set. This includes mutating tools but also covers
		// non-mutating-but-sensitive tools.
		timeout := time.Duration(cfg.Timeout) * time.Second
		return permission.NewAskUpstreamPolicy(tp, approvalRequiredToolSet(registry), timeout), nil
	case "policy-engine":
		env := permission.PolicyEngineEnv{
			RunID:     config.RunID,
			Mode:      config.Mode,
			Workspace: config.Executor.Workspace,
			// Cedar exposes context.dynamicContext as a Record of
			// String → String; per-entry sensitivity is not wired in yet.
			DynamicContext: config.DynamicContextValues(),
			Security:       secLogger,
			// ParentRunID and Capabilities are reserved for sub-agent
			// wiring, populated by the spawn_agent path in a future wave.
		}
		// fallback re-routes a fallback type name through this same switch.
		fallback := func(typeName string) (permission.PermissionPolicy, error) {
			if typeName == "policy-engine" {
				return nil, fmt.Errorf("policy-engine fallback may not itself be policy-engine")
			}
			fallbackCfg := types.PermissionPolicyConfig{
				Type:    typeName,
				Timeout: cfg.Timeout,
			}
			return buildPermissionPolicy(&types.RunConfig{
				PermissionPolicy: fallbackCfg,
				// The recursive call uses the same identity context;
				// only the Type is different.
				RunID:          config.RunID,
				Mode:           config.Mode,
				Executor:       config.Executor,
				DynamicContext: config.DynamicContext,
				SensitiveData:  config.SensitiveData,
			}, registry, tp, secLogger)
		}
		policy, err := permission.New(cfg, env, fallback)
		if err != nil {
			return nil, err
		}
		return policy, nil
	default:
		// Explicit error, not an allow-all fallback.
		return nil, fmt.Errorf("unsupported permissionPolicy.type %q", cfg.Type)
	}
}

// wrapPermissionPolicyMetrics wraps an already-built PermissionPolicy
// with a metric recorder labelled with the configured policy type. A
// nil metrics argument or an empty cfg.Type returns pp unchanged.
func wrapPermissionPolicyMetrics(pp permission.PermissionPolicy, cfg types.PermissionPolicyConfig, metrics *observability.Metrics) permission.PermissionPolicy {
	if pp == nil || metrics == nil || cfg.Type == "" {
		return pp
	}
	return permission.NewMetricRecorder(pp, metrics, cfg.Type)
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

// externalCommToolSet returns the names of registered tools that can
// move data outside the harness sandbox: run_command (shell egress),
// web_fetch (arbitrary HTTP), and every MCP-imported tool (the remote
// server receives each call's payload). The Rule-of-Two gate revokes
// exactly this set once the sensitive-data latch trips. Keys are
// internal tool IDs — the Presenter never rewrites dispatch names, so
// toolset-profile aliases cannot bypass the set.
func externalCommToolSet(registry *tool.Registry) map[string]bool {
	out := make(map[string]bool)
	for _, td := range registry.List() {
		if td.Name == "run_command" || td.Name == "web_fetch" || strings.HasPrefix(td.Name, "mcp_") {
			out[td.Name] = true
		}
	}
	return out
}

// wrapRuleOfTwoGate installs the Rule-of-Two enforcement gate around
// the permission policy when the runtime monitor is armed and
// enforcing with a gate-delivered action. redact and abort are
// loop-level actions with no permission-layer component, and
// observe-only monitors ("warn") never gate — those return pp
// unchanged. For onDetect=ask-upstream the AskUpstreamPolicy is
// pre-built eagerly here, idle until the latch trips, so enforcement
// time never constructs components; the validator already pinned
// transport=grpc for that action.
func wrapRuleOfTwoGate(pp permission.PermissionPolicy, monitor ruleoftwo.Monitor, arming ruleOfTwoArming, registry *tool.Registry, cfg *types.RunConfig, tp transport.Transport, metrics *observability.Metrics) permission.PermissionPolicy {
	if !arming.armed || !arming.enforcing {
		return pp
	}
	switch arming.action {
	case "block-external":
		return permission.NewRuleOfTwoGate(pp, monitor, externalCommToolSet(registry), nil, metrics)
	case "ask-upstream":
		// One external-comm set shared by the ask policy and the gate:
		// both must cover the same tools, and a single allocation keeps
		// them from silently diverging.
		extSet := externalCommToolSet(registry)
		timeout := time.Duration(cfg.PermissionPolicy.Timeout) * time.Second
		ask := permission.NewAskUpstreamPolicy(tp, extSet, timeout)
		return permission.NewRuleOfTwoGate(pp, monitor, extSet, ask, metrics)
	default:
		return pp
	}
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

// buildHookRunner constructs the lifecycle hook runner. Returns
// hook.Noop when cfg is nil or configures no hooks in either phase.
// exec is the run's own Executor, so hooks share its sandbox and
// egress posture with every agent tool call.
func buildHookRunner(cfg *types.HooksConfig, exec executor.Executor, logger *slog.Logger) hook.Runner {
	if cfg == nil || (len(cfg.PreRun) == 0 && len(cfg.PostRun) == 0) {
		return hook.NewNoop()
	}
	return &hook.ExecRunner{Hooks: cfg, Exec: exec, Logger: logger}
}

// resourceOptionsFromConfig assembles the OTel ResourceOptions for this
// run. The env-var/default precedence chain lives in
// observability.BuildResource; this just plumbs explicit values through.
func resourceOptionsFromConfig(cfg *types.RunConfig) observability.ResourceOptions {
	if cfg == nil {
		return observability.ResourceOptions{}
	}
	return observability.ResourceOptions{
		Environment:      cfg.Observability.Environment,
		ServiceNamespace: cfg.Observability.ServiceNamespace,
		RunMode:          cfg.Mode,
	}
}

func buildTraceEmitter(ctx context.Context, cfg types.TraceEmitterConfig, headers map[string]string, resourceOpts observability.ResourceOptions) (trace.TraceEmitter, error) {
	switch cfg.Type {
	case "otel":
		endpoint := cfg.Endpoint
		if endpoint == "" {
			endpoint = "localhost:4317"
		}
		return trace.NewOTelTraceEmitter(ctx, endpoint, cfg.Protocol, headers, resourceOpts, cfg.CaptureContent)
	case "gcs":
		// Default credential is gcp-workload-identity; see docs/cloud-run-jobs.md.
		credSrc, err := buildGCSTraceCredentialSource(cfg.Credential)
		if err != nil {
			return nil, fmt.Errorf("gcs trace emitter credential: %w", err)
		}
		return trace.NewGCSTraceEmitter(ctx, trace.GCSTraceEmitterOptions{
			Bucket:           cfg.Bucket,
			ObjectPrefix:     cfg.ObjectPrefix,
			CredentialSource: credSrc,
		})
	case "jsonl", "":
		var w io.Writer
		if cfg.FilePath != "" {
			f, err := os.OpenFile(cfg.FilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return nil, fmt.Errorf("open trace file: %w", err)
			}
			w = f
		} else {
			// No path: discard.
			w = &bytes.Buffer{}
		}
		return trace.NewJSONLTraceEmitter(w), nil
	default:
		return nil, fmt.Errorf("unsupported trace emitter type: %q (supported: jsonl, otel, gcs)", cfg.Type)
	}
}

// buildGCSTraceCredentialSource resolves the credential.Source for the
// gcs trace emitter; see docs/cloud-run-jobs.md. Defaults to
// gcp-workload-identity when TraceEmitterConfig.Credential is nil. Only
// GCP-shaped credential types are accepted — AWS/Azure/Anthropic-WIF
// are rejected with a clear error rather than a 401 from the GCS API.
func buildGCSTraceCredentialSource(cfg *types.CredentialConfig) (credential.Source, error) {
	if cfg == nil {
		return credential.NewGoogleWorkloadIdentitySource(), nil
	}
	switch cfg.Type {
	case "", "gcp-workload-identity":
		return credential.NewGoogleWorkloadIdentitySource(), nil
	case "gcp-default":
		// GCP Application Default Credentials. Useful for local dev
		// where GOOGLE_APPLICATION_CREDENTIALS points at a service
		// account JSON key.
		return &credential.GoogleADCSource{}, nil
	case "gcp-service-account":
		// The trace emitter has no field equivalent to
		// Provider.GCPCredentialsFile; surface the gap explicitly
		// rather than silently no-opping.
		return nil, fmt.Errorf(
			"credential.type=%q is not supported for the gcs trace emitter today; "+
				"use \"gcp-workload-identity\" on GCP runtimes or \"gcp-default\" with "+
				"GOOGLE_APPLICATION_CREDENTIALS set to a service-account JSON key",
			cfg.Type)
	default:
		return nil, fmt.Errorf(
			"credential.type=%q is not supported for the gcs trace emitter; "+
				"expected \"gcp-workload-identity\" or \"gcp-default\"",
			cfg.Type)
	}
}

// resolveCompatProfile maps the closed enum of supported
// compatProfile names to the rules the corresponding compat package
// exports. The set must stay aligned with validCompatProfiles in
// types/runconfig.go — ValidateRunConfig rejects unknown values at
// startup, so an unknown value reaching this switch is a defence-in-
// depth signal that the validator was bypassed (e.g. by a non-CLI
// caller).
//
// New entries here require a corresponding compat package under
// harness/internal/provider/compat/<name>/ and an addition to
// validCompatProfiles. Adding a name to the validator without a rule
// set would silently no-op for runs that selected the new profile.
func resolveCompatProfile(profile string) ([]quirks.Rule, error) {
	switch profile {
	case "zai-glm":
		return zai.CompatRules(), nil
	default:
		return nil, fmt.Errorf("unknown compat profile %q (supported: zai-glm)", profile)
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
