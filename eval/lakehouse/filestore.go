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

// writeJSON marshals v as indented JSON and writes it atomically.
func (fs *FileStore) writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write file %s: %w", path, err)
	}
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
func computeMetrics(traces []types.RunTrace) types.TraceMetrics {
	n := len(traces)
	if n == 0 {
		return types.TraceMetrics{}
	}

	var (
		passCount   int
		totalTurns  int
		totalTokens int
		durations   []float64
	)

	for _, t := range traces {
		if t.Outcome == "success" {
			passCount++
		}
		totalTurns += t.Turns
		totalTokens += t.TokenUsage.Input + t.TokenUsage.Output
		durationMs := float64(t.CompletedAt.Sub(t.StartedAt).Milliseconds())
		durations = append(durations, durationMs)
	}

	sort.Float64s(durations)

	return types.TraceMetrics{
		Count:       n,
		PassRate:    float64(passCount) / float64(n),
		MeanTurns:   float64(totalTurns) / float64(n),
		MeanTokens:  float64(totalTokens) / float64(n),
		P50Duration: percentile(durations, 0.50),
		P95Duration: percentile(durations, 0.95),
	}
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
