package security

import (
	"bytes"
	"io"
	"math/rand"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// secretFixture is one representative secret per pattern in secretPatterns.
// The table doubles as the proof that every pattern matches a single span
// shorter than ScrubCarryWindow, which is what makes the streaming writer's
// carry window sufficient.
type secretFixture struct {
	pattern   string
	text      string
	sensitive string
}

// fatJWTFixture is a JWT with a large payload. Its header and payload hold
// "_", as base64url segments of real tokens usually do.
func fatJWTFixture() string {
	payload := strings.Repeat("eyJzdWIiOiJzdGlycnVwLXJ1bm5lciIs_ImF1ZCI6InN0cy5hbXpvbmF3cy5jb20i", 96)
	return "ey" + "JhbGciOiJSUzI1NiIs_ImtpZCI6InN0aXJydXAtdGVzdCJ9." + payload + ".c2lnbmF0dXJlLXZhbHVlLTAxMjM0NTY3ODk"
}

func secretFixtures() []secretFixture {
	return []secretFixture{
		{"anthropic_wif_token", "sk-" + "ant-oat01-V1c0dGVzdFdJRnRva2VuMDEyMzQ1Njc4OQ", "V1c0dGVzdFdJRnRva2VuMDEyMzQ1Njc4OQ"},
		{"anthropic_api_key", "sk-" + "ant-api03-QUJDZGVmR0hJSktMbW5vcFFSU1RVdld4eXow", "QUJDZGVmR0hJSktMbW5vcFFSU1RVdld4eXow"},
		{"openai_api_key", "sk-" + "proj-T3BlbkFJdGVzdEtleTAxMjM0NTY3ODk", "T3BlbkFJdGVzdEtleTAxMjM0NTY3ODk"},
		{"stripe_live_key", "sk_" + "live_51QWERTYUIOPasdfghjkl1234567890", "51QWERTYUIOPasdfghjkl1234567890"},
		{"github_pat", "ghp_" + "16CharsOrMoreABCdef0123456789", "16CharsOrMoreABCdef0123456789"},
		{"github_app_token", "ghs_" + "AppTokenABCdef0123456789", "AppTokenABCdef0123456789"},
		{"aws_access_key_id", "AKIA" + "IOSFODNN7EXAMPLE", "AKIA" + "IOSFODNN7EXAMPLE"},
		{"aws_secret_access_key", `aws_secret_access_key = "` + "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" + `"`, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
		{"azure_storage_key", "account_key=" + "QWJjZGVmR2hpSmtsTW5vcFFyc3RVdnd4WXoxMjM0NTY3ODkwYWJjZGVmZ2g=", "QWJjZGVmR2hpSmtsTW5vcFFyc3RVdnd4WXoxMjM0NTY3ODkwYWJjZGVmZ2g"},
		{"slack_token", "xox" + "b-123456789012-123456789012-abcdefghijklmnopqrstuvwx", "123456789012-123456789012-abcdefghijklmnopqrstuvwx"},
		{"gcp_api_key", "AI" + "zaA2345678901234567890123456789012345", "zaA2345678901234567890123456789012345"},
		{"basic_auth", "Basic " + "c3RpcnJ1cDpzdXBlcnNlY3JldA==", "c3RpcnJ1cDpzdXBlcnNlY3JldA=="},
		{"bearer_token", "Bearer " + "abc.def.ghi-token-value-0123456789", "abc.def.ghi-token-value-0123456789"},
		{"pem_private_key", pemKey("EC PRIVATE KEY", pemBodyLines(3), "\n"), pemBodyLines(3)[2]},
		{"secret_ref", "secret:" + "//provider/api-key", "provider/api-key"},
		{"api_key_header", "x-api-key: " + "0123456789abcdefABCDEF", "0123456789abcdefABCDEF"},
		{"generic_hex_secret", "api_key = " + "0123456789abcdef0123456789abcdef", "0123456789abcdef0123456789abcdef"},
		{"oidc_jwt", fatJWTFixture(), "c2lnbmF0dXJlLXZhbHVlLTAxMjM0NTY3ODk"},
		{"gcp_access_token", "ya29." + "A0ARrdaMfakeTokenValue_0123456789", "A0ARrdaMfakeTokenValue_0123456789"},
	}
}

// multilineSecretFixtures cover the PEM pattern, the only one whose match
// spans a newline. Straddling one of these puts the last newline of the
// flushable region inside a secret, which is the only way the line-aware
// cut and the boundary scan interact.
func multilineSecretFixtures() []secretFixture {
	return []secretFixture{
		{"pem_private_key", "-----BEGIN\nRSA PRIVATE KEY-----", "-----BEGIN\nRSA PRIVATE KEY-----"},
		{"pem_private_key", pemKey("EC PRIVATE KEY", pemBodyLines(2), "\n"), pemBodyLines(2)[1]},
	}
}

// TestScrubWriter_RedactsSecretStraddlingChunkBoundary is the writer's
// central guarantee: a secret split across two chunks is still redacted, so
// chunked output is byte-identical to a whole-stream Scrub.
func TestScrubWriter_RedactsSecretStraddlingChunkBoundary(t *testing.T) {
	for _, fixture := range append(secretFixtures(), multilineSecretFixtures()...) {
		t.Run(fixture.pattern, func(t *testing.T) {
			must := require.New(t)
			window := 4 * len(fixture.text)
			if window < 256 {
				window = 256
			}
			chunk := 2 * window
			for _, overlap := range []int{1, len(fixture.text) / 2, len(fixture.text) - 1} {
				if overlap < 1 {
					continue
				}
				// The first flush cuts at chunk bytes, so a head of
				// chunk-overlap bytes straddles the secret across it.
				head := filler(chunk - overlap)
				input := head + fixture.text + " " + filler(window+len(fixture.text))
				for _, writeSize := range []int{len(input), 7} {
					var sink bytes.Buffer
					w := newScrubWriter(&sink, chunk, window)
					feed(t, w, input, writeSize)
					got := sink.String()
					must.NotContains(got, fixture.sensitive, "overlap=%d writeSize=%d: secret survived chunking", overlap, writeSize)
					must.Equal(Scrub(input), got, "overlap=%d writeSize=%d: chunked output diverges from whole-stream Scrub", overlap, writeSize)
				}
			}
		})
	}
}

// TestScrubWriter_RedactsFatJWTAtDefaultBoundary repeats the boundary case at
// the shipped chunk and window sizes with the largest fixture, so the
// default carry window is proven against a realistic worst case rather than
// only against test-sized chunks.
func TestScrubWriter_RedactsFatJWTAtDefaultBoundary(t *testing.T) {
	must := require.New(t)
	jwt := fatJWTFixture()
	must.Less(len(jwt), ScrubCarryWindow, "fixture JWT no longer fits the carry window")
	input := filler(scrubChunkBytes-len(jwt)/2) + jwt + " " + filler(ScrubCarryWindow)
	var sink bytes.Buffer
	w := NewScrubWriter(&sink)
	feed(t, w, input, 4<<10)
	got := sink.String()
	must.NotContains(got, "c2lnbmF0dXJlLXZhbHVlLTAxMjM0NTY3ODk", "fat JWT straddling the default chunk boundary was not fully redacted")
	must.Equal(Scrub(input), got, "chunked output diverges from whole-stream Scrub")
}

// TestScrubWriter_RedactsRSAKeyAtDefaultBoundary proves the default carry
// window against the largest PEM key the scrubber is expected to see whole.
func TestScrubWriter_RedactsRSAKeyAtDefaultBoundary(t *testing.T) {
	must := require.New(t)
	key := rsa4096PEMFixture()
	input := filler(scrubChunkBytes-len(key)/2) + key + "\n" + filler(ScrubCarryWindow)
	var sink bytes.Buffer
	w := NewScrubWriter(&sink)
	feed(t, w, input, 4<<10)
	got := sink.String()
	assertNoPEMBody(t, got, pemBodyLines(51))
	must.Equal(Scrub(input), got, "RSA key straddling the default chunk boundary was not redacted as one span")
}

// TestScrubWriter_RescansAfterLineAwareCutIsBlocked covers the case where the
// line-aware cut cannot be used: several patterns match across a newline
// (their separators are \s), so a span reaching offset 0 can block the cut at
// the last newline. The byte cut taken instead must be boundary-scanned in
// its own right, or a secret straddling it is scrubbed in two halves — and
// two halves that individually match nothing leave the secret intact.
func TestScrubWriter_RescansAfterLineAwareCutIsBlocked(t *testing.T) {
	blocker := "Basic \nQQQQQQQQ."
	secret := "AKIA" + "ABCDEFGHIJKLMNOP"
	sizes := []struct {
		name          string
		chunk, window int
	}{
		{"default sizes", scrubChunkBytes, ScrubCarryWindow},
		{"small sizes", 2048, 512},
	}
	for _, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			must := require.New(t)
			input := blocker + strings.Repeat("x", size.chunk-len(blocker)-len(secret)/2) +
				secret + strings.Repeat("y", size.window+64)
			var sink bytes.Buffer
			w := newScrubWriter(&sink, size.chunk, size.window)
			feed(t, w, input, 4096)
			got := sink.String()
			must.NotContains(got, secret, "secret straddling the byte cut survived")
			must.Equal(Scrub(input), got, "chunked output diverges from whole-stream Scrub")
		})
	}
}

