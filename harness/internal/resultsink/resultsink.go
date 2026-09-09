// Package resultsink emits the run's small RunResult payload at
// end-of-run. The sink is selected by RunConfig.ResultSink.Type; the
// closed set is documented in types.ResultSinkConfig and validated by
// types.ValidateRunConfig. Only "none" and "stdout-json" are
// implemented today — "gcp-pubsub" and "gcs" are reserved values that
// validation rejects before reaching this factory.
//
// The result sink is distinct from the trace emitter (which carries
// the run's *evidence* — full JSONL trace and spans). The sink carries
// the run's *answer*: a single small RunResult JSON payload that
// callers parse to drive downstream automation (e.g. a Cloud Run job
// consumed via Cloud Logging extraction). Keeping the two surfaces
// independent means a run can ship traces to Grafana Cloud while still
// emitting a parseable answer to stdout for a calling script.
package resultsink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/rxbynerd/stirrup/types"
)

// StdoutResultSentinel prefixes the stdout-json result line. The
// sentinel is grep-friendly so a Cloud Logging filter or a shell
// pipeline can extract the JSON payload without parsing every line of
// stdout. The exact bytes ("STIRRUP_RESULT " — one trailing space, no
// colon) are part of the wire contract: the smoke workflow greps for
// it verbatim. Changing the literal would silently break operator
// pipelines that already depend on it.
//
// Last-line-wins semantics: the harness always emits the sentinel as
// the last line written to stdout before process exit, via the
// emitRunResult call at the end of every run path in
// cmd/stirrup/cmd/root.go. Consumers relying on log-based extraction
// (Cloud Logging --limit=1 sorted descending; `grep STIRRUP_RESULT |
// tail -n1` from a shell pipeline) should treat the last matching
// line as authoritative. A model that happens to echo the sentinel
// prefix earlier in the run produces an earlier, superseded log
// entry; the structural ordering guarantee on the harness side is
// what makes the extraction unambiguous. Any future code path that
// writes the sentinel from a non-final position would silently break
// this contract and the Cloud Run smoke workflow's correctness.
const StdoutResultSentinel = "STIRRUP_RESULT "

// ResultSink emits a RunResult at end-of-run. Implementations are
// constructed by NewResultSink and are safe for a single Emit call per
// run; concurrent Emits are not required by the contract.
type ResultSink interface {
	// Emit serialises the RunResult and writes it to the configured
	// destination. Returns an error on serialisation or transport
	// failure; the caller decides whether the failure is fatal (today
	// it logs and continues — the run's outcome is already reflected
	// in the trace and the process exit code).
	Emit(ctx context.Context, result types.RunResult) error
}

// NoneSink is the default sink — discards the RunResult. Used when
// RunConfig.ResultSink is nil or Type is "none". Preserves the
// pre-#164 behaviour: nothing on stdout at end-of-run beyond the
// existing stderr summary.
type NoneSink struct{}

// Emit is a no-op. Returns nil unconditionally.
func (NoneSink) Emit(_ context.Context, _ types.RunResult) error { return nil }

// StdoutJSONSink writes the RunResult as a single line prefixed with
// StdoutResultSentinel. The line goes to os.Stdout by default; tests
// inject an io.Writer instead. The write is serialised under a mutex
// so a hostile RunResult marshalling path cannot interleave bytes
// with another writer on the same fd.
type StdoutJSONSink struct {
	mu     sync.Mutex
	writer io.Writer // nil means os.Stdout
}

// NewStdoutJSONSink returns a sink that writes to os.Stdout. Tests use
// NewStdoutJSONSinkTo to inject a buffer.
func NewStdoutJSONSink() *StdoutJSONSink {
	return &StdoutJSONSink{writer: os.Stdout}
}

// NewStdoutJSONSinkTo returns a sink that writes to w. Exported so
// embedders (and tests in this package) can capture the output
// without redirecting fd 1.
func NewStdoutJSONSinkTo(w io.Writer) *StdoutJSONSink {
	return &StdoutJSONSink{writer: w}
}

// maxEncodedResultBytes bounds the emitted line — sentinel, JSON
// payload, and trailing newline together. Set below, not at, Cloud
// Logging's ~256 KiB per-entry ceiling: that ceiling applies to the
// whole LogEntry (timestamp, insertId, resource labels, trace, and
// other fields the logging agent wraps the line in, not just this
// payload), and Google documents it as approximate. Unlike
// ResultSinkConfig.MaxFinalAssistantTextBytes this is not
// operator-configurable: it is Emit's own backstop against JSON
// escaping inflating an already-capped RunResult past that ceiling.
const maxEncodedResultBytes = 250 * 1024

