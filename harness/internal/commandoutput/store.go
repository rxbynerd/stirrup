// Package commandoutput owns complete, scrubbed run_command output capture.
//
// Command output is redacted on the way to disk by security.ScrubWriter, so
// the 0600 spool files under the 0700 temp directory hold scrubbed bytes at
// every instant and an unclean shutdown cannot leave a secret behind. Raw
// byte counts and SHA-256 digests come from an in-flight hash of the stream
// as the command produces it, never from re-reading the spool. See
// docs/configuration.md#command-output-capture for the residual a chunked
// scrubber carries and for the orphan sweep that reclaims spools left by a
// crashed run.
package commandoutput

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

const (
	ReadDefaultBytes int64 = 32 << 10
	ReadMaxBytes     int64 = 128 << 10

	spoolPrefix = "stirrup-command-output-"
	// orphanSpoolMaxAge keeps the startup sweep clear of live runs: a run's
	// wall-clock budget is bounded well below a day, so a spool root older
	// than this belongs to a process that never finalized.
	orphanSpoolMaxAge = 24 * time.Hour
)

var (
	ErrCaptureLimit = errors.New("command output capture limit exceeded")
	ErrCaptureIO    = errors.New("command output capture storage failed")
)

// Recorder receives bounded command metadata for the trace stream.
type Recorder interface {
	RecordCommandOutput(types.CommandOutputRecord)
}

// Uploader persists a completed archive and returns its durable URI.
type Uploader interface {
	UploadCommandOutputArchive(ctx context.Context, localPath, archiveID string) (string, error)
}

type Options struct {
	RunID       string
	Config      types.CommandOutputConfig
	ArchivePath string
	Uploader    Uploader
}

// Store is shared by a parent run and all subagents.
type Store struct {
	mu          sync.Mutex
	root        string
	archivePath string
	archiveID   string
	config      types.CommandOutputConfig
	uploader    Uploader
	recorder    Recorder
	totalRaw    int64
	fatalErr    error
	entries     map[string]*entry
	refs        map[string]streamRef
	finalized   bool
	archiveURI  string
}

type entry struct {
	mu           sync.Mutex
	record       types.CommandOutputRecord
	stdoutPath   string
	stderrPath   string
	initialPath  string
	modelFiles   []string
	recorderSent bool
}

type streamRef struct {
	entry  *entry
	stream string
	path   string
}

type Capture struct {
	store  *Store
	entry  *entry
	stdout *spoolWriter
	stderr *spoolWriter
}

// spoolWriter persists a stream through security.ScrubWriter. count and
// rawHash cover the stream as the command produced it; written and
// scrubbedHash cover what reached disk.
type spoolWriter struct {
	store        *Store
	file         *os.File
	member       string
	scrub        *security.ScrubWriter
	rawHash      hash.Hash
	scrubbedHash hash.Hash
	count        int64
	written      int64
	limit        int64
	cancel       context.CancelCauseFunc
	failed       error
	closed       bool
	mu           sync.Mutex
}

type Completion struct {
	ExitCode  int
	TimedOut  bool
	Cancelled bool
}

type Captured struct {
	Record types.CommandOutputRecord
	Stdout string
	Stderr string
}

type ReadResult struct {
	Content types.CommandOutputStreamRecord
	Bytes   []byte
	Offset  int64
	End     int64
	EOF     bool
	Stream  string
}

type manifest struct {
	SchemaVersion int                         `json:"schemaVersion"`
	ArchiveID     string                      `json:"archiveId"`
	CreatedAt     time.Time                   `json:"createdAt"`
	Complete      bool                        `json:"complete"`
	Failure       string                      `json:"failure,omitempty"`
	Commands      []types.CommandOutputRecord `json:"commands"`
}

