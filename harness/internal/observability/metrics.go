package observability

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Metrics holds all OTel metric instruments for the harness. When metrics are
// disabled (no endpoint configured), all instruments are no-ops via
// noop.MeterProvider. The provider field is nil for no-op instances.
type Metrics struct {
	provider *sdkmetric.MeterProvider // nil for noop
	meter    metric.Meter             // retained for late callback registration

	// Counters
	Runs                  metric.Int64Counter
	Turns                 metric.Int64Counter
	TokensInput           metric.Int64Counter
	TokensOutput          metric.Int64Counter
	ToolCalls             metric.Int64Counter
	ToolErrors            metric.Int64Counter
	ToolFailures          metric.Int64Counter
	ProviderRequests      metric.Int64Counter
	ProviderErrors        metric.Int64Counter
	ProviderRetryOutcomes metric.Int64Counter
	ContextCompactions    metric.Int64Counter
	SecurityEvents        metric.Int64Counter
	VerificationAttempts  metric.Int64Counter
	Stalls                metric.Int64Counter
	GuardChecks           metric.Int64Counter
	GuardErrors           metric.Int64Counter
	GuardSkips            metric.Int64Counter
	GuardSpotlights       metric.Int64Counter

	// --- Component-level instruments (issue #97) ---
	// Counters
	SubagentSpawns       metric.Int64Counter
	SubagentTokensInput  metric.Int64Counter
	SubagentTokensOutput metric.Int64Counter
	MCPCalls             metric.Int64Counter
	EditAttempts         metric.Int64Counter
	VerifierRuns         metric.Int64Counter
	CodeScannerScans     metric.Int64Counter
	CodeScannerFindings  metric.Int64Counter
	PermissionDecisions  metric.Int64Counter
	ContextStrategyRuns  metric.Int64Counter

	// Histograms
	RunDuration      metric.Float64Histogram
	TurnDuration     metric.Float64Histogram
	ToolCallDuration metric.Float64Histogram
	ProviderLatency  metric.Float64Histogram
	ProviderTTFB     metric.Float64Histogram
	GuardDuration    metric.Float64Histogram

	// --- Component-level histograms (issue #97) ---
	SubagentDuration metric.Float64Histogram
	MCPDuration      metric.Float64Histogram
	EditDuration     metric.Float64Histogram
	VerifierDuration metric.Float64Histogram

	// Observable gauge: per-run callbacks supply the live absolute token
	// estimate. Multiple concurrent runs each register their own callback;
	// observations are tagged with run.id and run.mode so they can be
	// distinguished downstream.
	ContextTokens metric.Int64ObservableGauge

	// callbacksMu guards ctxTokenCallbacks (rare writes; tests/factories
	// register one per run, then unregister at run end).
	callbacksMu       sync.Mutex
	ctxTokenCallbacks map[*ctxTokenCallback]metric.Registration
}

// NewMetrics creates a Metrics instance backed by an OTLP metric exporter
// connected to the given endpoint over the chosen wire protocol.
//
// protocol selects the OTLP wire protocol:
//   - "" or "grpc": OTLP/gRPC. endpoint is host:port.
//   - "http/protobuf": OTLP/HTTP with binary protobuf bodies. endpoint is
//     a full URL ending in the gateway base path; the SDK appends
//     "/v1/metrics". TLS is on for "https://" URLs and off for plain
//     "http://" or scheme-less endpoints (local collectors).
//
// headers is forwarded to the SDK transport unchanged; resolve any
// "secret://" references upstream via ResolveHeaders so the SDK only
// ever sees plaintext bearer tokens.
//
// resourceOpts threads the run-scoped resource attributes
// (deployment.environment, service.namespace, harness.run.mode) so metrics
// emitted here share a consistent resource identity with traces emitted
// from the same run. Callers without a config in hand can pass a zero
// ResourceOptions and the resource builder will fall through to env-var
// fallbacks and the documented defaults.
func NewMetrics(ctx context.Context, endpoint, protocol string, headers map[string]string, resourceOpts ResourceOptions) (*Metrics, error) {
	exporter, err := buildOTLPMetricExporter(ctx, endpoint, protocol, headers)
	if err != nil {
		return nil, err
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
		sdkmetric.WithResource(BuildResource(resourceOpts)),
	)
	meter := provider.Meter("stirrup-harness")

	m, err := newMetricsFromMeter(meter, provider)
	if err != nil {
		_ = provider.Shutdown(ctx)
		return nil, err
	}
	return m, nil
}

