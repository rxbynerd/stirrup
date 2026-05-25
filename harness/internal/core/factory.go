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
	"github.com/rxbynerd/stirrup/harness/internal/mcp"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/provider/compat/zai"
	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/security/codescanner"
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

	// Emit Rule-of-Two audit events. The validator already accepted the
	// config, so any all-three case here implies an explicit operator
	// override (RuleOfTwo.Enforce: false) or the ask-upstream policy.
	// Recording the event keeps the override auditable; the two-of-three
	// warning surfaces a heads-up that future capability creep would
	// trip the invariant.
	emitRuleOfTwoEvents(config, secLogger)

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
	// Thread the security logger into the container executor so the
	// in-process egress proxy (started when network.mode == "allowlist")
	// can emit egress_allowed / egress_blocked events through the same
	// SecurityLogger used for path/file events.
	exec, err := buildExecutor(ctx, config.Executor, secrets, secLogger)
	if err != nil {
		return nil, fmt.Errorf("build executor: %w", err)
	}
	if closer, ok := exec.(io.Closer); ok {
		ownedClosers = append(ownedClosers, closer)
	}

	// 5. Context strategy.
	cs := buildContextStrategy(config.ContextStrategy, prov, config.ModelRouter.Model, exec)

	// 6. Tool registry.
	// The base edit strategy is constructed first, then optionally wrapped
	// with a CodeScanner pass when one is configured. ValidateRunConfig
	// fills CodeScanner with a sensible default per mode (patterns for
	// execution, none for read-only) so cfg.CodeScanner is never nil at
	// this point — but defend in depth in case a non-CLI caller passes a
	// raw RunConfig that bypasses that defaulting.
	es := buildEditStrategy(config.EditStrategy)
	es, err = wrapWithCodeScanner(es, config.CodeScanner, secLogger)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("build code scanner: %w", err)
	}
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
	if config.SessionName != "" {
		// Attach SessionName as a default log attribute so every line
		// emitted from this loop (and any sub-loop sharing this logger)
		// carries the operator-supplied label. Reassigned, not shadowed,
		// so the value propagates into AgenticLoop.Logger below — a local
		// copy would be discarded.
		logger = logger.With("sessionName", config.SessionName)
	}

	// 6b. MCP tool discovery — connect to remote MCP servers and register
	// their tools into the registry alongside the built-in tools.
	// Connection failures are non-fatal: the server's tools are skipped
	// so the harness can still operate with its built-in tools.
	//
	// The MCP client's Metrics field is wired further below once the
	// run's *observability.Metrics is constructed; the Connect() loop
	// above only performs tools/list (no callTool yet), so the absence
	// of Metrics during Connect is acceptable. We retain a reference to
	// the client here so we can field-inject Metrics after metrics
	// construction.
	var mcpClient *mcp.Client
	if len(config.Tools.MCPServers) > 0 {
		mcpClient = mcp.NewClient(registry, nil)
		// Wire the logger before Connect so the per-server tool-count cap
		// warning (emitted during tools/list) reaches operators. Metrics is
		// field-injected later, after the run's metrics instance exists.
		mcpClient.Logger = logger
		ownedClosers = append(ownedClosers, mcpClient)
		for _, srv := range config.Tools.MCPServers {
			if err := mcpClient.Connect(ctx, srv, secrets); err != nil {
				logger.Warn("MCP server unavailable, skipping its tools", "server", srv.Name, "error", err)
			}
		}
	}

	// 7. Verifier. Declared here so step 8+ can reference it, but actual
	// construction is deferred to step 13 (line ~296) once the run's
	// metrics instance exists. buildVerifier wraps its result in a
	// metric-recorder when metrics is non-nil, so calling it twice (once
	// without metrics here, once with) would discard the first build —
	// hence the deferred single construction.
	var v verifier.Verifier

	// 8. GuardRail. Constructed AFTER providers are built so cloud-judge
	// can reuse the default ProviderAdapter. Returns guard.NewNoop() when
	// no guard is configured, so the loop's call sites are unconditional.
	gr, err := buildGuardRail(config.GuardRail, providers, prov)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("build guardrail: %w", err)
	}

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

	// 10. Permission policy. The raw policy is built once here; the
	// metric-recording wrapper is applied below after the run's
	// observability.Metrics instance is constructed. Splitting these
	// steps avoids re-reading the Cedar policy file on the rebuild
	// path — the previous double-call made the factory parse the
	// policy file twice on every policy-engine run, which both cost
	// extra latency and opened a TOCTOU window on workspace-relative
	// paths between the two reads (CWE-367).
	pp, err := buildPermissionPolicy(config, registry, tp, secLogger)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("build permission policy: %w", err)
	}

	// 11. Git strategy.
	gs := buildGitStrategy(config.GitStrategy)

	// 12. Trace emitter.
	// resourceOpts captures the run-scoped attributes that ride on the
	// OTel Resource (deployment.environment, service.namespace,
	// harness.run.mode). Threaded into both the trace emitter and the
	// metrics provider so the two signals share a consistent resource
	// identity — without that, a Grafana query that joins traces to
	// metrics on resource attributes would silently miss rows when the
	// two providers carried different defaults.
	//
	// resolvedHeaders dereferences any "secret://" values in
	// TraceEmitter.Headers via the SecretStore so neither the OTel SDK
	// nor any downstream code path sees the raw reference. Resolving
	// once here (rather than separately for traces and metrics) keeps
	// both signals authenticated identically and ensures a missing-env
	// failure surfaces with one error instead of two.
	resourceOpts := resourceOptionsFromConfig(config)
	resolvedHeaders, err := observability.ResolveHeaders(ctx, secrets, config.TraceEmitter.Headers)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("resolve trace emitter headers: %w", err)
	}
	te, err := buildTraceEmitter(ctx, config.TraceEmitter, resolvedHeaders, resourceOpts)
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
		metrics, err = observability.NewMetrics(ctx, metricsEndpoint, config.TraceEmitter.Protocol, resolvedHeaders, resourceOpts)
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

	// Field-inject Metrics into the MCP client so subsequent tools/call
	// dispatches record stirrup.mcp.calls / stirrup.mcp.duration_ms.
	// Done here (not at NewClient time) because the run's metrics
	// instance is built after MCP discovery — if we waited until then
	// to construct the client, callers would lose initial connection
	// telemetry. A nil mcpClient (no servers configured) is a no-op.
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

	// Wrap the previously-built permission policy with metrics so
	// each Check call records stirrup.permission.decisions tagged
	// with the policy class label. The wrapper is composition-only:
	// it does not re-construct the policy, so the policy-engine
	// branch's Cedar file is loaded exactly once.
	pp = wrapPermissionPolicyMetrics(pp, config.PermissionPolicy, metrics)

	// Wrap the context strategy with a metric recorder so each
	// Prepare() call records stirrup.context.strategy_runs tagged
	// with the strategy name and a kind label ("compaction"/"noop").
	// The strategy name is the configured type rather than the Go
	// type to keep dashboards consistent with the existing
	// context.compactions counter (which tags by Strategy field of
	// the CompactionEvent).
	cs = wrapContextStrategy(cs, config.ContextStrategy, metrics)

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
			pa.Logger = logger
		case *provider.OpenAIResponsesAdapter:
			pa.Tracer = tracer
			pa.Metrics = metrics
			pa.Logger = logger
		case *provider.BedrockAdapter:
			pa.Tracer = tracer
			pa.Metrics = metrics
		case *provider.GeminiAdapter:
			pa.Tracer = tracer
			pa.Metrics = metrics
			pa.Logger = logger
		}
	}

	// 14b. Optional BatchAdapter wrapping. Only the top-level provider is
	// wrapped — entries in config.Providers are streaming-only in v1, per
	// the BatchProviderConfig docstring. The streaming inner is retained
	// so cfg.FallbackOnTimeout can delegate to it without a second build.
	//
	// Two batch client implementations exist:
	//   - controlPlaneBatchClient (transport=grpc): the control plane
	//     owns the provider-side batch lifecycle.
	//   - harnessPollingBatchClient (transport=stdio): the harness polls
	//     the provider's batch API directly. Supports Anthropic and the
	//     two OpenAI dialects as of phase 6 (#139).
	//
	// ValidateRunConfig already enforces the transport/HarnessSidePolling
	// pairing — the stdio branch trusts that contract.
	if config.Provider.Batch != nil && config.Provider.Batch.Enabled {
		// MaxWaitSeconds is filled with the documented default
		// (86_400) by ValidateRunConfig when batch.enabled is true, so
		// the nil check below is defence-in-depth for callers bypassing
		// the validator.
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
			// Phase 6 (#139) extends the stdio polling path to OpenAI
			// Chat Completions and Responses. Bedrock and Gemini are
			// still out of scope (validBatchProviderTypes rejects them
			// in ValidateRunConfig); defence-in-depth this dispatch
			// matches that closed set so a misconfigured run fails at
			// build time rather than the first turn.
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
			// The credential source is rebuilt here (rather than
			// captured from buildProviders) because buildProviders
			// resolves the source once and hands the BearerToken
			// closure to the adapter; the polling client needs the
			// Source itself so each poll can re-resolve credentials
			// for forward compatibility with rotating sources.
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
			// validateBatchConfig already rejects any transport that
			// isn't grpc or stdio (transport.type itself is closed-set
			// validated), but defend in depth.
			cleanup()
			return nil, fmt.Errorf("batch is not supported for transport type %q", config.Transport.Type)
		}

		batchAdapter := provider.NewBatchAdapter(prov, batchClient, config.Provider.Batch, config.Provider.Type, config.RunID)
		// Thread the streaming inner adapter's quirks registry into
		// the BatchAdapter so the batch body-marshal path produces the
		// same wire shape the streaming path would have produced for
		// the same (provider, model) pair. Without this, a future
		// batch allow-list expansion that admits a compat-profile
		// provider (e.g. Z.ai) would silently use the default
		// registry and miss the compat rule's extras. v1's
		// validateBatchConfig allow-list does not include any compat-
		// profile provider today, but the wiring is unconditional so
		// the gap cannot reappear.
		if compatible, ok := prov.(*provider.OpenAICompatibleAdapter); ok {
			batchAdapter.Registry = compatible.Registry
		}
		prov = batchAdapter
		// Replace the entry in the providers map so model-router lookups
		// route to the batched wrapper rather than the raw streaming
		// adapter (#194-style cross-routing risk: a router that picks
		// the default provider by type would otherwise bypass batching
		// entirely).
		providers[config.Provider.Type] = prov
	}

	// 14c. Wrap every loop-facing provider adapter with the tool-name
	// normalizer. Applied as the outermost wrap (after batch and any
	// fallback wraps) so the loop's inbound tool_call reverse-mapping
	// reaches the wire-event stream before any consumer touches the
	// name. Skipping a provider that already happens to use only
	// well-formed names is intentional: every adapter goes through the
	// wrapper so the invariant ("provider never sees an invalid name")
	// holds for any future MCP server or operator-defined tool. See
	// issue #223.
	prov = provider.NewNormalizingAdapter(prov, config.Provider.Type)
	wrappedProviders := make(map[string]provider.ProviderAdapter, len(providers))
	for name, p := range providers {
		// The default provider's wrap above already pinned the type to
		// config.Provider.Type; mirror that for any additional providers
		// declared in config.Providers — their key is unique by name but
		// the policy must come from their declared Type, not their map
		// key (which may differ from the type discriminator).
		providerType := config.Provider.Type
		if name != config.Provider.Type {
			if cfg, ok := config.Providers[name]; ok {
				providerType = cfg.Type
			}
		}
		if name == config.Provider.Type {
			// The default-provider entry was just rebuilt above; reuse
			// that exact wrapper so identity is preserved across the
			// loop.Provider and loop.Providers[default] references —
			// some call sites (router fallback, guardrail) compare by
			// pointer.
			wrappedProviders[name] = prov
			continue
		}
		wrappedProviders[name] = provider.NewNormalizingAdapter(p, providerType)
	}
	providers = wrappedProviders

	// Tool-choice escalation policy (#230). OFF by default: when the
	// operator did not opt in via RunConfig.ToolChoiceEscalation,
	// EffectiveToolChoiceEscalationMaxRetries returns 0 and
	// buildEscalationPolicy returns nil, so the loop's escalation path is
	// inert and a bare run is unchanged. The capability resolver is the
	// quirks registry the default provider adapter resolves against, so
	// the native-vs-prompt choice matches the wire shape the adapter would
	// actually serialise (including a compat profile's registry).
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
		Escalation:   escalation,
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
		// set.) The policy may be wrapped in a metric recorder, so try
		// the wrapper's pass-through first before falling back to a
		// direct type assertion.
		addApprovalTool(pp, "spawn_agent")
	}

	// Apply the toolset-profile presentation (issue #234) last, after every
	// tool (built-ins, MCP, spawn_agent) is registered, so the alias mapping
	// covers the complete tool set. The presenter wraps the registry for the
	// loop's List/Resolve seam only; the permission policy, mutating-tool
	// set, and MCP registration above all keep operating on the raw registry
	// and the internal tool IDs, so aliasing changes the model-facing name
	// without touching dispatch gating. The profile name passed ValidateRunConfig
	// already; ProfileFor returning false here would mean a profile in the
	// validator's closed set has no table, which is a build-time bug we fail
	// loudly on rather than silently presenting no aliases.
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
// (#230) injected into the loop. A maxRetries <= 0 returns nil — the
// OFF-by-default case where the loop's escalation path is a no-op — so the
// only way to enable escalation is an explicit RunConfig.ToolChoiceEscalation
// with Enabled:true (which makes EffectiveToolChoiceEscalationMaxRetries
// positive).
//
// The capability resolver is quirks.DefaultRegistry(): tool-choice support
// is a cross-provider capability declared by each provider type's base
// rule, and no compat profile overrides it, so the default registry is the
// authoritative source for the native-vs-prompt fallback decision and
// matches what every adapter resolves against. The _ provider argument is
// reserved so a future per-provider registry (e.g. a compat profile that
// disables required tool choice for a specific gateway) can be threaded in
// without changing the call site.
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

	switch cfg.Type {
	case "anthropic":
		if cred.BearerToken == nil {
			return nil, fmt.Errorf("anthropic provider requires a bearer credential but the credential source produced none")
		}
		// Anthropic accepts two auth header shapes (issue #117 BLOCKING
		// B2). Static API keys (sk-ant-api03-...) ride x-api-key; WIF
		// OAuth access tokens (sk-ant-oat01-...) require Authorization:
		// Bearer. The credential source produces a Bearer token either
		// way; only the adapter knows which header to set.
		authMode := provider.AuthModeAPIKey
		if cfg.Credential != nil && cfg.Credential.Type == "anthropic-wif" {
			authMode = provider.AuthModeBearer
		}
		return provider.NewAnthropicAdapter(cred.BearerToken, authMode), nil
	case "openai-compatible":
		if cred.BearerToken == nil {
			return nil, fmt.Errorf("openai-compatible provider requires a bearer credential but the credential source produced none")
		}
		auth := provider.OpenAIAuthConfig{
			APIKeyHeader: cfg.APIKeyHeader,
			QueryParams:  cfg.QueryParams,
		}
		// ValidateRunConfig guarantees cfg.Retry is populated with the
		// defaulted ProviderRetryConfig — RetryPolicyFromConfig handles a
		// nil pointer defensively in case a non-CLI caller bypasses the
		// validator.
		retry := provider.RetryPolicyFromConfig(cfg.Retry)
		adapter := provider.NewOpenAICompatibleAdapter(cred.BearerToken, cfg.BaseURL, auth, retry)
		// Inject a compat rule into the adapter's registry when the
		// operator selected a compatProfile. The default registry is
		// already attached by the constructor; we replace it with a
		// new registry containing the compat rule appended after
		// BuiltinRules so the compat rule's specificity ordering wins
		// against any first-party glob it overlaps.
		if cfg.CompatProfile != "" {
			extra, err := resolveCompatProfile(cfg.CompatProfile)
			if err != nil {
				return nil, fmt.Errorf("resolve compat profile: %w", err)
			}
			rules := append(quirks.BuiltinRules(), extra)
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
		return provider.NewOpenAIResponsesAdapter(cred.BearerToken, cfg.BaseURL, auth), nil
	case "bedrock":
		return provider.NewBedrockAdapter(cfg.Region, cfg.Profile, cred.AWSCredentials)
	case "gemini":
		if cred.BearerToken == nil {
			return nil, fmt.Errorf("gemini provider requires GCP credentials but the credential source produced none")
		}
		// No compat profiles for Gemini in v1; registry defaults to DefaultRegistry().
		return provider.NewGeminiAdapter(cred.BearerToken, cfg.GCPProject, cfg.GCPLocation, cfg.GeminiSafetySettings), nil
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
			Image:          cfg.Image,
			HostDir:        workspace,
			Network:        cfg.Network,
			Resources:      cfg.Resources,
			Runtime:        cfg.Runtime,
			EgressSecurity: secLogger,
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
	// grep_files and find_files replace the old search_files tool. Both
	// tools are filesystem-read primitives — find_files is pure Go and
	// never shells out; grep_files's native walker only needs read
	// access, and the ripgrep fast path checks CanExec internally before
	// invoking exec.Exec. Gating on CanRead therefore matches semantics:
	// a future read-only sandboxed executor (CanRead=true, CanExec=false)
	// gets working content/name search instead of silently losing both.
	if toolEnabled(cfg.BuiltIn, "grep_files") && caps.CanRead {
		registry.Register(builtins.GrepFilesTool(exec))
	}
	if toolEnabled(cfg.BuiltIn, "find_files") && caps.CanRead {
		registry.Register(builtins.FindFilesTool(exec))
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

// emitRuleOfTwoEvents records two security events at run start:
//
//   - rule_of_two_disabled when all three Rule-of-Two flags hold AND the
//     operator explicitly disabled enforcement via RuleOfTwo.Enforce:false.
//     This is the audit trail for the override; the validator would
//     otherwise have rejected the config.
//   - rule_of_two_warning when exactly two of the three flags hold. The
//     run is legal, but any added capability would tip it into all-three.
//     The event names which two so reviewers can spot capability creep.
//
// The event names "untrusted-input", "sensitive-data", and
// "external-communication" mirror the validator's rejection message so
// downstream tooling can grep for the same identifiers in both places.
func emitRuleOfTwoEvents(config *types.RunConfig, sec *security.SecurityLogger) {
	if sec == nil || config == nil {
		return
	}
	u, s, e := types.RuleOfTwoState(config)

	if u && s && e {
		// All three hold: validator only accepted because of the
		// ask-upstream policy or an explicit Enforce:false override.
		// Only the override case is interesting for audit — the
		// ask-upstream path is the documented happy case.
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
// landed on an ask-upstream policy. Centralising the unwrap means the
// metric wrapper does not need its own AddApprovalTool delegation —
// the wrapper preserves Check() semantics; reaching the concrete
// policy is the caller's job.
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
		return edit.NewMultiStrategy(fuzzyThreshold)
	}
}

// buildVerifier constructs a Verifier from cfg. Each leaf verifier (and
// the composite at every level) is wrapped with verifier.NewMetricRecorder
// when metrics is non-nil, so dashboards can see runs and durations
// attributed to the specific verifier type — including individual
// children of a composite. Passing metrics=nil skips wrapping entirely
// (used during the first construction pass before the run's Metrics
// instance is built; the factory rebuilds the verifier with metrics
// once it's available).
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

// buildPermissionPolicy constructs the configured PermissionPolicy.
//
// The returned policy is raw: it is never wrapped in a metric recorder
// here. Callers that want metric instrumentation should compose the
// result through wrapPermissionPolicyMetrics — splitting the steps lets
// the factory build the policy once (which, for the policy-engine arm,
// involves a Cedar file read and parse) and wrap it with metrics
// afterwards without re-reading the file. The previous design called
// this function twice — once before metrics was constructed, once after
// — and the second call re-loaded the policy file from disk, opening a
// TOCTOU window on workspace-relative paths (CWE-367).
//
// The policy-engine arm requires loading a Cedar policy file from disk
// and wiring a fallback policy in case Cedar returns "no decision". The
// FallbackBuilder closure is the seam between the permission package
// (which doesn't know about the registry / transport / secLogger) and
// the factory (which has all of those in scope) — it maps a fallback
// type name back to the same construction logic the non-policy-engine
// arms use, so a policy-engine config with fallback="ask-upstream"
// behaves identically to top-level ask-upstream when Cedar abstains.
//
// Errors are bubbled because policy-engine construction can fail on a
// missing or malformed policy file; the legacy arms cannot fail and
// could remain non-error-returning, but a single signature is simpler.
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
			// Project DynamicContext entries → values map: the Cedar
			// engine exposes context.dynamicContext as a Record of
			// String → String. Per-entry sensitivity is carried on the
			// RunConfig.DynamicContext map but is not wired into Cedar
			// today — a follow-up may surface it as
			// `context.sensitive_dynamic_context` for policies that
			// want to reason about it.
			DynamicContext: config.DynamicContextValues(),
			Security:       secLogger,
			// ParentRunID and Capabilities are reserved for sub-agent
			// wiring and capability propagation respectively; both
			// are populated by the spawn_agent path in a future wave.
		}
		// The fallback closure maps a fallback type name to the same
		// constructor the non-policy-engine arms use. We deliberately
		// re-route through this switch (via a recursive nested call)
		// so any future change to a fallback policy's construction
		// (e.g. a new ask-upstream timeout default) lands in one place.
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
		// Pre-fix this returned NewAllowAll() — silent permission
		// bypass for any unknown type when callers skipped
		// ValidateRunConfig. Match the rest of buildExecutor /
		// buildVerifier and surface an explicit error (S2).
		return nil, fmt.Errorf("unsupported permissionPolicy.type %q", cfg.Type)
	}
}