func New(opts Options) (*Store, error) {
	opts.Config = (types.ToolsConfig{CommandOutput: opts.Config}).EffectiveCommandOutput()
	sweepOrphanedSpools(os.TempDir(), orphanSpoolMaxAge, time.Now())
	root, err := os.MkdirTemp("", spoolPrefix)
	if err != nil {
		return nil, fmt.Errorf("create command output store: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("secure command output store: %w", err)
	}
	archiveID := safeID(opts.RunID)
	if archiveID == "" {
		archiveID = fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	archivePath := opts.ArchivePath
	if archivePath == "" {
		archivePath = filepath.Join(os.TempDir(), archiveID+".command-output.tar.gz")
	}
	absArchive, err := filepath.Abs(archivePath)
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("resolve command output archive path: %w", err)
	}
	return &Store{
		root: root, archivePath: absArchive, archiveID: archiveID,
		config: opts.Config, uploader: opts.Uploader,
		entries: map[string]*entry{}, refs: map[string]streamRef{},
	}, nil
}

func (s *Store) SetRecorder(recorder Recorder) {
	s.mu.Lock()
	s.recorder = recorder
	s.mu.Unlock()
}

func (s *Store) FatalError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fatalErr
}

func (s *Store) Archive() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.archiveURI != "" {
		return s.archiveURI
	}
	return s.archivePath
}

func (s *Store) HasEntries() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries) > 0
}

func (s *Store) Begin(ctx context.Context, cancel context.CancelCauseFunc) (*Capture, error) {
	s.mu.Lock()
	if s.fatalErr != nil {
		err := s.fatalErr
		s.mu.Unlock()
		cancel(err)
		return nil, fmt.Errorf("command output store is failed: %w", err)
	}
	s.mu.Unlock()
	meta := tool.CallContextFrom(ctx)
	if meta.ToolUseID == "" {
		meta.ToolUseID = fmt.Sprintf("command-%d", time.Now().UnixNano())
	}
	key := meta.RunID + "\x00" + meta.ToolUseID
	memberID := encodedID(meta.RunID + "-" + meta.ToolUseID)
	if err := os.MkdirAll(filepath.Join(s.root, "commands", memberID), 0o700); err != nil {
		s.fail(fmt.Errorf("%w: create command spool directory: %v", ErrCaptureIO, err))
		return nil, s.FatalError()
	}
	stdout, err := newSpoolWriter(s, streamMember(memberID, "stdout"), s.config.MaxBytesPerStream, cancel)
	if err != nil {
		return nil, err
	}
	stderr, err := newSpoolWriter(s, streamMember(memberID, "stderr"), s.config.MaxBytesPerStream, cancel)
	if err != nil {
		_ = stdout.close()
		return nil, err
	}
	e := &entry{record: types.CommandOutputRecord{
		ArchiveID: s.archiveID, RunID: meta.RunID, ParentRunID: meta.ParentRunID,
		Turn: meta.Turn, ToolUseID: meta.ToolUseID, StartedAt: time.Now(),
	}}
	s.mu.Lock()
	if _, exists := s.entries[key]; exists {
		s.mu.Unlock()
		_ = stdout.close()
		_ = stderr.close()
		return nil, fmt.Errorf("duplicate command output capture for tool use %q", meta.ToolUseID)
	}
	s.entries[key] = e
	s.mu.Unlock()
	return &Capture{store: s, entry: e, stdout: stdout, stderr: stderr}, nil
}

// newSpoolWriter opens the canonical archive member for a stream. The spool
// and the archived copy are the same file: with scrubbing applied on write
// there is no second, redacted copy to produce at completion.
func newSpoolWriter(store *Store, member string, limit int64, cancel context.CancelCauseFunc) (*spoolWriter, error) {
	path := filepath.Join(store.root, filepath.FromSlash(member))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		wrapped := fmt.Errorf("%w: create spool: %v", ErrCaptureIO, err)
		store.fail(wrapped)
		cancel(wrapped)
		return nil, wrapped
	}
	w := &spoolWriter{
		store: store, file: f, member: member, rawHash: sha256.New(),
		scrubbedHash: sha256.New(), limit: limit, cancel: cancel,
	}
	w.scrub = security.NewScrubWriter(spoolSink{w})
	return w, nil
}