// buildOTLPMetricExporter dispatches on the configured wire protocol and
// returns the matching OTel SDK metric exporter. Mirrors the trace
// counterpart in harness/internal/trace/otel.go so an operator who sets
// --otel-protocol=http/protobuf gets identical routing for both signals.
func buildOTLPMetricExporter(ctx context.Context, endpoint, protocol string, headers map[string]string) (sdkmetric.Exporter, error) {
	switch protocol {
	case "", "grpc":
		opts := []otlpmetricgrpc.Option{
			otlpmetricgrpc.WithEndpoint(endpoint),
			otlpmetricgrpc.WithInsecure(),
		}
		if len(headers) > 0 {
			opts = append(opts, otlpmetricgrpc.WithHeaders(headers))
		}
		return otlpmetricgrpc.New(ctx, opts...)
	case "http/protobuf":
		opts := []otlpmetrichttp.Option{
			otlpmetrichttp.WithEndpoint(stripURLScheme(endpoint)),
		}
		if path := urlPath(endpoint); path != "" {
			opts = append(opts, otlpmetrichttp.WithURLPath(joinMetricsPath(path)))
		}
		if isInsecureEndpoint(endpoint) {
			opts = append(opts, otlpmetrichttp.WithInsecure())
		}
		if len(headers) > 0 {
			opts = append(opts, otlpmetrichttp.WithHeaders(headers))
		}
		return otlpmetrichttp.New(ctx, opts...)
	default:
		return nil, fmt.Errorf("unsupported OTLP protocol %q (allowed: grpc, http/protobuf)", protocol)
	}
}

// stripURLScheme returns the host:port portion of an OTLP endpoint URL
// for use with otlpmetrichttp.WithEndpoint, which expects a bare host
// and toggles TLS via WithInsecure(). When the endpoint has no scheme
// (e.g. "localhost:4318"), the value is returned unchanged. Path
// components are dropped here and re-applied separately via
// WithURLPath. Duplicated from the trace package because the two
// packages are siblings under harness/internal and exporting helpers
// for HTTP-URL parsing from one to the other adds public API surface
// for a small amount of code.
func stripURLScheme(endpoint string) string {
	for _, scheme := range []string{"https://", "http://"} {
		if strings.HasPrefix(endpoint, scheme) {
			rest := strings.TrimPrefix(endpoint, scheme)
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				return rest[:i]
			}
			return rest
		}
	}
	if i := strings.IndexByte(endpoint, '/'); i >= 0 {
		return endpoint[:i]
	}
	return endpoint
}

// urlPath returns the path component of an OTLP endpoint URL, or the
// empty string when the endpoint has no path beyond the host.
func urlPath(endpoint string) string {
	rest := endpoint
	for _, scheme := range []string{"https://", "http://"} {
		if strings.HasPrefix(endpoint, scheme) {
			rest = strings.TrimPrefix(endpoint, scheme)
			break
		}
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[i:]
	}
	return ""
}

// joinMetricsPath appends the per-signal "/v1/metrics" suffix to a base
// gateway path. See joinTracesPath in the trace package for the
// rationale; symmetrical here so an operator pointing at
// "https://otlp-gateway-prod-us-east-0.grafana.net/otlp" gets traces
// shipped to .../otlp/v1/traces and metrics to .../otlp/v1/metrics.
func joinMetricsPath(basePath string) string {
	return strings.TrimRight(basePath, "/") + "/v1/metrics"
}