// wrapPermissionPolicyMetrics wraps an already-built PermissionPolicy
// with a metric recorder labelled with the configured policy type. A
// nil metrics argument or an empty cfg.Type returns pp unchanged so the
// no-metrics deployment has zero overhead. Splitting the wrap from
// buildPermissionPolicy means the factory can construct the policy once
// (avoiding a second Cedar policy file read) and add metric
// instrumentation afterwards.
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

// resourceOptionsFromConfig assembles the OTel ResourceOptions for this run
// from the RunConfig. The precedence chain (explicit RunConfig field ->
// env var -> default) is implemented inside observability.BuildResource;
// this helper just plumbs the explicit values through and pins the run
// mode (which is always available from the config and has no env-var
// fallback).
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
		return trace.NewOTelTraceEmitter(ctx, endpoint, cfg.Protocol, headers, resourceOpts)
	case "gcs":
		// CredentialConfig is optional — the documented default is
		// gcp-workload-identity against the runtime's metadata server,
		// which is the canonical Cloud Run / GKE Workload Identity
		// shape. An explicit Credential block overrides this (e.g. for
		// a service-account JSON key file on a non-GCP host).
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
			// Write to a discard buffer if no path specified.
			w = &bytes.Buffer{}
		}
		return trace.NewJSONLTraceEmitter(w), nil
	default:
		return nil, fmt.Errorf("unsupported trace emitter type: %q (supported: jsonl, otel, gcs)", cfg.Type)
	}
}

