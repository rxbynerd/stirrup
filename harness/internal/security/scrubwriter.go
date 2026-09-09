package security

import (
	"bytes"
	"errors"
	"io"
)

const (
	// ScrubCarryWindow is the number of trailing bytes a ScrubWriter holds
	// back from its sink so a secret split across two writes is still matched
	// as one span. Every pattern in secretPatterns matches a single span well
	// under this size, including a fat JWT, which is what
	// TestSecretPatternsMatchSingleSpanUnderCarryWindow pins.
	ScrubCarryWindow = 16 << 10

	// scrubChunkBytes is the scrubbable payload buffered before a flush; the
	// writer holds at most scrubChunkBytes+ScrubCarryWindow bytes, so peak
	// allocation is bounded by the chunk rather than the stream.
	scrubChunkBytes = 64 << 10
)

var errScrubWriterClosed = errors.New("scrub writer is closed")

// ScrubWriter applies the Scrub pattern set to a byte stream on its way to
// dst. Chunks are cut on a line boundary when one falls inside the flushable
// region and at a byte cap otherwise, and a carry window of trailing bytes is
// retained so a secret spanning two chunks is redacted as one span.
//
// The residual: a span longer than the carry window cannot be held for
// reassembly, so it is cut mid-match and only its leading portion is
// redacted. Every pattern in secretPatterns matches far below the window, so
// reaching this requires a match the patterns do not produce — an
// unterminated Bearer token running for tens of kilobytes, say. Anything
// swallowed by such a span shares its fate, including a shorter secret
// nested inside it.
//
// A ScrubWriter is not safe for concurrent use.
type ScrubWriter struct {
	dst        io.Writer
	chunk      int
	window     int
	maxPending int
	pending    []byte
	count      int
	seen       map[string]struct{}
	closed     bool
	err        error
}

// NewScrubWriter returns a ScrubWriter with the shipped chunk and carry
// window sizes.
func NewScrubWriter(dst io.Writer) *ScrubWriter {
	return newScrubWriter(dst, scrubChunkBytes, ScrubCarryWindow)
}

func newScrubWriter(dst io.Writer, chunk, window int) *ScrubWriter {
	maxPending := chunk + window
	return &ScrubWriter{
		dst: dst, chunk: chunk, window: window, maxPending: maxPending,
		pending: make([]byte, 0, maxPending), seen: map[string]struct{}{},
	}
}

func (w *ScrubWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.closed {
		return 0, errScrubWriterClosed
	}
	accepted := 0
	for len(p) > 0 {
		n := w.maxPending - len(w.pending)
		if n > len(p) {
			n = len(p)
		}
		w.pending = append(w.pending, p[:n]...)
		p = p[n:]
		accepted += n
		for len(w.pending) >= w.maxPending {
			if err := w.flush(false); err != nil {
				w.err = err
				return accepted, err
			}
		}
	}
	return accepted, nil
}

// Close scrubs and emits the retained tail. It does not close dst.
func (w *ScrubWriter) Close() error {
	if w.closed {
		return w.err
	}
	w.closed = true
	if w.err != nil {
		return w.err
	}
	if err := w.flush(true); err != nil {
		w.err = err
	}
	return w.err
}

// Stats reports the redactions performed across every chunk so far. Pattern
// names come back in secretPatterns order, matching ScrubWithStats, so a
// trace record does not depend on which chunk a secret happened to land in.
func (w *ScrubWriter) Stats() ScrubStats {
	stats := ScrubStats{Count: w.count}
	for _, p := range secretPatterns {
		if _, ok := w.seen[p.name]; ok {
			stats.Patterns = append(stats.Patterns, p.name)
		}
	}
	return stats
}

func (w *ScrubWriter) flush(force bool) error {
	if force {
		return w.emit(len(w.pending))
	}
	if len(w.pending) <= w.window {
		return nil
	}
	limit := len(w.pending) - w.window
	cut := limit
	if i := bytes.LastIndexByte(w.pending[:limit], '\n'); i >= 0 {
		cut = i + 1
	}
	cut = w.boundaryCut(cut)
	if cut == 0 {
		if len(w.pending) < w.maxPending {
			return nil
		}
		// A span reaching offset 0 blocks the line-aware cut — several
		// patterns match across a newline. The byte cut taken instead needs
		// its own boundary scan; cutting blind here splits any secret
		// straddling it. Only a span the carry window cannot reassemble
		// leaves no scanned cut at all, which is the documented residual.
		if cut = w.boundaryCut(limit); cut == 0 {
			cut = limit
		}
	}
	return w.emit(cut)
}

// boundaryCut moves cut back to the start of any secret span crossing it. A
// span scrubbed in two halves would leave the tail of a secret in the sink,
// so the whole span is retained until the next flush sees it complete.
// Scanning starts one carry window before the cut: an earlier start implies a
// span longer than the window, which is the residual this writer documents
// rather than defends against.
func (w *ScrubWriter) boundaryCut(cut int) int {
	lo := cut - w.window
	if lo < 0 {
		lo = 0
	}
	region := w.pending[lo:]
	rel := cut - lo
	var spans [][]int
	for _, p := range secretPatterns {
		spans = append(spans, p.re.FindAllIndex(region, -1)...)
	}
	for moved := true; moved; {
		moved = false
		for _, span := range spans {
			if span[0] < rel && span[1] > rel {
				rel = span[0]
				moved = true
			}
		}
	}
	return lo + rel
}

func (w *ScrubWriter) emit(cut int) error {
	if cut == 0 {
		return nil
	}
	scrubbed, stats := ScrubWithStats(string(w.pending[:cut]))
	w.count += stats.Count
	for _, name := range stats.Patterns {
		w.seen[name] = struct{}{}
	}
	if _, err := io.WriteString(w.dst, scrubbed); err != nil {
		return err
	}
	w.pending = append(w.pending[:0], w.pending[cut:]...)
	return nil
}