// spoolSink is the ScrubWriter's destination: the only bytes it ever sees
// are scrubbed, so hashing and counting here describes exactly what is on
// disk.
type spoolSink struct{ w *spoolWriter }

func (s spoolSink) Write(p []byte) (int, error) {
	n, err := s.w.file.Write(p)
	if n > 0 {
		_, _ = s.w.scrubbedHash.Write(p[:n])
		s.w.written += int64(n)
	}
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func (w *spoolWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed != nil {
		return 0, w.failed
	}
	if w.closed {
		return 0, os.ErrClosed
	}
	w.store.mu.Lock()
	streamExceeded := w.count+int64(len(p)) > w.limit
	runExceeded := w.store.totalRaw+int64(len(p)) > w.store.config.MaxBytesPerRun
	if streamExceeded || runExceeded {
		err := fmt.Errorf("%w: per-stream=%d/%d run=%d/%d", ErrCaptureLimit,
			w.count+int64(len(p)), w.limit, w.store.totalRaw+int64(len(p)), w.store.config.MaxBytesPerRun)
		// A limit breach always cancels the offending command, but only
		// the strict posture poisons the store: under bestEffort later
		// commands keep capturing (run-total accounting still applies —
		// once totalRaw is at the cap every subsequent write breaches).
		if w.store.config.FailurePosture != types.CommandOutputPostureBestEffort && w.store.fatalErr == nil {
			w.store.fatalErr = err
		}
		w.store.mu.Unlock()
		w.failed = err
		w.cancel(err)
		return 0, err
	}
	w.store.totalRaw += int64(len(p))
	w.store.mu.Unlock()
	// The raw hash covers the stream as produced, so it stays independent of
	// how much of it the scrubber has flushed to disk.
	_, _ = w.rawHash.Write(p)
	w.count += int64(len(p))
	n, err := w.scrub.Write(p)
	if err != nil {
		wrapped := fmt.Errorf("%w: write spool: %v", ErrCaptureIO, err)
		w.failed = wrapped
		w.store.fail(wrapped)
		w.cancel(wrapped)
		return n, wrapped
	}
	return len(p), nil
}

func (w *spoolWriter) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return w.failed
	}
	w.closed = true
	if err := w.scrub.Close(); err != nil && w.failed == nil {
		w.failed = fmt.Errorf("%w: flush spool: %v", ErrCaptureIO, err)
		w.store.fail(w.failed)
	}
	if err := w.file.Close(); err != nil && w.failed == nil {
		w.failed = fmt.Errorf("%w: close spool: %v", ErrCaptureIO, err)
		w.store.fail(w.failed)
	}
	return w.failed
}

func (w *spoolWriter) rawSum() string { return hex.EncodeToString(w.rawHash.Sum(nil)) }

func (w *spoolWriter) scrubbedSum() string { return hex.EncodeToString(w.scrubbedHash.Sum(nil)) }

func (s *Store) fail(err error) {
	// Pure limit breaches are per-command failures under bestEffort;
	// storage (IO) failures poison the store in both postures.
	if s.config.FailurePosture == types.CommandOutputPostureBestEffort &&
		errors.Is(err, ErrCaptureLimit) && !errors.Is(err, ErrCaptureIO) {
		return
	}
	s.mu.Lock()
	if s.fatalErr == nil {
		s.fatalErr = err
	}
	s.mu.Unlock()
}

func (c *Capture) Stdout() io.Writer { return c.stdout }
func (c *Capture) Stderr() io.Writer { return c.stderr }