// TestSecretPatternsMatchSingleSpanUnderCarryWindow pins the assumption the
// carry window rests on. A new pattern without a fixture, or a fixture whose
// match exceeds the window, fails here rather than silently widening the
// residual documented in the package comment.
func TestSecretPatternsMatchSingleSpanUnderCarryWindow(t *testing.T) {
	fixtures := map[string]secretFixture{}
	for _, f := range secretFixtures() {
		fixtures[f.pattern] = f
	}
	for _, p := range SecretPatterns() {
		t.Run(p.Name, func(t *testing.T) {
			must := require.New(t)
			f, ok := fixtures[p.Name]
			must.True(ok, "pattern %q has no boundary fixture; add one so the carry window stays proven", p.Name)
			span := p.Re.FindStringIndex(f.text)
			must.NotNil(span, "pattern %q does not match its own fixture", p.Name)
			must.Less(span[1]-span[0], ScrubCarryWindow, "pattern %q matches a span the carry window cannot reassemble", p.Name)
			must.Contains(f.text[span[0]:span[1]], f.sensitive, "pattern %q matches a span that excludes the sensitive value", p.Name)
		})
	}
	// A PEM match spans the whole key, so the largest keys in use must
	// still fit: RSA-4096 is about 3.3 KiB and RSA-8192 about 6.4 KiB.
	keys := map[string]string{
		"rsa-4096": rsa4096PEMFixture(),
		"rsa-8192": pemKey("RSA PRIVATE KEY", pemBodyLines(100), "\n"),
	}
	for name, key := range keys {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			span := secretPatternRe(t, "pem_private_key").FindStringIndex(key)
			must.NotNil(span, "key of %d bytes has no PEM match", len(key))
			must.Equal(len(key), span[1]-span[0], "PEM match does not cover the whole %d byte key", len(key))
			must.Less(len(key), ScrubCarryWindow, "key does not fit the %d byte carry window", ScrubCarryWindow)
		})
	}
}

