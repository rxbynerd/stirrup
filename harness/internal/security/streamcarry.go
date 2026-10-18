package security

import (
	"bytes"
	"unicode/utf8"
)

const (
	// StreamCarryTail is the number of trailing bytes a StreamCarry always
	// holds back. It exceeds the longest text that can begin a match
	// without yet being one: a key-name pattern's name, separator and
	// quote, followed by one byte fewer than its fixed-length value.
	StreamCarryTail = 96

	// StreamCarryMax bounds the bytes a StreamCarry holds. Past it the
	// oldest bytes are released even when a match could still cross them,
	// so a span longer than this is scrubbed in pieces and its tail
	// reaches the sink.
	StreamCarryMax = 8 << 10

	// streamCarryRescanBytes is the held size above which the carry
	// rescans only once the held text has grown by a quarter, so a long
	// held span costs amortised linear time rather than a full scan per
	// fragment.
	streamCarryRescanBytes = 256
)

// StreamCarry reassembles a stream of text fragments into pieces that a
// secret pattern cannot match across, so a sink that scrubs each piece on
// its own still redacts a secret split between two fragments.
//
// It holds back the last StreamCarryTail bytes, the trailing run of token
// characters, and every match that crosses the cut or could still grow: one
// touching the end of the held text, or a PEM header whose END line has not
// arrived. Flush releases the rest and must be called wherever the stream
// ends. A StreamCarry is not safe for concurrent use.
type StreamCarry struct {
	pending   []byte
	unscanned int
}

// Push adds fragment and returns the text that can be released.
func (c *StreamCarry) Push(fragment string) string {
	c.pending = append(c.pending, fragment...)
	c.unscanned += len(fragment)
	var out []byte
	if over := len(c.pending) - StreamCarryMax; over > 0 {
		out = c.take(runeStartAtOrBefore(c.pending, over))
	}
	if len(c.pending) <= streamCarryRescanBytes || c.unscanned >= len(c.pending)/4 {
		c.unscanned = 0
		out = append(out, c.take(streamCarryCut(c.pending))...)
	}
	return string(out)
}

// Flush returns the held text and empties the carry.
func (c *StreamCarry) Flush() string {
	out := string(c.pending)
	c.pending, c.unscanned = c.pending[:0], 0
	return out
}

func (c *StreamCarry) take(n int) []byte {
	if n <= 0 {
		return nil
	}
	out := bytes.Clone(c.pending[:n])
	c.pending = append(c.pending[:0], c.pending[n:]...)
	return out
}

func streamCarryCut(p []byte) int {
	cut := len(p) - StreamCarryTail
	if run := tokenRunStart(p); run < cut {
		cut = run
	}
	if cut <= 0 {
		return 0
	}
	return runeStartAtOrBefore(p, heldMatchStart(p, cut))
}

func runeStartAtOrBefore(p []byte, i int) int {
	for i > 0 && i < len(p) && !utf8.RuneStart(p[i]) {
		i--
	}
	return i
}

// tokenRunStart returns where the trailing run of the characters that the
// patterns' open-ended values are built from begins.
func tokenRunStart(p []byte) int {
	i := len(p)
	for i > 0 && isTokenByte(p[i-1]) {
		i--
	}
	return i
}

func isTokenByte(b byte) bool {
	switch {
	case 'a' <= b && b <= 'z', 'A' <= b && b <= 'Z', '0' <= b && b <= '9':
		return true
	}
	return bytes.IndexByte([]byte("._~+/=-"), b) >= 0
}

// heldMatchStart moves cut back to the start of every match that crosses
// it. A cut is always short of the end, so this holds every match that
// touches the end too, and an open PEM header is treated as reaching it.
// Validators are not consulted: a match they would reject is held all the
// same.
func heldMatchStart(p []byte, cut int) int {
	var held [][]int
	for _, pat := range secretPatterns {
		for _, m := range pat.re.FindAllIndex(p, -1) {
			if pat.name == "pem_private_key" && !bytes.Contains(p[m[0]:m[1]], []byte("-----END")) {
				m[1] = len(p)
			}
			held = append(held, m)
		}
	}
	for moved := true; moved; {
		moved = false
		for _, m := range held {
			if m[0] < cut && m[1] > cut {
				cut = m[0]
				moved = true
			}
		}
	}
	return cut
}
