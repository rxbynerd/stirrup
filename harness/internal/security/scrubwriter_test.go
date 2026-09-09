package security

import (
	"bytes"
	"io"
	"runtime"
	"strings"
	"testing"
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

func fatJWTFixture() string {
	payload := strings.Repeat("eyJzdWIiOiJzdGlycnVwLXJ1bm5lciIsImF1ZCI6InN0cy5hbXpvbmF3cy5jb20i", 96)
	return "ey" + "JhbGciOiJSUzI1NiIsImtpZCI6InN0aXJydXAtdGVzdCJ9." + payload + ".c2lnbmF0dXJlLXZhbHVlLTAxMjM0NTY3ODk"
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
		{"pem_private_key", "-----BEGIN RSA PRIVATE KEY-----", "-----BEGIN RSA PRIVATE KEY-----"},
		{"secret_ref", "secret:" + "//provider/api-key", "provider/api-key"},
		{"api_key_header", "x-api-key: " + "0123456789abcdefABCDEF", "0123456789abcdefABCDEF"},
		{"generic_hex_secret", "api_key = " + "0123456789abcdef0123456789abcdef", "0123456789abcdef0123456789abcdef"},
		{"oidc_jwt", fatJWTFixture(), "c2lnbmF0dXJlLXZhbHVlLTAxMjM0NTY3ODk"},
		{"gcp_access_token", "ya29." + "A0ARrdaMfakeTokenValue_0123456789", "A0ARrdaMfakeTokenValue_0123456789"},
	}
}

// TestScrubWriterRedactsSecretStraddlingChunkBoundary is the writer's
// central guarantee: a secret split across two chunks is still redacted, so
// chunked output is byte-identical to a whole-stream Scrub.
func TestScrubWriterRedactsSecretStraddlingChunkBoundary(t *testing.T) {
	for _, fixture := range secretFixtures() {
		t.Run(fixture.pattern, func(t *testing.T) {
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
					if strings.Contains(got, fixture.sensitive) {
						t.Fatalf("overlap=%d writeSize=%d: secret survived chunking", overlap, writeSize)
					}
					if want := Scrub(input); got != want {
						t.Fatalf("overlap=%d writeSize=%d: chunked output diverges from whole-stream Scrub", overlap, writeSize)
					}
				}
			}
		})
	}
}

// TestScrubWriterRedactsFatJWTAtDefaultBoundary repeats the boundary case at
// the shipped chunk and window sizes with the largest fixture, so the
// default carry window is proven against a realistic worst case rather than
// only against test-sized chunks.
func TestScrubWriterRedactsFatJWTAtDefaultBoundary(t *testing.T) {
	jwt := fatJWTFixture()
	if len(jwt) >= ScrubCarryWindow {
		t.Fatalf("fixture JWT of %d bytes no longer fits the %d byte carry window", len(jwt), ScrubCarryWindow)
	}
	input := filler(scrubChunkBytes-len(jwt)/2) + jwt + " " + filler(ScrubCarryWindow)
	var sink bytes.Buffer
	w := NewScrubWriter(&sink)
	feed(t, w, input, 4<<10)
	if got := sink.String(); strings.Contains(got, "c2lnbmF0dXJlLXZhbHVlLTAxMjM0NTY3ODk") || got != Scrub(input) {
		t.Fatal("fat JWT straddling the default chunk boundary was not fully redacted")
	}
}

// TestScrubWriterRescansAfterLineAwareCutIsBlocked covers the case where the
// line-aware cut cannot be used: several patterns match across a newline
// (their separators are \s), so a span reaching offset 0 can block the cut at
// the last newline. The byte cut taken instead must be boundary-scanned in
// its own right, or a secret straddling it is scrubbed in two halves — and
// two halves that individually match nothing leave the secret intact.
func TestScrubWriterRescansAfterLineAwareCutIsBlocked(t *testing.T) {
	blocker := "Basic \nQQQQQQQQ."
	secret := "AKIA" + "ABCDEFGHIJKLMNOP"
	sizes := []struct{ chunk, window int }{
		{scrubChunkBytes, ScrubCarryWindow},
		{2048, 512},
	}
	for _, size := range sizes {
		input := blocker + strings.Repeat("x", size.chunk-len(blocker)-len(secret)/2) +
			secret + strings.Repeat("y", size.window+64)
		var sink bytes.Buffer
		w := newScrubWriter(&sink, size.chunk, size.window)
		feed(t, w, input, 4096)
		got := sink.String()
		if strings.Contains(got, secret) {
			t.Errorf("chunk=%d window=%d: secret straddling the byte cut survived", size.chunk, size.window)
		}
		if want := Scrub(input); got != want {
			t.Errorf("chunk=%d window=%d: chunked output diverges from whole-stream Scrub", size.chunk, size.window)
		}
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
		f, ok := fixtures[p.Name]
		if !ok {
			t.Errorf("pattern %q has no boundary fixture; add one so the carry window stays proven", p.Name)
			continue
		}
		span := p.Re.FindStringIndex(f.text)
		if span == nil {
			t.Errorf("pattern %q does not match its own fixture", p.Name)
			continue
		}
		if got := span[1] - span[0]; got >= ScrubCarryWindow {
			t.Errorf("pattern %q matches a %d byte span, which the %d byte carry window cannot reassemble", p.Name, got, ScrubCarryWindow)
		}
		if !strings.Contains(f.text[span[0]:span[1]], f.sensitive) {
			t.Errorf("pattern %q matches a span that excludes the sensitive value", p.Name)
		}
	}
}

