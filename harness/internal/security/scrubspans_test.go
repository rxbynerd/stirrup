package security

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// scrubSpanCorpus is every scrubber fixture in this package plus inputs
// where patterns overlap: a secret inside another pattern's value, a match
// running across an earlier placeholder, literal placeholder text in the
// input, and the pattern-order cases.
func scrubSpanCorpus() []string {
	corpus := []string{
		"", "plain", "this is a normal log line",
		"keys: sk-ant-abc123 and ghp_token456",
		"sk-ant-abc123_DEF-456", "sk-ant-oat01-abcDEF_123-xyz", "sk-abcdefghijklmnop12345",
		"ghp_abcdefghijklmnop123456", "ghs_appTokenValue123", "AKIAIOSFODNN7EXAMPLE",
		"aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"primary_access_key: abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQ=",
		slackTokenFixture(), stripeLiveKeyFixture(), gcpAPIKeyFixture(),
		"Bearer eyJhbGciOiJSUzI1NiJ9.abc", "Bearer aaa.bbb.ccc", "Bearer x", "basic documentation",
		"Basic dXNlcjpwYXNz", "Basic Basic Basic dXNlcjpwYXNz",
		"-----BEGIN RSA PRIVATE KEY-----", rsa4096PEMFixture(),
		"ref: secret://ANTHROPIC_KEY", "SECRET://x", "api-key: 0123456789abcdef0123456789abcdef",
		"x-api-key: SOME-VENDOR-VALUE", "Ocp-Apim-Subscription-Key: opaque-token-here",
		"eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.abc123", "ya29.AbCdEfGhIjKl",
		"client_secret=0123456789abcdef0123456789abcdef",
		"/tmp/eval-task-narrow-edit-leaves-surroundings-alone-1530359655/target.txt",
		"OPENAI_API_KEY=sk-abcdefghijklmnop12345", `{"key":"sk-ant-oat01-abcDEF_123-xyz"}`,
		// A secret inside a Bearer value: the key goes first, then the
		// Bearer match ends at the placeholder it cannot cross.
		"Authorization: Bearer abc.sk-ant-api03-QUJDZGVmR0hJSktM tail",
		"Authorization: Bearer sk-ant-api03-QUJDZGVmR0hJSktM",
		// secret_ref runs across the placeholder of a Bearer match whose
		// whitespace had stopped it on the original text.
		"secret://aBearer tok!more and after",
		"secret://sk-ant-api03-QUJDZGVmR0hJSktM!rest",
		// Literal placeholder text, alone and adjacent to matches.
		"[REDACTED]", "x[REDACTED]sk-ant-api03-QUJDZGVmR0hJSktM", "secret://[REDACTED]x y",
		"Bearer [REDACTED]", "[REDACTED]ghp_abc[REDACTED]",
		// Pattern order: the WIF token before the API key, Basic before
		// Bearer, the header before the generic hex rule.
		"sk-ant-oat01-a sk-ant-b sk-cccccccccccccccccc", "Basic dXNlcjpwYXNz Bearer dXNlcjpwYXNz",
		"api-key: token=0123456789abcdef0123456789abcdef",
		"token: Bearer ghp_abc", "secret://Basic dXNlcjpwYXNz", "ghp_ghs_ghp_x",
		"aws_secret_access_key=" + strings.Repeat("A", 40) + "AKIA" + strings.Repeat("B", 16),
	}
	for _, f := range append(secretFixtures(), multilineSecretFixtures()...) {
		corpus = append(corpus, f.text, "pre "+f.text+" post", f.text+f.text)
	}
	return corpus
}

// checkScrubSpans asserts that ScrubWithSpans agrees with ScrubWithStats
// and that its segments account for every byte: kept segments are the
// original bytes, in order, and the segment ranges tile the input, so a
// byte Scrub does not keep lies in a redacted range.
func checkScrubSpans(t *testing.T, value string) {
	t.Helper()
	got, spans, stats := ScrubWithSpans(value)
	want, wantStats := ScrubWithStats(value)
	if got != want {
		t.Fatalf("ScrubWithSpans(%q) = %q, Scrub = %q", value, got, want)
	}
	if !reflect.DeepEqual(stats, wantStats) {
		t.Fatalf("ScrubWithSpans(%q) stats = %+v, ScrubWithStats = %+v", value, stats, wantStats)
	}
	if !spansExplainScrub(value, want, spans, 0, 0) {
		t.Fatalf("%q: Scrub = %q is not the input with each of spans %v replaced by placeholders", value, want, spans)
	}
	segs, _ := scrubSegments(value)
	var text strings.Builder
	pos := 0
	var redacted []ScrubSpan
	for _, seg := range segs {
		text.WriteString(seg.text)
		if seg.start != pos || seg.end < seg.start {
			t.Fatalf("%q: segment range [%d,%d) does not continue at %d", value, seg.start, seg.end, pos)
		}
		pos = seg.end
		if !seg.redacted {
			if seg.text != value[seg.start:seg.end] {
				t.Fatalf("%q: kept segment %q is not the original bytes %q", value, seg.text, value[seg.start:seg.end])
			}
			continue
		}
		if seg.start == seg.end {
			continue
		}
		if n := len(redacted); n > 0 && redacted[n-1].End == seg.start {
			redacted[n-1].End = seg.end
		} else {
			redacted = append(redacted, ScrubSpan{Start: seg.start, End: seg.end})
		}
	}
	if pos != len(value) {
		t.Fatalf("%q: segments end at %d of %d bytes", value, pos, len(value))
	}
	if text.String() != want {
		t.Fatalf("%q: segments spell %q, Scrub = %q", value, text.String(), want)
	}
	if !reflect.DeepEqual(spans, redacted) {
		t.Fatalf("%q: spans = %v, redacted segments = %v", value, spans, redacted)
	}
	for i, s := range spans {
		if s.Start >= s.End || (i > 0 && spans[i-1].End >= s.Start) {
			t.Fatalf("%q: spans %v are not ordered, disjoint and non-empty", value, spans)
		}
	}
}

