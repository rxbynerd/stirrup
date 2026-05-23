package types

import "context"

// TraceLakehouse is the read surface eval consumers (baseline,
// mine-failures, drift, compare-to-production, replay) depend on.
// Implementations may back this with files, BigQuery, ClickHouse, or
// any other store — the eval framework consumes it without caring
// about the backing store.
//
// As of #109 the write surface (StoreTrace, StoreRecording) is NOT
// part of this interface. Production writes flow through the control
// plane; the local OSS path writes via the concrete `*FileStore` type
// that `stirrup-eval ingest` constructs directly. Cloud-backed
// adapters never implement the write methods — see the issue body
// for the architectural rationale.
type TraceLakehouse interface {
	// QueryTraces returns traces matching the filter, ordered by StartedAt desc.
	QueryTraces(ctx context.Context, filter TraceFilter) ([]RunTrace, error)

	// QueryRecordings returns recordings matching the filter.
	QueryRecordings(ctx context.Context, filter TraceFilter) ([]RunRecording, error)

	// Metrics computes aggregate metrics over matching traces.
	Metrics(ctx context.Context, filter TraceFilter) (TraceMetrics, error)

	// Close releases any resources held by the lakehouse.
	Close() error
}
