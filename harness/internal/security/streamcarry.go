package security

import (
	"slices"
	"unicode/utf8"
)

const (
	// streamCarryMax bounds the bytes a StreamCarry holds unreleased while
	// no possible match outgrows it. Once one does, the held text from the
	// start of that match goes out as one placeholder, and the rest of the
	// match is held until it ends, up to streamCarryRunMax.
	streamCarryMax = 8 << 10

	// streamCarryRunMax bounds the bytes held for a match that outgrew
	// streamCarryMax. Past it, nothing more is released until Flush.
	streamCarryRunMax = 1 << 20

	// streamCarryRescanBytes is the held size above which the carry
	// rescans only once the held text has grown by a quarter, so a long
	// held span costs amortised linear time rather than a full scan per
	// fragment.
	streamCarryRescanBytes = 64
)

// StreamCarry scrubs a stream of text fragments as it arrives, so a secret
// split between two fragments is redacted as if the stream had been
// scrubbed whole.
//
// It holds back only the text from the earliest point at which a pattern
// could still begin or continue a match, together with any complete match
// crossing that point, so ordinary prose is released as it arrives. A
// match released just before the held text stays in the buffer, unreleased
// again, so a later pattern still sees its placeholder. Flush releases the
// rest and must be called wherever the stream ends. A StreamCarry is not
// safe for concurrent use.
type StreamCarry struct {
	// pending is the context, its first ctx bytes, then the held text;
	// afterWord records whether the stream byte before it is a word byte.
	pending   []byte
	ctx       int
	afterWord bool
	unscanned int
	// run is set while the context is the head of a match that outgrew
	// streamCarryMax, already released as a placeholder; drain once that
	// match outgrew streamCarryRunMax.
	run, drain bool
	// scanned is the bytes scanned and forced the number of releases the
	// cap forced; both are read by tests.
	scanned int
	forced  int
}

// Push adds fragment and returns the scrubbed text that can be released
// and the names of the patterns redacted in it.
func (c *StreamCarry) Push(fragment string) (string, []string) {
	if c.drain {
		return "", nil
	}
	c.pending = append(c.pending, fragment...)
	c.unscanned += len(fragment)
	switch held := c.held(); {
	case c.run && len(c.pending) > streamCarryRunMax:
		c.pending, c.ctx, c.drain = nil, 0, true
		return "", nil
	case c.run:
		if c.unscanned < len(c.pending)/4 {
			return "", nil
		}
	case held > streamCarryRescanBytes && c.unscanned < held/4 && held <= streamCarryMax:
		return "", nil
	}
	c.unscanned = 0
	return c.release(false)
}

// Flush returns the held text, scrubbed, and the names of the patterns
// redacted in it, and empties the carry.
func (c *StreamCarry) Flush() (string, []string) {
	var out string
	var patterns []string
	if !c.drain {
		out, patterns = c.release(true)
	}
	c.pending, c.ctx, c.unscanned, c.afterWord, c.run, c.drain = c.pending[:0], 0, 0, false, false, false
	return out, patterns
}

func (c *StreamCarry) held() int { return len(c.pending) - c.ctx }

// release scans the buffer, releases what it can, or everything when all
// is set, and keeps the context the next scan needs.
func (c *StreamCarry) release(all bool) (string, []string) {
	if c.held() == 0 {
		return "", nil
	}
	p := c.pending
	c.scanned += len(p)
	segs, ranges, spans, viable := carryScan(string(p), c.afterWord)
	cut := len(p)
	if !all {
		cut = runeStartAtOrBefore(p, retreatCutPastSpans(spans, max(viable, c.ctx)))
		if !c.run && len(p)-cut > streamCarryMax {
			return c.forceRelease(p, segs, viable, cut)
		}
	}
	if cut <= c.ctx {
		return "", nil
	}
	out, patterns := releasedText(segs, c.ctx, cut)
	// The next scan starts at a complete match that ends at the cut, so a
	// later pattern sees its placeholder as the whole stream would.
	next := cut
	for _, r := range ranges {
		if r[1] == cut {
			next = min(next, r[0])
		}
	}
	c.keep(p, next, cut)
	c.run = false
	return out, patterns
}

// forceRelease releases the held text before cut, scrubbed, and the text
// from cut on, which a possible match longer than streamCarryMax holds, as
// one placeholder. That text stays as context and the rest of the match is
// held until a scan shows where it ends, so none of the match is released.
func (c *StreamCarry) forceRelease(p []byte, segs []scrubSegment, viable, cut int) (string, []string) {
	c.forced++
	from := max(cut, c.ctx)
	out, patterns := releasedText(segs, c.ctx, from)
	for _, name := range heldPatterns(string(p), segs, from, viable) {
		if !slices.Contains(patterns, name) {
			patterns = append(patterns, name)
		}
	}
	c.keep(p, cut, runeStartAtOrBefore(p, len(p)))
	c.run = true
	return out + redactedPlaceholder, patterns
}