// isInsecureEndpoint returns true for plain "http://" or scheme-less
// endpoints. Mirrors the trace-side logic so a Grafana Cloud HTTPS URL
// never falls back to an unencrypted POST.
func isInsecureEndpoint(endpoint string) bool {
	if strings.HasPrefix(endpoint, "https://") {
		return false
	}
	if strings.HasPrefix(endpoint, "http://") {
		return true
	}
	return true
}

// NewNoopMetrics returns a Metrics instance where all instruments are no-ops.
// Used when metrics are disabled (no endpoint configured).
func NewNoopMetrics() *Metrics {
	meter := noop.NewMeterProvider().Meter("stirrup-harness")
	// noop meter never returns errors, so we can safely ignore them.
	m, _ := newMetricsFromMeter(meter, nil)
	return m
}

// newMetricsFromMeter registers all instruments on the given meter. When
// provider is nil the instance is treated as no-op (Close is a no-op).
// This constructor is unexported so tests can inject a ManualReader-backed
// MeterProvider for in-memory metric collection.
func newMetricsFromMeter(meter metric.Meter, provider *sdkmetric.MeterProvider) (*Metrics, error) {
	m := &Metrics{
		provider:          provider,
		meter:             meter,
		ctxTokenCallbacks: make(map[*ctxTokenCallback]metric.Registration),
	}
	var err error

	// --- Counters ---

	m.Runs, err = meter.Int64Counter("stirrup.harness.runs",
		metric.WithUnit("{run}"),
		metric.WithDescription("Total agentic loop runs started"),
	)
	if err != nil {
		return nil, err
	}

	m.Turns, err = meter.Int64Counter("stirrup.harness.turns",
		metric.WithUnit("{turn}"),
		metric.WithDescription("Total agentic loop turns executed"),
	)
	if err != nil {
		return nil, err
	}

	m.TokensInput, err = meter.Int64Counter("stirrup.harness.tokens.input",
		metric.WithUnit("{token}"),
		metric.WithDescription("Total input tokens consumed"),
	)
	if err != nil {
		return nil, err
	}

	m.TokensOutput, err = meter.Int64Counter("stirrup.harness.tokens.output",
		metric.WithUnit("{token}"),
		metric.WithDescription("Total output tokens consumed"),
	)
	if err != nil {
		return nil, err
	}

	m.ToolCalls, err = meter.Int64Counter("stirrup.harness.tool_calls",
		metric.WithUnit("{call}"),
		metric.WithDescription("Total tool calls dispatched"),
	)
	if err != nil {
		return nil, err
	}

	m.ToolErrors, err = meter.Int64Counter("stirrup.harness.tool_errors",
		metric.WithUnit("{call}"),
		metric.WithDescription("Total tool calls that failed"),
	)
	if err != nil {
		return nil, err
	}

	// ToolFailures decomposes ToolErrors by normalised failure category
	// and labels each observation with provider.type, provider.model,
	// tool.name, run.mode, and category. The category attribute is
	// drawn from the closed ToolFailureCategory enum (see
	// toolfailure.go) so series cardinality is bounded regardless of
	// adversary-influenceable inputs. Includes turn-level provider
	// failures attributable to the tool-use pipeline (request
	// rejection, mid-stream parser errors with tools attached) and
	// stall-detector terminations triggered by tool failure patterns,
	// in addition to the dispatch-site failures already counted by
	// ToolErrors.
	m.ToolFailures, err = meter.Int64Counter("stirrup.harness.tool_failures",
		metric.WithUnit("{failure}"),
		metric.WithDescription("Tool-use failures decomposed by provider, model, tool, and bounded failure category. "+
			"For provider-scope failures (provider_request_failed, provider_stream_failed) where no individual tool call is in scope, "+
			"tool.name is the empty string (see observability.ToolNameProviderScope). "+
			"Stall-terminated batches co-emit one stall_consecutive_failures observation alongside the N per-call failure observations, "+
			"so sum(stirrup.harness.tool_failures) counts N+1 for such a batch; this double-count is intentional and load-bearing for stall alerts."),
	)
	if err != nil {
		return nil, err
	}

	m.ProviderRequests, err = meter.Int64Counter("stirrup.harness.provider_requests",
		metric.WithUnit("{request}"),
		metric.WithDescription("Total provider streaming requests"),
	)
	if err != nil {
		return nil, err
	}

	m.ProviderErrors, err = meter.Int64Counter("stirrup.harness.provider_errors",
		metric.WithUnit("{request}"),
		metric.WithDescription("Total provider request errors"),
	)
	if err != nil {
		return nil, err
	}

	m.ProviderRetryOutcomes, err = meter.Int64Counter("stirrup.harness.provider_retry_outcomes",
		metric.WithUnit("{outcome}"),
		metric.WithDescription("Outcome of each DoWithRetry invocation"),
	)
	if err != nil {
		return nil, err
	}

	m.ContextCompactions, err = meter.Int64Counter("stirrup.harness.context_compactions",
		metric.WithUnit("{compaction}"),
		metric.WithDescription("Total context compaction events"),
	)
	if err != nil {
		return nil, err
	}

	m.SecurityEvents, err = meter.Int64Counter("stirrup.harness.security_events",
		metric.WithUnit("{event}"),
		metric.WithDescription("Total security events recorded"),
	)
	if err != nil {
		return nil, err
	}

	m.VerificationAttempts, err = meter.Int64Counter("stirrup.harness.verification_attempts",
		metric.WithUnit("{attempt}"),
		metric.WithDescription("Total verification attempts"),
	)
	if err != nil {
		return nil, err
	}

	m.Stalls, err = meter.Int64Counter("stirrup.harness.stalls",
		metric.WithUnit("{stall}"),
		metric.WithDescription("Total stall-detected loop terminations"),
	)
	if err != nil {
		return nil, err
	}

	// Guard instruments. The five new counters/histogram are tagged with
	// guard.id and guard.phase so a multi-stage composite (e.g. granite
	// + cloud-judge) reports correctly attributed metrics. Skips are
	// distinct from regular allows because a min-chunk skip never
	// contacts the upstream classifier — counting them as allows would
	// hide cost-saving optimisation behaviour.
	m.GuardChecks, err = meter.Int64Counter("stirrup.guard.checks",
		metric.WithUnit("{check}"),
		metric.WithDescription("Total guard checks dispatched (allow + deny + spotlight)"),
	)
	if err != nil {
		return nil, err
	}

	m.GuardErrors, err = meter.Int64Counter("stirrup.guard.errors",
		metric.WithUnit("{error}"),
		metric.WithDescription("Total guard checks that returned a transport / parse error"),
	)
	if err != nil {
		return nil, err
	}

	m.GuardSkips, err = meter.Int64Counter("stirrup.guard.skips",
		metric.WithUnit("{skip}"),
		metric.WithDescription("Total guard checks short-circuited (e.g. content below MinChunkChars)"),
	)
	if err != nil {
		return nil, err
	}

	m.GuardSpotlights, err = meter.Int64Counter("stirrup.guard.spotlights",
		metric.WithUnit("{spotlight}"),
		metric.WithDescription("Total guard checks that returned VerdictAllowSpot"),
	)
	if err != nil {
		return nil, err
	}

	// --- Component-level counters (issue #97) ---
	//
	// These instruments expose per-component activity (sub-agent, MCP,
	// edit, verifier, codescanner, permission, context) so dashboards
	// can attribute cost and latency to specific subsystems. Wiring at
	// call sites is a follow-up chunk; the foundation lands first so
	// the names are stable before any producer references them.

	m.SubagentSpawns, err = meter.Int64Counter("stirrup.subagent.spawns",
		metric.WithUnit("{spawn}"),
		metric.WithDescription("Sub-agent spawns dispatched"),
	)
	if err != nil {
		return nil, err
	}

	m.SubagentTokensInput, err = meter.Int64Counter("stirrup.subagent.tokens.input",
		metric.WithUnit("{token}"),
		metric.WithDescription("Sub-agent input tokens"),
	)
	if err != nil {
		return nil, err
	}

	m.SubagentTokensOutput, err = meter.Int64Counter("stirrup.subagent.tokens.output",
		metric.WithUnit("{token}"),
		metric.WithDescription("Sub-agent output tokens"),
	)
	if err != nil {
		return nil, err
	}

	m.MCPCalls, err = meter.Int64Counter("stirrup.mcp.calls",
		metric.WithUnit("{call}"),
		metric.WithDescription("MCP tools/call dispatches"),
	)
	if err != nil {
		return nil, err
	}

	m.EditAttempts, err = meter.Int64Counter("stirrup.edit.attempts",
		metric.WithUnit("{attempt}"),
		metric.WithDescription("Edit strategy attempts"),
	)
	if err != nil {
		return nil, err
	}

	m.VerifierRuns, err = meter.Int64Counter("stirrup.verifier.runs",
		metric.WithUnit("{run}"),
		metric.WithDescription("Verifier runs"),
	)
	if err != nil {
		return nil, err
	}

	m.CodeScannerScans, err = meter.Int64Counter("stirrup.codescanner.scans",
		metric.WithUnit("{scan}"),
		metric.WithDescription("Code scanner scans"),
	)
	if err != nil {
		return nil, err
	}

	m.CodeScannerFindings, err = meter.Int64Counter("stirrup.codescanner.findings",
		metric.WithUnit("{finding}"),
		metric.WithDescription("Code scanner findings"),
	)
	if err != nil {
		return nil, err
	}

	m.PermissionDecisions, err = meter.Int64Counter("stirrup.permission.decisions",
		metric.WithUnit("{decision}"),
		metric.WithDescription("Permission decisions"),
	)
	if err != nil {
		return nil, err
	}

	m.ContextStrategyRuns, err = meter.Int64Counter("stirrup.context.strategy_runs",
		metric.WithUnit("{run}"),
		metric.WithDescription("Context strategy invocations"),
	)
	if err != nil {
		return nil, err
	}

	// --- Histograms ---

	m.RunDuration, err = meter.Float64Histogram("stirrup.harness.run_duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Duration of complete agentic loop runs"),
	)
	if err != nil {
		return nil, err
	}

	m.TurnDuration, err = meter.Float64Histogram("stirrup.harness.turn_duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Duration of individual turns"),
	)
	if err != nil {
		return nil, err
	}

	m.ToolCallDuration, err = meter.Float64Histogram("stirrup.harness.tool_call_duration",
		metric.WithUnit("ms"),
		metric.WithDescription("Duration of individual tool calls"),
	)
	if err != nil {
		return nil, err
	}

	m.ProviderLatency, err = meter.Float64Histogram("stirrup.harness.provider_latency",
		metric.WithUnit("ms"),
		metric.WithDescription("Total provider request latency"),
	)
	if err != nil {
		return nil, err
	}

	m.ProviderTTFB, err = meter.Float64Histogram("stirrup.harness.provider_ttfb",
		metric.WithUnit("ms"),
		metric.WithDescription("Provider time to first byte"),
	)
	if err != nil {
		return nil, err
	}

	m.GuardDuration, err = meter.Float64Histogram("stirrup.guard.duration_ms",
		metric.WithUnit("ms"),
		metric.WithDescription("Wall-clock latency of guard.Check calls"),
	)
	if err != nil {
		return nil, err
	}

	// --- Component-level histograms (issue #97) ---
	// Default histogram buckets are reused; per-component bucket
	// configuration is intentionally deferred until call-site wiring
	// reveals the actual latency distribution.

	m.SubagentDuration, err = meter.Float64Histogram("stirrup.subagent.duration_ms",
		metric.WithUnit("ms"),
		metric.WithDescription("Sub-agent run duration"),
	)
	if err != nil {
		return nil, err
	}

	m.MCPDuration, err = meter.Float64Histogram("stirrup.mcp.duration_ms",
		metric.WithUnit("ms"),
		metric.WithDescription("MCP call duration"),
	)
	if err != nil {
		return nil, err
	}

	m.EditDuration, err = meter.Float64Histogram("stirrup.edit.duration_ms",
		metric.WithUnit("ms"),
		metric.WithDescription("Edit strategy duration"),
	)
	if err != nil {
		return nil, err
	}

	m.VerifierDuration, err = meter.Float64Histogram("stirrup.verifier.duration_ms",
		metric.WithUnit("ms"),
		metric.WithDescription("Verifier run duration"),
	)
	if err != nil {
		return nil, err
	}

	// --- Observable gauge ---
	//
	// ContextTokens reports the live (absolute) context-window token
	// estimate per run. Each AgenticLoop registers a callback at run start
	// via RegisterContextTokensCallback and unregisters it at run end. The
	// gauge value is tagged with run.id and run.mode so concurrent runs can
	// be distinguished downstream — there is no shared cumulative counter
	// to confuse with delta sums.
	m.ContextTokens, err = meter.Int64ObservableGauge("stirrup.harness.context_tokens",
		metric.WithUnit("{token}"),
		metric.WithDescription("Live context window token usage per run"),
	)
	if err != nil {
		return nil, err
	}

	return m, nil
}

