// Package trace provides offline parsers for the JSONL trace files
// stirrup writes via traceEmitter.type=jsonl. See docs/trace-inspection.md
// for the on-wire shapes and CLI usage.
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

// MaxLineBytes is the per-record line cap the reader honours, matching
// the trace emitter's own cap.
const MaxLineBytes = 4 * 1024 * 1024

// initialScanBuf is the bufio.Scanner starting buffer; it grows up to
// MaxLineBytes for larger records.
const initialScanBuf = 256 * 1024

// defaultTailPollInterval is Tail's poll cadence when the caller does
// not override it.
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

// resetScanner discards any cached EOF so a follower can resume past
// the previous end of file once new bytes have been appended.
func (r *Reader) resetScanner() {
	r.scanner = bufio.NewScanner(r.src)
	r.scanner.Buffer(make([]byte, 0, initialScanBuf), MaxLineBytes)
}

// Next returns the next RunTrace record in the stream. Both wire
// shapes are accepted (docs/trace-inspection.md); on a streaming file
// it yields only run_finished events, skipping the rest.
//
// Malformed lines and lines exceeding MaxLineBytes are skipped with a
// slog.Debug log. Returns io.EOF when exhausted; other scanner errors
// are surfaced verbatim.
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
			continue
		}
		return trace, nil
	}
	if err := r.scanner.Err(); err != nil {
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
// from run_started, turn_record, and run_finished events. An
// interrupted run with no run_finished is returned with
// FinalOutcome.ID == "". A legacy single-blob trace is treated as a
// recording with no transcript turns.
//
// The reader is consumed end-to-end; the caller does not call Next
// afterwards.
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
					recording.RunID = ev.Trace.ID
					recording.Config = ev.Trace.Config
				}
			}
		default:
			// unknown kind: skip.
		}
	}
	if err := r.scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			r.logger.Debug("trace.Reader: skip oversized line",
				"cap", MaxLineBytes,
			)
			r.scanner = bufio.NewScanner(r.src)
			r.scanner.Buffer(make([]byte, 0, initialScanBuf), MaxLineBytes)
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
// RunTrace, or an error if no records were present.
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
// "-" reads from os.Stdin and ignores Follow. Truncation and rotation
// are not followed; see docs/trace-inspection.md.
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
