package security

import (
	"math/rand"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// streamThroughCarry feeds fragments through a StreamCarry, scrubs each
// released piece on its own as a transport does, and returns the joined
// output and the raw pieces.
func streamThroughCarry(fragments []string) (string, []string) {
	var c StreamCarry
	var pieces []string
	for _, f := range fragments {
		if out := c.Push(f); out != "" {
			pieces = append(pieces, out)
		}
	}
	if out := c.Flush(); out != "" {
		pieces = append(pieces, out)
	}
	var b strings.Builder
	for _, p := range pieces {
		b.WriteString(Scrub(p))
	}
	return b.String(), pieces
}

func streamFixtures() []secretFixture {
	encrypted := "-----BEGIN RSA PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,0123456789ABCDEF\n\n" +
		strings.Join(pemBodyLines(12), "\n") + "\n-----END RSA PRIVATE KEY-----"
	return append(append(secretFixtures(), multilineSecretFixtures()...),
		secretFixture{"pem_private_key", rsa4096PEMFixture(), pemBodyLines(51)[50]},
		secretFixture{"pem_private_key", encrypted, pemBodyLines(12)[11]},
	)
}

// TestStreamCarry_SecretSplitAcrossTwoFragmentsIsScrubbed is the carry's
// central guarantee: whichever byte a secret is split at, scrubbing the
// released pieces one by one gives the same output as scrubbing the whole
// stream.
func TestStreamCarry_SecretSplitAcrossTwoFragmentsIsScrubbed(t *testing.T) {
	for _, f := range streamFixtures() {
		input := "The model wrote this out: " + f.text + " and then went on with ordinary prose."
		start := strings.Index(input, f.text)
		want := Scrub(input)
		// Every byte of a short secret; a stride through a long one.
		step := max(1, len(f.text)/256)
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

// TestStreamCarry_RandomFragmentsMatchWholeStreamScrub searches layouts
// the two-fragment case does not: several secrets, small and uneven
// fragments, and secrets back to back.
func TestStreamCarry_RandomFragmentsMatchWholeStreamScrub(t *testing.T) {
	rng := rand.New(rand.NewSource(5)) //nolint:gosec // deterministic test input, not a security decision
	fixtures := streamFixtures()
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
		if strings.Join(pieces, "") != input {
			t.Fatalf("seed %d: released pieces do not rebuild the stream", seed)
		}
		for _, p := range pieces {
			if !utf8.ValidString(p) {
				t.Fatalf("seed %d: piece %q splits a rune", seed, p)
			}
		}
	}
}

// TestStreamCarry_ReleasesProseBehindTheTail bounds the latency the carry
// adds to ordinary text: everything but the tail and the word being
// written is released as it arrives.
func TestStreamCarry_ReleasesProseBehindTheTail(t *testing.T) {
	var c StreamCarry
	released := 0
	prose := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 40)
	for i := 0; i < len(prose); i += 5 {
		released += len(c.Push(prose[i:min(i+5, len(prose))]))
		if held := i + 5 - released; i+5 <= len(prose) && held > StreamCarryTail+len("jumps ")+5 {
			t.Fatalf("after %d bytes the carry holds %d, want at most the tail and one word", i+5, held)
		}
	}
	if rest := c.Flush(); released+len(rest) != len(prose) {
		t.Fatalf("released %d + flushed %d bytes, want %d", released, len(rest), len(prose))
	}
}

// TestStreamCarry_HoldIsBounded pins StreamCarryMax: a run no pattern ends
// is released once it outgrows the bound, so a stream cannot be stalled.
func TestStreamCarry_HoldIsBounded(t *testing.T) {
	var c StreamCarry
	run := "Bearer " + strings.Repeat("a", 4*StreamCarryMax)
	released := 0
	for i := 0; i < len(run); i += 7 {
		released += len(c.Push(run[i:min(i+7, len(run))]))
		if held := min(i+7, len(run)) - released; held > StreamCarryMax {
			t.Fatalf("carry holds %d bytes, above StreamCarryMax", held)
		}
	}
	if released == 0 {
		t.Fatal("nothing was released from a run four times StreamCarryMax")
	}
}

// TestStreamCarry_AdversarialStreamsStayFast pins that streaming stays
// linear when a span is held for a long time. The bound is relative to one
// whole-stream Scrub of the same input, so it holds under the race
// detector and on slow machines; an unbounded hold rescanned per fragment
// costs several thousand Scrubs.
func TestStreamCarry_AdversarialStreamsStayFast(t *testing.T) {
	const size = 100 << 10
	shapes := map[string]string{
		"token run":         strings.Repeat("A", size),
		"secret ref run":    "secret://" + strings.Repeat("<a>", size/3),
		"open PEM":          "-----BEGIN RSA PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\n" + strings.Repeat("QUJD IEFC\n", size/10),
		"repeated prefixes": strings.Repeat("sk-ant- Bearer ", size/15),
		"prose":             strings.Repeat("plain words and more words. ", size/28),
	}
	for name, in := range shapes {
		baseline := time.Duration(1<<63 - 1)
		for range 3 {
			start := time.Now()
			Scrub(in)
			baseline = min(baseline, time.Since(start))
		}
		start := time.Now()
		var c StreamCarry
		for i := 0; i < len(in); i += 7 {
			c.Push(in[i:min(i+7, len(in))])
		}
		c.Flush()
		if elapsed := time.Since(start); elapsed > 100*baseline+100*time.Millisecond {
			t.Errorf("streaming 100 KiB of %s in 7-byte fragments took %v, over 100 times the %v of one Scrub", name, elapsed, baseline)
		}
	}
}
