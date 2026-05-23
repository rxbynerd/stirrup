package lakehouse

// manifest.go implements the append-only JSONL manifest the FileStore
// uses to answer QueryTraces / QueryRecordings without loading every
// JSON file on disk (#275).
//
// Wire shape: <lakehouse>/manifest.jsonl, one event per line:
//
//	{"kind":"trace","id":"...","startedAt":"...","outcome":"...","mode":"...","model":"...","provider":"..."}
//	{"kind":"recording","runId":"...","startedAt":"...","outcome":"...","mode":"...","model":"...","provider":"..."}
//
// StoreTrace / StoreRecording append one entry per call. Re-ingest of
// the same id appends a duplicate; the read path uses the LAST entry
// per id so last-write-wins matches the JSON-file write semantics.
//
// Concurrency: a single os.OpenFile with O_APPEND ensures every write
// is atomic on POSIX for sub-PIPE_BUF payloads (4 KiB on Linux, larger
// on macOS — a manifest line is well under that). A second writer
// appending concurrently sees a consistent on-disk shape; the
// only ordering guarantee is "if A's append returned before B's
// started, A's bytes precede B's." The read path tolerates any order
// because last-write-wins is symmetric.
//
// Missing or corrupt manifest is recoverable: the read path detects
// the failure, falls back to scanning every JSON file, and rebuilds
// the manifest as a side effect with a single info-level log line.

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

// manifestEntry is one line in the manifest. Fields cover the
// filterable surface of TraceFilter (Outcome, Mode, Model, Provider,
// time-range via StartedAt); a future filter addition that needs a
// new field bumps the manifest's effective version implicitly because
// the read path skips entries lacking the field and falls through to
// a JSON-file read.
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

// manifestPath returns the absolute path of the manifest file rooted
// at the FileStore's directory.
func (fs *FileStore) manifestPath() string {
	return filepath.Join(fs.rootDir, manifestFile)
}

// appendManifest appends a single entry to manifest.jsonl.
// O_APPEND makes the write atomic at the kernel layer for sub-
// PIPE_BUF payloads; manifest lines are well under that. A failure
// is logged but does not propagate — the JSON file write already
// succeeded by the time appendManifest is called, so a transient
// FS error must not cause the operator to see a "store failed"
// when the data is on disk. The next query falls back to a
// rebuild from the directory listing.
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
// ones (last-write-wins, matching the JSON-file write semantics).
// A missing or unparseable manifest yields ok=false; the caller
// should fall back to a directory rebuild.
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
			// A single malformed line invalidates the whole
			// manifest's "I know what's on disk" claim. Rebuild
			// is cheaper than guessing which entries to trust.
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
// and writes a fresh manifest.jsonl. Used by the query path when
// the manifest is missing or corrupt. Atomicity is delivered via the
// writeJSON-style write-then-rename pattern (#267) so a concurrent
// reader never sees a half-written manifest.
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

// matchesManifestEntry reports whether an entry passes a
// TraceFilter. Mirrors matchesTraceFilter but reads from the
// pre-decoded manifest fields. The function exists separately so
// the manifest path can short-circuit a JSON-file load: if the
// manifest entry doesn't match the filter, we skip loading the
// underlying file.
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
	// after the file is loaded — the manifest entry only carries a
	// formatted string and parsing here twice (manifest read + post-
	// load) doubles the parse cost without value. The full
	// matchesTraceFilter re-evaluates the time bounds on the loaded
	// trace, so the manifest's role is to skip files that can't
	// match Outcome/Mode/Model/Provider regardless of time.
	return true
}