func (c *Capture) Complete(status Completion) (Captured, error) {
	stdoutClose := c.stdout.close()
	stderrClose := c.stderr.close()
	record := &c.entry.record
	record.CompletedAt = time.Now()
	record.ExitCode = status.ExitCode
	record.TimedOut = status.TimedOut
	record.Cancelled = status.Cancelled

	stdout, stdoutPath, stdoutMeta, stdoutErr := c.canonicalize("stdout", c.stdout)
	stderr, stderrPath, stderrMeta, stderrErr := c.canonicalize("stderr", c.stderr)
	record.Stdout, record.Stderr = stdoutMeta, stderrMeta
	c.entry.stdoutPath, c.entry.stderrPath = stdoutPath, stderrPath

	err := errors.Join(stdoutClose, stderrClose, stdoutErr, stderrErr)
	record.CaptureComplete = err == nil
	if err != nil {
		record.CaptureError = security.Scrub(err.Error())
		c.store.fail(err)
	}
	return Captured{Record: *record, Stdout: stdout, Stderr: stderr}, err
}

// canonicalize records the completed stream. The spool file is already the
// canonical scrubbed member, so this only publishes its metadata and reads
// back the bounded tail the model sees — the complete stream is never held
// in memory.
func (c *Capture) canonicalize(stream string, w *spoolWriter) (string, string, types.CommandOutputStreamRecord, error) {
	path := w.file.Name()
	stats := w.scrub.Stats()
	// The reference must stay short: security.GuardToolCall's
	// encoded_payload rule rejects base64-like runs over 100 characters,
	// and an earlier reference embedding archiveID plus the base64url
	// member ID tripped it, denying the model's first read. A truncated
	// digest stays under the threshold; refs carries the file mapping.
	refID := sha256.Sum256([]byte(c.entry.record.RunID + "\x00" + c.entry.record.ToolUseID))
	ref := fmt.Sprintf("stirrup://command-output/%s/%s", hex.EncodeToString(refID[:8]), stream)
	meta := types.CommandOutputStreamRecord{
		RawBytes: w.count, RawSHA256: w.rawSum(),
		ScrubbedBytes: w.written, ScrubbedSHA256: w.scrubbedSum(),
		ArchiveMember: w.member, Reference: ref,
		RedactionCount: stats.Count, RedactionPatterns: stats.Patterns,
	}
	c.store.mu.Lock()
	c.store.refs[ref] = streamRef{entry: c.entry, stream: stream, path: path}
	c.store.mu.Unlock()
	retain := c.store.config.InlineMaxBytes
	if c.store.config.PreviewBytesPerStream > retain {
		retain = c.store.config.PreviewBytesPerStream
	}
	modelContent, err := readTail(path, w.written, retain)
	if err != nil {
		return "", path, meta, fmt.Errorf("%w: read %s tail: %v", ErrCaptureIO, stream, err)
	}
	return modelContent, path, meta, nil
}

// readTail returns the last retain bytes of a completed stream.
func readTail(path string, size, retain int64) (string, error) {
	if size <= 0 || retain <= 0 {
		return "", nil
	}
	offset := int64(0)
	if size > retain {
		offset = size - retain
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, size-offset)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return string(buf[:n]), nil
}

func streamMember(memberID, stream string) string {
	return filepath.ToSlash(filepath.Join("commands", memberID, stream+".txt"))
}

// sweepOrphanedSpools removes spool roots left behind by a run that never
// finalized. The age gate is what keeps it from racing a live run's root.
func sweepOrphanedSpools(dir string, maxAge time.Duration, now time.Time) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), spoolPrefix) {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < maxAge {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err == nil {
			removed++
		}
	}
	return removed
}

// RecordInitial persists the exact scrubbed result exposed to the model and
// emits the now-complete metadata record to the configured trace recorder.
func (s *Store) RecordInitial(record *types.CommandOutputRecord, result string) error {
	key := record.RunID + "\x00" + record.ToolUseID
	s.mu.Lock()
	e := s.entries[key]
	recorder := s.recorder
	s.mu.Unlock()
	if e == nil {
		return fmt.Errorf("command output record not found for %q", record.ToolUseID)
	}
	memberID := encodedID(record.RunID + "-" + record.ToolUseID)
	member := filepath.ToSlash(filepath.Join("model-visible", memberID, "initial-result.txt"))
	path := filepath.Join(s.root, filepath.FromSlash(member))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		wrapped := fmt.Errorf("%w: write initial result directory: %v", ErrCaptureIO, err)
		s.fail(wrapped)
		return wrapped
	}
	if err := os.WriteFile(path, []byte(result), 0o600); err != nil {
		wrapped := fmt.Errorf("%w: write initial result: %v", ErrCaptureIO, err)
		s.fail(wrapped)
		return wrapped
	}
	sum := sha256.Sum256([]byte(result))
	e.mu.Lock()
	e.initialPath = path
	e.record.InitialResultSHA256 = hex.EncodeToString(sum[:])
	e.record.InitialResultMember = member
	*record = e.record
	copyRecord := e.record
	e.recorderSent = true
	e.mu.Unlock()
	if recorder != nil {
		recorder.RecordCommandOutput(copyRecord)
	}
	return nil
}