// buildGCSTraceCredentialSource resolves the credential.Source for the
// gcs trace emitter. The default — used when TraceEmitterConfig.Credential
// is nil — is gcp-workload-identity, which matches the Cloud Run / GKE
// runtime contract this emitter targets. An explicit Credential block
// supports the broader cross-cloud federation surface (e.g.
// gcp-service-account from a mounted key file).
//
// The accepted credential types are intentionally narrower than the
// general provider.credential set: only GCP-shaped sources make sense
// here because the target is GCS. AWS / Azure / Anthropic-WIF flavours
// are rejected with a clear error rather than reaching a 401 from the
// GCS API at run-end.
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
		// The provider-side validation enforces that a service-account
		// path is set on Provider.GCPCredentialsFile. The trace emitter
		// has no equivalent field today, so the operator must fall
		// through to ADC (via GOOGLE_APPLICATION_CREDENTIALS) or use
		// workload-identity. Surface the gap explicitly rather than
		// silently no-opping.
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
// compatProfile names to the rule the corresponding compat package
// exports. The set must stay aligned with validCompatProfiles in
// types/runconfig.go — ValidateRunConfig rejects unknown values at
// startup, so an unknown value reaching this switch is a defence-in-
// depth signal that the validator was bypassed (e.g. by a non-CLI
// caller).
//
// New entries here require a corresponding compat package under
// harness/internal/provider/compat/<name>/ and an addition to
// validCompatProfiles. Adding a name to the validator without a rule
// would silently no-op for runs that selected the new profile.
func resolveCompatProfile(profile string) (quirks.Rule, error) {
	switch profile {
	case "zai-glm":
		return zai.CompatRule(), nil
	default:
		return quirks.Rule{}, fmt.Errorf("unknown compat profile %q (supported: zai-glm)", profile)
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