func secretPatternRe(t *testing.T, name string) *regexp.Regexp {
	t.Helper()
	must := require.New(t)
	var re *regexp.Regexp
	for _, p := range SecretPatterns() {
		if p.Name == name {
			re = p.Re
			break
		}
	}
	must.NotNil(re, "no secret pattern %q", name)
	return re
}

// TestScrubWriter_OversizedSecretRedactsOnlyLeadingPortion documents the
// residual risk the chunked writer accepts: a single secret longer than the
// writer's whole buffer cannot be held for reassembly, so only its leading
// portion is redacted and the remainder reaches the sink.
func TestScrubWriter_OversizedSecretRedactsOnlyLeadingPortion(t *testing.T) {
	must := require.New(t)
	const window, chunk = 512, 2048
	tail := strings.Repeat("a", 8*(chunk+window))
	input := "Bearer " + tail + " end"
	var sink bytes.Buffer
	w := newScrubWriter(&sink, chunk, window)
	feed(t, w, input, 256)
	got := sink.String()
	must.True(strings.HasPrefix(got, redactedPlaceholder), "leading portion of an oversized secret must still be redacted, got %.32q", got)
	must.Contains(got, strings.Repeat("a", chunk), "residual is expected: the tail of a secret longer than the buffer survives")
}

