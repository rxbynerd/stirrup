package security

import (
	"regexp"
	"regexp/syntax"
	"unicode"
)

// streamPrefixes holds, for each entry of secretPatterns, a regexp that
// matches a suffix of the text exactly when that suffix could still begin
// or continue a match once more text arrives; streamPrefixAny matches when
// any of them does, which costs one scan rather than one per pattern.
var streamPrefixes, streamPrefixAny = buildStreamPrefixes()

// patternAlphabets holds, for each entry of secretPatterns, ranges holding
// every rune a match of it can contain.
var patternAlphabets = buildPatternAlphabets()

func buildPatternAlphabets() [][][2]rune {
	out := make([][][2]rune, len(secretPatterns))
	for i, p := range secretPatterns {
		re, err := syntax.Parse(p.re.String(), syntax.Perl)
		if err != nil {
			panic("security: reparse " + p.name + ": " + err.Error())
		}
		out[i] = alphabetOf(re, nil)
	}
	return out
}

func alphabetOf(re *syntax.Regexp, out [][2]rune) [][2]rune {
	switch re.Op {
	case syntax.OpLiteral:
		for _, r := range re.Rune {
			out = append(out, [2]rune{r, r})
			if re.Flags&syntax.FoldCase != 0 {
				for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
					out = append(out, [2]rune{f, f})
				}
			}
		}
	case syntax.OpCharClass:
		for j := 0; j+1 < len(re.Rune); j += 2 {
			out = append(out, [2]rune{re.Rune[j], re.Rune[j+1]})
		}
	case syntax.OpAnyCharNotNL:
		out = append(out, [2]rune{0, '\n' - 1}, [2]rune{'\n' + 1, unicode.MaxRune})
	case syntax.OpAnyChar:
		out = append(out, [2]rune{0, unicode.MaxRune})
	}
	for _, sub := range re.Sub {
		out = alphabetOf(sub, out)
	}
	return out
}

// anyAlphabetHas reports whether r can occur in a match of a pattern
// marked in patterns.
func anyAlphabetHas(patterns []bool, r rune) bool {
	for i, on := range patterns {
		if !on {
			continue
		}
		for _, rg := range patternAlphabets[i] {
			if rg[0] <= r && r <= rg[1] {
				return true
			}
		}
	}
	return false
}

func buildStreamPrefixes() ([]*regexp.Regexp, *regexp.Regexp) {
	out := make([]*regexp.Regexp, len(secretPatterns))
	anyOf := &syntax.Regexp{Op: syntax.OpAlternate}
	for i, p := range secretPatterns {
		re, err := syntax.Parse(p.re.String(), syntax.Perl)
		if err != nil {
			panic("security: reparse " + p.name + ": " + err.Error())
		}
		prefixes := prefixesOf(re)
		anyOf.Sub = append(anyOf.Sub, prefixes)
		out[i] = regexp.MustCompile(atEnd(prefixes).String())
	}
	return out, regexp.MustCompile(atEnd(anyOf).String())
}

func atEnd(re *syntax.Regexp) *syntax.Regexp {
	return concatOf(re, &syntax.Regexp{Op: syntax.OpEndText})
}

// prefixesOf returns a regexp matching every prefix of every string re
// matches, the empty string and the whole matches included. Assertions are
// taken as satisfied and a counted repetition's lower bound as zero, so the
// result matches a superset of those prefixes: the carry may hold a little
// more text than it must, never less.
func prefixesOf(re *syntax.Regexp) *syntax.Regexp {
	switch re.Op {
	case syntax.OpNoMatch:
		return re
	case syntax.OpLiteral:
		var acc *syntax.Regexp
		for i := len(re.Rune) - 1; i >= 0; i-- {
			lit := &syntax.Regexp{Op: syntax.OpLiteral, Rune: []rune{re.Rune[i]}, Flags: re.Flags}
			if acc != nil {
				lit = concatOf(lit, acc)
			}
			acc = questOf(lit)
		}
		if acc == nil {
			return emptyMatch()
		}
		return acc
	case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return questOf(re)
	case syntax.OpCapture:
		return prefixesOf(re.Sub[0])
	case syntax.OpRepeat:
		if re.Max == 0 {
			return emptyMatch()
		}
		if re.Max > 0 {
			return concatOf(&syntax.Regexp{Op: syntax.OpRepeat, Min: 0, Max: re.Max - 1, Sub: []*syntax.Regexp{re.Sub[0]}, Flags: re.Flags}, prefixesOf(re.Sub[0]))
		}
		return concatOf(&syntax.Regexp{Op: syntax.OpStar, Sub: []*syntax.Regexp{re.Sub[0]}, Flags: re.Flags}, prefixesOf(re.Sub[0]))
	case syntax.OpStar, syntax.OpPlus:
		return concatOf(&syntax.Regexp{Op: syntax.OpStar, Sub: []*syntax.Regexp{re.Sub[0]}, Flags: re.Flags}, prefixesOf(re.Sub[0]))
	case syntax.OpQuest:
		return prefixesOf(re.Sub[0])
	case syntax.OpConcat:
		if len(re.Sub) == 0 {
			return emptyMatch()
		}
		acc := prefixesOf(re.Sub[len(re.Sub)-1])
		for i := len(re.Sub) - 2; i >= 0; i-- {
			acc = &syntax.Regexp{Op: syntax.OpAlternate, Sub: []*syntax.Regexp{prefixesOf(re.Sub[i]), concatOf(re.Sub[i], acc)}}
		}
		return acc
	case syntax.OpAlternate:
		alt := &syntax.Regexp{Op: syntax.OpAlternate}
		for _, sub := range re.Sub {
			alt.Sub = append(alt.Sub, prefixesOf(sub))
		}
		return alt
	default:
		// OpEmptyMatch and the assertions: the empty string is their only
		// prefix.
		return emptyMatch()
	}
}

func emptyMatch() *syntax.Regexp { return &syntax.Regexp{Op: syntax.OpEmptyMatch} }

func questOf(re *syntax.Regexp) *syntax.Regexp {
	return &syntax.Regexp{Op: syntax.OpQuest, Sub: []*syntax.Regexp{re}}
}

func concatOf(a, b *syntax.Regexp) *syntax.Regexp {
	return &syntax.Regexp{Op: syntax.OpConcat, Sub: []*syntax.Regexp{a, b}}
}
