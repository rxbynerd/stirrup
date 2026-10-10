// Package trace provides offline parsers for the JSONL trace files
// stirrup writes via traceEmitter.type=jsonl. See docs/trace-inspection.md
// for the on-wire shapes and CLI usage.
package trace

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
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

	// skipped counts lines longer than MaxLineBytes that the scanner
	// discarded; discarding is true while the tail of such a line is
	// still being dropped. Both live on the Reader so they survive
	// resetScanner.
	skipped    int
	discarding bool
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
		src:    r,
		logger: slog.Default(),
	}
	reader.resetScanner()
	for _, opt := range opts {
		opt(reader)
	}
	return reader
}

// SkippedLines reports how many lines longer than MaxLineBytes the
// reader has discarded so far.
func (r *Reader) SkippedLines() int {
	return r.skipped
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
	r.scanner.Split(r.splitLines)
}

// splitLines is bufio.ScanLines that drops lines longer than
// MaxLineBytes instead of failing the scan. Once the buffer holds
// MaxLineBytes without a newline, the buffered bytes are discarded and
// every following byte up to and including the next newline is dropped
// as well, so the scanner resumes on the next line boundary.
func (r *Reader) splitLines(data []byte, atEOF bool) (int, []byte, error) {
	if r.discarding {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			r.discarding = false
			return i + 1, nil, nil
		}
		return len(data), nil, nil
	}
	advance, token, err := bufio.ScanLines(data, atEOF)
	if token != nil || advance > 0 || err != nil {
		return advance, token, err
	}
	if len(data) >= MaxLineBytes {
		r.skipped++
		r.discarding = true
		r.logger.Debug("trace.Reader: skip oversized line",
			"cap", MaxLineBytes,
		)
		return len(data), nil, nil
	}
	return 0, nil, nil
}

// Next returns the next RunTrace record in the stream. Both wire
// shapes are accepted (docs/trace-inspection.md); on a streaming file
// it yields only run_finished events, skipping the rest.
//
// Malformed lines and lines exceeding MaxLineBytes are skipped with a
// slog.Debug log; SkippedLines reports the oversized count. Returns
// io.EOF when exhausted; other scanner errors are surfaced verbatim.
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
		return nil, fmt.Errorf("reading trace file: %w", err)
	}
	return nil, io.EOF
}

// EventKind names a streaming-trace event kind. The values match the
// discriminator emitted by harness/internal/trace; they are defined
// here as untyped string constants so this package does not import the
// harness internal trace package (which depends on types).
const (
	eventKindRunStarted          = "run_started"
	eventKindTurnRecord          = "turn_record"
	eventKindToolCallRecord      = "tool_call_record"
	eventKindRunFinished         = "run_finished"
	eventKindCommandOutputRecord = "command_output_record"
	eventKindSubRunStarted       = "subrun_started"
	eventKindSubRunFinished      = "subrun_finished"
	eventKindSystemPrompt        = "system_prompt"
)

