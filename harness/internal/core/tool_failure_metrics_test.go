package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace/noop"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// denyPolicy is a PermissionPolicy that always denies. Drives the
// permission_denied category test without depending on Cedar wiring.
type denyPolicy struct{}

func (denyPolicy) Check(_ context.Context, _ types.ToolDefinition, _ json.RawMessage) (*permission.PermissionResult, error) {
	return &permission.PermissionResult{Allowed: false, Reason: "test deny"}, nil
}

// errorPolicy is a PermissionPolicy whose Check always returns an error.
// Drives the permission_error category test.
type errorPolicy struct{}

func (errorPolicy) Check(_ context.Context, _ types.ToolDefinition, _ json.RawMessage) (*permission.PermissionResult, error) {
	return nil, errors.New("upstream ask disconnected")
}

// buildMetricsHarness returns a loop wired up with a manual-reader-backed
// metric provider so tests can assert on stirrup.harness.tool_failures
// observations after a planAndDispatch call. The returned reader's
// Collect method snapshots the current metric state.
//
// Pass tr=nil to use NullTransport (sufficient for sync tools and
// preflight-failing async tools where the transport never matters);
// pass an asyncTestTransport when the test needs a live control plane
// to round-trip tool_result_request/response (only the
// async_preflight_error test currently needs this distinction).
func buildMetricsHarness(t *testing.T, tools []*tool.Tool, perm permission.PermissionPolicy, gr guard.GuardRail, tr transport.Transport) (*AgenticLoop, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	metrics, err := observability.NewMetricsForTesting(mp)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}
	registry := tool.NewRegistry()
	for _, tl := range tools {
		registry.Register(tl)
	}
	if perm == nil {
		perm = permission.NewAllowAll()
	}
	if tr == nil {
		tr = transport.NewNullTransport()
	}
	loop := &AgenticLoop{
		Provider:     nil,
		Router:       router.NewStaticRouter("anthropic", "claude-sonnet-4-6"),
		Prompt:       prompt.NewDefaultPromptBuilder(),
		Context:      contextpkg.NewSlidingWindowStrategy(),
		Tools:        registry,
		Edit:         edit.NewWholeFileStrategy(),
		Verifier:     verifier.NewNoneVerifier(),
		Permissions:  perm,
		Git:          git.NewNoneGitStrategy(),
		GuardRail:    gr,
		Transport:    tr,
		Trace:        trace.NewJSONLTraceEmitter(&bytes.Buffer{}),
		Tracer:       noop.NewTracerProvider().Tracer(""),
		TraceContext: context.Background(),
		Metrics:      metrics,
		Logger:       slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
	}
	return loop, reader
}

// failureDataPoint is a typed view of one stirrup.harness.tool_failures
// observation: its value and the label set the metric was recorded with.
type failureDataPoint struct {
	value    int64
	tool     string
	category string
	provider string
	model    string
	mode     string
}

// collectFailures pulls every observation of the
// stirrup.harness.tool_failures counter from the manual reader and
// projects them onto a flat slice keyed by label values. Returns an
// empty slice when the counter has no observations.
func collectFailures(t *testing.T, reader *sdkmetric.ManualReader) []failureDataPoint {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var out []failureDataPoint
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "stirrup.harness.tool_failures" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("expected Sum[int64] for tool_failures, got %T", m.Data)
			}
			for _, dp := range sum.DataPoints {
				fp := failureDataPoint{value: dp.Value}
				for _, kv := range dp.Attributes.ToSlice() {
					switch string(kv.Key) {
					case "tool.name":
						fp.tool = kv.Value.Emit()
					case "category":
						fp.category = kv.Value.Emit()
					case "provider.type":
						fp.provider = kv.Value.Emit()
					case "provider.model":
						fp.model = kv.Value.Emit()
					case "run.mode":
						fp.mode = kv.Value.Emit()
					}
				}
				out = append(out, fp)
			}
		}
	}
	return out
}

// schemaTool is a sync tool with a required field so we can drive the
// schema_validation_failed category by submitting an empty input.
func schemaTool() *tool.Tool {
	return &tool.Tool{
		Name:        "needs_path",
		Description: "tool whose schema requires 'path'",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "ok", nil
		},
	}
}