// TestScrubWriter_DifferentialAgainstWholeStreamScrub searches the space the
// hand-written boundary fixtures cannot: random chunk and window sizes,
// random write sizes, filler with and without newlines, secrets at random
// offsets, and spans the buffer cannot reassemble mixed in. A secret shorter
// than the carry window must never survive, whatever the layout around it.
//
// The generator is seeded, so a failure is reproducible from the reported
// seed. The test catches a writer that emits a chunk boundary without
// re-scanning it.
func TestScrubWriter_DifferentialAgainstWholeStreamScrub(t *testing.T) {
	must := require.New(t)
	rng := rand.New(rand.NewSource(7)) //nolint:gosec // deterministic test input, not a security decision
	fixtures := append(secretFixtures(), multilineSecretFixtures()...)
	// Spans that begin a chunk and reach across its last newline block the
	// line-aware cut, so the byte cut and its rescan must handle them.
	blockers := []string{"Basic \nQQQQQQQQ.", "api_key =\naaaaaaaa", "Bearer\nzzzz", "account_key:\nbbbbbbbbbbbb"}
	for seed := 0; seed < 1500; seed++ {
		window := 256 + rng.Intn(512)
		chunk := window + 1 + rng.Intn(3*window)
		solid := rng.Intn(2) == 0
		oversized := false
		var stream strings.Builder
		var placed []secretFixture
		if rng.Intn(2) == 0 {
			stream.WriteString(blockers[rng.Intn(len(blockers))])
		}
		for i := 1 + rng.Intn(4); i > 0; i-- {
			stream.WriteString(randomFiller(rng, rng.Intn(2*chunk), solid) + " ")
			if rng.Intn(6) == 0 {
				stream.WriteString("Bearer " + strings.Repeat("Z", chunk+window+rng.Intn(1000)))
				oversized = true
				continue
			}
			f := fixtures[rng.Intn(len(fixtures))]
			stream.WriteString(f.text + " ")
			if len(f.text)*2 < window {
				placed = append(placed, f)
				continue
			}
			// The fat JWT is larger than the smaller random windows, which
			// makes it an oversized span like the blob above.
			oversized = oversized || len(f.text) >= window
		}
		stream.WriteString(randomFiller(rng, rng.Intn(2*chunk), solid))
		input := stream.String()
		var sink bytes.Buffer
		w := newScrubWriter(&sink, chunk, window)
		feed(t, w, input, 1+rng.Intn(4096))
		got := sink.String()
		want := Scrub(input)
		for _, f := range placed {
			if !strings.Contains(want, f.sensitive) {
				must.NotContains(got, f.sensitive, "seed=%d chunk=%d window=%d solid=%v: %s leaked", seed, chunk, window, solid, f.pattern)
			}
		}
		// A span the buffer cannot hold is cut mid-match by design, so only
		// streams without one are required to match byte for byte.
		if !oversized {
			must.Equal(want, got, "seed=%d chunk=%d window=%d solid=%v: output diverges from whole-stream Scrub", seed, chunk, window, solid)
		}
	}
}

