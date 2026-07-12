package provider

import (
	"io"
	"net/http"
	"net/http/httputil"
	"os"
	"sync"
)

// WireTapTransport wraps base with a RoundTripper that dumps every raw,
// UNREDACTED HTTP request and response — including streaming SSE frames
// as they arrive over the wire — to out. It exists solely to back
// --trace-wire (issue #220).
//
// This function itself carries no build tag: it is an ordinary,
// always-compiled HTTP utility, safe to unit test in a normal build.
// The security property lives at the CALL SITE — harness/internal/core's
// factory only installs it when both the --trace-wire flag was set AND
// debugbuild.DebugBuildEnabled() is true, so a release binary never
// wires a WireTapTransport into a live provider client. See
// docs/security.md#debug-builds.
//
// out defaults to os.Stderr when nil. base defaults to
// http.DefaultTransport when nil (mirroring http.Client's own default).
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

	// mu serialises individual writes to out so concurrent in-flight
	// requests (e.g. a retry racing a follow-up call) cannot interleave
	// their dumped frames mid-write. It is held only for the duration of
	// a single Write, never across a streaming response body, so it
	// never blocks a long-lived SSE read.
	mu sync.Mutex
}

func (t *wireTapRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// DumpRequestOut drains and restores req.Body internally (via
	// drainBody), so it is safe to call before the real RoundTrip
	// consumes the body. It also reproduces headers http.Transport adds
	// itself (e.g. User-Agent), which a hand-rolled dump would miss.
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

	// Response headers are dumped immediately via DumpResponse with
	// body=false (a header-only dump does not drain resp.Body, so this
	// costs nothing and cannot block). The body itself is NOT dumped via
	// DumpResponse(resp, true): that call blocks until the entire body is
	// read, which would defeat streaming SSE — the caller would see no
	// tokens until the provider closed the connection. Instead the body
	// is tee'd below so each chunk is dumped exactly when the real
	// caller reads it, preserving live streaming behaviour.
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
