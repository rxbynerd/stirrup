// Package trace provides offline parsers for the JSONL trace files
// stirrup writes via traceEmitter.type=jsonl.
//
// The on-wire shape is one types.RunTrace per line. A run can produce
// multiple lines (e.g. when a partial trace is appended ahead of the
// final summary by future emitters); consumers are expected to walk
// every record and either project them into typed sub-events or
// collapse onto the last record (the historical contract of the eval
// runner's parseTraceFile).
//
// Reader is the single-pass streaming entry point. Tail consumes the
// same scanner shape but polls the underlying file for appended data
// so operators can watch an in-progress run. Both helpers skip
// malformed lines with a slog.Debug log — a truncated tail of an
// in-flight JSONL file must not fail the surrounding command.
package trace

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/rxbynerd/stirrup/types"
)

// MaxLineBytes is the per-record line cap the reader honours. Matches
// the trace emitter's own cap so a record that fit on write also fits
// on read. A line larger than the cap is skipped with a slog.Debug log
// (consistent with the malformed-line policy) rather than aborting the
// scan.
const MaxLineBytes = 4 * 1024 * 1024

// initialScanBuf is the bufio.Scanner starting buffer. It grows up to
// MaxLineBytes when a single record is larger than 256 KiB. The choice
// matches eval/runner/runner.go's prior local copy so the
// extracted reader preserves the same memory footprint for the common
// "small line" case.
const initialScanBuf = 256 * 1024

// defaultTailPollInterval is the poll cadence Tail uses when the
// caller does not override it. 100 ms keeps a live `tail -f` view
// responsive without burning CPU on a quiet file.
const defaultTailPollInterval = 100 * time.Millisecond

// Reader streams RunTrace records from a JSONL source.
//
// Reader is single-threaded; concurrent calls to Next on the same
// Reader are not supported. The reader owns the bufio.Scanner and the
// optional io.Closer it was constructed with.
type Reader struct {
	src     io.Reader
	closer  io.Closer
	scanner *bufio.Scanner
	logger  *slog.Logger
}

// ReaderOption customises a Reader.
type ReaderOption func(*Reader)

// WithLogger installs a slog.Logger that receives debug-level messages
// for every malformed or oversized line the reader skips. The default
// is slog.Default(); pass slog.New(slog.DiscardHandler) to silence the
// reader entirely.
func WithLogger(l *slog.Logger) ReaderOption {
	return func(r *Reader) {
		if l != nil {
			r.logger = l
		}
	}
}

// NewReader wraps r in a Reader. The caller retains ownership of r;
// Close on the returned Reader does NOT close r.
func NewReader(r io.Reader, opts ...ReaderOption) *Reader {
	reader := &Reader{
		src:     r,
		logger:  slog.Default(),
		scanner: bufio.NewScanner(r),
	}
	reader.scanner.Buffer(make([]byte, 0, initialScanBuf), MaxLineBytes)
	for _, opt := range opts {
		opt(reader)
	}
	return reader
}

// Open opens path for reading and returns a Reader that owns the file.
// The special path "-" reads from os.Stdin; the resulting Reader does
// not close stdin on Close.
func Open(path string, opts ...ReaderOption) (*Reader, error) {
	if path == "-" {
		return NewReader(os.Stdin, opts...), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening trace file %q: %w", path, err)
	}
	r := NewReader(f, opts...)
	r.closer = f
	return r, nil
}

// Close releases the underlying file when Reader was constructed via
// Open on a real path. NewReader-constructed Readers and stdin-backed
// Readers report nil.
func (r *Reader) Close() error {
	if r.closer == nil {
		return nil
	}
	return r.closer.Close()
}

// resetScanner discards any cached EOF on the underlying scanner so a
// follower can resume past the previous end of file once new bytes
// have been appended. The file offset is preserved by the underlying
// io.Reader, so a fresh scanner picks up exactly at the previous
// stopping point.
func (r *Reader) resetScanner() {
	r.scanner = bufio.NewScanner(r.src)
	r.scanner.Buffer(make([]byte, 0, initialScanBuf), MaxLineBytes)
}

