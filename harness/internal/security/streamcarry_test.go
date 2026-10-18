package security

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// streamThroughCarry feeds fragments through a StreamCarry and returns
// the joined output and the released pieces.
func streamThroughCarry(fragments []string) (string, []string) {
	var c StreamCarry
	var pieces []string
	for _, f := range fragments {
		if out, _ := c.Push(f); out != "" {
			pieces = append(pieces, out)
		}
	}
	if out, _ := c.Flush(); out != "" {
		pieces = append(pieces, out)
	}
	return strings.Join(pieces, ""), pieces
}

// chunk splits s into pieces of n bytes, the last one shorter.
func chunk(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

func streamFixtures() []secretFixture {
	encrypted := "-----BEGIN RSA PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,0123456789ABCDEF\n\n" +
		strings.Join(pemBodyLines(12), "\n") + "\n-----END RSA PRIVATE KEY-----"
	return append(append(secretFixtures(), multilineSecretFixtures()...),
		secretFixture{"pem_private_key", rsa4096PEMFixture(), pemBodyLines(51)[50]},
		secretFixture{"pem_private_key", encrypted, pemBodyLines(12)[11]},
	)
}

// prefixFixtures are the stream fixtures plus the shapes a hand-picked
// fixture set tends to miss: case variants of the case-insensitive
// patterns, quoted values, the longest key-name prefix a pattern accepts
// before its value, and a match running across an earlier placeholder.
func prefixFixtures() []secretFixture {
	azure := strings.Repeat("QWJjZGVmR2hp", 8)[:88]
	return append(streamFixtures(),
		secretFixture{"azure_storage_key", `azure_storage_account_key: "` + azure + `=="`, azure},
		secretFixture{"azure_storage_key", `AZURE_STORAGE_ACCOUNT_KEY='` + azure + `'`, azure},
		secretFixture{"aws_secret_access_key", `AWS_SECRET_ACCESS_KEY: '` + "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY" + `'`, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
		secretFixture{"bearer_token", "bEaReR " + "abc.def.ghi-token-value-0123456789", "abc.def.ghi-token-value-0123456789"},
		secretFixture{"basic_auth", "BASIC " + "c3RpcnJ1cDpzdXBlcnNlY3JldA==", "c3RpcnJ1cDpzdXBlcnNlY3JldA=="},
		secretFixture{"secret_ref", "SECRET:" + "//provider/api-key", "provider/api-key"},
		secretFixture{"api_key_header", "OCP-APIM-SUBSCRIPTION-KEY: " + "0123456789abcdefABCDEF", "0123456789abcdefABCDEF"},
		secretFixture{"generic_hex_secret", `CLIENT_SECRET: "` + "0123456789abcdef0123456789abcdef" + `"`, "0123456789abcdef0123456789abcdef"},
		// secret_ref runs on across the Bearer placeholder.
		secretFixture{"secret_ref", "secret://aBearer tok!more and after", "tok!more"},
		secretFixture{"pem_private_key", "-----BEGIN PGP PRIVATE KEY BLOCK-----\nVersion: 1\n\n" + strings.Join(pemBodyLines(4), "\n") + "\n=ABCD\n-----END PGP PRIVATE KEY BLOCK-----", pemBodyLines(4)[3]},
	)
}

// TestStreamCarry_EverySecretPatternHasFixture keeps the prefix test below
// complete: a pattern added without a fixture fails here.
func TestStreamCarry_EverySecretPatternHasFixture(t *testing.T) {
	have := map[string]bool{}
	for _, f := range prefixFixtures() {
		have[f.pattern] = true
	}
	for _, p := range SecretPatterns() {
		if !have[p.Name] {
			t.Errorf("secret pattern %q has no stream fixture", p.Name)
		}
	}
}

// TestStreamCarry_SentinelNeverBeginsAMatch pins what carryScan's "_"
// sentinel relies on: no pattern, and no prefix of one, starts with it.
func TestStreamCarry_SentinelNeverBeginsAMatch(t *testing.T) {
	for _, f := range prefixFixtures() {
		for i, p := range secretPatterns {
			in := "_" + f.text
			if loc := p.re.FindStringIndex(in); loc != nil && loc[0] == 0 {
				t.Errorf("%s matches from the sentinel in %q", p.name, in)
			}
			if loc := streamPrefixes[i].FindStringIndex(in); loc != nil && loc[0] == 0 {
				t.Errorf("%s prefix matches from the sentinel in %q", p.name, in)
			}
		}
	}
}

// TestStreamCarry_EveryPrefixOfEveryFixtureIsHeld is the carry's central
// guarantee for each pattern on its own: whichever byte a secret is split
// at, the released pieces equal the whole secret scrubbed.
func TestStreamCarry_EveryPrefixOfEveryFixtureIsHeld(t *testing.T) {
	for _, f := range prefixFixtures() {
		want := Scrub(f.text)
		// Every byte of a short secret; a stride through a long one.
		step := 1
		if len(f.text) > 256 {
			step = len(f.text) / 64
		}
		for k := 1; k < len(f.text); k += step {
			got, _ := streamThroughCarry([]string{f.text[:k], f.text[k:]})
			if got != want {
				t.Fatalf("%s split at %d of %d: streamed output %q, want %q", f.pattern, k, len(f.text), got, want)
			}
		}
	}
}

// TestStreamCarry_SecretSplitAcrossTwoFragmentsIsScrubbed checks the same
// guarantee with prose around each secret.
func TestStreamCarry_SecretSplitAcrossTwoFragmentsIsScrubbed(t *testing.T) {
	for _, f := range streamFixtures() {
		input := "The model wrote this out: " + f.text + " and then went on with ordinary prose."
		start := strings.Index(input, f.text)
		want := Scrub(input)
		// Every byte of a short secret; a stride through a long one.
		step := max(1, len(f.text)/64)
		for split := start + 1; split < start+len(f.text); split += step {
			got, _ := streamThroughCarry([]string{input[:split], input[split:]})
			if strings.Contains(got, f.sensitive) {
				t.Fatalf("%s split at %d: secret survived: %q", f.pattern, split-start, got)
			}
			if got != want {
				t.Fatalf("%s split at %d: streamed output %q, want %q", f.pattern, split-start, got, want)
			}
		}
	}
}

// TestStreamCarry_JWTWithUnderscoreSplitBeforeSecondDot covers a JWT
// whose header and payload hold "_", streamed in small fragments with a
// split at every offset before the signature.
func TestStreamCarry_JWTWithUnderscoreSplitBeforeSecondDot(t *testing.T) {
	jwt := "ey" + "JhbGciOiJSUzI1NiIs_ImtpZCI6In0." + strings.Repeat("eyJzdWIiOiJy_dW5uZXIiLCJhdWQiOiJzdHMifQ", 2) + ".c2lnbmF0dXJl_LXZhbHVl"
	input := "token: " + jwt + " end"
	want := Scrub(input)
	start := strings.Index(input, jwt)
	secondDot := start + strings.LastIndexByte(jwt, '.')
	for _, size := range []int{1, 7} {
		for k := start + 1; k <= secondDot; k++ {
			fragments := append(chunk(input[:k], size), chunk(input[k:], size)...)
			if got, _ := streamThroughCarry(fragments); got != want {
				t.Fatalf("%d-byte fragments split at %d: streamed output %q, want %q", size, k-start, got, want)
			}
		}
	}
}

// TestStreamCarry_RandomFragmentsMatchWholeStreamScrub searches layouts
// the two-fragment case does not: several secrets, small and uneven
// fragments, and secrets back to back.
func TestStreamCarry_RandomFragmentsMatchWholeStreamScrub(t *testing.T) {
	rng := rand.New(rand.NewSource(5)) //nolint:gosec // deterministic test input, not a security decision
	fixtures := prefixFixtures()
	words := []string{"the", "result", "is", "ready.", "see", "below:", "\n", "ok,", "café", "naïve", "日本語"}
	for seed := 0; seed < 400; seed++ {
		var b strings.Builder
		var placed []secretFixture
		for n := 1 + rng.Intn(4); n > 0; n-- {
			for w := rng.Intn(40); w > 0; w-- {
				b.WriteString(words[rng.Intn(len(words))] + " ")
			}
			f := fixtures[rng.Intn(len(fixtures))]
			b.WriteString(f.text + " ")
			placed = append(placed, f)
		}
		input := b.String()
		var fragments []string
		for rest := input; rest != ""; {
			n := min(1+rng.Intn(24), len(rest))
			for n < len(rest) && !utf8.RuneStart(rest[n]) {
				n++
			}
			fragments = append(fragments, rest[:n])
			rest = rest[n:]
		}
		got, pieces := streamThroughCarry(fragments)
		for _, f := range placed {
			if strings.Contains(got, f.sensitive) {
				t.Fatalf("seed %d: %s survived streaming", seed, f.pattern)
			}
		}
		if want := Scrub(input); got != want {
			t.Fatalf("seed %d: streamed output diverges from whole-stream Scrub\n got %q\nwant %q", seed, got, want)
		}
		for _, p := range pieces {
			if !utf8.ValidString(p) {
				t.Fatalf("seed %d: piece %q splits a rune", seed, p)
			}
		}
	}
}

// TestStreamCarry_ReleasesOrdinaryProseImmediately bounds the latency the
// carry adds to text no pattern can match: only a short word-fragment
// that could begin a keyword is ever held.
func TestStreamCarry_ReleasesOrdinaryProseImmediately(t *testing.T) {
	var c StreamCarry
	released := 0
	prose := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 40)
	for i := 0; i < len(prose); i += 5 {
		end := min(i+5, len(prose))
		out, _ := c.Push(prose[i:end])
		released += len(out)
		if held := end - released; held > 10 {
			t.Fatalf("after %d bytes the carry holds %d: %q", end, held, prose[released:end])
		}
	}
	if rest, _ := c.Flush(); released+len(rest) != len(prose) {
		t.Fatalf("released %d + flushed %d bytes, want %d", released, len(rest), len(prose))
	}
}

// TestStreamCarry_TruncatedPEMThenProseDoesNotLeakBody pins that an
// unterminated key is released as soon as prose shows no END line can
// follow, with its header, so the scrubbed piece still redacts the body.
func TestStreamCarry_TruncatedPEMThenProseDoesNotLeakBody(t *testing.T) {
	body := pemBodyLines(2)
	input := "Here is the key:\n-----BEGIN RSA PRIVATE KEY-----\n" + strings.Join(body, "\n") + "\n\n" +
		strings.Repeat("Then the model kept writing ordinary prose about it. ", 15<<10/53)
	rng := rand.New(rand.NewSource(3)) //nolint:gosec // deterministic test input, not a security decision
	var fragments []string
	for rest := input; rest != ""; {
		n := min(1+rng.Intn(12), len(rest))
		fragments = append(fragments, rest[:n])
		rest = rest[n:]
	}
	var c StreamCarry
	maxHeld := 0
	var b strings.Builder
	for _, f := range fragments {
		out, _ := c.Push(f)
		b.WriteString(out)
		maxHeld = max(maxHeld, c.held())
	}
	out, _ := c.Flush()
	b.WriteString(out)
	assertNoPEMBody(t, b.String(), body)
	if maxHeld > 1024 {
		t.Errorf("carry held %d bytes; an unterminated key followed by prose should be released early", maxHeld)
	}
}

// TestStreamCarry_ShortSecretBehindLongHoldIsRedacted pins that a secret
// arriving just after an unterminated PEM header, followed by more than
// streamCarryMax bytes of prose, is released whole in one piece.
func TestStreamCarry_ShortSecretBehindLongHoldIsRedacted(t *testing.T) {
	prose := strings.Repeat("plain words here. ", 9<<10/18)
	input := "-----BEGIN RSA PRIVATE KEY-----\nnext line: ghp_" + "AbC123xyzAbC123xyzAbC1 and then " + prose
	for _, size := range []int{1, 3, 7} {
		got, _ := streamThroughCarry(chunk(input, size))
		if strings.Contains(got, "AbC1") {
			t.Fatalf("%d-byte fragments: github token survived", size)
		}
	}
}

// TestStreamCarry_SpanOverCapLeaksOnlyBeyondTheCap pins the documented
// residual for a match longer than streamCarryMax: everything through its
// first streamCarryMax bytes is redacted, and only body lines starting
// after that offset can reach the sink.
func TestStreamCarry_SpanOverCapLeaksOnlyBeyondTheCap(t *testing.T) {
	body := pemBodyLines(200)
	for i := range body {
		body[i] = fmt.Sprintf("Z%03d", i) + body[i][4:]
	}
	key := pemKey("RSA PRIVATE KEY", body, "\n")
	input := "key: " + key + "\nafter"
	keyStart := strings.Index(input, key)
	for _, size := range []int{1, 7, 50, 500, 3000} {
		got, _ := streamThroughCarry(chunk(input, size))
		for _, line := range body {
			at := keyStart + strings.Index(key, line)
			leaked := strings.Contains(got, line)
			if leaked && at-keyStart < streamCarryMax {
				t.Fatalf("%d-byte fragments: body line at key offset %d leaked, inside the first %d bytes", size, at-keyStart, streamCarryMax)
			}
		}
		if !strings.HasPrefix(got, "key: [REDACTED]") {
			t.Fatalf("%d-byte fragments: key head not redacted: %q", size, got[:40])
		}
	}
}

// TestStreamCarry_RuneStraddlingForcedCut pins that the release forced at
// the cap never splits a multi-byte rune, even when fragments do.
func TestStreamCarry_RuneStraddlingForcedCut(t *testing.T) {
	input := "secret://" + strings.Repeat("日本語", 4<<10) + " done"
	for _, size := range []int{1, 2, 5} {
		got, pieces := streamThroughCarry(chunk(input, size))
		if !strings.HasPrefix(got, "[REDACTED]") || !strings.HasSuffix(got, "日本語 done") {
			t.Fatalf("%d-byte fragments: output %q...%q, want the redacted head and the plain tail", size, got[:20], got[len(got)-20:])
		}
		for _, p := range pieces {
			if !utf8.ValidString(p) {
				t.Fatalf("%d-byte fragments: piece splits a rune: %q", size, p[:min(len(p), 16)])
			}
		}
	}
}

// TestStreamCarry_HoldIsBounded pins streamCarryMax: a run no pattern ends
// is released once it outgrows the bound, so a stream cannot be stalled.
func TestStreamCarry_HoldIsBounded(t *testing.T) {
	var c StreamCarry
	run := "Bearer " + strings.Repeat("a", 4*streamCarryMax)
	released := 0
	for i := 0; i < len(run); i += 7 {
		out, _ := c.Push(run[i:min(i+7, len(run))])
		released += len(out)
		if c.held() > streamCarryMax {
			t.Fatalf("carry holds %d bytes, above streamCarryMax", c.held())
		}
	}
	if released == 0 {
		t.Fatal("nothing was released from a run four times streamCarryMax")
	}
}

// TestStreamCarry_WhitespaceAfterKeyword pins that whitespace inside a
// match is held like the rest of it: a secret split after a whitespace run
// is redacted while the keyword and whitespace fit in streamCarryMax, and
// reaches the sink once they do not.
func TestStreamCarry_WhitespaceAfterKeyword(t *testing.T) {
	pem := "RSA PRIVATE KEY-----\n" + strings.Join(pemBodyLines(3), "\n") + "\n-----END RSA PRIVATE KEY-----"
	cases := []struct{ keyword, secret, sensitive string }{
		{"Bearer", "abc.def.ghi-token-value-0123456789", "ghi-token-value"},
		{"Basic", "c3RpcnJ1cDpzdXBlcnNlY3JldA==", "c3RpcnJ1cDpzdXBlcnNlY3JldA"},
		{"api_key", "= 0123456789abcdef0123456789abcdef", "0123456789abcdef0123456789abcdef"},
		{"-----BEGIN", pem, pemBodyLines(3)[2]},
	}
	for _, tc := range cases {
		limit := streamCarryMax - len(tc.keyword)
		for _, ws := range []int{0, 80, 90, 100, 3000, limit, limit + 1} {
			head := tc.keyword + strings.Repeat(" ", max(ws, 1))
			got, _ := streamThroughCarry(append(chunk(head, 64), tc.secret+" tail"))
			leaked := strings.Contains(got, tc.sensitive)
			if want := ws > limit; leaked != want {
				t.Errorf("%s followed by %d spaces: secret leaked = %v, want %v", tc.keyword, ws, leaked, want)
			}
		}
	}
}

// TestStreamCarry_RescanIsAmortised pins that a long held span is not
// rescanned in full for every fragment: the bytes scanned stay within a
// constant multiple of the stream.
func TestStreamCarry_RescanIsAmortised(t *testing.T) {
	run := "Bearer " + strings.Repeat("Ab0.", 16<<10)
	var c StreamCarry
	for i := range len(run) {
		c.Push(run[i : i+1])
	}
	c.Push(strings.Repeat("Ab0.", 256<<10))
	c.Flush()
	if n := len(run) + 1<<20; c.scanned > 16*n {
		t.Errorf("carry scanned %d bytes for a %d byte stream, over 16 times its length", c.scanned, n)
	}
}

// TestStreamCarry_AdversarialStreamsStayFast pins that streaming stays
// linear when a span is held for a long time. The bound is relative to one
// whole-stream Scrub of the same input, so it holds under the race
// detector and on slow machines; an unbounded hold rescanned per fragment
// costs several thousand Scrubs.
func TestStreamCarry_AdversarialStreamsStayFast(t *testing.T) {
	const size = 32 << 10
	shapes := map[string]string{
		"token run":         "Bearer " + strings.Repeat("A", size),
		"secret ref run":    "secret://" + strings.Repeat("<a>", size/3),
		"open PEM":          "-----BEGIN RSA PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\n\n" + strings.Repeat("QUJDIEFC\n", size/9),
		"repeated prefixes": strings.Repeat("sk-ant- Bearer ", size/15),
		"prose":             strings.Repeat("plain words and more words. ", size/28),
	}
	for name, in := range shapes {
		baseline := fastest(func() { Scrub(in) })
		start := time.Now()
		var c StreamCarry
		for i := 0; i < len(in); i += 7 {
			c.Push(in[i:min(i+7, len(in))])
		}
		c.Flush()
		if elapsed := time.Since(start); elapsed > 200*baseline+100*time.Millisecond {
			t.Errorf("streaming 32 KiB of %s in 7-byte fragments took %v, over 200 times the %v of one Scrub", name, elapsed, baseline)
		}
	}
}

// FuzzStreamCarry checks the carry's invariants on arbitrary input split
// into alternating fragment sizes: unless the cap forced a release, the
// output is the whole stream scrubbed; no piece of valid input splits a
// rune; the hold stays bounded; and Flush empties the carry.
func FuzzStreamCarry(f *testing.F) {
	for _, fx := range prefixFixtures() {
		f.Add("before "+fx.text+" after", uint8(1), uint8(7))
	}
	f.Add("café 日本語 Bearer x", uint8(1), uint8(2))
	f.Fuzz(func(t *testing.T, input string, a, b uint8) {
		sizes := [2]int{1 + int(a), 1 + int(b)}
		var c StreamCarry
		var out strings.Builder
		valid := utf8.ValidString(input)
		emit := func(piece string, _ []string) {
			if valid && !utf8.ValidString(piece) {
				t.Fatalf("piece %q of valid input is not valid UTF-8", piece)
			}
			out.WriteString(piece)
		}
		for i, rest := 0, input; rest != ""; i++ {
			n := min(sizes[i%2], len(rest))
			emit(c.Push(rest[:n]))
			rest = rest[n:]
			if c.held() > streamCarryMax {
				t.Fatalf("carry holds %d bytes, above streamCarryMax", c.held())
			}
		}
		emit(c.Flush())
		if want := Scrub(input); c.forced == 0 && out.String() != want {
			t.Fatalf("released text %q, want %q", out.String(), want)
		}
		if rest, _ := c.Flush(); rest != "" {
			t.Fatalf("second Flush returned %q", rest)
		}
	})
}