// keep drops the buffer before next and makes [next, cut) the context.
func (c *StreamCarry) keep(p []byte, next, cut int) {
	if next > 0 {
		c.afterWord = isWordByte(p[next-1])
	}
	c.pending = append(c.pending[:0], p[next:]...)
	c.ctx = cut - next
}

// heldPatterns names the patterns behind a forced placeholder: those
// redacted from offset from on, and those whose match could begin at
// viable.
func heldPatterns(p string, segs []scrubSegment, from, viable int) []string {
	var names []string
	for _, seg := range segs {
		if seg.redacted && seg.start >= from && !slices.Contains(names, seg.pattern) {
			names = append(names, seg.pattern)
		}
	}
	for i, re := range streamPrefixes {
		if loc := re.FindStringIndex(p[viable:]); loc != nil && loc[0] == 0 && !slices.Contains(names, secretPatterns[i].name) {
			names = append(names, secretPatterns[i].name)
		}
	}
	return names
}

// carryScan scrubs p and returns its segments, the ranges the scrubber
// replaces, the spans a cut must not fall inside, and the earliest offset
// from which a pattern could still match once more text arrives. A pattern
// scanning text an earlier replacement rewrote is tested on that text too,
// since its match can run across a placeholder.
//
// While some text is held, the spans include each pattern's matches in p
// as written, not only in the rewritten text: a match an earlier pattern's
// placeholder hides now can surface once later text changes that earlier
// match, so text it covers is held with the rest.
//
// When the stream byte before p is a word byte, p is scanned behind a "_"
// so a leading \b sees it as the whole stream does; no pattern or prefix
// can begin with "_", so the sentinel itself never matches.
func carryScan(p string, afterWord bool) ([]scrubSegment, [][]int, [][]int, int) {
	shift := 0
	if afterWord {
		p, shift = "_"+p, 1
	}
	viable := len(p)
	if loc := streamPrefixAny.FindStringIndex(p); loc != nil {
		viable = loc[0]
	}
	segs, _ := scrubSegments(p, func(i int, current string, segs []scrubSegment) {
		if len(segs) == 1 {
			return
		}
		if loc := streamPrefixes[i].FindStringIndex(current); loc != nil {
			viable = min(viable, originalOffset(segs, loc[0]))
		}
	})
	var ranges, spans [][]int
	for _, r := range redactedRanges(segs) {
		ranges = append(ranges, []int{r.Start - shift, r.End - shift})
	}
	spans = append(spans, ranges...)
	if viable < len(p) {
		for _, sp := range secretPatterns {
			for _, m := range patternMatches(sp, p) {
				spans = append(spans, []int{m[0] - shift, m[1] - shift})
			}
		}
	}
	if shift > 0 {
		segs = unshiftSegments(segs)
	}
	return segs, ranges, spans, viable - shift
}

// unshiftSegments drops the one-byte sentinel carryScan prepends, which
// always lies in the first, kept segment.
func unshiftSegments(segs []scrubSegment) []scrubSegment {
	out := make([]scrubSegment, 0, len(segs))
	for i, seg := range segs {
		if i == 0 {
			seg.text = seg.text[1:]
			seg.end--
			if seg.end == 0 && !seg.redacted {
				continue
			}
		} else {
			seg.start--
			seg.end--
		}
		out = append(out, seg)
	}
	return out
}

func isWordByte(b byte) bool {
	return b == '_' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9'
}

// releasedText returns the scrubbed text standing for original bytes
// [from, to) of the text segs spell and the patterns redacted in it. A
// placeholder belongs to the side its replaced bytes start on: one that
// starts in the context stands for bytes a placeholder already went out
// for, so nothing is released for them.
func releasedText(segs []scrubSegment, from, to int) (string, []string) {
	var out []byte
	var patterns []string
	for _, seg := range segs {
		if seg.redacted {
			if seg.start >= from && seg.start < to {
				out = append(out, seg.text...)
				if !slices.Contains(patterns, seg.pattern) {
					patterns = append(patterns, seg.pattern)
				}
			}
			continue
		}
		lo, hi := max(seg.start, from), min(seg.end, to)
		if lo < hi {
			out = append(out, seg.text[lo-seg.start:hi-seg.start]...)
		}
	}
	return string(out), patterns
}

// runeStartAtOrBefore returns the start of the rune holding byte i, or,
// for i at the end of p, the start of a trailing rune a fragment left
// incomplete.
func runeStartAtOrBefore(p []byte, i int) int {
	if i >= len(p) {
		s := len(p) - 1
		for s > 0 && len(p)-s < utf8.UTFMax && !utf8.RuneStart(p[s]) {
			s--
		}
		if s >= 0 && !utf8.FullRune(p[s:]) {
			return s
		}
		return len(p)
	}
	for i > 0 && !utf8.RuneStart(p[i]) {
		i--
	}
	return max(i, 0)
}
