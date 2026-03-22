package types

import "context"

// TraceLakehouse abstracts storage and querying of production run data.
// Implementations may back this with files, Postgres JSONB, BigQuery, or
// any other store — the eval framework consumes it without caring about
// the backing store.
type TraceLakehouse interface {
	// StoreTrace persists a completed RunTrace.
	StoreTrace(ctx context.Context, trace RunTrace) error

	// StoreRecording persists a full RunRecording.
	StoreRecording(ctx context.Context, recording RunRecording) error

	// QueryTraces returns traces matching the filter, ordered by StartedAt desc.
	QueryTraces(ctx context.Context, filter TraceFilter) ([]RunTrace, error)

	// QueryRecordings returns recordings matching the filter.
	QueryRecordings(ctx context.Context, filter TraceFilter) ([]RunRecording, error)

	// Metrics computes aggregate metrics over matching traces.
	Metrics(ctx context.Context, filter TraceFilter) (TraceMetrics, error)

	// Close releases any resources held by the lakehouse.
	Close() error
}
