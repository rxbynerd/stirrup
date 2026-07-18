package observability

// ToolFailureCategory is a bounded enum of normalised tool-use failure
// reasons. Every value in this file MUST correspond to a concrete failure
// site in the harness (dispatch, provider adapter, stall detector) — the
// metric `stirrup.harness.tool_failures` labels series by category, and
// inventing a value with no producer would create an empty timeseries that
// confuses dashboards.
//
// Cardinality is bounded by design: this is a closed enum, the tool name
// is the internal canonical name from tool.Tool.Name, and provider/model
// come from the router's ModelSelection. Free-form error strings live in
// scrubbed trace records (ToolCallTrace.ErrorReason) and never become
// metric labels.
type ToolFailureCategory string

const (
	// ToolFailureUnknownTool — model emitted a tool_use block whose Name
	// does not resolve via tool.Registry.Resolve.
	ToolFailureUnknownTool ToolFailureCategory = "unknown_tool"

	// ToolFailureSchemaValidation — security.ValidateJSONSchema rejected
	// the model's input against the tool's published InputSchema.
	ToolFailureSchemaValidation ToolFailureCategory = "schema_validation_failed"

	// ToolFailureSecurityGuard — security.GuardToolCall returned findings
	// (e.g. write-tool denylist hit).
	ToolFailureSecurityGuard ToolFailureCategory = "security_guard_denied"

	// ToolFailurePermissionDenied — permission.PermissionPolicy.Check
	// returned Allowed=false for a workspace-mutating / approval-requiring
	// tool.
	ToolFailurePermissionDenied ToolFailureCategory = "permission_denied"

	// ToolFailurePermissionError — Permission.Check itself errored (e.g.
	// upstream ask transport disconnected); a control-plane fault, not an
	// allow/deny decision.
	ToolFailurePermissionError ToolFailureCategory = "permission_error"

	// ToolFailureGuardrailDenied — PhasePreTool GuardRail returned
	// VerdictDeny (or a fail-closed error).
	ToolFailureGuardrailDenied ToolFailureCategory = "guardrail_denied"

	// ToolFailureHandlerError — the sync tool Handler returned a non-nil
	// error.
	ToolFailureHandlerError ToolFailureCategory = "handler_error"

	// ToolFailureHandlerMissing — the resolved tool has neither a sync
	// Handler nor an AsyncHandler; indicates a registry misconfiguration.
	ToolFailureHandlerMissing ToolFailureCategory = "handler_missing"

	// ToolFailureAsyncPreflight — the AsyncHandler preflight errored
	// before the loop emitted tool_result_request.
	ToolFailureAsyncPreflight ToolFailureCategory = "async_preflight_error"

	// ToolFailureAsyncTransport — async tool dispatch attempted with no
	// control-plane transport, or the correlator raised a
	// transport_disconnect.
	ToolFailureAsyncTransport ToolFailureCategory = "async_transport_unavailable"

	// ToolFailureAsyncTimeout — the per-call timeout (default 60s,
	// AsyncDispatch.Timeout override) fired before a matching
	// tool_result_response arrived.
	ToolFailureAsyncTimeout ToolFailureCategory = "async_timeout"

	// ToolFailureAsyncCancelled — the run context was cancelled while the
	// dispatch was blocked on the correlator.
	ToolFailureAsyncCancelled ToolFailureCategory = "async_cancelled"

	// ToolFailureAsyncUpstreamError — the control plane delivered a
	// tool_result_response with IsError=true.
	ToolFailureAsyncUpstreamError ToolFailureCategory = "async_upstream_error"

	// ToolFailureAsyncPanic — an async handler goroutine panicked and was
	// recovered by the dispatch fan-out.
	ToolFailureAsyncPanic ToolFailureCategory = "async_panic"

	// ToolFailureAsyncInternal — defensive: extractAsyncToolResult
	// delivered an unexpected payload type; presence on a dashboard
	// indicates a wiring regression.
	ToolFailureAsyncInternal ToolFailureCategory = "async_internal_error"

	// ToolFailureProviderRequest — provider.Stream errored before the
	// first stream event, on a request with tool definitions attached.
	ToolFailureProviderRequest ToolFailureCategory = "provider_request_failed"

	// ToolFailureProviderStream — provider stream errored mid-flight after
	// the request opened, on a turn with tool definitions attached.
	ToolFailureProviderStream ToolFailureCategory = "provider_stream_failed"

	// ToolFailureStallRepeated — stall detector observed
	// maxRepeatedToolCalls identical consecutive (name, input) calls.
	ToolFailureStallRepeated ToolFailureCategory = "stall_repeated_calls"

	// ToolFailureStallConsecutiveFailures — stall detector observed
	// maxConsecutiveFailures consecutive failed calls.
	ToolFailureStallConsecutiveFailures ToolFailureCategory = "stall_consecutive_failures"

	// ToolFailureNoToolWhenRequired — model returned without calling any
	// tool when the harness required tool use, on a workspace-dependent
	// task's first turn.
	ToolFailureNoToolWhenRequired ToolFailureCategory = "no_tool_when_required"
)

// ToolNameProviderScope is the value emitted for the tool.name metric
// label on tool_failures observations that have no individual tool call
// in scope — the provider-scope categories ToolFailureProviderRequest
// and ToolFailureProviderStream, emitted from runInnerLoop where the
// failure precedes (or supersedes) any per-call dispatch. The wire value
// is the empty string; the constant exists so the emission-site intent
// is explicit and dashboard authors can grep for the sentinel rather
// than puzzle over an unexplained empty-string series.
const ToolNameProviderScope = ""

// allToolFailureCategories is the closed set of valid category values.
// Used by IsValid (and tests asserting the cardinality bound) to confirm
// metric labels are drawn from the enum rather than a free-form string.
var allToolFailureCategories = map[ToolFailureCategory]struct{}{
	ToolFailureUnknownTool:              {},
	ToolFailureSchemaValidation:         {},
	ToolFailureSecurityGuard:            {},
	ToolFailurePermissionDenied:         {},
	ToolFailurePermissionError:          {},
	ToolFailureGuardrailDenied:          {},
	ToolFailureHandlerError:             {},
	ToolFailureHandlerMissing:           {},
	ToolFailureAsyncPreflight:           {},
	ToolFailureAsyncTransport:           {},
	ToolFailureAsyncTimeout:             {},
	ToolFailureAsyncCancelled:           {},
	ToolFailureAsyncUpstreamError:       {},
	ToolFailureAsyncPanic:               {},
	ToolFailureAsyncInternal:            {},
	ToolFailureProviderRequest:          {},
	ToolFailureProviderStream:           {},
	ToolFailureStallRepeated:            {},
	ToolFailureStallConsecutiveFailures: {},
	ToolFailureNoToolWhenRequired:       {},
}

// IsValid reports whether c is a recognised tool failure category. Used by
// the metric emission site to refuse free-form values at the boundary; an
// unknown category indicates a producer mistake and would silently widen
// label cardinality otherwise.
func (c ToolFailureCategory) IsValid() bool {
	_, ok := allToolFailureCategories[c]
	return ok
}

// String returns the category's canonical wire string. Included for
// completeness — most callers will use the typed constant directly.
func (c ToolFailureCategory) String() string {
	return string(c)
}
