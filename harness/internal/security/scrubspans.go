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
// may be empty when a later match split the placeholder, and names the
// pattern whose match it replaced.
type scrubSegment struct {
	text       string
	start, end int
	redacted   bool
	pattern    string
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
	segs, stats := scrubSegments(value, nil)
	return joinSegments(segs), redactedRanges(segs), stats
}

// redactedRanges returns the original ranges the redacted segments stand
// for, with touching ranges merged.
func redactedRanges(segs []scrubSegment) []ScrubRange {
	var ranges []ScrubRange
	for _, seg := range segs {
		if !seg.redacted || seg.start == seg.end {
			continue
		}
		if n := len(ranges); n > 0 && ranges[n-1].End == seg.start {
			ranges[n-1].End = seg.end
			continue
		}
		ranges = append(ranges, ScrubRange{Start: seg.start, End: seg.end})
	}
	return ranges
}

// originalOffset maps offset x of the text segs spell to the original
// offset it stands for; an offset inside a placeholder maps to the start
// of the bytes the placeholder replaced.
func originalOffset(segs []scrubSegment, x int) int {
	pos := 0
	for _, seg := range segs {
		if x < pos+len(seg.text) {
			if seg.redacted {
				return seg.start
			}
			return seg.start + x - pos
		}
		pos += len(seg.text)
	}
	if n := len(segs); n > 0 {
		return segs[n-1].end
	}
	return 0
}

// scrubSegments runs the patterns in turn over value, as ScrubWithStats
// does, and returns the segments of the result. A non-nil visit is called
// with each pattern's index and the text that pattern is about to scan.
func scrubSegments(value string, visit func(pattern int, current string, segs []scrubSegment)) ([]scrubSegment, ScrubStats) {
	segs := []scrubSegment{{text: value, start: 0, end: len(value)}}
	stats := ScrubStats{}
	for i, p := range secretPatterns {
		current := joinSegments(segs)
		if visit != nil {
			visit(i, current, segs)
		}
		matches := patternMatches(p, current)
		if len(matches) == 0 {
			continue
		}
		stats.Count += len(matches)
		stats.Patterns = append(stats.Patterns, p.name)
		segs = replaceSegments(segs, matches, p.name)
	}
	return segs, stats
}

// patternMatches returns the matches of p in value that its validator, if
// any, accepts.
func patternMatches(p namedPattern, value string) [][]int {
	if valid := matchValidators[p.name]; valid != nil {
		return findValidated(p.re, value, valid)
	}
	return p.re.FindAllStringIndex(value, -1)
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
func replaceSegments(segs []scrubSegment, matches [][]int, pattern string) []scrubSegment {
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
		placeholder := scrubSegment{text: redactedPlaceholder, redacted: true, pattern: pattern}
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
	return scrubSegment{text: seg.text[:k], start: seg.start, end: seg.start, redacted: true, pattern: seg.pattern},
		scrubSegment{text: seg.text[k:], start: seg.start, end: seg.end, redacted: true, pattern: seg.pattern}
}
