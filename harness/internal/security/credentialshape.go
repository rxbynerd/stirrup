package security

import (
	"encoding/base64"
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// alwaysScrubbedPatterns name the scrubber patterns whose matches carry no
// token body to classify, so ScrubLikelyCredentials redacts every match.
var alwaysScrubbedPatterns = map[string]bool{
	"pem_private_key": true,
	"secret_ref":      true,
}

var (
	credentialFieldPrefix = regexp.MustCompile(`^(?i)(?:(?:bearer|basic)\s+|[\w-]+\s*[:=]\s*["']?)`)
	credentialTokenPrefix = regexp.MustCompile(`^(?:sk-ant-(?:oat01-|api\d+-)?|sk_live_|sk-|gh[ps]_|AKIA|xox[abprs]-|AIza|ya29\.)`)
)

// ScrubLikelyCredentials redacts the scrubber matches in value that
// LooksLikeCredential accepts and leaves the others intact. It is for the
// copy of untrusted content a guard classifies: text an attacker dresses
// up as a token stays readable to the guard, as it is to the model. Logs,
// transports and history keep using Scrub.
func ScrubLikelyCredentials(value string) string {
	for _, p := range secretPatterns {
		base := matchValidators[p.name]
		always := alwaysScrubbedPatterns[p.name]
		value, _ = replaceValidated(p.re, value, func(m string) bool {
			if base != nil && !base(m) {
				return false
			}
			return always || LooksLikeCredential(m)
		})
	}
	return value
}

// LooksLikeCredential reports whether a scrubber match is shaped like a
// real credential: its body, with any scheme, field name or known prefix
// removed, has the character entropy of random output rather than filler,
// and holds no run of English words, neither as written nor once base64 or
// JWT segments are decoded. It is a heuristic; see docs/security.md for
// its limits.
func LooksLikeCredential(match string) bool {
	body := credentialFieldPrefix.ReplaceAllString(match, "")
	body = strings.Trim(body, `"'`)
	body = credentialTokenPrefix.ReplaceAllString(body, "")
	if body == "" || !highEntropy(body) {
		return false
	}
	for _, text := range readableForms(body) {
		if hasWordRun(text) {
			return false
		}
	}
	return true
}

// highEntropy reports whether s is not degenerate: at least half the
// Shannon entropy a string of its length drawn from its alphabet could
// reach. Random credentials sit far above the bar; repeated or patterned
// filler sits below it.
func highEntropy(s string) bool {
	counts := map[rune]int{}
	n := 0
	for _, r := range s {
		counts[r]++
		n++
	}
	var h float64
	for _, c := range counts {
		p := float64(c) / float64(n)
		h -= p * math.Log2(p)
	}
	return h >= 0.5*math.Log2(float64(min(n, alphabetSize(s))))
}

func alphabetSize(s string) int {
	hex, upperDigits := true, true
	for _, r := range s {
		isDigit := r >= '0' && r <= '9'
		if !isDigit && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			hex = false
		}
		if !isDigit && (r < 'A' || r > 'Z') {
			upperDigits = false
		}
	}
	switch {
	case hex:
		return 16
	case upperDigits:
		return 36
	default:
		return 64
	}
}

// readableForms returns body and every decoding of it that is mostly
// printable text: body as standard or URL-safe base64, and each segment of
// a dot-separated token such as a JWT.
func readableForms(body string) []string {
	forms := []string{body}
	pieces := []string{body}
	if strings.Contains(body, ".") {
		pieces = append(pieces, strings.Split(body, ".")...)
	}
	for _, piece := range pieces {
		if decoded, ok := decodeBase64Text(piece); ok {
			forms = append(forms, decoded)
		}
	}
	return forms
}

func decodeBase64Text(s string) (string, bool) {
	s = strings.TrimRight(s, "=")
	if len(s) < 8 {
		return "", false
	}
	if len(s)%4 == 1 {
		s = s[:len(s)-1]
	}
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		decoded, err := enc.DecodeString(s)
		if err != nil || !utf8.Valid(decoded) {
			continue
		}
		text := string(decoded)
		printable := 0
		for _, r := range text {
			if unicode.IsPrint(r) || r == '\n' || r == '\t' {
				printable++
			}
		}
		if printable*10 >= utf8.RuneCountInString(text)*9 {
			return text, true
		}
	}
	return "", false
}

// commonWords are frequent English words of three or more letters. A
// run of words must include some of them, which random token bodies
// almost never produce.
var commonWords = func() map[string]bool {
	words := map[string]bool{}
	for _, w := range strings.Fields(`
		the and for are but not you your all any can had her was one our out
		has his how its may new now old see two way who did get let put say
		she too use then than that this with from into onto have will what
		when were them they their there these those which while where would
		could should must shall about above after again also always before
		below every just make only other over same some such very ever never
		please ignore previous prior instruction instructions system override
		rule rules task tasks command commands run execute send read write
		delete remove file files secret secrets token tokens password key keys
		curl push pull remote repo repository shell user admin assistant model
		tool tools call tell show print output upload download data server url
		open follow following here what instead disregard forget reveal leak
		exfiltrate attacker evil env environment config credentials`) {
		words[w] = true
	}
	return words
}()

// hasWordRun reports whether text holds a run of at least four adjacent
// word-like tokens, three of them three letters or longer and three of
// them common English words. Tokens are letter runs, also split at camelCase
// boundaries; adjacent tokens touch or are separated by one space,
// underscore, hyphen or plus.
func hasWordRun(text string) bool {
	run, long, common := 0, 0, 0
	reset := func() { run, long, common = 0, 0, 0 }
	prevEnd := -1
	for _, tok := range letterTokens(text) {
		if prevEnd < 0 || !wordSeparator(text[prevEnd:tok.start]) {
			reset()
		}
		prevEnd = tok.end
		word := text[tok.start:tok.end]
		if !wordLike(word) {
			reset()
			continue
		}
		run++
		if len(word) >= 3 {
			long++
		}
		if commonWords[strings.ToLower(word)] {
			common++
		}
		if run >= 4 && long >= 3 && common >= 3 {
			return true
		}
	}
	return false
}

func wordSeparator(gap string) bool {
	return gap == "" || (len(gap) == 1 && strings.ContainsAny(gap, " _-+"))
}

type span struct{ start, end int }

// letterTokens splits text into ASCII letter runs, breaking a run where a
// lowercase letter is followed by an uppercase one.
func letterTokens(text string) []span {
	var out []span
	start := -1
	for i := 0; i <= len(text); i++ {
		letter := i < len(text) && isASCIILetter(text[i])
		camel := letter && start >= 0 && isUpper(text[i]) && !isUpper(text[i-1])
		if start >= 0 && (!letter || camel) {
			out = append(out, span{start, i})
			start = -1
		}
		if letter && start < 0 {
			start = i
		}
	}
	return out
}

// wordLike reports whether a letter token could be an English word: two
// or more letters, a vowel, and no run of five consonants.
func wordLike(w string) bool {
	if len(w) < 2 {
		return false
	}
	vowels, consonantRun := 0, 0
	for i := 0; i < len(w); i++ {
		if strings.IndexByte("aeiouyAEIOUY", w[i]) >= 0 {
			vowels++
			consonantRun = 0
			continue
		}
		consonantRun++
		if consonantRun >= 5 {
			return false
		}
	}
	return vowels > 0
}

func isASCIILetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