// Emit renders result as "STIRRUP_RESULT <json>\n" and writes it to
// the configured writer. Encoding errors are wrapped so the caller
// can distinguish them from transport failures; in practice the only
// structural failure is a non-serialisable payload, which RunResult's
// typed shape rules out.
//
// HTML escaping is disabled (json.Encoder.SetEscapeHTML(false)),
// since "<", ">", and "&" carry no special meaning on a Cloud
// Logging line and leaving them escaped is the largest common source
// of size inflation. That does not fully close the gap: Go's
// encoder unconditionally escapes U+2028 and U+2029 regardless of
// SetEscapeHTML, turning a 3-byte rune into a 6-byte \u2028 or
// \u2029 escape. Rather than assume that residual expansion away,
// Emit bounds the *encoded* line to maxEncodedResultBytes, truncating
// result.VerifierVerdict.Feedback and result.FinalAssistantText — the
// two variable-size fields — as far as needed and re-encoding until
// the line fits, the same way types.CapFinalAssistantText caps
// FinalAssistantText's raw string upstream in buildRunResult.
func (s *StdoutJSONSink) Emit(ctx context.Context, result types.RunResult) error {
	line, err := encodeResultLine(ctx, result)
	if err != nil {
		return fmt.Errorf("marshal RunResult: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	w := s.writer
	if w == nil {
		w = os.Stdout
	}

	// Single Write call so the sentinel and the JSON payload share an
	// atomic write on a line-buffered fd. A two-call sequence
	// (Write(prefix); Write(data)) would let a concurrent writer on
	// the same fd interleave bytes between them.
	if _, err := w.Write(line); err != nil {
		return fmt.Errorf("write RunResult to stdout: %w", err)
	}
	return nil
}

// encodeResultLine renders result as "STIRRUP_RESULT <json>\n",
// shrinking result's variable-size fields as needed so the returned
// line never exceeds maxEncodedResultBytes.
//
// VerifierVerdict.Feedback is shrunk before FinalAssistantText: the
// judge's verdict is secondary to the run's own answer, so it is the
// first field sacrificed. If fully truncating Feedback still does not
// fit, FinalAssistantText is shrunk next, with Feedback held at its
// floor.
//
// If even the smallest candidate (both fields at their truncation
// marker) does not bring the line under the cap, the rest of the
// RunResult envelope exceeds it on its own; encodeResultLine logs a
// warning and returns the over-cap line, since it has no further
// field left to shrink.
func encodeResultLine(ctx context.Context, result types.RunResult) ([]byte, error) {
	line, err := marshalResultLine(result)
	if err != nil {
		return nil, err
	}
	if len(line) <= maxEncodedResultBytes {
		return line, nil
	}

	if result.VerifierVerdict != nil && result.VerifierVerdict.Feedback != "" {
		original := result.VerifierVerdict.Feedback
		encoded, ok, err := searchTruncation(result, original, func(candidate *types.RunResult, budget int) {
			verdict := *result.VerifierVerdict
			verdict.Feedback, _ = types.CapFinalAssistantText(original, budget)
			candidate.VerifierVerdict = &verdict
		})
		if err != nil {
			return nil, err
		}
		if ok {
			return encoded, nil
		}
		// Even a fully-truncated Feedback does not fit; carry that
		// floor forward and let FinalAssistantText absorb the rest.
		verdict := *result.VerifierVerdict
		verdict.Feedback, _ = types.CapFinalAssistantText(original, 0)
		result.VerifierVerdict = &verdict
		if line, err = marshalResultLine(result); err != nil {
			return nil, err
		}
	}

	encoded, ok, err := searchTruncation(result, result.FinalAssistantText, func(candidate *types.RunResult, budget int) {
		candidate.FinalAssistantText, candidate.FinalAssistantTextTruncated = types.CapFinalAssistantText(result.FinalAssistantText, budget)
	})
	if err != nil {
		return nil, err
	}
	if ok {
		return encoded, nil
	}

	slog.WarnContext(ctx, "resultSink: RunResult envelope exceeds the encoded line cap after truncating all variable-size fields; emitting over-cap line",
		"bytes", len(line), "cap", maxEncodedResultBytes)
	return line, nil
}

// searchTruncation binary-searches the largest byte budget in
// [0, len(original)-1] for which apply(candidate, budget) — mutating
// a copy of result and re-encoding — brings the line to at most
// maxEncodedResultBytes. Binary search rather than a budget guessed
// from the escaping's worst-case expansion factor, since
// types.CapFinalAssistantText's truncated length is monotonic in the
// budget it is given on that range, and re-marshalling at each
// candidate is the only way to know the actual encoded cost given the
// field's specific escape density. hi is clamped to
// maxEncodedResultBytes: a raw prefix already longer than the cap
// encodes to at least that many bytes, so the search never needs to
// probe further out regardless of how large original is.
//
// Returns ok=false, with line unset, if no budget in that range fits
// — including the trivial len(original) == 0 case, which the caller
// handles by moving on to the next field or reporting the envelope
// alone as over-cap.
func searchTruncation(result types.RunResult, original string, apply func(candidate *types.RunResult, budget int)) (line []byte, ok bool, err error) {
	hi := len(original) - 1
	if hi > maxEncodedResultBytes {
		hi = maxEncodedResultBytes
	}
	lo := 0
	for lo <= hi {
		mid := lo + (hi-lo)/2
		candidate := result
		apply(&candidate, mid)
		encoded, err := marshalResultLine(candidate)
		if err != nil {
			return nil, false, err
		}
		if len(encoded) <= maxEncodedResultBytes {
			line, ok = encoded, true
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return line, ok, nil
}

// marshalResultLine renders result as a single "STIRRUP_RESULT
// <json>\n" line with HTML escaping disabled.
func marshalResultLine(result types.RunResult) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(StdoutResultSentinel)
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(result); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// NewResultSink returns the ResultSink selected by cfg. Defence-in-depth
// for reserved values: validation already rejects gcp-pubsub and gcs at
// config-load, but a programmatic caller that bypasses
// types.ValidateRunConfig (e.g. a test or a future embedding API path)
// would otherwise reach a nil-component crash here.
func NewResultSink(cfg *types.ResultSinkConfig) (ResultSink, error) {
	if cfg == nil {
		return NoneSink{}, nil
	}
	switch cfg.Type {
	case "", "none":
		return NoneSink{}, nil
	case "stdout-json":
		return NewStdoutJSONSink(), nil
	case "gcp-pubsub", "gcs":
		return nil, fmt.Errorf("resultSink.type=%q is reserved but not yet implemented", cfg.Type)
	default:
		return nil, fmt.Errorf("unsupported resultSink.type: %q", cfg.Type)
	}
}
