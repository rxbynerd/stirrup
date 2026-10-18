package security

import (
	"slices"
	"unicode/utf8"
)

const (
	// streamCarryMax bounds the bytes a StreamCarry holds unreleased.
	// Text one possible match spans beyond it, whitespace inside a match
	// included, is released anyway: a match longer than this is redacted
	// through its first streamCarryMax bytes and its tail reaches the sink.
	streamCarryMax = 8 << 10

	// streamCarryRescanBytes is the held size above which the carry
	// rescans only once the held text has grown by a quarter, so a long
	// held span costs amortised linear time rather than a full scan per
	// fragment.
	streamCarryRescanBytes = 256
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
	// scanned counts the bytes scanned and forced the releases made at
	// the cap, so a test can pin that rescanning stays amortised and tell
	// a stream the cap cut.
	scanned int
	forced  int
}

// Push adds fragment and returns the scrubbed text that can be released
// and the names of the patterns redacted in it.
func (c *StreamCarry) Push(fragment string) (string, []string) {
	c.pending = append(c.pending, fragment...)
	c.unscanned += len(fragment)
	if held := c.held(); held <= streamCarryRescanBytes || c.unscanned >= held/4 || held > streamCarryMax {
		c.unscanned = 0
		return c.release(false)
	}
	return "", nil
}

// Flush returns the held text, scrubbed, and the names of the patterns
// redacted in it, and empties the carry.
func (c *StreamCarry) Flush() (string, []string) {
	out, patterns := c.release(true)
	c.pending, c.ctx, c.unscanned, c.afterWord = c.pending[:0], 0, 0, false
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
	segs, ranges, viable := carryScan(string(p), c.afterWord)
	cut, forced := len(p), false
	if !all {
		cut, forced = c.cut(p, ranges, viable)
	}
	if cut <= c.ctx {
		return "", nil
	}
	out, patterns := releasedText(p, segs, c.ctx, cut)
	// The next scan starts at a complete match that ends at the cut, so a
	// later pattern sees its placeholder as the whole stream would. After
	// a forced cut the match may still grow, so it is not rescanned.
	next := cut
	for _, r := range ranges {
		if r[1] == cut && !forced {
			next = min(next, r[0])
		}
	}
	if next > 0 {
		c.afterWord = isWordByte(p[next-1])
	}
	c.pending = append(c.pending[:0], p[next:]...)
	c.ctx = cut - next
	return out, patterns
}

// cut returns the offset in p up to which held text can be released and
// whether the cap forced it.
func (c *StreamCarry) cut(p []byte, ranges [][]int, viable int) (int, bool) {
	cut := runeStartAtOrBefore(p, retreatCutPastSpans(ranges, max(viable, c.ctx)))
	if len(p)-cut <= streamCarryMax {
		return cut, false
	}
	// One possible match outgrew the cap. Release at least the excess,
	// holding back what could begin a match within the last
	// streamCarryMax bytes, as if the stream started there.
	c.forced++
	lo := runeStartAtOrAfter(p, len(p)-streamCarryMax)
	c.scanned += len(p) - lo
	_, _, rest := carryScan(string(p[lo:]), isWordByte(p[lo-1]))
	forced := lo + rest
	if cut = retreatCutPastSpans(ranges, forced); cut < lo {
		cut = forced
	}
	return runeStartAtOrAfter(p, cut), true
}

// carryScan scrubs p and returns its segments, the ranges the scrubber
// replaces, and the earliest offset from which a pattern could still match
// once more text arrives. A pattern scanning text an earlier replacement
// rewrote is tested on that text too, since its match can run across a
// placeholder.
//
// When the stream byte before p is a word byte, p is scanned behind a "_"
// so a leading \b sees it as the whole stream does; no pattern or prefix
// can begin with "_", so the sentinel itself never matches.
func carryScan(p string, afterWord bool) ([]scrubSegment, [][]int, int) {
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
	if shift > 0 {
		segs = unshiftSegments(segs)
		viable--
	}
	var ranges [][]int
	for _, r := range redactedRanges(segs) {
		ranges = append(ranges, []int{r.Start, r.End})
	}
	return segs, ranges, viable
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
// [from, to) of p and the patterns redacted in it. A placeholder belongs to
// the side its replaced bytes start on, so one a forced cut crosses is
// released whole with the head of its match; the bytes of one that starts
// in the context are released as they are, since its head already was.
func releasedText(p []byte, segs []scrubSegment, from, to int) (string, []string) {
	var out []byte
	var patterns []string
	for _, seg := range segs {
		if seg.redacted {
			if seg.start < from && seg.end > from {
				out = append(out, p[from:min(seg.end, to)]...)
				continue
			}
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

func runeStartAtOrAfter(p []byte, i int) int {
	for i > 0 && i < len(p) && !utf8.RuneStart(p[i]) {
		i++
	}
	if i >= len(p) {
		return runeStartAtOrBefore(p, len(p))
	}
	return i
}