// streamedEvent mirrors the on-wire shape of harness/internal/trace.Event
// at the fields the reader cares about. Re-defining the shape locally
// avoids an internal-package import while keeping the decoder lean.
type streamedEvent struct {
	Kind          string                     `json:"kind"`
	SchemaVersion string                     `json:"schemaVersion,omitempty"`
	RunID         string                     `json:"runId,omitempty"`
	ParentRunID   string                     `json:"parentRunId,omitempty"`
	StartedAt     *time.Time                 `json:"startedAt,omitempty"`
	CompletedAt   *time.Time                 `json:"completedAt,omitempty"`
	Config        *types.RunConfig           `json:"config,omitempty"`
	Turn          int                        `json:"turn,omitempty"`
	ModelInput    *types.ModelInput          `json:"modelInput,omitempty"`
	ModelOutput   []types.ContentBlock       `json:"modelOutput,omitempty"`
	ToolCalls     []types.ToolCallRecord     `json:"toolCalls,omitempty"`
	ToolCall      *types.ToolCallRecord      `json:"toolCall,omitempty"`
	ResponseModel string                     `json:"responseModel,omitempty"`
	Trace         *types.RunTrace            `json:"trace,omitempty"`
	CommandOutput *types.CommandOutputRecord `json:"commandOutput,omitempty"`
	SubRun        *types.SubRunSummary       `json:"subRun,omitempty"`

	HarnessVersion      string `json:"harnessVersion,omitempty"`
	SystemPrompt        string `json:"systemPrompt,omitempty"`
	SystemPromptSHA256  string `json:"systemPromptSha256,omitempty"`
	OutputLimitRecovery bool   `json:"outputLimitRecovery,omitempty"`
	StallNudge          bool   `json:"stallNudge,omitempty"`
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
// from run_started, system_prompt, turn_record, and run_finished events. An
// interrupted run with no run_finished is returned with
// FinalOutcome.ID == "". A legacy single-blob trace is treated as a
// recording with no transcript turns.
//
// A turn_record that names a parent run and carries a run ID other than
// the recording's belongs to a sub-agent: it is grouped into
// RunRecording.SubRuns together with that child's subrun_started and
// subrun_finished events, and never appears in Turns. Records without
// run IDs, as in traces written before sub-run tagging, stay in Turns.
//
// Lines longer than MaxLineBytes are dropped without discarding the
// records around them; RunRecording.SkippedLines reports how many, and
// a non-zero count means the recording is incomplete.
//
// The reader is consumed end-to-end; the caller does not call Next
// afterwards.
func (r *Reader) ReadRecording() (*types.RunRecording, error) {
	recording := &types.RunRecording{}
	seenStarted := false
	subRunIndex := map[string]int{}
	subRun := func(runID, parentRunID string) *types.SubRunRecording {
		i, ok := subRunIndex[runID]
		if !ok {
			i = len(recording.SubRuns)
			subRunIndex[runID] = i
			recording.SubRuns = append(recording.SubRuns, types.SubRunRecording{RunID: runID})
		}
		child := &recording.SubRuns[i]
		if parentRunID != "" {
			child.ParentRunID = parentRunID
		}
		return child
	}
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
			recording.HarnessVersion = ev.HarnessVersion
			if ev.Config != nil {
				recording.Config = *ev.Config
			}
		case eventKindSystemPrompt:
			if ev.ParentRunID == "" {
				recording.SystemPrompt = ev.SystemPrompt
				recording.SystemPromptSHA256 = ev.SystemPromptSHA256
			}
		case eventKindTurnRecord:
			tr := types.TurnRecord{
				Turn:                ev.Turn,
				ModelOutput:         ev.ModelOutput,
				ToolCalls:           ev.ToolCalls,
				RunID:               ev.RunID,
				ParentRunID:         ev.ParentRunID,
				ResponseModel:       ev.ResponseModel,
				OutputLimitRecovery: ev.OutputLimitRecovery,
				StallNudge:          ev.StallNudge,
			}
			if ev.ModelInput != nil {
				tr.ModelInput = *ev.ModelInput
			}
			if ev.ParentRunID != "" && ev.RunID != "" && ev.RunID != recording.RunID {
				child := subRun(ev.RunID, ev.ParentRunID)
				child.Turns = append(child.Turns, tr)
				continue
			}
			recording.Turns = append(recording.Turns, tr)
		case eventKindSubRunStarted:
			if ev.RunID == "" {
				continue
			}
			child := subRun(ev.RunID, ev.ParentRunID)
			if ev.Config != nil {
				child.Config = *ev.Config
			}
		case eventKindSubRunFinished:
			if ev.RunID == "" || ev.SubRun == nil {
				continue
			}
			child := subRun(ev.RunID, ev.ParentRunID)
			summary := *ev.SubRun
			child.Summary = &summary
		case eventKindRunFinished:
			if ev.Trace != nil {
				recording.FinalOutcome = *ev.Trace
				if !seenStarted {
					recording.RunID = ev.Trace.ID
					recording.Config = ev.Trace.Config
				}
			}
		case eventKindCommandOutputRecord:
			if ev.CommandOutput != nil {
				recording.CommandOutputs = append(recording.CommandOutputs, *ev.CommandOutput)
			}
		default:
			// unknown kind: skip.
		}
	}
	recording.SkippedLines = r.skipped
	if r.skipped > 0 {
		r.logger.Warn("trace.Reader: recording is partial; oversized lines were skipped",
			"skippedLines", r.skipped,
			"cap", MaxLineBytes,
			"runId", recording.RunID,
		)
	}
	if err := r.scanner.Err(); err != nil {
		return recording, fmt.Errorf("reading trace file: %w", err)
	}
	synthesizeLegacyCommandOutputs(recording)
	return recording, nil
}

func synthesizeLegacyCommandOutputs(recording *types.RunRecording) {
	if recording == nil {
		return
	}
	seen := make(map[string]bool, len(recording.CommandOutputs))
	for _, record := range recording.CommandOutputs {
		seen[record.RunID+"\x00"+record.ToolUseID] = true
	}
	turns := slices.Clone(recording.Turns)
	for _, child := range recording.SubRuns {
		turns = append(turns, child.Turns...)
	}
	for _, turn := range turns {
		for _, call := range turn.ToolCalls {
			if call.Name != "run_command" && call.InternalName != "run_command" {
				continue
			}
			runID := turn.RunID
			if runID == "" {
				runID = recording.RunID
			}
			key := runID + "\x00" + call.ID
			if seen[key] {
				continue
			}
			sum := sha256.Sum256([]byte(call.Output))
			hash := hex.EncodeToString(sum[:])
			stream := types.CommandOutputStreamRecord{RawBytes: int64(len(call.Output)), RawSHA256: hash, ScrubbedBytes: int64(len(call.Output)), ScrubbedSHA256: hash}
			recording.CommandOutputs = append(recording.CommandOutputs, types.CommandOutputRecord{
				RunID: runID, ParentRunID: turn.ParentRunID, Turn: turn.Turn, ToolUseID: call.ID,
				CaptureComplete: true, Stdout: stream, InitialResultSHA256: hash, LegacySingleRepresentation: true,
			})
			seen[key] = true
		}
	}
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