// spansExplainScrub reports whether out is value with each span from
// spans[i:] replaced by one or more placeholders and every other byte
// kept, matching from value offset at and out offset j. It is independent
// of the segment bookkeeping it checks.
func spansExplainScrub(value, out string, spans []ScrubSpan, at, j int) bool {
	if len(spans) == 0 {
		return out[j:] == value[at:]
	}
	kept := value[at:spans[0].Start]
	if !strings.HasPrefix(out[j:], kept) {
		return false
	}
	j += len(kept)
	for strings.HasPrefix(out[j:], redactedPlaceholder) {
		j += len(redactedPlaceholder)
		if spansExplainScrub(value, out, spans[1:], spans[0].End, j) {
			return true
		}
	}
	return false
}

func TestScrubWithSpans_ConsistentWithScrub(t *testing.T) {
	for _, value := range scrubSpanCorpus() {
		checkScrubSpans(t, value)
	}
}

// TestScrubWithSpans_RandomLayoutsConsistentWithScrub joins corpus entries
// with random separators, so matches meet, nest and abut in layouts the
// hand-written cases do not.
func TestScrubWithSpans_RandomLayoutsConsistentWithScrub(t *testing.T) {
	rng := rand.New(rand.NewSource(11)) //nolint:gosec // deterministic test input, not a security decision
	corpus := scrubSpanCorpus()
	seps := []string{"", " ", "\n", ".", "=", ":", "-", "_", "/", "\"", "[REDACTED]", "secret://", "Bearer ", "Basic ", "sk-", "eyJ"}
	for i := 0; i < 3000; i++ {
		var b strings.Builder
		for n := 1 + rng.Intn(5); n > 0; n-- {
			b.WriteString(seps[rng.Intn(len(seps))])
			entry := corpus[rng.Intn(len(corpus))]
			if len(entry) > 0 && rng.Intn(3) == 0 {
				entry = entry[rng.Intn(len(entry)):]
			}
			b.WriteString(entry)
		}
		checkScrubSpans(t, b.String())
	}
}

func TestScrubWithSpans_SpansCoverTheReplacedText(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"key sk-ant-api03-QUJDZGVmR0hJSktM here", []string{"sk-ant-api03-QUJDZGVmR0hJSktM"}},
		{"Bearer abc.sk-ant-api03-QUJDZGVmR0hJSktM tail", []string{"Bearer abc.sk-ant-api03-QUJDZGVmR0hJSktM"}},
		{"secret://aBearer tok!more and after", []string{"secret://aBearer tok!more"}},
		{"a ghp_x b ghs_y c", []string{"ghp_x", "ghs_y"}},
		{"nothing here", nil},
	}
	for _, tc := range cases {
		_, spans, _ := ScrubWithSpans(tc.in)
		var got []string
		for _, s := range spans {
			got = append(got, tc.in[s.Start:s.End])
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ScrubWithSpans(%q) spans = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestReplaceSegments_MatchEndingInsidePlaceholder covers a match that
// starts or ends inside an earlier placeholder, which no shipped pattern
// produces: the placeholder's original range must stay inside a redacted
// segment either side of the cut.
func TestReplaceSegments_MatchEndingInsidePlaceholder(t *testing.T) {
	segs := []scrubSegment{
		{text: "aa", start: 0, end: 2},
		{text: redactedPlaceholder, start: 2, end: 8, redacted: true},
		{text: "bb", start: 8, end: 10},
	}
	// "a[REDAC" in "aa[REDACTED]bb".
	got := replaceSegments(append([]scrubSegment(nil), segs...), [][]int{{1, 8}})
	want := []scrubSegment{
		{text: "a", start: 0, end: 1},
		{text: redactedPlaceholder, start: 1, end: 2, redacted: true},
		{text: "TED]", start: 2, end: 8, redacted: true},
		{text: "bb", start: 8, end: 10},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("segments = %+v, want %+v", got, want)
	}
	// "ED]b" in "aa[REDACTED]bb".
	got = replaceSegments(append([]scrubSegment(nil), segs...), [][]int{{9, 13}})
	want = []scrubSegment{
		{text: "aa", start: 0, end: 2},
		{text: "[REDACT", start: 2, end: 2, redacted: true},
		{text: redactedPlaceholder, start: 2, end: 9, redacted: true},
		{text: "b", start: 9, end: 10},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("segments = %+v, want %+v", got, want)
	}
}

func FuzzScrubWithSpans(f *testing.F) {
	for _, value := range scrubSpanCorpus() {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		checkScrubSpans(t, value)
	})
}
