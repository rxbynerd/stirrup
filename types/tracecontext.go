package types

// TraceContext is a W3C Trace Context carrier
// (https://www.w3.org/TR/trace-context/) exchanged with a control plane.
// It describes causality only; request and tool-use IDs keep their
// correlation role.
type TraceContext struct {
	Traceparent string `json:"traceparent"`
	Tracestate  string `json:"tracestate,omitempty"`
}
