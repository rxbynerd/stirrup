// Package commandoutput owns complete, scrubbed run_command output capture.
//
// Raw (unscrubbed) bytes are spooled to run-scoped 0600 files under a 0700
// temp directory while a command streams, and each spool is deleted when its
// command completes and whole-stream redaction has produced the scrubbed
// canonical copy. Durable archives contain scrubbed bytes only. The raw
// spool's lifetime therefore depends on clean completion: a crash or SIGKILL
// mid-command can leave raw spool files in the OS temp directory until it is
// cleared (scrub-on-write, which would remove the raw-bytes-at-rest window
// entirely, is tracked as a follow-up).
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

type spoolWriter struct {
	store  *Store
	file   *os.File
	hash   hash.Hash
	count  int64
	limit  int64
	cancel context.CancelCauseFunc
	failed error
	closed bool
	mu     sync.Mutex
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
	root, err := os.MkdirTemp("", "stirrup-command-output-")
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
	dirName := encodedID(meta.RunID + "-" + meta.ToolUseID)
	dir := filepath.Join(s.root, "commands", dirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.fail(fmt.Errorf("%w: create command spool directory: %v", ErrCaptureIO, err))
		return nil, s.FatalError()
	}
	stdout, err := newSpoolWriter(s, filepath.Join(dir, "stdout.raw"), s.config.MaxBytesPerStream, cancel)
	if err != nil {
		return nil, err
	}
	stderr, err := newSpoolWriter(s, filepath.Join(dir, "stderr.raw"), s.config.MaxBytesPerStream, cancel)
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

func newSpoolWriter(store *Store, path string, limit int64, cancel context.CancelCauseFunc) (*spoolWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		wrapped := fmt.Errorf("%w: create spool: %v", ErrCaptureIO, err)
		store.fail(wrapped)
		cancel(wrapped)
		return nil, wrapped
	}
	return &spoolWriter{store: store, file: f, hash: sha256.New(), limit: limit, cancel: cancel}, nil
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
	n, err := w.file.Write(p)
	if n > 0 {
		_, _ = w.hash.Write(p[:n])
		w.count += int64(n)
	}
	if err != nil || n != len(p) {
		if err == nil {
			err = io.ErrShortWrite
		}
		wrapped := fmt.Errorf("%w: write spool: %v", ErrCaptureIO, err)
		w.failed = wrapped
		w.store.fail(wrapped)
		w.cancel(wrapped)
		return n, wrapped
	}
	return n, nil
}

func (w *spoolWriter) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return w.failed
	}
	w.closed = true
	if err := w.file.Close(); err != nil && w.failed == nil {
		w.failed = fmt.Errorf("%w: close spool: %v", ErrCaptureIO, err)
		w.store.fail(w.failed)
	}
	return w.failed
}

func (w *spoolWriter) sum() string { return hex.EncodeToString(w.hash.Sum(nil)) }

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

func (c *Capture) canonicalize(stream string, w *spoolWriter) (string, string, types.CommandOutputStreamRecord, error) {
	rawPath := w.file.Name()
	meta := types.CommandOutputStreamRecord{RawBytes: w.count, RawSHA256: w.sum()}
	defer func() { _ = os.Remove(rawPath) }()
	raw, err := os.ReadFile(rawPath)
	if err != nil {
		return "", "", meta, fmt.Errorf("%w: read complete %s spool: %v", ErrCaptureIO, stream, err)
	}
	scrubbed, stats := security.ScrubWithStats(string(raw))
	memberID := encodedID(c.entry.record.RunID + "-" + c.entry.record.ToolUseID)
	member := filepath.ToSlash(filepath.Join("commands", memberID, stream+".txt"))
	path := filepath.Join(c.store.root, filepath.FromSlash(member))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", "", meta, fmt.Errorf("%w: create canonical directory: %v", ErrCaptureIO, err)
	}
	if err := os.WriteFile(path, []byte(scrubbed), 0o600); err != nil {
		return "", "", meta, fmt.Errorf("%w: write canonical %s: %v", ErrCaptureIO, stream, err)
	}
	sum := sha256.Sum256([]byte(scrubbed))
	// The model-visible reference must stay short: the loop's tool guard
	// rejects inputs containing base64-like runs longer than 100
	// characters (security.GuardToolCall's encoded_payload rule), and a
	// reference embedding archiveID plus the base64url member ID tripped
	// it — the model's first read_command_output call was denied. A
	// truncated digest of the store-unique entry key keeps the reference
	// opaque, collision-safe within the run, and far under the guard's
	// threshold; the refs map carries the actual file mapping.
	refID := sha256.Sum256([]byte(c.entry.record.RunID + "\x00" + c.entry.record.ToolUseID))
	ref := fmt.Sprintf("stirrup://command-output/%s/%s", hex.EncodeToString(refID[:8]), stream)
	meta.ScrubbedBytes = int64(len(scrubbed))
	meta.ScrubbedSHA256 = hex.EncodeToString(sum[:])
	meta.ArchiveMember = member
	meta.Reference = ref
	meta.RedactionCount = stats.Count
	meta.RedactionPatterns = append([]string(nil), stats.Patterns...)
	c.store.mu.Lock()
	c.store.refs[ref] = streamRef{entry: c.entry, stream: stream, path: path}
	c.store.mu.Unlock()
	modelContent := scrubbed
	retain := c.store.config.InlineMaxBytes
	if c.store.config.PreviewBytesPerStream > retain {
		retain = c.store.config.PreviewBytesPerStream
	}
	if int64(len(modelContent)) > retain {
		// Copy the tail so the small preview does not retain the complete
		// scrubbed stream's backing allocation after canonicalization.
		modelContent = string(append([]byte(nil), modelContent[len(modelContent)-int(retain):]...))
	}
	return modelContent, path, meta, nil
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
