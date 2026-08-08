package provider

import (
	"io"
	"net/http"
	"net/http/httputil"
	"os"
	"sync"
)

// WireTapTransport wraps base with a RoundTripper that dumps every raw,
// UNREDACTED request and response — streaming SSE frames included — to out,
// backing --trace-wire. Nil out and base default to os.Stderr and
// http.DefaultTransport.
//
// Deliberately untagged: this is an ordinary HTTP utility, testable in a
// normal build. The security property lives at the CALL SITE, where the
// factory installs it only under a debug build. See
// docs/security.md#debug-builds.
func WireTapTransport(base http.RoundTripper, out io.Writer) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if out == nil {
		out = os.Stderr
	}
	return &wireTapRoundTripper{base: base, out: out}
}

// wireTapRoundTripper is the RoundTripper WireTapTransport returns.
type wireTapRoundTripper struct {
	base http.RoundTripper
	out  io.Writer

	// mu keeps concurrent in-flight requests from interleaving their dumped
	// frames mid-write. Held per Write, never across a streaming body, so
	// it cannot block a long-lived SSE read.
	mu sync.Mutex
}

func (t *wireTapRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// DumpRequestOut restores req.Body internally, so it is safe ahead of
	// the real RoundTrip, and reproduces the headers http.Transport adds
	// itself (e.g. User-Agent) that a hand-rolled dump would miss.
	if dump, err := httputil.DumpRequestOut(req, true); err == nil {
		t.write("---- REQUEST ----\n")
		t.write(string(dump))
		t.write("\n")
	} else {
		t.write("---- REQUEST (dump failed: " + err.Error() + ") ----\n")
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	// Headers only: DumpResponse(resp, true) would block until the whole
	// body is read, defeating streaming SSE — no tokens would surface until
	// the provider closed the connection. The body is tee'd below instead,
	// dumping each chunk as the real caller reads it.
	if dump, derr := httputil.DumpResponse(resp, false); derr == nil {
		t.write("---- RESPONSE HEADERS ----\n")
		t.write(string(dump))
		t.write("\n")
	} else {
		t.write("---- RESPONSE HEADERS (dump failed: " + derr.Error() + ") ----\n")
	}

	if resp.Body != nil {
		t.write("---- RESPONSE BODY ----\n")
		resp.Body = &teeReadCloser{
			r: io.TeeReader(resp.Body, &wireTapWriter{tap: t}),
			c: resp.Body,
		}
	}

	return resp, nil
}

// write is the single serialisation point for anything the tap sends to
// out.
func (t *wireTapRoundTripper) write(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = io.WriteString(t.out, s)
}

// wireTapWriter adapts wireTapRoundTripper.write to io.Writer so it can
// back an io.TeeReader over a streaming response body.
type wireTapWriter struct {
	tap *wireTapRoundTripper
}

func (w *wireTapWriter) Write(p []byte) (int, error) {
	w.tap.write(string(p))
	return len(p), nil
}

// teeReadCloser pairs an io.Reader (the TeeReader wrapping the real
// body) with the original body's Close, so the caller's normal
// `defer resp.Body.Close()` still releases the underlying connection.
type teeReadCloser struct {
	r io.Reader
	c io.Closer
}

func (t *teeReadCloser) Read(p []byte) (int, error) { return t.r.Read(p) }
func (t *teeReadCloser) Close() error               { return t.c.Close() }
