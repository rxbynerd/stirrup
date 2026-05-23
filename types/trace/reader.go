// Package trace provides offline parsers for the JSONL trace files
// stirrup writes via traceEmitter.type=jsonl.
//
// Two on-wire shapes are supported, and Reader transparently handles
// both:
//
//   - The legacy single-blob shape: one types.RunTrace per line, with
//     a complete run emitting one line at Finish.
//   - The streaming event shape (since #270): line-delimited events
//     with a "kind" discriminator — run_started, turn_record,
//     tool_call_record, run_finished. The legacy shape is treated as
//     an implicit run_finished event with no preceding events.
//
// For backward compatibility, Reader.Next continues to yield
// types.RunTrace values; on a streaming-format file it surfaces the
// trace embedded in the run_finished event and skips the other event
// kinds. Consumers that want full transcript reassembly use
// ReadRecording, which walks the stream and returns a
// *types.RunRecording.
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
// Both wire shapes are accepted: a legacy single-blob line is decoded
// as a types.RunTrace directly; a streaming-event line is decoded as
// an event and yields a *RunTrace only when its kind is run_finished
// (other event kinds — run_started, turn_record, tool_call_record —
// are silently skipped so legacy consumers see one RunTrace per
// completed run regardless of the on-disk format).
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
		trace, ok, err := decodeRunTraceLine(line)
		if err != nil {
			r.logger.Debug("trace.Reader: skip malformed line",
				"err", err.Error(),
				"bytes", len(line),
			)
			continue
		}
		if !ok {
			// Streaming event with no embedded RunTrace (run_started,
			// turn_record, tool_call_record). Legacy consumers see
			// only run_finished records; skip the rest.
			continue
		}
		return trace, nil
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

// EventKind names a streaming-trace event kind. The values match the
// discriminator emitted by harness/internal/trace; they are defined
// here as untyped string constants so this package does not import the
// harness internal trace package (which depends on types).
const (
	eventKindRunStarted     = "run_started"
	eventKindTurnRecord     = "turn_record"
	eventKindToolCallRecord = "tool_call_record"
	eventKindRunFinished    = "run_finished"
)

// streamedEvent mirrors the on-wire shape of harness/internal/trace.Event
// at the fields the reader cares about. Re-defining the shape locally
// avoids an internal-package import while keeping the decoder lean.
type streamedEvent struct {
	Kind          string                 `json:"kind"`
	SchemaVersion string                 `json:"schemaVersion,omitempty"`
	RunID         string                 `json:"runId,omitempty"`
	StartedAt     *time.Time             `json:"startedAt,omitempty"`
	CompletedAt   *time.Time             `json:"completedAt,omitempty"`
	Config        *types.RunConfig       `json:"config,omitempty"`
	Turn          int                    `json:"turn,omitempty"`
	ModelInput    *types.ModelInput      `json:"modelInput,omitempty"`
	ModelOutput   []types.ContentBlock   `json:"modelOutput,omitempty"`
	ToolCalls     []types.ToolCallRecord `json:"toolCalls,omitempty"`
	ToolCall      *types.ToolCallRecord  `json:"toolCall,omitempty"`
	Trace         *types.RunTrace        `json:"trace,omitempty"`
}

// kindOnly is used to peek at the discriminator without decoding the
// rest of the line. A line missing "kind" is the legacy single-blob
// shape and decodes directly as types.RunTrace.
type kindOnly struct {
	Kind string `json:"kind"`
}

// decodeRunTraceLine decodes a single JSONL line and returns either a
// RunTrace (legacy shape, or the trace embedded in a run_finished
// event), nothing (ok=false: a streaming event with no embedded
// trace), or an error (malformed JSON).
func decodeRunTraceLine(line []byte) (*types.RunTrace, bool, error) {
	var probe kindOnly
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, false, err
	}
	if probe.Kind == "" {
		// Legacy single-blob shape.
		var trace types.RunTrace
		if err := json.Unmarshal(line, &trace); err != nil {
			return nil, false, err
		}
		return &trace, true, nil
	}
	if probe.Kind != eventKindRunFinished {
		return nil, false, nil
	}
	var ev streamedEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil, false, err
	}
	if ev.Trace == nil {
		// A run_finished without an embedded trace is malformed but
		// not fatal — the reader skips it like any other unusable
		// line.
		return nil, false, nil
	}
	return ev.Trace, true, nil
}