// ctxTokenCallback is the function signature for ContextTokens gauge
// callbacks. It returns the current absolute token count and the attribute
// set to attach to the observation (typically run.id and run.mode).
type ctxTokenCallback func() (val int64, attrs []attribute.KeyValue)

// RegisterContextTokensCallback registers a callback that the OTel SDK will
// invoke at each collection cycle to observe the current ContextTokens
// value. Returns an unregister function the caller MUST invoke when the
// run finishes — otherwise the callback continues firing after the run
// ends.
//
// Multiple concurrent registrations are supported (one per active run).
// A nil callback is rejected; the returned unregister function is always
// safe to call (it is a no-op when registration failed).
func (m *Metrics) RegisterContextTokensCallback(fn ctxTokenCallback) (unregister func(), err error) {
	if fn == nil {
		return func() {}, nil
	}

	// Capture the callback identity so we can remove it from the map when
	// the unregister function fires.
	key := &fn

	cb := func(_ context.Context, o metric.Observer) error {
		val, attrs := fn()
		o.ObserveInt64(m.ContextTokens, val, metric.WithAttributes(attrs...))
		return nil
	}

	reg, err := m.meter.RegisterCallback(cb, m.ContextTokens)
	if err != nil {
		return func() {}, err
	}

	m.callbacksMu.Lock()
	m.ctxTokenCallbacks[key] = reg
	m.callbacksMu.Unlock()

	return func() {
		m.callbacksMu.Lock()
		stored, ok := m.ctxTokenCallbacks[key]
		if ok {
			delete(m.ctxTokenCallbacks, key)
		}
		m.callbacksMu.Unlock()
		if ok {
			_ = stored.Unregister()
		}
	}, nil
}

// Close shuts down the underlying MeterProvider, flushing any buffered metrics.
// For no-op instances (provider == nil), this is a no-op.
func (m *Metrics) Close() error {
	if m.provider == nil {
		return nil
	}
	return m.provider.Shutdown(context.Background())
}

// NewMetricsForTesting builds a Metrics instance backed by the supplied
// MeterProvider (typically a ManualReader-backed provider). This is exposed
// so tests in dependent packages (provider, core, transport, security) can
// assert that instruments are recorded without requiring an OTLP endpoint.
//
// The returned Metrics does not own provider; callers are responsible for
// shutting it down.
func NewMetricsForTesting(provider *sdkmetric.MeterProvider) (*Metrics, error) {
	meter := provider.Meter("stirrup-harness-test")
	// Pass nil so Close() on the returned Metrics is a no-op — the caller
	// owns the provider.
	return newMetricsFromMeter(meter, nil)
}
