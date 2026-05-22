// Package lakehouse provides file-based implementations of the TraceLakehouse interface.
package lakehouse

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"github.com/rxbynerd/stirrup/types"
)

const (
	tracesDir     = "traces"
	recordingsDir = "recordings"
)

// FileStore implements types.TraceLakehouse backed by JSON files on disk.
// Traces are stored in <root>/traces/<id>.json and recordings in
// <root>/recordings/<runId>.json.
type FileStore struct {
	rootDir string
}

// NewFileStore creates a FileStore rooted at rootDir, creating the necessary
// subdirectories if they don't already exist.
func NewFileStore(rootDir string) (*FileStore, error) {
	for _, sub := range []string{tracesDir, recordingsDir} {
		if err := os.MkdirAll(filepath.Join(rootDir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("create %s directory: %w", sub, err)
		}
	}
	return &FileStore{rootDir: rootDir}, nil
}

// StoreTrace writes a RunTrace as JSON to traces/<id>.json.
func (fs *FileStore) StoreTrace(_ context.Context, trace types.RunTrace) error {
	if trace.ID == "" {
		return fmt.Errorf("trace ID is empty")
	}
	return fs.writeJSON(filepath.Join(fs.rootDir, tracesDir, trace.ID+".json"), trace)
}

// StoreRecording writes a RunRecording as JSON to recordings/<runId>.json.
func (fs *FileStore) StoreRecording(_ context.Context, recording types.RunRecording) error {
	if recording.RunID == "" {
		return fmt.Errorf("recording RunID is empty")
	}
	return fs.writeJSON(filepath.Join(fs.rootDir, recordingsDir, recording.RunID+".json"), recording)
}

// QueryTraces reads all stored traces, applies the filter, sorts by StartedAt
// descending, and applies the limit.
func (fs *FileStore) QueryTraces(_ context.Context, filter types.TraceFilter) ([]types.RunTrace, error) {
	entries, err := os.ReadDir(filepath.Join(fs.rootDir, tracesDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read traces directory: %w", err)
	}

	var traces []types.RunTrace
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		var trace types.RunTrace
		if err := fs.readJSON(filepath.Join(fs.rootDir, tracesDir, entry.Name()), &trace); err != nil {
			return nil, fmt.Errorf("read trace %s: %w", entry.Name(), err)
		}
		if matchesTraceFilter(trace, filter) {
			traces = append(traces, trace)
		}
	}

	sort.Slice(traces, func(i, j int) bool {
		return traces[i].StartedAt.After(traces[j].StartedAt)
	})

	if filter.Limit > 0 && len(traces) > filter.Limit {
		traces = traces[:filter.Limit]
	}
	return traces, nil
}

// QueryRecordings reads all stored recordings, applies the filter (using the
// recording's FinalOutcome fields), sorts by FinalOutcome.StartedAt descending,
// and applies the limit.
func (fs *FileStore) QueryRecordings(_ context.Context, filter types.TraceFilter) ([]types.RunRecording, error) {
	entries, err := os.ReadDir(filepath.Join(fs.rootDir, recordingsDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read recordings directory: %w", err)
	}

	var recordings []types.RunRecording
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		var rec types.RunRecording
		if err := fs.readJSON(filepath.Join(fs.rootDir, recordingsDir, entry.Name()), &rec); err != nil {
			return nil, fmt.Errorf("read recording %s: %w", entry.Name(), err)
		}
		if matchesTraceFilter(rec.FinalOutcome, filter) {
			recordings = append(recordings, rec)
		}
	}

	sort.Slice(recordings, func(i, j int) bool {
		return recordings[i].FinalOutcome.StartedAt.After(recordings[j].FinalOutcome.StartedAt)
	})

	if filter.Limit > 0 && len(recordings) > filter.Limit {
		recordings = recordings[:filter.Limit]
	}
	return recordings, nil
}

// Metrics computes aggregate TraceMetrics over traces matching the filter.
func (fs *FileStore) Metrics(ctx context.Context, filter types.TraceFilter) (types.TraceMetrics, error) {
	traces, err := fs.QueryTraces(ctx, filter)
	if err != nil {
		return types.TraceMetrics{}, err
	}
	return computeMetrics(traces), nil
}

// Close is a no-op for the file-based store.
func (fs *FileStore) Close() error {
	return nil
}

// writeJSON marshals v as indented JSON and writes it atomically via
// the write-then-rename pattern. POSIX rename(2) within a single
// directory is atomic: concurrent readers observe either the old or
// the new contents, never a torn / zero-byte intermediate state.
//
// The temp file is created in the target directory so the rename
// stays inside one filesystem (cross-mount rename falls back to a
// non-atomic copy on most kernels). On any failure path the temp
// file is cleaned up; the caller never sees a leaked .tmp-*.json.
//
// This guards against three torn-file scenarios the previous
// os.WriteFile path could not:
//
//   - A reader running concurrent QueryTraces seeing a zero-byte file
//     between the O_TRUNC and the write.
//   - A reader seeing a partial JSON document if the writing process
//     is killed mid-write.
//   - Two concurrent ingest processes writing the same trace ID
//     interleaving their bytes into a corrupt document — under
//     write-then-rename, the last successful rename wins atomically.
//
// See #267 for the failure modes and a worked example.
func (fs *FileStore) writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*.json")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	// Match the perm the previous os.WriteFile call used so behaviour
	// is unchanged from a file-permissions perspective. os.CreateTemp
	// defaults to 0o600; without this chmod a downstream tool that
	// reads the file under a different uid would start to see EACCES.
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp to %s: %w", path, err)
	}
	cleanup = false
	return nil
}