// ReadRecording walks the stream and reassembles a *types.RunRecording
// from a streaming-event trace file. A complete recording requires a
// run_started event (for RunID and Config) plus zero or more
// turn_record events (for the transcript) plus a run_finished event
// (for FinalOutcome). An interrupted run with no run_finished event
// is returned with FinalOutcome zero-valued; the caller can detect
// this via FinalOutcome.ID == "".
//
// A legacy single-blob trace (no kind discriminator) is treated as a
// recording with no transcript turns: RunID and Config come from the
// embedded RunTrace, FinalOutcome is the trace itself, and Turns is
// nil. This lets a single consumer accept both wire shapes without
// branching on the format.
//
// Malformed and oversized lines are skipped with a slog.Debug log per
// the policy that governs Next. The reader is consumed end-to-end;
// the caller does not call Next afterwards.
func (r *Reader) ReadRecording() (*types.RunRecording, error) {
	recording := &types.RunRecording{}
	seenStarted := false
	for r.scanner.Scan() {
		line := r.scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var probe kindOnly
		if err := json.Unmarshal(line, &probe); err != nil {
			r.logger.Debug("trace.Reader: skip malformed line",
				"err", err.Error(),
				"bytes", len(line),
			)
			continue
		}
		if probe.Kind == "" {
			// Legacy: one RunTrace blob is the entire recording.
			var trace types.RunTrace
			if err := json.Unmarshal(line, &trace); err != nil {
				r.logger.Debug("trace.Reader: skip malformed legacy line",
					"err", err.Error(),
					"bytes", len(line),
				)
				continue
			}
			recording.RunID = trace.ID
			recording.Config = trace.Config
			recording.FinalOutcome = trace
			continue
		}
		var ev streamedEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			r.logger.Debug("trace.Reader: skip malformed event",
				"err", err.Error(),
				"bytes", len(line),
			)
			continue
		}
		switch ev.Kind {
		case eventKindRunStarted:
			seenStarted = true
			recording.RunID = ev.RunID
			if ev.Config != nil {
				recording.Config = *ev.Config
			}
		case eventKindTurnRecord:
			tr := types.TurnRecord{
				Turn:        ev.Turn,
				ModelOutput: ev.ModelOutput,
				ToolCalls:   ev.ToolCalls,
			}
			if ev.ModelInput != nil {
				tr.ModelInput = *ev.ModelInput
			}
			recording.Turns = append(recording.Turns, tr)
		case eventKindRunFinished:
			if ev.Trace != nil {
				recording.FinalOutcome = *ev.Trace
				if !seenStarted {
					// No run_started observed (e.g. a legacy stream
					// that prefixed a single blob with the new wire
					// shape, or a recording that lost its preamble).
					// Fall back to fields on the embedded trace.
					recording.RunID = ev.Trace.ID
					recording.Config = ev.Trace.Config
				}
			}
		default:
			// Unknown kind: skip. Forward compatibility — a future
			// event kind added to the wire must not abort old
			// readers.
		}
	}
	if err := r.scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			r.logger.Debug("trace.Reader: skip oversized line",
				"cap", MaxLineBytes,
			)
			r.scanner = bufio.NewScanner(r.src)
			r.scanner.Buffer(make([]byte, 0, initialScanBuf), MaxLineBytes)
			// Re-enter the loop to consume any remaining lines past
			// the oversized one. ReadRecording returns whatever it
			// accumulated; a single bad line should not lose the
			// surrounding context.
			return r.ReadRecording()
		}
		return recording, fmt.Errorf("reading trace file: %w", err)
	}
	return recording, nil
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
//
// Truncation and rotation are NOT followed: Tail keeps the same file
// handle open across polls, so if the file is truncated or rotated
// out from under it, the kernel offset stays past the new EOF and
// the follow loop appears to stall with no further output and no
// error. Restart the command to pick up the rotated file. Full
// inode-tracking semantics (`tail --follow=name`) are intentionally
// out of scope; trace files are written append-only by stirrup.
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