// Next returns the next RunTrace record in the stream.
//
// Malformed JSON lines and lines exceeding MaxLineBytes are skipped
// with a slog.Debug log; Next continues past them to the next
// well-formed record. An io.EOF is returned when the stream is
// exhausted with no further well-formed records to yield. Any other
// I/O error from the underlying scanner is surfaced verbatim.
func (r *Reader) Next() (*types.RunTrace, error) {
	for r.scanner.Scan() {
		line := r.scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var trace types.RunTrace
		if err := json.Unmarshal(line, &trace); err != nil {
			r.logger.Debug("trace.Reader: skip malformed line",
				"err", err.Error(),
				"bytes", len(line),
			)
			continue
		}
		return &trace, nil
	}
	if err := r.scanner.Err(); err != nil {
		// bufio.ErrTooLong is the cap-exceeded signal; surface it as a
		// debug-skipped record and reset the scanner so the next Next
		// can resume past the offending line. The reader's contract
		// promises a single oversized record never aborts the scan.
		if errors.Is(err, bufio.ErrTooLong) {
			r.logger.Debug("trace.Reader: skip oversized line",
				"cap", MaxLineBytes,
			)
			r.scanner = bufio.NewScanner(r.src)
			r.scanner.Buffer(make([]byte, 0, initialScanBuf), MaxLineBytes)
			return r.Next()
		}
		return nil, fmt.Errorf("reading trace file: %w", err)
	}
	return nil, io.EOF
}

// All drains the reader into a slice. The slice is empty when the
// stream contained no well-formed records (e.g. the file is empty or
// every line was malformed); io.EOF is consumed and not surfaced.
func (r *Reader) All() ([]types.RunTrace, error) {
	var out []types.RunTrace
	for {
		t, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, *t)
	}
}

// Last reads the stream to completion and returns the last well-formed
// RunTrace, or an error if no records were present. Matches the
// historical semantics the eval runner's parseTraceFile exposed: the
// trailing line is the authoritative summary of a completed run.
func (r *Reader) Last() (*types.RunTrace, error) {
	var last *types.RunTrace
	for {
		t, err := r.Next()
		if errors.Is(err, io.EOF) {
			if last == nil {
				return nil, fmt.Errorf("trace file is empty")
			}
			return last, nil
		}
		if err != nil {
			return last, err
		}
		last = t
	}
}

// TailOptions configures Tail.
type TailOptions struct {
	// Follow keeps Tail running past EOF, polling the underlying file
	// for appended records. When false, Tail terminates the moment the
	// stream is exhausted (one-shot semantics, identical to All).
	Follow bool

	// PollInterval is the cadence Tail polls the file for appended
	// data when Follow is true. Defaults to 100 ms when zero.
	PollInterval time.Duration

	// Logger receives debug messages for malformed/oversized lines.
	// nil falls back to slog.Default().
	Logger *slog.Logger
}

// Tail reads path and invokes handle for every well-formed RunTrace
// record, returning when ctx is cancelled, the stream is exhausted
// (Follow=false), or handle returns a non-nil error. The special path
// "-" reads from os.Stdin and ignores Follow (stdin already has the
// "block until appended" semantics built in).
//
// Tail uses a polling loop — no fsnotify or platform-specific file
// notification — because the JSONL trace files are append-only and a
// 100 ms poll is sufficient to keep an operator's `tail -f` view
// responsive without inflating the dependency tree.
func Tail(ctx context.Context, path string, opts TailOptions, handle func(*types.RunTrace) error) error {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = defaultTailPollInterval
	}

	if path == "-" {
		r := NewReader(os.Stdin, WithLogger(logger))
		return streamReader(ctx, r, handle)
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening trace file %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	r := NewReader(f, WithLogger(logger))

	for {
		if err := streamReader(ctx, r, handle); err != nil {
			return err
		}
		if !opts.Follow {
			return nil
		}
		// bufio.Scanner caches the EOF it returned, so a second pass
		// over the same scanner would short-circuit even after new
		// bytes land on disk. Re-attach a fresh scanner to the same
		// open file: the file's offset is preserved, so we resume
		// exactly where the previous pass stopped.
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(poll):
		}
		r.resetScanner()
	}
}

// streamReader pumps every available record from r through handle until
// handle errors, ctx is cancelled, or the stream EOFs.
func streamReader(ctx context.Context, r *Reader, handle func(*types.RunTrace) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		t, err := r.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := handle(t); err != nil {
			return err
		}
	}
}
