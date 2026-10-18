package security

import "strings"

// ScrubRange is a range [Start, End) of UTF-8 byte offsets into the input
// of ScrubWithSpans.
type ScrubRange struct {
	Start, End int
}

// scrubSegment is one piece of the progressively rewritten string Scrub
// builds. A kept segment is the original bytes value[start:end]; a
// redacted one is placeholder text standing for value[start:end], which
// may be empty when a later match split the placeholder.
type scrubSegment struct {
	text       string
	start, end int
	redacted   bool
}

// ScrubWithSpans returns what ScrubWithStats returns and, in addition, the
// byte ranges of value the scrubber replaced: ordered, disjoint and never
// touching, since touching ranges are merged. Every byte outside the
// ranges appears unchanged and in order in the scrubbed string, so the
// ranges hold every byte Scrub redacts. TestScrubWithSpans_ConsistentWithScrub
// and the random-layout test hold it in parity with ScrubWithStats.
//
// Patterns run in turn over the rewritten string, as in ScrubWithStats, so
// a later match can extend across an earlier placeholder; its range is
// mapped back to the original bytes the placeholder stood for.
func ScrubWithSpans(value string) (string, []ScrubRange, ScrubStats) {
	segs, stats := scrubSegments(value)
	var b strings.Builder
	b.Grow(len(value))
	var spans []ScrubRange
	for _, seg := range segs {
		b.WriteString(seg.text)
		if !seg.redacted || seg.start == seg.end {
			continue
		}
		if n := len(spans); n > 0 && spans[n-1].End == seg.start {
			spans[n-1].End = seg.end
			continue
		}
		spans = append(spans, ScrubRange{Start: seg.start, End: seg.end})
	}
	return b.String(), spans, stats
}

func scrubSegments(value string) ([]scrubSegment, ScrubStats) {
	segs := []scrubSegment{{text: value, start: 0, end: len(value)}}
	stats := ScrubStats{}
	for _, p := range secretPatterns {
		current := joinSegments(segs)
		var matches [][]int
		if valid := matchValidators[p.name]; valid != nil {
			matches = findValidated(p.re, current, valid)
		} else {
			matches = p.re.FindAllStringIndex(current, -1)
		}
		if len(matches) == 0 {
			continue
		}
		stats.Count += len(matches)
		stats.Patterns = append(stats.Patterns, p.name)
		segs = replaceSegments(segs, matches)
	}
	return segs, stats
}

func joinSegments(segs []scrubSegment) string {
	if len(segs) == 1 {
		return segs[0].text
	}
	var b strings.Builder
	for _, seg := range segs {
		b.WriteString(seg.text)
	}
	return b.String()
}

// replaceSegments replaces each match, given in rewritten-string offsets,
// with one placeholder segment standing for every original byte the
// matched segments stood for. A segment a match boundary falls inside is
// split first.
func replaceSegments(segs []scrubSegment, matches [][]int) []scrubSegment {
	var out []scrubSegment
	pos := 0 // rewritten offset of the next unconsumed byte of segs[i]
	i := 0
	for _, m := range matches {
		for ; i < len(segs) && pos+len(segs[i].text) <= m[0]; i++ {
			out = append(out, segs[i])
			pos += len(segs[i].text)
		}
		if i < len(segs) && pos < m[0] {
			left, right := splitSegment(segs[i], m[0]-pos)
			out = append(out, left)
			segs[i] = right
			pos = m[0]
		}
		placeholder := scrubSegment{text: redactedPlaceholder, redacted: true}
		first := true
		for i < len(segs) && pos < m[1] {
			seg := segs[i]
			if pos+len(seg.text) > m[1] {
				var rest scrubSegment
				seg, rest = splitSegment(seg, m[1]-pos)
				segs[i] = rest
			} else {
				i++
			}
			if first {
				placeholder.start = seg.start
				first = false
			}
			placeholder.end = seg.end
			pos += len(seg.text)
		}
		out = append(out, placeholder)
	}
	return append(out, segs[i:]...)
}

// splitSegment cuts seg after k bytes of its text. A kept segment splits
// its original range with its text; a redacted one has no byte-for-byte
// mapping, so the left part keeps none of the range and the right part
// keeps all of it, which leaves the union unchanged.
func splitSegment(seg scrubSegment, k int) (scrubSegment, scrubSegment) {
	if !seg.redacted {
		return scrubSegment{text: seg.text[:k], start: seg.start, end: seg.start + k},
			scrubSegment{text: seg.text[k:], start: seg.start + k, end: seg.end}
	}
	return scrubSegment{text: seg.text[:k], start: seg.start, end: seg.start, redacted: true},
		scrubSegment{text: seg.text[k:], start: seg.start, end: seg.end, redacted: true}
}