// trivialTool is a sync tool with a permissive schema that always
// succeeds. Used as the baseline against which success-vs-failure
// emissions can be compared.
func trivialTool() *tool.Tool {
	return &tool.Tool{
		Name:        "trivial",
		Description: "always-OK sync tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "ok", nil
		},
	}
}

// erroringTool is a sync tool whose Handler returns an error every time.
// Drives the handler_error category test.
func erroringTool() *tool.Tool {
	return &tool.Tool{
		Name:        "explode",
		Description: "always-error sync tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "", errors.New("kaboom")
		},
	}
}

// mutatingTool is workspace-mutating so the permission gate fires for it.
func mutatingTool() *tool.Tool {
	return &tool.Tool{
		Name:              "write_something",
		Description:       "mutating tool",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{}}`),
		WorkspaceMutating: true,
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "wrote", nil
		},
	}
}

// asyncPreflightFailingTool returns an AsyncHandler that errors before
// the loop emits tool_result_request. Drives the
// async_preflight_error category test.
func asyncPreflightFailingTool() *tool.Tool {
	return &tool.Tool{
		Name:        "async_broken",
		Description: "async tool whose preflight always fails",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		AsyncHandler: func(_ context.Context, _ json.RawMessage) (tool.AsyncDispatch, error) {
			return tool.AsyncDispatch{}, errors.New("preflight fail")
		},
	}
}

// TestToolFailureMetrics_TableDriven verifies that each
// dispatch-site failure category emits the
// stirrup.harness.tool_failures counter with the correct (tool.name,
// category, provider.type, provider.model, run.mode) label set.
//
// One row per category that can fire from dispatchToolCallCategorized
// or planAndDispatch's pre-dispatch guard check.
func TestToolFailureMetrics_TableDriven(t *testing.T) {
	const (
		wantProvider = "anthropic"
		wantModel    = "claude-sonnet-4-6"
		wantMode     = "execution"
	)
	cases := []struct {
		name         string
		tools        []*tool.Tool
		perm         permission.PermissionPolicy
		guard        guard.GuardRail
		transport    transport.Transport
		call         types.ToolCall
		wantTool     string
		wantCategory observability.ToolFailureCategory
	}{
		{
			name:         "unknown_tool",
			tools:        []*tool.Tool{trivialTool()},
			call:         types.ToolCall{ID: "tc1", Name: "does_not_exist", Input: json.RawMessage(`{}`)},
			wantTool:     "does_not_exist",
			wantCategory: observability.ToolFailureUnknownTool,
		},
		{
			name:         "schema_validation_failed",
			tools:        []*tool.Tool{schemaTool()},
			call:         types.ToolCall{ID: "tc2", Name: "needs_path", Input: json.RawMessage(`{}`)},
			wantTool:     "needs_path",
			wantCategory: observability.ToolFailureSchemaValidation,
		},
		{
			name:         "handler_error",
			tools:        []*tool.Tool{erroringTool()},
			call:         types.ToolCall{ID: "tc3", Name: "explode", Input: json.RawMessage(`{}`)},
			wantTool:     "explode",
			wantCategory: observability.ToolFailureHandlerError,
		},
		{
			name:         "permission_denied",
			tools:        []*tool.Tool{mutatingTool()},
			perm:         denyPolicy{},
			call:         types.ToolCall{ID: "tc4", Name: "write_something", Input: json.RawMessage(`{}`)},
			wantTool:     "write_something",
			wantCategory: observability.ToolFailurePermissionDenied,
		},
		{
			name:         "permission_error",
			tools:        []*tool.Tool{mutatingTool()},
			perm:         errorPolicy{},
			call:         types.ToolCall{ID: "tc5", Name: "write_something", Input: json.RawMessage(`{}`)},
			wantTool:     "write_something",
			wantCategory: observability.ToolFailurePermissionError,
		},
		{
			name:         "guardrail_denied",
			tools:        []*tool.Tool{trivialTool()},
			guard:        &stubDenyGuardRail{reason: "blocked"},
			call:         types.ToolCall{ID: "tc6", Name: "trivial", Input: json.RawMessage(`{}`)},
			wantTool:     "trivial",
			wantCategory: observability.ToolFailureGuardrailDenied,
		},
		{
			name: "async_preflight_error",
			// Async preflight runs only after ensureAsyncCorrelator
			// succeeds; with NullTransport the dispatch would
			// short-circuit to async_transport_unavailable before
			// the preflight is invoked. Use a live test transport
			// so the preflight branch is reachable.
			tools:        []*tool.Tool{asyncPreflightFailingTool()},
			transport:    newAsyncTestTransport(),
			call:         types.ToolCall{ID: "tc7", Name: "async_broken", Input: json.RawMessage(`{}`)},
			wantTool:     "async_broken",
			wantCategory: observability.ToolFailureAsyncPreflight,
		},
		{
			name:         "async_transport_unavailable",
			tools:        []*tool.Tool{asyncEchoTool()},
			call:         types.ToolCall{ID: "tc8", Name: "async_echo", Input: json.RawMessage(`{}`)},
			wantTool:     "async_echo",
			wantCategory: observability.ToolFailureAsyncTransport,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loop, reader := buildMetricsHarness(t, tc.tools, tc.perm, tc.guard, tc.transport)
			cfg := &types.RunConfig{
				RunID:        "test-run",
				Mode:         wantMode,
				ToolDispatch: &types.ToolDispatchConfig{MaxParallel: 1},
			}
			_, _, outcome := loop.planAndDispatch(
				context.Background(), cfg,
				[]types.ToolCall{tc.call}, &stallDetector{},
				wantProvider, wantModel,
			)
			if outcome != "" {
				t.Fatalf("unexpected stall outcome %q", outcome)
			}
			failures := collectFailures(t, reader)
			if len(failures) != 1 {
				t.Fatalf("expected 1 tool_failures observation, got %d: %+v", len(failures), failures)
			}
			got := failures[0]
			if got.value != 1 {
				t.Errorf("value = %d, want 1", got.value)
			}
			if got.tool != tc.wantTool {
				t.Errorf("tool.name = %q, want %q", got.tool, tc.wantTool)
			}
			if got.category != tc.wantCategory.String() {
				t.Errorf("category = %q, want %q", got.category, tc.wantCategory.String())
			}
			if got.provider != wantProvider {
				t.Errorf("provider.type = %q, want %q", got.provider, wantProvider)
			}
			if got.model != wantModel {
				t.Errorf("provider.model = %q, want %q", got.model, wantModel)
			}
			if got.mode != wantMode {
				t.Errorf("run.mode = %q, want %q", got.mode, wantMode)
			}
		})
	}
}

// TestToolFailureMetrics_NoEmissionOnSuccess pins the converse: a
// successful sync tool MUST NOT bump stirrup.harness.tool_failures.
// Without this guard, a regression that swapped the !success branch
// for an unconditional emit would inflate the counter on every call.
func TestToolFailureMetrics_NoEmissionOnSuccess(t *testing.T) {
	loop, reader := buildMetricsHarness(t, []*tool.Tool{trivialTool()}, nil, nil, nil)
	cfg := &types.RunConfig{
		RunID:        "test-run-ok",
		Mode:         "execution",
		ToolDispatch: &types.ToolDispatchConfig{MaxParallel: 1},
	}
	_, _, outcome := loop.planAndDispatch(
		context.Background(), cfg,
		[]types.ToolCall{{ID: "tc_ok", Name: "trivial", Input: json.RawMessage(`{}`)}},
		&stallDetector{}, "anthropic", "claude-sonnet-4-6",
	)
	if outcome != "" {
		t.Fatalf("unexpected stall outcome %q", outcome)
	}
	if failures := collectFailures(t, reader); len(failures) != 0 {
		t.Errorf("expected no tool_failures observations on success path, got %+v", failures)
	}
}

// TestToolFailureMetrics_StallTermination verifies the stall detector
// surfaces its terminations into the tool-failure series. Five
// consecutive failed calls trip the consecutive-failures path; the
// metric emission must carry the stall_consecutive_failures category
// alongside the per-call handler_error emissions.
func TestToolFailureMetrics_StallTermination(t *testing.T) {
	loop, reader := buildMetricsHarness(t, []*tool.Tool{erroringTool()}, nil, nil, nil)
	cfg := &types.RunConfig{
		RunID:        "test-run-stall",
		Mode:         "execution",
		ToolDispatch: &types.ToolDispatchConfig{MaxParallel: 1},
	}
	// Use distinct inputs so the repeated-call heuristic does not trip
	// first; we want the consecutive-failures path specifically.
	calls := []types.ToolCall{
		{ID: "tc_s0", Name: "explode", Input: json.RawMessage(`{"i":0}`)},
		{ID: "tc_s1", Name: "explode", Input: json.RawMessage(`{"i":1}`)},
		{ID: "tc_s2", Name: "explode", Input: json.RawMessage(`{"i":2}`)},
		{ID: "tc_s3", Name: "explode", Input: json.RawMessage(`{"i":3}`)},
		{ID: "tc_s4", Name: "explode", Input: json.RawMessage(`{"i":4}`)},
	}
	_, _, outcome := loop.planAndDispatch(
		context.Background(), cfg,
		calls, &stallDetector{},
		"anthropic", "claude-sonnet-4-6",
	)
	if outcome != "tool_failures" {
		t.Fatalf("expected stall outcome tool_failures, got %q", outcome)
	}
	failures := collectFailures(t, reader)
	// We expect: 5 handler_error emissions (one per failed call) and
	// exactly one stall_consecutive_failures emission tagged with the
	// fifth tool name.
	var (
		handlerErrors int64
		stallCount    int64
	)
	for _, fp := range failures {
		switch fp.category {
		case observability.ToolFailureHandlerError.String():
			handlerErrors += fp.value
		case observability.ToolFailureStallConsecutiveFailures.String():
			stallCount += fp.value
		}
	}
	if handlerErrors != 5 {
		t.Errorf("handler_error count = %d, want 5", handlerErrors)
	}
	if stallCount != 1 {
		t.Errorf("stall_consecutive_failures count = %d, want 1", stallCount)
	}
}

// TestToolFailureCategory_BoundedCardinality is the cardinality guard:
// every category value emitted by the harness MUST pass IsValid. A
// future producer that invents a free-form string (e.g. "
// permission_denied_v2") would silently widen the metric's label
// cardinality; this test pins the bound by asserting every emission's
// category is recognised.
//
// Combined with the table-driven test above (which covers each known
// category at its dispatch site), this defines an interlock: any new
// category MUST be added to the enum to be IsValid, AND any free-form
// string slipped past the dispatch site fails this assertion.
func TestToolFailureCategory_BoundedCardinality(t *testing.T) {
	// Pick a mix of categories drawn from different dispatch sites so
	// the assertion sweeps every emission path simultaneously: an
	// unknown_tool failure (dispatchToolCall pre-checks), a
	// schema_validation_failed failure (mid-dispatchToolCall), a
	// guardrail_denied failure (planAndDispatch pre-dispatch guard),
	// a handler_error failure (terminal Handler), and a series of
	// failed calls that trip the stall path.
	loop, reader := buildMetricsHarness(t,
		[]*tool.Tool{trivialTool(), schemaTool(), erroringTool()},
		nil, nil, nil,
	)
	cfg := &types.RunConfig{
		RunID:        "test-run-card",
		Mode:         "execution",
		ToolDispatch: &types.ToolDispatchConfig{MaxParallel: 1},
	}
	calls := []types.ToolCall{
		{ID: "u1", Name: "does_not_exist", Input: json.RawMessage(`{}`)},
		{ID: "s1", Name: "needs_path", Input: json.RawMessage(`{}`)},
		{ID: "e1", Name: "explode", Input: json.RawMessage(`{"i":1}`)},
		{ID: "e2", Name: "explode", Input: json.RawMessage(`{"i":2}`)},
		{ID: "e3", Name: "explode", Input: json.RawMessage(`{"i":3}`)},
		{ID: "e4", Name: "explode", Input: json.RawMessage(`{"i":4}`)},
	}
	_, _, _ = loop.planAndDispatch(
		context.Background(), cfg,
		calls, &stallDetector{},
		"anthropic", "claude-sonnet-4-6",
	)
	failures := collectFailures(t, reader)
	if len(failures) == 0 {
		t.Fatal("expected at least one tool_failures observation; none were emitted")
	}
	for _, fp := range failures {
		if fp.category == "" {
			t.Errorf("emission with empty category: %+v", fp)
			continue
		}
		if !observability.ToolFailureCategory(fp.category).IsValid() {
			t.Errorf("emission carried unknown category %q: %+v", fp.category, fp)
		}
	}
}

// TestToolFailureCategory_EnumIsValid sanity-checks the enum itself:
// every exported ToolFailure* constant declared in toolfailure.go must
// be registered in allToolFailureCategories. A new constant added
// without a matching map entry would IsValid()=false at runtime and
// silently drop emissions; this test catches that omission at build
// time of the test binary.
func TestToolFailureCategory_EnumIsValid(t *testing.T) {
	known := []observability.ToolFailureCategory{
		observability.ToolFailureUnknownTool,
		observability.ToolFailureSchemaValidation,
		observability.ToolFailureSecurityGuard,
		observability.ToolFailurePermissionDenied,
		observability.ToolFailurePermissionError,
		observability.ToolFailureGuardrailDenied,
		observability.ToolFailureHandlerError,
		observability.ToolFailureHandlerMissing,
		observability.ToolFailureAsyncPreflight,
		observability.ToolFailureAsyncTransport,
		observability.ToolFailureAsyncTimeout,
		observability.ToolFailureAsyncCancelled,
		observability.ToolFailureAsyncUpstreamError,
		observability.ToolFailureAsyncPanic,
		observability.ToolFailureAsyncInternal,
		observability.ToolFailureProviderRequest,
		observability.ToolFailureProviderStream,
		observability.ToolFailureStallRepeated,
		observability.ToolFailureStallConsecutiveFailures,
	}
	for _, c := range known {
		if !c.IsValid() {
			t.Errorf("constant %q is not registered in allToolFailureCategories", c)
		}
		if c.String() == "" {
			t.Errorf("constant has empty wire string")
		}
	}
	// Free-form strings must always fail validation.
	for _, bogus := range []string{"", "not_a_category", "PermissionDenied", "UNKNOWN_TOOL"} {
		if observability.ToolFailureCategory(bogus).IsValid() {
			t.Errorf("bogus category %q must not validate", bogus)
		}
	}
}

// erroringProvider is a ProviderAdapter whose Stream always returns an
// error before producing any events. Drives the
// provider_request_failed category test.
type erroringProvider struct{}

func (erroringProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	return nil, errors.New("provider rejected request")
}

// streamErroringProvider opens the stream then emits an error event
// mid-flight. Drives the provider_stream_failed category test.
type streamErroringProvider struct{}

func (streamErroringProvider) Stream(_ context.Context, _ types.StreamParams) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent, 1)
	ch <- types.StreamEvent{Type: "error", Error: errors.New("stream parser exploded")}
	close(ch)
	return ch, nil
}

// buildProviderFailureLoop builds a loop wired to a manual-reader
// metric provider with the supplied test provider and a test tool
// (which forces the loop to attach tool definitions to its request,
// gating the tool-failure co-emission).
func buildProviderFailureLoop(t *testing.T, prov interface {
	Stream(context.Context, types.StreamParams) (<-chan types.StreamEvent, error)
}) (*AgenticLoop, *sdkmetric.ManualReader, *types.RunConfig) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	metrics, err := observability.NewMetricsForTesting(mp)
	if err != nil {
		t.Fatalf("NewMetricsForTesting: %v", err)
	}
	registry := tool.NewRegistry()
	registry.Register(trivialTool())
	loop := &AgenticLoop{
		Provider:    prov,
		Router:      router.NewStaticRouter("anthropic", "claude-sonnet-4-6"),
		Prompt:      prompt.NewDefaultPromptBuilder(),
		Context:     contextpkg.NewSlidingWindowStrategy(),
		Tools:       registry,
		Edit:        edit.NewWholeFileStrategy(),
		Verifier:    verifier.NewNoneVerifier(),
		Permissions: permission.NewAllowAll(),
		Git:         git.NewNoneGitStrategy(),
		Transport:   transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}),
		Trace:       trace.NewJSONLTraceEmitter(&bytes.Buffer{}),
		Tracer:      noop.NewTracerProvider().Tracer(""),
		Metrics:     metrics,
		Logger:      slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)),
	}
	cfg := &types.RunConfig{
		RunID:            "test-prov-fail",
		Mode:             "execution",
		Prompt:           "go",
		Provider:         types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://TEST"},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "claude-sonnet-4-6"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: "/tmp"},
		EditStrategy:     types.EditStrategyConfig{Type: "whole-file"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		MaxTurns:         2,
	}
	return loop, reader, cfg
}

// TestToolFailureMetrics_ProviderRequestRejected verifies the
// provider-side request-rejection failure path: when a turn has tool
// definitions attached and the provider's Stream() call errors out
// before any events flow, the stirrup.harness.tool_failures counter
// must increment with category=provider_request_failed.
func TestToolFailureMetrics_ProviderRequestRejected(t *testing.T) {
	loop, reader, cfg := buildProviderFailureLoop(t, erroringProvider{})
	_, _ = loop.Run(context.Background(), cfg)
	failures := collectFailures(t, reader)
	found := false
	for _, fp := range failures {
		if fp.category == observability.ToolFailureProviderRequest.String() {
			found = true
			if fp.provider != "anthropic" || fp.model != "claude-sonnet-4-6" || fp.mode != "execution" {
				t.Errorf("labels = %+v, want provider/model/mode populated", fp)
			}
		}
	}
	if !found {
		t.Errorf("expected provider_request_failed observation, got %+v", failures)
	}
}

// TestToolFailureMetrics_ProviderStreamFailed verifies the
// mid-stream-failure path: a tool-bearing turn whose stream errors
// after opening must emit category=provider_stream_failed.
func TestToolFailureMetrics_ProviderStreamFailed(t *testing.T) {
	loop, reader, cfg := buildProviderFailureLoop(t, streamErroringProvider{})
	_, _ = loop.Run(context.Background(), cfg)
	failures := collectFailures(t, reader)
	found := false
	for _, fp := range failures {
		if fp.category == observability.ToolFailureProviderStream.String() {
			found = true
		}
	}
	if !found {
		t.Errorf("expected provider_stream_failed observation, got %+v", failures)
	}
}

// TestToolFailureMetrics_ErrorCategoryInTrace verifies the
// co-emission contract: every failed RecordToolCall must carry the
// same category that lands on stirrup.harness.tool_failures so JSONL
// traces and metrics dashboards report the same taxonomy.
func TestToolFailureMetrics_ErrorCategoryInTrace(t *testing.T) {
	loop, _ := buildMetricsHarness(t, []*tool.Tool{erroringTool()}, nil, nil, nil)
	rec := &recordingTraceEmitter{}
	loop.Trace = rec
	cfg := &types.RunConfig{
		RunID:        "test-trace",
		Mode:         "execution",
		ToolDispatch: &types.ToolDispatchConfig{MaxParallel: 1},
	}
	_, _, _ = loop.planAndDispatch(
		context.Background(), cfg,
		[]types.ToolCall{{ID: "tc_x", Name: "explode", Input: json.RawMessage(`{}`)}},
		&stallDetector{},
		"anthropic", "claude-sonnet-4-6",
	)
	_, calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 recorded tool call, got %d", len(calls))
	}
	if calls[0].Success {
		t.Fatal("expected failed call")
	}
	if calls[0].ErrorCategory != observability.ToolFailureHandlerError.String() {
		t.Errorf("ErrorCategory = %q, want %q", calls[0].ErrorCategory, observability.ToolFailureHandlerError)
	}
}