func (s *Store) Read(ref string, offset, limit int64) (ReadResult, error) {
	if offset < 0 {
		return ReadResult{}, fmt.Errorf("offset must not be negative")
	}
	if limit <= 0 {
		limit = ReadDefaultBytes
	}
	if limit > ReadMaxBytes {
		limit = ReadMaxBytes
	}
	s.mu.Lock()
	r, ok := s.refs[ref]
	s.mu.Unlock()
	if !ok {
		return ReadResult{}, fmt.Errorf("unknown command output reference")
	}
	var meta types.CommandOutputStreamRecord
	r.entry.mu.Lock()
	if r.stream == "stdout" {
		meta = r.entry.record.Stdout
	} else {
		meta = r.entry.record.Stderr
	}
	r.entry.mu.Unlock()
	if offset > meta.ScrubbedBytes {
		return ReadResult{}, fmt.Errorf("offset %d exceeds stream size %d", offset, meta.ScrubbedBytes)
	}
	f, err := os.Open(r.path)
	if err != nil {
		return ReadResult{}, fmt.Errorf("open command output: %w", err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, limit)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return ReadResult{}, fmt.Errorf("read command output: %w", err)
	}
	buf = buf[:n]
	end := offset + int64(n)
	return ReadResult{Content: meta, Bytes: buf, Offset: offset, End: end, EOF: end >= meta.ScrubbedBytes, Stream: r.stream}, nil
}

// RecordRead adds the exact model-visible read result to the access ledger.
// The archive member is scoped by the reader's run ID, matching every other
// member path: the store is shared across a parent run and its subagents, so
// a bare tool-use ID can collide across conversations and silently overwrite
// a ledger file.
func (s *Store) RecordRead(reader tool.CallContext, ref string, result ReadResult, modelVisible string) error {
	s.mu.Lock()
	r, ok := s.refs[ref]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown command output reference")
	}
	member := filepath.ToSlash(filepath.Join("model-visible", encodedID(reader.RunID+"-"+reader.ToolUseID), "chunk.txt"))
	path := filepath.Join(s.root, filepath.FromSlash(member))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		wrapped := fmt.Errorf("%w: write read ledger directory: %v", ErrCaptureIO, err)
		s.fail(wrapped)
		return wrapped
	}
	if err := os.WriteFile(path, []byte(modelVisible), 0o600); err != nil {
		wrapped := fmt.Errorf("%w: write read ledger: %v", ErrCaptureIO, err)
		s.fail(wrapped)
		return wrapped
	}
	sum := sha256.Sum256([]byte(modelVisible))
	read := types.CommandOutputReadRecord{
		ToolUseID: reader.ToolUseID, Reference: ref, Offset: result.Offset,
		EndOffset: result.End, EOF: result.EOF, ResultSHA256: hex.EncodeToString(sum[:]),
		ArchiveMember: member,
	}
	r.entry.mu.Lock()
	r.entry.record.Reads = append(r.entry.record.Reads, read)
	r.entry.modelFiles = append(r.entry.modelFiles, path)
	r.entry.mu.Unlock()
	return nil
}

