package types

// TraceReference identifies the run's OTel root span so a control plane
// can attach later operations, such as a feedback score, to the trace an
// observability backend stored. Absent when the run records no OTel span.
type TraceReference struct {
	// TraceID is the W3C trace ID as 32 lowercase hex characters.
	TraceID string `json:"traceId"`
	// RunSpanID is the run span's ID as 16 lowercase hex characters.
	RunSpanID string `json:"runSpanId"`
	// Sampled is the trace flags' sampled bit. An unsampled run span is
	// not exported, so a backend holds no trace for the reference.
	Sampled bool `json:"sampled"`
}