// readJSON reads a JSON file and unmarshals it into v.
func (fs *FileStore) readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// matchesTraceFilter returns true if the trace satisfies all non-zero filter fields.
func matchesTraceFilter(trace types.RunTrace, f types.TraceFilter) bool {
	if f.After != nil && !trace.StartedAt.After(*f.After) {
		return false
	}
	if f.Before != nil && !trace.StartedAt.Before(*f.Before) {
		return false
	}
	if f.Outcome != "" && trace.Outcome != f.Outcome {
		return false
	}
	if f.Mode != "" && trace.Config.Mode != f.Mode {
		return false
	}
	if f.Model != "" && trace.Config.ModelRouter.Model != f.Model {
		return false
	}
	return true
}

// computeMetrics aggregates TraceMetrics from a slice of traces.
//
// As of #55, RunTrace.ToolCalls may contain entries forwarded from
// sub-agent runs alongside parent-only entries (see types.RunTrace
// doc). Any aggregation over ToolCalls in this function must use
// parentOnlyToolCalls to avoid double-counting sub-agent activity
// against parent-run aggregates. The current TraceMetrics shape
// does not expose a tool-call count, but the filter is applied here
// so that adding one later cannot silently regress the contract.
func computeMetrics(traces []types.RunTrace) types.TraceMetrics {
	n := len(traces)
	if n == 0 {
		return types.TraceMetrics{}
	}

	var (
		passCount    int
		totalTurns   int
		totalTokens  int
		streamingDur []float64
		batchDur     []float64
	)

	for _, t := range traces {
		if t.Outcome == "success" {
			passCount++
		}
		totalTurns += t.Turns
		totalTokens += t.TokenUsage.Input + t.TokenUsage.Output
		// Filter is intentionally evaluated even though the result is
		// not yet aggregated: this exercises the helper on every run
		// so a regression breaks the parentOnlyToolCalls test path,
		// and prepares the loop body for a future per-run tool-count
		// aggregate without having to revisit this filter contract.
		_ = parentOnlyToolCalls(t)
		durationMs := float64(t.CompletedAt.Sub(t.StartedAt).Milliseconds())
		if isBatchRun(t) {
			batchDur = append(batchDur, durationMs)
		} else {
			streamingDur = append(streamingDur, durationMs)
		}
	}

	sort.Float64s(streamingDur)
	sort.Float64s(batchDur)

	return types.TraceMetrics{
		Count:            n,
		PassRate:         float64(passCount) / float64(n),
		MeanTurns:        float64(totalTurns) / float64(n),
		MeanTokens:       float64(totalTokens) / float64(n),
		P50Duration:      percentile(streamingDur, 0.50),
		P95Duration:      percentile(streamingDur, 0.95),
		BatchP50Duration: percentile(batchDur, 0.50),
		BatchP95Duration: percentile(batchDur, 0.95),
	}
}

// isBatchRun reports whether a trace's RunConfig opted into batch
// provider submission. A nil Batch or Batch.Enabled=false counts as
// streaming so legacy traces (predating the batch field) and
// streaming-only runs fall into the streaming bucket. Eval drift
// detection compares streaming-vs-streaming and batch-vs-batch on
// the strength of this classifier (#138).
func isBatchRun(t types.RunTrace) bool {
	return t.Config.Provider.IsBatchEnabled()
}

// parentOnlyToolCalls returns the subset of trace.ToolCalls that
// originated in the parent run, excluding sub-agent calls that were
// forwarded to the parent's trace emitter. A forwarded sub-agent call
// is recognised by ParentRunID being set OR by RunID being set to a
// value other than trace.ID.
//
// Without this filter, any aggregation over RunTrace.ToolCalls double-
// counts sub-agent activity against parent-run aggregates (#55).
func parentOnlyToolCalls(trace types.RunTrace) []types.ToolCallSummary {
	if len(trace.ToolCalls) == 0 {
		return nil
	}
	out := trace.ToolCalls[:0:0]
	for _, tc := range trace.ToolCalls {
		if tc.ParentRunID != "" {
			continue
		}
		if tc.RunID != "" && tc.RunID != trace.ID {
			continue
		}
		out = append(out, tc)
	}
	return out
}

// percentile computes the p-th percentile from a sorted slice of values
// using linear interpolation.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	// Use the "exclusive" percentile method: rank = p * (n + 1) - 1
	// but clamp to valid indices.
	rank := p * float64(n-1)
	lower := int(math.Floor(rank))
	upper := lower + 1
	if upper >= n {
		return sorted[n-1]
	}
	frac := rank - float64(lower)
	return sorted[lower] + frac*(sorted[upper]-sorted[lower])
}