func (s *Store) Finalize(ctx context.Context) (string, error) {
	s.mu.Lock()
	if s.finalized {
		archive := s.archiveURI
		if archive == "" {
			archive = s.archivePath
		}
		s.mu.Unlock()
		return archive, nil
	}
	s.finalized = true
	entries := make([]*entry, 0, len(s.entries))
	for _, e := range s.entries {
		entries = append(entries, e)
	}
	fatal := s.fatalErr
	s.mu.Unlock()
	if len(entries) == 0 {
		_ = os.RemoveAll(s.root)
		return "", nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].record.ToolUseID < entries[j].record.ToolUseID })
	commands := make([]types.CommandOutputRecord, 0, len(entries))
	allComplete := true
	for _, e := range entries {
		e.mu.Lock()
		commands = append(commands, e.record)
		allComplete = allComplete && e.record.CaptureComplete
		e.mu.Unlock()
	}
	// Complete requires every capture to have finished cleanly, not just
	// the absence of a store-fatal error: under bestEffort a limit breach
	// fails only its own command and must still be visible here.
	m := manifest{SchemaVersion: 1, ArchiveID: s.archiveID, CreatedAt: time.Now(), Complete: fatal == nil && allComplete, Commands: commands}
	if fatal != nil {
		m.Failure = security.Scrub(fatal.Error())
	}
	if err := s.writeArchive(m, entries); err != nil {
		s.fail(fmt.Errorf("archive finalization: %w", err))
		m.Complete = false
		m.Failure = security.Scrub(err.Error())
		_ = s.writeFailureArchive(m)
		return s.archivePath, err
	}
	archive := s.archivePath
	if s.uploader != nil {
		uri, err := s.uploader.UploadCommandOutputArchive(ctx, s.archivePath, s.archiveID)
		if err != nil {
			s.fail(fmt.Errorf("archive upload: %w", err))
			m.Complete = false
			m.Failure = security.Scrub(err.Error())
			_ = s.writeArchive(m, entries)
			return s.archivePath, err
		}
		archive = uri
		s.mu.Lock()
		s.archiveURI = uri
		s.mu.Unlock()
		_ = os.Remove(s.archivePath)
	}
	_ = os.RemoveAll(s.root)
	return archive, nil
}

// Close removes unfinished spool state. Finalized archives are outside root
// and are never removed by Close.
func (s *Store) Close() error { return os.RemoveAll(s.root) }

func (s *Store) writeArchive(m manifest, entries []*entry) error {
	if err := os.MkdirAll(filepath.Dir(s.archivePath), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.archivePath), ".command-output-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	manifestBytes, err := json.MarshalIndent(m, "", "  ")
	if err == nil {
		err = writeTarBytes(tw, "manifest.json", manifestBytes)
	}
	if err == nil {
		for _, e := range entries {
			e.mu.Lock()
			files := []struct{ name, path string }{
				{e.record.Stdout.ArchiveMember, e.stdoutPath}, {e.record.Stderr.ArchiveMember, e.stderrPath},
				{e.record.InitialResultMember, e.initialPath},
			}
			for i, read := range e.record.Reads {
				if i < len(e.modelFiles) {
					files = append(files, struct{ name, path string }{read.ArchiveMember, e.modelFiles[i]})
				}
			}
			e.mu.Unlock()
			for _, f := range files {
				if f.name == "" || f.path == "" {
					continue
				}
				if err = writeTarFile(tw, f.name, f.path); err != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
	}
	if closeErr := tw.Close(); err == nil {
		err = closeErr
	}
	if closeErr := gz.Close(); err == nil {
		err = closeErr
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.archivePath); err != nil {
		return err
	}
	return os.Chmod(s.archivePath, 0o600)
}

func (s *Store) writeFailureArchive(m manifest) error { return s.writeArchive(m, nil) }

func writeTarBytes(tw *tar.Writer, name string, data []byte) error {
	h := &tar.Header{Name: filepath.ToSlash(name), Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func writeTarFile(tw *tar.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	h := &tar.Header{Name: filepath.ToSlash(name), Mode: 0o600, Size: info.Size(), ModTime: time.Now()}
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

func encodedID(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func safeID(value string) string {
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), ".")
}
