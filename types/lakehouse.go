package types

import "context"

// TraceLakehouse is the read surface eval consumers (baseline,
// mine-failures, drift, compare-to-production, replay) depend on.
// Implementations may back this with files, BigQuery, ClickHouse, or
// any other store.
//
// The write surface (StoreTrace, StoreRecording) is deliberately not
// part of this interface: production writes flow through the control
// plane, and the local OSS path writes via the concrete *FileStore
// type instead.
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
