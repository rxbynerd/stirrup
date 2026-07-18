// Package lakehouse provides a file-based implementation of the read
// surface of types.TraceLakehouse plus the concrete write methods
// (StoreTrace, StoreRecording) that stirrup-eval ingest calls
// directly. Write methods are intentionally not part of the
// TraceLakehouse interface: production writes flow through the
// control plane, and cloud-backed adapters never implement write.
package lakehouse

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rxbynerd/stirrup/types"
)

const (
	tracesDir     = "traces"
	recordingsDir = "recordings"
)

// FileStore implements the read surface of types.TraceLakehouse
// backed by JSON files on disk, plus the concrete StoreTrace and
// StoreRecording methods that `stirrup-eval ingest` uses directly.
// Traces are stored in <root>/traces/<id>.json, recordings in
// <root>/recordings/<runId>.json.
type FileStore struct {
	rootDir string
}

var _ types.TraceLakehouse = (*FileStore)(nil)

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

// StoreTrace writes a RunTrace as JSON to traces/<id>.json and
// appends a manifest entry so QueryTraces can skip loading files it
// filters out. A manifest append failure is logged but does not
// propagate: the JSON file is already on disk, and the next query
// falls back to a directory rebuild.
func (fs *FileStore) StoreTrace(_ context.Context, trace types.RunTrace) error {
	if trace.ID == "" {
		return fmt.Errorf("trace ID is empty")
	}
	if err := fs.writeJSON(filepath.Join(fs.rootDir, tracesDir, trace.ID+".json"), trace); err != nil {
		return err
	}
	fs.appendManifest(manifestEntryForTrace(trace))
	return nil
}

// StoreRecording writes a RunRecording as JSON to
// recordings/<runId>.json and appends a manifest entry. See the
// StoreTrace godoc for the manifest's role.
func (fs *FileStore) StoreRecording(_ context.Context, recording types.RunRecording) error {
	if recording.RunID == "" {
		return fmt.Errorf("recording RunID is empty")
	}
	if err := fs.writeJSON(filepath.Join(fs.rootDir, recordingsDir, recording.RunID+".json"), recording); err != nil {
		return err
	}
	fs.appendManifest(manifestEntryForRecording(recording))
	return nil
}

// QueryTraces reads stored traces matching filter and sorts by
// StartedAt descending.
//
// When a manifest is present and parseable, entries the filter
// rejects on pre-decoded fields (Outcome / Mode / Model / Provider)
// are skipped without a JSON-file load; After/Before still require
// the file's StartedAt to be parsed post-load. A missing or
// unparseable manifest falls back to a full directory scan and
// rebuilds the manifest as a side effect.
func (fs *FileStore) QueryTraces(_ context.Context, filter types.TraceFilter) ([]types.RunTrace, error) {
	manifestTraces, _, ok := fs.loadManifestIndex()
	if !ok {
		// Ignore rebuild errors; fall through to the full scan below.
		_ = fs.rebuildManifest()
	}

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
		// Skip atomic-write tmp files left behind by a crashed
		// writer; the rename pattern guarantees these are never
		// the canonical content.
		if strings.HasPrefix(entry.Name(), ".tmp") {
			continue
		}
		// id is the manifest key.
		id := strings.TrimSuffix(entry.Name(), ".json")
		if ok && manifestTraces != nil {
			if me, found := manifestTraces[id]; found && !matchesManifestEntry(me, filter) {
				continue
			}
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

// QueryRecordings reads stored recordings matching filter and sorts
// by FinalOutcome.StartedAt descending. Recordings are 10-100x
// larger than traces (full conversation history + tool I/O) so the
// manifest-driven short-circuit — same behaviour as QueryTraces —
// pays its biggest dividend here.
func (fs *FileStore) QueryRecordings(_ context.Context, filter types.TraceFilter) ([]types.RunRecording, error) {
	_, manifestRecordings, ok := fs.loadManifestIndex()
	if !ok {
		_ = fs.rebuildManifest()
	}

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
		// Skip atomic-write tmp files left behind by a crashed
		// writer; the rename pattern guarantees these are never
		// the canonical content.
		if strings.HasPrefix(entry.Name(), ".tmp") {
			continue
		}
		runID := strings.TrimSuffix(entry.Name(), ".json")
		if ok && manifestRecordings != nil {
			if me, found := manifestRecordings[runID]; found && !matchesManifestEntry(me, filter) {
				continue
			}
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
// the write-then-rename pattern: POSIX rename(2) within a single
// directory means concurrent readers see either the old or the new
// contents, never a torn/zero-byte file. The temp file is created in
// the target directory so the rename stays on one filesystem. On any
// failure path the temp file is cleaned up.
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
	// os.CreateTemp defaults to 0o600; chmod so a downstream tool
	// reading under a different uid doesn't see EACCES.
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
//
// TraceFilter.Provider matches RunTrace.Config.Provider.Type, the
// top-level provider selector. Sub-agent and model-router runs that
// switch providers mid-execution carry their parent's Provider.Type
// here; per-turn provider routing on individual TurnTraces is not
// consulted.
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
	if f.Provider != "" && trace.Config.Provider.Type != f.Provider {
		return false
	}
	return true
}

// computeMetrics aggregates TraceMetrics from a slice of traces.
//
// RunTrace.ToolCalls may contain entries forwarded from sub-agent
// runs alongside parent-only entries; any aggregation over ToolCalls
// here must go through parentOnlyToolCalls to avoid double-counting
// sub-agent activity against parent-run aggregates.
func computeMetrics(traces []types.RunTrace) types.TraceMetrics {
	n := len(traces)
	if n == 0 {
		return types.TraceMetrics{}
	}

	var (
		passCount         int
		failCount         int
		inconclusiveCount int
		totalTurns        int
		totalTokens       int
		streamingDur      []float64
		batchDur          []float64
	)

	for _, t := range traces {
		// PassRate is a quality signal, not a raw outcome tally: a
		// success the verifier disagreed with does not count as a
		// pass, and limit-hit terminations bucket as inconclusive
		// rather than boosting the failure rate.
		switch types.EvalOutcomeFor(t) {
		case types.EvalPassed:
			passCount++
		case types.EvalFailed:
			failCount++
		case types.EvalInconclusive:
			inconclusiveCount++
		}
		totalTurns += t.Turns
		totalTokens += t.TokenUsage.Input + t.TokenUsage.Output
		// Result unused today, but calling this on every run keeps a
		// regression in the filter caught by tests before any
		// aggregate depends on it.
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
		FailRate:         float64(failCount) / float64(n),
		InconclusiveRate: float64(inconclusiveCount) / float64(n),
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
// streaming, so legacy traces predating the batch field fall into
// the streaming bucket.
func isBatchRun(t types.RunTrace) bool {
	return t.Config.Provider.IsBatchEnabled()
}

// parentOnlyToolCalls returns the subset of trace.ToolCalls that
// originated in the parent run, excluding sub-agent calls forwarded
// to the parent's trace emitter. A forwarded call is recognised by
// ParentRunID being set, or by RunID set to a value other than
// trace.ID.
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
