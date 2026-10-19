package security

import (
	"fmt"
	"math/rand"
	"regexp/syntax"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
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

// streamThrough is streamThroughCarry on a caller's carry, so a test can
// read its counters.
func streamThrough(c *StreamCarry, fragments []string) string {
	var b strings.Builder
	for _, f := range fragments {
		out, _ := c.Push(f)
		b.WriteString(out)
	}
	out, _ := c.Flush()
	b.WriteString(out)
	return b.String()
}

// clip shortens s for a failure message.
func clip(s string) string {
	if len(s) > 120 {
		return s[:60] + "…" + s[len(s)-60:]
	}
	return s
}

// secretWindow is the length of the substrings assertNoSecretWindows looks
// for: long enough that one does not recur in plain text by chance.
const secretWindow = 16

// assertNoSecretWindows fails when got holds a secretWindow-byte substring
// of text whole-stream Scrub redacts from input that whole-stream Scrub
// does not also release elsewhere.
func assertNoSecretWindows(t *testing.T, input, got string) {
	t.Helper()
	whole, ranges, _ := ScrubWithSpans(input)
	windows := func(s string) map[string]bool {
		set := map[string]bool{}
		for i := 0; i+secretWindow <= len(s); i++ {
			set[s[i:i+secretWindow]] = true
		}
		return set
	}
	released, allowed := windows(got), windows(whole)
	for _, r := range ranges {
		for i := r.Start; i+secretWindow <= r.End; i++ {
			if w := input[i : i+secretWindow]; released[w] && !allowed[w] {
				t.Fatalf("released text holds %q, redacted at offset %d of the input", w, i)
			}
		}
	}
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
		secretFixture{"pem_private_key", "-----BEGIN PGP PRIVATE KEY BLOCK-----\nVersion: 1\n\n" + strings.Join(pemBodyLines(4), "\n") + "\n=ABCD\n-----END PGP PRIVATE KEY BLOCK-----", pemBodyLines(4)[3]},
		secretFixture{"pem_private_key", "a.pem:1:-----BEGIN EC PRIVATE KEY-----\na.pem:2:" + strings.Join(pemBodyLines(3), "\na.pem:3:") + "\na.pem:5:-----END EC PRIVATE KEY-----", pemBodyLines(3)[2]},
		secretFixture{"pem_private_key", `"-----BEGIN EC PRIVATE KEY-----\n" +` + "\n\t\"" + strings.Join(pemBodyLines(3), `\n" + "`) + `\n"`, pemBodyLines(3)[2]},
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
// The check reads each pattern's possible first characters, so it covers
// text no fixture holds.
func TestStreamCarry_SentinelNeverBeginsAMatch(t *testing.T) {
	for i, p := range secretPatterns {
		re, err := syntax.Parse(p.re.String(), syntax.Perl)
		if err != nil {
			t.Fatal(err)
		}
		if first := firstRunes(re.Simplify()); first == nil || slices.ContainsFunc(first, func(r [2]rune) bool { return r[0] <= '_' && '_' <= r[1] }) {
			t.Errorf("%s can begin with %q", p.name, '_')
		}
		for _, f := range prefixFixtures() {
			in := "_" + f.text
			if loc := streamPrefixes[i].FindStringIndex(in); loc != nil && loc[0] == 0 {
				t.Errorf("%s prefix matches from the sentinel in %q", p.name, in)
			}
		}
	}
}

// firstRunes returns ranges holding every rune a match of re can begin
// with, or nil when re can match the empty string or its first rune
// cannot be bounded.
func firstRunes(re *syntax.Regexp) [][2]rune {
	fold := func(lo, hi rune) [][2]rune {
		out := [][2]rune{{lo, hi}}
		for r := lo; r <= hi && r < 0x80; r++ {
			if u := unicode.SimpleFold(r); u != r {
				out = append(out, [2]rune{u, u})
			}
		}
		return out
	}
	switch re.Op {
	case syntax.OpLiteral:
		if len(re.Rune) == 0 {
			return nil
		}
		if re.Flags&syntax.FoldCase != 0 {
			return fold(re.Rune[0], re.Rune[0])
		}
		return [][2]rune{{re.Rune[0], re.Rune[0]}}
	case syntax.OpCharClass:
		var out [][2]rune
		for j := 0; j+1 < len(re.Rune); j += 2 {
			out = append(out, [2]rune{re.Rune[j], re.Rune[j+1]})
		}
		return out
	case syntax.OpCapture, syntax.OpPlus:
		return firstRunes(re.Sub[0])
	case syntax.OpRepeat:
		if re.Min == 0 {
			return nil
		}
		return firstRunes(re.Sub[0])
	case syntax.OpConcat:
		for _, sub := range re.Sub {
			switch sub.Op {
			case syntax.OpWordBoundary, syntax.OpNoWordBoundary, syntax.OpBeginLine, syntax.OpBeginText:
				continue
			}
			return firstRunes(sub)
		}
		return nil
	case syntax.OpAlternate:
		var out [][2]rune
		for _, sub := range re.Sub {
			f := firstRunes(sub)
			if f == nil {
				return nil
			}
			out = append(out, f...)
		}
		return out
	default:
		return nil
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

// TestStreamCarry_InterleavedPatternsMatchWholeStreamScrub pins the carry
// against pattern order: text a later pattern matches is held while an
// earlier pattern's placeholder hides it, since later text can remove that
// placeholder and expose the later match.
func TestStreamCarry_InterleavedPatternsMatchWholeStreamScrub(t *testing.T) {
	atoms := []string{"ya29.", "Bearer ", "Basic ", "AKIA", "ABCDEFGHIJKLMNOP", "sk-", "sk-ant-", "secret://", "eyJ", ".", " ", "\n",
		"-----BEGIN ", "RSA PRIVATE KEY-----\n", "-----END RSA PRIVATE KEY-----", "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo=", "x", "_",
		"ghp_", "token=", "0123456789abcdef0123456789abcdef", "api-key: ", "xoxb-", "AIza", "=", "\"", ":"}
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // deterministic test input, not a security decision
	for seed := 0; seed < 6000; seed++ {
		var b strings.Builder
		for n := 2 + rng.Intn(8); n > 0; n-- {
			b.WriteString(atoms[rng.Intn(len(atoms))])
		}
		input := b.String()
		var fragments []string
		for rest := input; rest != ""; {
			n := min(1+rng.Intn(6), len(rest))
			fragments = append(fragments, rest[:n])
			rest = rest[n:]
		}
		if got, _ := streamThroughCarry(fragments); got != Scrub(input) {
			t.Fatalf("seed %d: %q streamed as %q, want %q", seed, input, got, Scrub(input))
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

// TestStreamCarry_SpanOverCapIsRedactedWhole pins that a match longer
// than streamCarryMax is redacted through its end: the forced release sends
// its head as a placeholder, and the rest is withheld until the match ends.
func TestStreamCarry_SpanOverCapIsRedactedWhole(t *testing.T) {
	body := pemBodyLines(200)
	for i := range body {
		body[i] = fmt.Sprintf("Z%03d", i) + body[i][4:]
	}
	input := "key: " + pemKey("RSA PRIVATE KEY", body, "\n") + "\nafter"
	for _, size := range []int{1, 7, 50, 500, 3000} {
		var c StreamCarry
		got := streamThrough(&c, chunk(input, size))
		assertNoPEMBody(t, got, body)
		if want := Scrub(input); got != want {
			t.Fatalf("%d-byte fragments: streamed output %q, want %q", size, got, want)
		}
		if c.forced == 0 {
			t.Fatalf("%d-byte fragments: the cap forced no release", size)
		}
	}
}

// TestStreamCarry_UnmatchedHeadOverCapIsWithheld pins that text the cap
// forces out while it could still begin a match goes out as a placeholder:
// a JWT whose header and payload pass streamCarryMax before the second dot
// arrives has no match yet, and none of it may reach the sink.
func TestStreamCarry_UnmatchedHeadOverCapIsWithheld(t *testing.T) {
	payload := strings.Repeat("eyJzdWIiOiJydW5uZXIi", 9<<10/20)
	jwt := "eyJhbGciOiJSUzI1NiJ9." + payload + ".c2lnbmF0dXJl"
	cases := map[string]string{
		"jwt":              "token: " + jwt + " end",
		"bearer, no dots":  "auth: Bearer x" + strings.Repeat("eyJhbGciOiJSUzI1NiJ9", 9<<10/20) + " end",
		"jwt behind prose": strings.Repeat("words ", 1300) + jwt + " end",
	}
	for name, input := range cases {
		for _, size := range []int{1, 7, 500, 4093} {
			var c StreamCarry
			got := streamThrough(&c, chunk(input, size))
			assertNoSecretWindows(t, input, got)
			if want := Scrub(input); got != want {
				t.Fatalf("%s, %d-byte fragments: streamed output %q, want %q", name, size, clip(got), clip(want))
			}
		}
	}
}

// TestStreamCarry_RunPastTheCapIsBounded pins the two bounds on a match the
// cap cut: only its head is held once the cap forces a release, and past
// streamCarryRunMax nothing more is released until Flush.
func TestStreamCarry_RunPastTheCapIsBounded(t *testing.T) {
	var c StreamCarry
	run := "Bearer " + strings.Repeat("a", 4*streamCarryMax)
	got := ""
	for i := 0; i < len(run); i += 7 {
		out, _ := c.Push(run[i:min(i+7, len(run))])
		got += out
		if !c.run && c.held() > streamCarryMax {
			t.Fatalf("carry holds %d bytes, above streamCarryMax", c.held())
		}
	}
	if got != "[REDACTED]" {
		t.Fatalf("released %q from a run four times streamCarryMax, want one placeholder", clip(got))
	}
	for i := 0; i < streamCarryRunMax; i += 4 << 10 {
		if out, _ := c.Push(strings.Repeat("a", 4<<10)); out != "" {
			t.Fatalf("released %q from an open run", clip(out))
		}
	}
	c.Push(" then prose")
	if !c.drain || len(c.pending) != 0 {
		t.Fatalf("a run past streamCarryRunMax still holds %d bytes", len(c.pending))
	}
	if out, _ := c.Flush(); out != "" {
		t.Fatalf("Flush after a drained run released %q", clip(out))
	}
	if out, _ := c.Push("next stream"); out != "next stream" {
		t.Fatalf("carry after Flush released %q", out)
	}
}

// TestStreamCarry_ForcedRescanIsAmortised pins that a match that keeps
// repeating its own leading literal past the cap is not rescanned in full
// for every fragment.
func TestStreamCarry_ForcedRescanIsAmortised(t *testing.T) {
	for name, run := range map[string]string{
		"openai keys": strings.Repeat("sk-", 10000),
		"secret refs": strings.Repeat("secret://a/", 3000),
	} {
		var c StreamCarry
		for i := range len(run) {
			c.Push(run[i : i+1])
		}
		c.Flush()
		if c.scanned > 16*len(run) {
			t.Errorf("%s: carry scanned %d bytes for a %d byte stream, over 16 times its length", name, c.scanned, len(run))
		}
	}
}

// TestStreamCarry_RuneStraddlingForcedCut pins that the release forced at
// the cap never splits a multi-byte rune, even when fragments do.
func TestStreamCarry_RuneStraddlingForcedCut(t *testing.T) {
	input := "-----BEGIN A KEY-----\nComment: " + strings.Repeat("日本語", 4<<10) + " done"
	for _, size := range []int{1, 2, 5} {
		got, pieces := streamThroughCarry(chunk(input, size))
		if !strings.HasPrefix(got, "[REDACTED]") {
			t.Fatalf("%d-byte fragments: output starts %q, want the redacted head", size, got[:20])
		}
		for _, p := range pieces {
			if !utf8.ValidString(p) {
				t.Fatalf("%d-byte fragments: piece splits a rune: %q", size, p[:min(len(p), 16)])
			}
		}
	}
}

// TestStreamCarry_WhitespaceAfterKeyword pins that whitespace inside a
// match is held like the rest of it: a secret split after a whitespace run
// is redacted whether or not the keyword and whitespace fit in
// streamCarryMax.
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
			if strings.Contains(got, tc.sensitive) {
				t.Errorf("%s followed by %d spaces: secret leaked", tc.keyword, ws)
			}
			if want := Scrub(head + tc.secret + " tail"); got != want {
				t.Errorf("%s followed by %d spaces: streamed output %q, want %q", tc.keyword, ws, clip(got), clip(want))
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

	// Many short tokens, each held while it streams and released when it
	// ends, below the size at which the quarter-growth rule applies.
	hovering := strings.Repeat("Bearer "+strings.Repeat("Ab0", 64)+" then ", 64)
	c = StreamCarry{}
	for i := 0; i < len(hovering); i += 3 {
		c.Push(hovering[i:min(i+3, len(hovering))])
	}
	c.Flush()
	if c.scanned > 16*len(hovering) {
		t.Errorf("carry scanned %d bytes for %d bytes of short tokens, over 16 times their length", c.scanned, len(hovering))
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

// FuzzStreamCarry checks the carry's invariants on head followed by reps
// copies of body, split into alternating fragment sizes: no raw text
// whole-stream Scrub redacts is released; unless the cap forced a release,
// the output is the whole stream scrubbed; no piece of valid input splits
// a rune; the hold stays bounded; and Flush empties the carry.
func FuzzStreamCarry(f *testing.F) {
	for _, fx := range prefixFixtures() {
		f.Add("before "+fx.text+" after", "", uint16(0), uint8(1), uint8(7))
	}
	f.Add("café 日本語 Bearer x", "", uint16(0), uint8(1), uint8(2))
	f.Add("-ya29.Bearer AKIAABCDEFGHIJKLMNOP", " tail", uint16(1), uint8(0), uint8(3))
	f.Add("ya29.api-key: sk-ant-RSA PRIVATE KEY-----\n", "", uint16(0), uint8(2), uint8(4))
	f.Add("eyJya29.secret://Bearer ghp_", "", uint16(0), uint8(0), uint8(5))
	f.Add("token: eyJhbGciOiJSUzI1NiJ9.", "eyJzdWIiOiJydW5uZXIi", uint16(500), uint8(6), uint8(200))
	f.Add("", "sk-", uint16(4000), uint8(0), uint8(0))
	f.Add("", "secret://a/", uint16(1000), uint8(2), uint8(0))
	f.Add("Bearer ", " ", uint16(9000), uint8(63), uint8(0))
	f.Fuzz(func(t *testing.T, head, body string, reps uint16, a, b uint8) {
		input := head + strings.Repeat(body, min(int(reps), (64<<10)/max(len(body), 1)))
		sizes := [2]int{1 + int(a), 1 + int(b)}
		var c StreamCarry
		var out strings.Builder
		valid := utf8.ValidString(input)
		emit := func(piece string, _ []string) {
			if valid && !utf8.ValidString(piece) {
				t.Fatalf("piece %q of valid input is not valid UTF-8", clip(piece))
			}
			out.WriteString(piece)
		}
		for i, rest := 0, input; rest != ""; i++ {
			n := min(sizes[i%2], len(rest))
			emit(c.Push(rest[:n]))
			rest = rest[n:]
			if !c.run && c.held() > streamCarryMax {
				t.Fatalf("carry holds %d bytes, above streamCarryMax", c.held())
			}
		}
		emit(c.Flush())
		assertNoSecretWindows(t, input, out.String())
		if want := Scrub(input); c.forced == 0 && out.String() != want {
			t.Fatalf("released text %q, want %q", clip(out.String()), clip(want))
		}
		if rest, _ := c.Flush(); rest != "" {
			t.Fatalf("second Flush returned %q", rest)
		}
	})
}
