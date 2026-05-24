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
	// ToolFailureUnknownTool — model emitted a tool_use block whose
	// Name does not resolve via tool.Registry.Resolve. Fires from
	// dispatchToolCall.
	ToolFailureUnknownTool ToolFailureCategory = "unknown_tool"

	// ToolFailureSchemaValidation — security.ValidateJSONSchema rejected
	// the model's input against the tool's published InputSchema. Fires
	// from dispatchToolCall after prototype-pollution stripping.
	ToolFailureSchemaValidation ToolFailureCategory = "schema_validation_failed"

	// ToolFailureSecurityGuard — security.GuardToolCall returned
	// findings (e.g. write-tool denylist hit). Fires from
	// dispatchToolCall before permission check.
	ToolFailureSecurityGuard ToolFailureCategory = "security_guard_denied"

	// ToolFailurePermissionDenied — permission.PermissionPolicy.Check
	// returned Allowed=false for a workspace-mutating / approval-
	// requiring tool. Fires from dispatchToolCall.
	ToolFailurePermissionDenied ToolFailureCategory = "permission_denied"

	// ToolFailurePermissionError — Permission.Check itself errored
	// (e.g. upstream ask transport disconnected). Distinct from
	// PermissionDenied because it represents a control-plane fault,
	// not an allow/deny decision.
	ToolFailurePermissionError ToolFailureCategory = "permission_error"

	// ToolFailureGuardrailDenied — PhasePreTool GuardRail returned
	// VerdictDeny (or fail-closed error). Fires from planAndDispatch's
	// pre-dispatch guard check.
	ToolFailureGuardrailDenied ToolFailureCategory = "guardrail_denied"

	// ToolFailureHandlerError — the sync tool Handler returned a
	// non-nil error. Fires from dispatchToolCall.
	ToolFailureHandlerError ToolFailureCategory = "handler_error"

	// ToolFailureHandlerMissing — the resolved tool has neither a sync
	// Handler nor an AsyncHandler. Defensive; indicates a registry
	// misconfiguration rather than a model-side failure.
	ToolFailureHandlerMissing ToolFailureCategory = "handler_missing"

	// ToolFailureAsyncPreflight — the AsyncHandler preflight returned
	// an error before the loop emitted tool_result_request. Fires from
	// dispatchAsyncToolCall.
	ToolFailureAsyncPreflight ToolFailureCategory = "async_preflight_error"

	// ToolFailureAsyncTransport — async tool dispatch attempted on a
	// loop with no control-plane transport, or the correlator's emit
	// raised a transport_disconnect.
	ToolFailureAsyncTransport ToolFailureCategory = "async_transport_unavailable"

	// ToolFailureAsyncTimeout — the per-call timeout (default 60s,
	// AsyncDispatch.Timeout override) fired before a matching
	// tool_result_response arrived.
	ToolFailureAsyncTimeout ToolFailureCategory = "async_timeout"

	// ToolFailureAsyncCancelled — the run context was cancelled while
	// the dispatch was blocked on the correlator.
	ToolFailureAsyncCancelled ToolFailureCategory = "async_cancelled"

	// ToolFailureAsyncUpstreamError — the control plane delivered a
	// tool_result_response with IsError=true.
	ToolFailureAsyncUpstreamError ToolFailureCategory = "async_upstream_error"

	// ToolFailureAsyncPanic — an async handler goroutine panicked and
	// was recovered by the dispatch fan-out.
	ToolFailureAsyncPanic ToolFailureCategory = "async_panic"

	// ToolFailureAsyncInternal — defensive: extractAsyncToolResult
	// delivered a payload of an unexpected type. Should not occur in
	// practice; presence on a dashboard indicates a wiring regression.
	ToolFailureAsyncInternal ToolFailureCategory = "async_internal_error"

	// ToolFailureProviderRequest — provider.Stream returned an error
	// before producing the first stream event, while tool definitions
	// were attached to the request. Includes serialization rejections
	// and HTTP-level rejections (4xx) of tool-bearing requests. Fires
	// from runInnerLoop.
	ToolFailureProviderRequest ToolFailureCategory = "provider_request_failed"

	// ToolFailureProviderStream — provider stream errored mid-flight
	// after the request opened, on a turn that had tool definitions
	// attached. Captures stream parser errors and SSE-side abrupt
	// terminations during tool-call assembly. Fires from runInnerLoop.
	ToolFailureProviderStream ToolFailureCategory = "provider_stream_failed"

	// ToolFailureStallRepeated — stall detector observed
	// maxRepeatedToolCalls identical consecutive (name, input) calls.
	// Fires from planAndDispatch when stall.recordToolCall returns
	// "stalled".
	ToolFailureStallRepeated ToolFailureCategory = "stall_repeated_calls"

	// ToolFailureStallConsecutiveFailures — stall detector observed
	// maxConsecutiveFailures consecutive failed calls. Fires from
	// planAndDispatch when stall.recordToolCall returns "tool_failures".
	ToolFailureStallConsecutiveFailures ToolFailureCategory = "stall_consecutive_failures"
)

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
