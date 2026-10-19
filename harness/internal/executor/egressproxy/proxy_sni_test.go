package egressproxy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProxy_CONNECT_SNIAbsent_IsDropped: a client that CONNECTs to an
// allowlisted host but suppresses the SNI extension on the inner
// ClientHello must be dropped, not allowed through.
func TestProxy_CONNECT_SNIAbsent_IsDropped(t *testing.T) {
	upstream := startEchoTCPServer(t)
	upstreamHost, upstreamPort := splitHostPort(t, upstream.Addr().String())

	emitter := &fakeEmitter{}
	allowEntry := fmt.Sprintf("%s:%d", upstreamHost, upstreamPort)
	p := startTestProxy(t, []string{allowEntry}, emitter)

	conn := dialConnectTunnel(t, p.Addr(), upstreamHost, upstreamPort)

	// Inner ClientHello has no SNI extension — the proxy must deny.
	hello := minimalClientHelloNoSNI()
	if _, err := conn.Write(hello); err != nil {
		t.Logf("write hello: %v", err)
	}

	// The protocol-compliant ordering writes the 200 before the proxy can
	// inspect the (absent) SNI, so a 200 may appear on the wire. The security
	// guarantee is NOT "no 200" — it is that the sni_absent deny fires (it
	// returns before p.dialer is ever called, so no upstream connection is
	// opened) and the hijacked tunnel is then dropped. Assert exactly that.
	awaitDeny(t, emitter, "sni_absent")

	// The proxy must drop the connection rather than splice it to an upstream;
	// after draining the optional 200 the read hits EOF (no echoed bytes).
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)), "set read deadline")
	got := readAllUntilEOF(t, conn)
	require.NotContains(t, string(got), string(hello), "absent-SNI tunnel echoed client bytes back, so the upstream was reached")
}

// TestProxy_CONNECT_CompliantClient_Succeeds: an RFC 7231 §4.3.6 client
// reads the proxy's 2xx response before sending any tunnel bytes (curl,
// browsers, Go net/http, git all do this), so the proxy must write the
// 200 before peeking the ClientHello.
func TestProxy_CONNECT_CompliantClient_Succeeds(t *testing.T) {
	upstream := startEchoTCPServer(t)
	upstreamHost, upstreamPort := splitHostPort(t, upstream.Addr().String())

	emitter := &fakeEmitter{}
	allowEntry := fmt.Sprintf("%s:%d", upstreamHost, upstreamPort)
	p := startTestProxy(t, []string{allowEntry}, emitter)

	conn := dialConnectTunnel(t, p.Addr(), upstreamHost, upstreamPort)
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)), "set deadline")

	// Compliant ordering: read the CONNECT response first, only then send
	// the ClientHello.
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
	require.NoError(t, err, "read CONNECT response")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	hello := clientHelloWithSNI(upstreamHost)
	_, err = conn.Write(hello)
	require.NoError(t, err, "write hello after 200")

	// The proxy replays the ClientHello to the echo upstream, which echoes it
	// back through the spliced tunnel — proof the tunnel is live end to end.
	got := make([]byte, len(hello))
	_, err = io.ReadFull(r, got)
	require.NoError(t, err, "read echoed bytes through tunnel")
	assert.Equal(t, hello, got, "upstream echo mismatch through compliant-client tunnel")

	assert.Eventually(t, func() bool {
		return emitter.hasEvent("egress_allowed")
	}, time.Second, 20*time.Millisecond, "expected egress_allowed event for the compliant CONNECT")
}

// TestProxy_TLSParseFailed_DropsConnection: a non-handshake first byte
// after CONNECT (TLS Application Data 0x17 instead of Handshake 0x16)
// must produce a tls_parse_failed deny without hanging.
func TestProxy_TLSParseFailed_DropsConnection(t *testing.T) {
	upstream := startEchoTCPServer(t)
	upstreamHost, upstreamPort := splitHostPort(t, upstream.Addr().String())

	emitter := &fakeEmitter{}
	allowEntry := fmt.Sprintf("%s:%d", upstreamHost, upstreamPort)
	p := startTestProxy(t, []string{allowEntry}, emitter)

	conn := dialConnectTunnel(t, p.Addr(), upstreamHost, upstreamPort)

	// Write a 5-byte TLS record header announcing application_data
	// (type 0x17) plus a few bytes of body. This is well-formed at
	// the TCP layer but invalid as a handshake first record.
	bogus := []byte{0x17, 0x03, 0x03, 0x00, 0x05, 0xde, 0xad, 0xbe, 0xef, 0x00}
	if _, err := conn.Write(bogus); err != nil {
		t.Logf("write bogus: %v", err)
	}

	awaitDeny(t, emitter, "tls_parse_failed")
}

// TestProxy_OversizedClientHello_Rejected: a TLS record header
// announcing a length over maxClientHello must be rejected without
// allocating a buffer that size.
func TestProxy_OversizedClientHello_Rejected(t *testing.T) {
	upstream := startEchoTCPServer(t)
	upstreamHost, upstreamPort := splitHostPort(t, upstream.Addr().String())

	emitter := &fakeEmitter{}
	allowEntry := fmt.Sprintf("%s:%d", upstreamHost, upstreamPort)
	p := startTestProxy(t, []string{allowEntry}, emitter)

	conn := dialConnectTunnel(t, p.Addr(), upstreamHost, upstreamPort)

	// Record header for type=handshake, length=0xFFFF (65535) which
	// is well over the 16640-byte cap.
	hdr := []byte{0x16, 0x03, 0x03, 0xff, 0xff}
	if _, err := conn.Write(hdr); err != nil {
		t.Logf("write oversized header: %v", err)
	}

	awaitDeny(t, emitter, "tls_parse_failed")
}