// TestScrubWriterResidualExceedsBuffer documents the residual risk the
// chunked writer accepts: a single secret longer than the writer's whole
// buffer cannot be held for reassembly, so only its leading portion is
// redacted and the remainder reaches the sink.
func TestScrubWriterResidualExceedsBuffer(t *testing.T) {
	const window, chunk = 512, 2048
	tail := strings.Repeat("a", 8*(chunk+window))
	input := "Bearer " + tail + " end"
	var sink bytes.Buffer
	w := newScrubWriter(&sink, chunk, window)
	feed(t, w, input, 256)
	got := sink.String()
	if !strings.HasPrefix(got, redactedPlaceholder) {
		t.Fatalf("leading portion of an oversized secret must still be redacted, got %.32q", got)
	}
	if !strings.Contains(got, strings.Repeat("a", chunk)) {
		t.Fatal("residual is expected: the tail of a secret longer than the buffer survives")
	}
}

// TestScrubWriterStatsMatchWholeStreamScrub keeps the redaction statistics
// reported to the trace correct after the move to per-chunk scrubbing.
func TestScrubWriterStatsMatchWholeStreamScrub(t *testing.T) {
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
	if sink.String() != want {
		t.Fatal("chunked output diverges from whole-stream Scrub")
	}
	got := w.Stats()
	if got.Count != wantStats.Count {
		t.Fatalf("redaction count=%d want %d", got.Count, wantStats.Count)
	}
	if strings.Join(sortedCopy(got.Patterns), ",") != strings.Join(sortedCopy(wantStats.Patterns), ",") {
		t.Fatalf("redaction patterns=%v want %v", got.Patterns, wantStats.Patterns)
	}
}

// TestScrubWriterBufferStaysBounded proves the peak allocation claim: the
// writer's buffer never grows with the stream, only with its chunk and
// window.
func TestScrubWriterBufferStaysBounded(t *testing.T) {
	w := NewScrubWriter(io.Discard)
	block := []byte(strings.Repeat("stirrup command output line\n", 8<<10))
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	streamed := 0
	for i := 0; i < 16; i++ {
		if _, err := w.Write(block); err != nil {
			t.Fatal(err)
		}
		streamed += len(block)
		if cap(w.pending) > w.maxPending {
			t.Fatalf("pending buffer grew to %d bytes, above the %d byte cap", cap(w.pending), w.maxPending)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)
	if live := int64(after.HeapAlloc) - int64(before.HeapAlloc); live > int64(w.maxPending)*8 {
		t.Fatalf("live heap grew by %d bytes across a %d byte stream", live, streamed)
	}
}

func BenchmarkScrubWriter(b *testing.B) {
	block := []byte(strings.Repeat("stirrup command output line\n", 4<<10))
	b.SetBytes(int64(len(block)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := NewScrubWriter(io.Discard)
		if _, err := w.Write(block); err != nil {
			b.Fatal(err)
		}
		if err := w.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func feed(t *testing.T, w *ScrubWriter, input string, writeSize int) {
	t.Helper()
	for offset := 0; offset < len(input); offset += writeSize {
		end := offset + writeSize
		if end > len(input) {
			end = len(input)
		}
		n, err := w.Write([]byte(input[offset:end]))
		if err != nil {
			t.Fatalf("write: %v", err)
		}
		if n != end-offset {
			t.Fatalf("short write %d of %d", n, end-offset)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
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

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
