package lakehouse

// manifest.go implements the append-only JSONL manifest FileStore
// uses to answer QueryTraces/QueryRecordings without loading every
// JSON file on disk. Wire shape and concurrency model are documented
// in docs/eval.md under "Trace lakehouse".

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/rxbynerd/stirrup/types"
)

const manifestFile = "manifest.jsonl"

// manifestKind discriminates the two record types the FileStore
// indexes. They share the same filterable fields but live in
// different on-disk subdirectories.
type manifestKind string

const (
	manifestKindTrace     manifestKind = "trace"
	manifestKindRecording manifestKind = "recording"
)

// manifestEntry is one line in the manifest, covering the filterable
// surface of TraceFilter (Outcome, Mode, Model, Provider, StartedAt).
type manifestEntry struct {
	Kind      manifestKind `json:"kind"`
	ID        string       `json:"id,omitempty"`    // trace ID
	RunID     string       `json:"runId,omitempty"` // recording RunID
	StartedAt string       `json:"startedAt"`       // ISO-8601 / RFC3339
	Outcome   string       `json:"outcome,omitempty"`
	Mode      string       `json:"mode,omitempty"`
	Model     string       `json:"model,omitempty"`
	Provider  string       `json:"provider,omitempty"`
}

// manifestPath returns the manifest file's absolute path.
func (fs *FileStore) manifestPath() string {
	return filepath.Join(fs.rootDir, manifestFile)
}