// TestParseSNIFromHandshake_MalformedInputs covers each truncation and
// overrun return in parseSNIFromHandshake (sni.go). Every case must fail.
func TestParseSNIFromHandshake_MalformedInputs(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{
			name: "empty",
			body: []byte{},
		},
		{
			name: "body too short",
			body: []byte{0x01, 0x00, 0x00},
		},
		{
			name: "wrong msg type",
			body: append([]byte{0x02 /* not ClientHello */, 0x00, 0x00, 0x00}, make([]byte, 60)...),
		},
		{
			name: "header truncated",
			body: append([]byte{0x01, 0x00, 0x00, 0x00}, make([]byte, 30)...),
		},
		{
			// session_id length 255 with no bytes following.
			name: "session_id length present, bytes overrun",
			body: append(clientHelloHead(), 0xff),
		},
		{
			// Empty session_id, and only one byte of the 2-byte cipher_suites length.
			name: "cipher_suites length truncated",
			body: append(clientHelloHead(), 0x00, 0x00),
		},
		{
			// Empty session_id, cipher_suites length 65535 with no bytes following.
			name: "cipher_suites overrun",
			body: append(clientHelloHead(), 0x00, 0xff, 0xff),
		},
		{
			// Empty session_id and cipher_suites, no compression_methods length.
			name: "compression_methods length missing",
			body: append(clientHelloHead(), 0x00, 0x00, 0x00),
		},
		{
			// compression_methods length 255 with no bytes following.
			name: "compression_methods overrun",
			body: append(clientHelloHead(), 0x00, 0x00, 0x00, 0xff),
		},
		{
			// Only one byte of the 2-byte extensions length.
			name: "extensions length truncated",
			body: append(helloBeforeExtensions(), 0x00),
		},
		{
			// extensions length 65535 with no bytes following.
			name: "extensions overrun",
			body: append(helloBeforeExtensions(), 0xff, 0xff),
		},
		{
			// The extensions block holds 4 bytes: an extension header claiming
			// server_name with length 65535.
			name: "extension length overrun",
			body: append(helloBeforeExtensions(), 0x00, 0x04, 0x00, 0x00, 0xff, 0xff),
		},
		{
			// server_name list length 65535 inside a 2-byte extension body.
			name: "server_name list overrun",
			body: append(helloBeforeExtensions(), 0x00, 0x06, 0x00, 0x00, 0x00, 0x02, 0xff, 0xff),
		},
		{
			// The server_name list claims 4 bytes, but its single host_name
			// entry claims a 1024-byte name with only one byte present.
			name: "server_name entry overrun",
			body: append(helloBeforeExtensions(), 0x00, 0x0a, 0x00, 0x00, 0x00, 0x06, 0x00, 0x04, 0x00, 0x04, 0x00, 0x00),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSNIFromHandshake(tc.body)
			require.Error(t, err)
		})
	}
}

// clientHelloHead returns a handshake message header, a TLS 1.2 version and
// 32 bytes of random. Malformed cases extend it to reach one parser branch.
func clientHelloHead() []byte {
	head := []byte{0x01, 0x00, 0x00, 0x00, 0x03, 0x03}
	return append(head, make([]byte, 32)...)
}

// helloBeforeExtensions extends clientHelloHead with an empty session_id,
// empty cipher_suites and a single null compression method, so the next
// bytes are the extensions length.
func helloBeforeExtensions() []byte {
	return append(clientHelloHead(), 0x00, 0x00, 0x00, 0x01, 0x00)
}

// dialConnectTunnel dials the proxy and sends a CONNECT for
// upstreamHost:upstreamPort. The connection is closed when the test ends.
func dialConnectTunnel(t *testing.T, proxyAddr, upstreamHost string, upstreamPort int) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	require.NoError(t, err, "dial proxy")
	t.Cleanup(func() { _ = conn.Close() })

	connectLine := fmt.Sprintf("CONNECT %s:%d HTTP/1.1\r\nHost: %s:%d\r\n\r\n", upstreamHost, upstreamPort, upstreamHost, upstreamPort)
	_, err = conn.Write([]byte(connectLine))
	require.NoError(t, err, "write CONNECT")
	return conn
}

// awaitDeny polls the emitter for an egress_blocked event with the
// given reason.
func awaitDeny(t *testing.T, emitter *fakeEmitter, reason string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range emitter.snapshot() {
			if ev.Event == "egress_blocked" {
				if got, ok := ev.Data["reason"].(string); ok && got == reason {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.Failf(t, "egress_blocked event not observed", "reason %q; events: %+v", reason, emitter.snapshot())
}

// readAllUntilEOF reads from conn until EOF or the existing read
// deadline trips, so a deny assertion isn't fooled by partial reads of
// buffered bytes.
func readAllUntilEOF(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	var out bytes.Buffer
	buf := make([]byte, 256)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if err != nil {
			break
		}
		if out.Len() > 4096 {
			break
		}
	}
	return out.Bytes()
}