func randomFiller(rng *rand.Rand, n int, solid bool) string {
	// Lowercase, space, and newline only: no secret pattern can match filler,
	// so any redaction the comparison sees comes from a placed fixture.
	alphabet := "abcdefghijklmnopqrstuvwxyz     \n\n"
	if solid {
		alphabet = "abcdefghijklmnopqrstuvwxyz     "
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return string(b)
}

// TestScrubWriter_StatsMatchWholeStreamScrub checks that the redaction
// statistics reported to the trace match a whole-stream scrub when the writer
// scrubs per chunk.
func TestScrubWriter_StatsMatchWholeStreamScrub(t *testing.T) {
	must := require.New(t)
	var b strings.Builder
	for round := 0; b.Len() < 4*(scrubChunkBytes+ScrubCarryWindow); round++ {
		for i, f := range secretFixtures() {
			b.WriteString(filler(600 + 37*i + 11*round))
			b.WriteString(f.text)
			b.WriteString("\n")
		}
	}
	input := b.String()
	var sink bytes.Buffer
	w := NewScrubWriter(&sink)
	feed(t, w, input, 3000)
	want, wantStats := ScrubWithStats(input)
	must.Equal(want, sink.String(), "chunked output diverges from whole-stream Scrub")
	got := w.Stats()
	must.Equal(wantStats.Count, got.Count)
	must.ElementsMatch(wantStats.Patterns, got.Patterns)
}

// TestScrubWriter_BufferStaysBounded proves the peak allocation claim: the
// writer's buffer never grows with the stream, only with its chunk and
// window.
func TestScrubWriter_BufferStaysBounded(t *testing.T) {
	must := require.New(t)
	w := NewScrubWriter(io.Discard)
	block := []byte(strings.Repeat("stirrup command output line\n", 8<<10))
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	streamed := 0
	for i := 0; i < 16; i++ {
		_, err := w.Write(block)
		must.NoError(err)
		streamed += len(block)
		must.LessOrEqual(cap(w.pending), w.maxPending, "pending buffer grew above the cap")
	}
	must.NoError(w.Close())
	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)
	live := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	must.LessOrEqual(live, int64(w.maxPending)*8, "live heap growth across a %d byte stream", streamed)
}

func BenchmarkScrubWriter(b *testing.B) {
	block := []byte(strings.Repeat("stirrup command output line\n", 4<<10))
	b.SetBytes(int64(len(block)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := NewScrubWriter(io.Discard)
		_, err := w.Write(block)
		require.NoError(b, err)
		require.NoError(b, w.Close())
	}
}

func feed(t *testing.T, w *ScrubWriter, input string, writeSize int) {
	t.Helper()
	must := require.New(t)
	for offset := 0; offset < len(input); offset += writeSize {
		end := offset + writeSize
		if end > len(input) {
			end = len(input)
		}
		n, err := w.Write([]byte(input[offset:end]))
		must.NoError(err, "write at offset %d", offset)
		must.Equal(end-offset, n, "short write")
	}
	must.NoError(w.Close(), "close")
}

// filler produces secret-free output of an exact length that always ends on
// a delimiter, so an adjacent fixture keeps the word boundary its pattern
// needs.
func filler(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("x", n-1) + " "
}

func TestRetreatCutPastSpans_CutIsNotInsideAnySpan(t *testing.T) {
	cases := map[string]struct {
		spans [][]int
		cut   int
		want  int
	}{
		"no spans":           {nil, 10, 10},
		"cut inside a span":  {[][]int{{4, 12}}, 10, 4},
		"cut at span start":  {[][]int{{10, 12}}, 10, 10},
		"cut at span end":    {[][]int{{4, 10}}, 10, 10},
		"second pass needed": {[][]int{{6, 12}, {2, 8}}, 10, 2},
		"unrelated spans":    {[][]int{{0, 2}, {12, 20}}, 10, 10},
		"cut at zero":        {[][]int{{0, 5}}, 0, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			got := retreatCutPastSpans(tc.spans, tc.cut)
			must.Equal(tc.want, got, "retreatCutPastSpans(%v, %d)", tc.spans, tc.cut)
		})
	}
}