// appendManifest appends a single entry to manifest.jsonl. A failure
// is logged but not propagated: the JSON file write already
// succeeded, so a transient FS error here should not surface as a
// store failure. The next query falls back to a rebuild.
func (fs *FileStore) appendManifest(e manifestEntry) {
	data, err := json.Marshal(e)
	if err != nil {
		slog.Default().Info("lakehouse manifest: marshal failed",
			"err", err.Error())
		return
	}
	data = append(data, '\n')
	f, err := os.OpenFile(fs.manifestPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		slog.Default().Info("lakehouse manifest: open failed",
			"err", err.Error())
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(data); err != nil {
		slog.Default().Info("lakehouse manifest: write failed",
			"err", err.Error())
	}
}

// loadManifestIndex reads manifest.jsonl and returns one map per
// kind, keyed by id. Later entries with the same id replace earlier
// ones (last-write-wins). A missing or unparseable manifest yields
// ok=false; the caller should fall back to a directory rebuild.
func (fs *FileStore) loadManifestIndex() (traces, recordings map[string]manifestEntry, ok bool) {
	f, err := os.Open(fs.manifestPath())
	if err != nil {
		if os.IsNotExist(err) {
			// Common case on a fresh lakehouse: not an error.
			return nil, nil, false
		}
		slog.Default().Info("lakehouse manifest: open failed; rebuilding",
			"err", err.Error())
		return nil, nil, false
	}
	defer func() { _ = f.Close() }()

	traces = make(map[string]manifestEntry)
	recordings = make(map[string]manifestEntry)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e manifestEntry
		if err := json.Unmarshal(line, &e); err != nil {
			// A single malformed line invalidates the manifest;
			// rebuild is cheaper than guessing which entries to trust.
			slog.Default().Info("lakehouse manifest: corrupt line; rebuilding",
				"err", err.Error())
			return nil, nil, false
		}
		switch e.Kind {
		case manifestKindTrace:
			if e.ID != "" {
				traces[e.ID] = e
			}
		case manifestKindRecording:
			if e.RunID != "" {
				recordings[e.RunID] = e
			}
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Default().Info("lakehouse manifest: scan failed; rebuilding",
			"err", err.Error())
		return nil, nil, false
	}
	return traces, recordings, true
}

// rebuildManifest scans the on-disk trace and recording directories
// and writes a fresh manifest.jsonl, used by the query path when the
// manifest is missing or corrupt. Uses the same write-then-rename
// pattern as writeJSON so a concurrent reader never sees a
// half-written manifest.
func (fs *FileStore) rebuildManifest() error {
	tmp, err := os.CreateTemp(fs.rootDir, ".tmp-manifest-*.jsonl")
	if err != nil {
		return fmt.Errorf("create temp manifest: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := fs.scanAndWriteTraces(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := fs.scanAndWriteRecordings(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp manifest: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return fmt.Errorf("chmod temp manifest: %w", err)
	}
	if err := os.Rename(tmpPath, fs.manifestPath()); err != nil {
		return fmt.Errorf("rename temp manifest: %w", err)
	}
	cleanup = false
	return nil
}

func (fs *FileStore) scanAndWriteTraces(out *os.File) error {
	entries, err := os.ReadDir(filepath.Join(fs.rootDir, tracesDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read traces directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if strings.HasPrefix(entry.Name(), ".tmp") {
			continue
		}
		var t types.RunTrace
		if err := fs.readJSON(filepath.Join(fs.rootDir, tracesDir, entry.Name()), &t); err != nil {
			return fmt.Errorf("read trace %s during manifest rebuild: %w", entry.Name(), err)
		}
		if err := writeManifestEntry(out, manifestEntryForTrace(t)); err != nil {
			return err
		}
	}
	return nil
}

func (fs *FileStore) scanAndWriteRecordings(out *os.File) error {
	entries, err := os.ReadDir(filepath.Join(fs.rootDir, recordingsDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read recordings directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if strings.HasPrefix(entry.Name(), ".tmp") {
			continue
		}
		var rec types.RunRecording
		if err := fs.readJSON(filepath.Join(fs.rootDir, recordingsDir, entry.Name()), &rec); err != nil {
			return fmt.Errorf("read recording %s during manifest rebuild: %w", entry.Name(), err)
		}
		if err := writeManifestEntry(out, manifestEntryForRecording(rec)); err != nil {
			return err
		}
	}
	return nil
}

func writeManifestEntry(out *os.File, e manifestEntry) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal manifest entry: %w", err)
	}
	data = append(data, '\n')
	if _, err := out.Write(data); err != nil {
		return fmt.Errorf("write manifest entry: %w", err)
	}
	return nil
}

func manifestEntryForTrace(t types.RunTrace) manifestEntry {
	return manifestEntry{
		Kind:      manifestKindTrace,
		ID:        t.ID,
		StartedAt: t.StartedAt.Format("2006-01-02T15:04:05.999999999Z07:00"),
		Outcome:   t.Outcome,
		Mode:      t.Config.Mode,
		Model:     t.Config.ModelRouter.Model,
		Provider:  t.Config.Provider.Type,
	}
}

func manifestEntryForRecording(rec types.RunRecording) manifestEntry {
	return manifestEntry{
		Kind:      manifestKindRecording,
		RunID:     rec.RunID,
		StartedAt: rec.FinalOutcome.StartedAt.Format("2006-01-02T15:04:05.999999999Z07:00"),
		Outcome:   rec.FinalOutcome.Outcome,
		Mode:      rec.Config.Mode,
		Model:     rec.Config.ModelRouter.Model,
		Provider:  rec.Config.Provider.Type,
	}
}

// matchesManifestEntry reports whether an entry passes a TraceFilter,
// mirroring matchesTraceFilter but reading pre-decoded manifest
// fields so the caller can skip a JSON-file load on mismatch.
func matchesManifestEntry(e manifestEntry, f types.TraceFilter) bool {
	if f.Outcome != "" && e.Outcome != f.Outcome {
		return false
	}
	if f.Mode != "" && e.Mode != f.Mode {
		return false
	}
	if f.Model != "" && e.Model != f.Model {
		return false
	}
	if f.Provider != "" && e.Provider != f.Provider {
		return false
	}
	// After/Before are checked against the trace's actual StartedAt
	// once the file is loaded; matchesTraceFilter re-evaluates them.
	return true
}
