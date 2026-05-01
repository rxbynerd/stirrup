package egressproxy

import (
	"bytes"
	"fmt"
	"net"
	"testing"
	"time"
)

// TestProxy_CONNECT_SNIAbsent_IsDropped exercises M2: a client that
// CONNECTs to an allowlisted host but suppresses the SNI extension on
// the inner ClientHello must be dropped, not allowed through. Without
// this, a tampered binary could tunnel arbitrary TLS to an allowlisted
// IP under cover of the CONNECT cross-check.
func TestProxy_CONNECT_SNIAbsent_IsDropped(t *testing.T) {
	upstream := startEchoTCPServer(t)
	upstreamHost, upstreamPort := splitHostPort(t, upstream.Addr().String())

	emitter := &fakeEmitter{}
	allowEntry := fmt.Sprintf("%s:%d", upstreamHost, upstreamPort)
	p := startTestProxy(t, []string{allowEntry}, emitter)

	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()

	connectLine := fmt.Sprintf("CONNECT %s:%d HTTP/1.1\r\nHost: %s:%d\r\n\r\n", upstreamHost, upstreamPort, upstreamHost, upstreamPort)
	if _, err := conn.Write([]byte(connectLine)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	// Inner ClientHello has no SNI extension — the proxy must deny.
	hello := minimalClientHelloNoSNI()
	if _, err := conn.Write(hello); err != nil {
		t.Logf("write hello: %v", err)
	}

	// No 200 should ever land on the wire and the connection must close.
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	if n > 0 && bytes.Contains(buf[:n], []byte("200")) {
		t.Errorf("expected no 200 response on absent SNI, got %q", string(buf[:n]))
	}

	awaitDeny(t, emitter, "sni_absent")
}

// TestProxy_CONNECT_200WrittenAfterSNIVerification confirms the ordering
// fix from M2: the 200 line is only emitted after SNI is verified to
// match the CONNECT host. A test that read 200 before sending the
// hello would silently regress because of TCP buffering — we instead
// inspect for the 200 byte sequence after writing a deny-triggering
// hello and assert it is absent.
func TestProxy_CONNECT_200WrittenAfterSNIVerification(t *testing.T) {
	upstream := startEchoTCPServer(t)
	upstreamHost, upstreamPort := splitHostPort(t, upstream.Addr().String())

	emitter := &fakeEmitter{}
	allowEntry := fmt.Sprintf("%s:%d", upstreamHost, upstreamPort)
	p := startTestProxy(t, []string{allowEntry}, emitter)

	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()

	connectLine := fmt.Sprintf("CONNECT %s:%d HTTP/1.1\r\nHost: %s:%d\r\n\r\n", upstreamHost, upstreamPort, upstreamHost, upstreamPort)
	if _, err := conn.Write([]byte(connectLine)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	if _, err := conn.Write(clientHelloWithSNI("evil.example")); err != nil {
		t.Logf("write hello: %v", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	all := readAllUntilEOF(t, conn)
	if bytes.Contains(all, []byte("HTTP/1.1 200")) {
		t.Errorf("200 response written before SNI verification: %q", string(all))
	}
}

// TestProxy_TLSParseFailed_DropsConnection covers S4: a non-handshake
// first byte after CONNECT (TLS Application Data 0x17 instead of
// Handshake 0x16) must produce a tls_parse_failed deny without OOM
// or hang. The first 5-byte record header must be readable; the type
// check fires immediately.
func TestProxy_TLSParseFailed_DropsConnection(t *testing.T) {
	upstream := startEchoTCPServer(t)
	upstreamHost, upstreamPort := splitHostPort(t, upstream.Addr().String())

	emitter := &fakeEmitter{}
	allowEntry := fmt.Sprintf("%s:%d", upstreamHost, upstreamPort)
	p := startTestProxy(t, []string{allowEntry}, emitter)

	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()

	connectLine := fmt.Sprintf("CONNECT %s:%d HTTP/1.1\r\nHost: %s:%d\r\n\r\n", upstreamHost, upstreamPort, upstreamHost, upstreamPort)
	if _, err := conn.Write([]byte(connectLine)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	// Write a 5-byte TLS record header announcing application_data
	// (type 0x17) plus a few bytes of body. This is well-formed at
	// the TCP layer but invalid as a handshake first record.
	bogus := []byte{0x17, 0x03, 0x03, 0x00, 0x05, 0xde, 0xad, 0xbe, 0xef, 0x00}
	if _, err := conn.Write(bogus); err != nil {
		t.Logf("write bogus: %v", err)
	}

	awaitDeny(t, emitter, "tls_parse_failed")
}

// TestProxy_OversizedClientHello_Rejected covers S4's OOM-resistance
// claim: a TLS record header announcing length > maxClientHello must
// be rejected without trying to allocate a 16640-plus-byte buffer.
func TestProxy_OversizedClientHello_Rejected(t *testing.T) {
	upstream := startEchoTCPServer(t)
	upstreamHost, upstreamPort := splitHostPort(t, upstream.Addr().String())

	emitter := &fakeEmitter{}
	allowEntry := fmt.Sprintf("%s:%d", upstreamHost, upstreamPort)
	p := startTestProxy(t, []string{allowEntry}, emitter)

	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()

	connectLine := fmt.Sprintf("CONNECT %s:%d HTTP/1.1\r\nHost: %s:%d\r\n\r\n", upstreamHost, upstreamPort, upstreamHost, upstreamPort)
	if _, err := conn.Write([]byte(connectLine)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	// Record header for type=handshake, length=0xFFFF (65535) which
	// is well over the 16640-byte cap.
	hdr := []byte{0x16, 0x03, 0x03, 0xff, 0xff}
	if _, err := conn.Write(hdr); err != nil {
		t.Logf("write oversized header: %v", err)
	}

	awaitDeny(t, emitter, "tls_parse_failed")
}

// TestParseSNIFromHandshake_MalformedInputs is a table-driven unit test
// covering the truncation/overrun paths inside parseSNIFromHandshake.
// Each branch was hand-counted from the parser; together they cover
// every "len(body) < ...; return err" return in sni.go.
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
			name: "session_id length present, bytes overrun",
			body: func() []byte {
				b := []byte{0x01, 0x00, 0x00, 0x00}
				b = append(b, []byte{0x03, 0x03}...)        // version
				b = append(b, make([]byte, 32)...)          // random
				b = append(b, 0xff /* sid len 255 */)       // truncated sid
				return b
			}(),
		},
		{
			name: "cipher_suites length truncated",
			body: func() []byte {
				b := []byte{0x01, 0x00, 0x00, 0x00}
				b = append(b, []byte{0x03, 0x03}...)
				b = append(b, make([]byte, 32)...)
				b = append(b, 0x00) // empty sid
				// cipher_suites: only one byte present, length is 2
				b = append(b, 0x00)
				return b
			}(),
		},
		{
			name: "cipher_suites overrun",
			body: func() []byte {
				b := []byte{0x01, 0x00, 0x00, 0x00}
				b = append(b, []byte{0x03, 0x03}...)
				b = append(b, make([]byte, 32)...)
				b = append(b, 0x00)
				b = append(b, 0xff, 0xff) // 65535-byte cs claim, body has none
				return b
			}(),
		},
		{
			name: "compression_methods length missing",
			body: func() []byte {
				b := []byte{0x01, 0x00, 0x00, 0x00}
				b = append(b, []byte{0x03, 0x03}...)
				b = append(b, make([]byte, 32)...)
				b = append(b, 0x00)
				b = append(b, 0x00, 0x00) // empty cs
				// no compression_methods length byte
				return b
			}(),
		},
		{
			name: "compression_methods overrun",
			body: func() []byte {
				b := []byte{0x01, 0x00, 0x00, 0x00}
				b = append(b, []byte{0x03, 0x03}...)
				b = append(b, make([]byte, 32)...)
				b = append(b, 0x00)
				b = append(b, 0x00, 0x00)
				b = append(b, 0xff) // 255-byte cm claim, body has none
				return b
			}(),
		},
		{
			name: "extensions length truncated",
			body: func() []byte {
				b := []byte{0x01, 0x00, 0x00, 0x00}
				b = append(b, []byte{0x03, 0x03}...)
				b = append(b, make([]byte, 32)...)
				b = append(b, 0x00)
				b = append(b, 0x00, 0x00)
				b = append(b, 0x01, 0x00) // cm len 1, value 0
				b = append(b, 0x00)       // only one byte of the 2-byte ext length
				return b
			}(),
		},
		{
			name: "extensions overrun",
			body: func() []byte {
				b := []byte{0x01, 0x00, 0x00, 0x00}
				b = append(b, []byte{0x03, 0x03}...)
				b = append(b, make([]byte, 32)...)
				b = append(b, 0x00)
				b = append(b, 0x00, 0x00)
				b = append(b, 0x01, 0x00)
				b = append(b, 0xff, 0xff) // 65535-byte ext claim, body has none
				return b
			}(),
		},
		{
			name: "extension length overrun",
			body: func() []byte {
				b := []byte{0x01, 0x00, 0x00, 0x00}
				b = append(b, []byte{0x03, 0x03}...)
				b = append(b, make([]byte, 32)...)
				b = append(b, 0x00)
				b = append(b, 0x00, 0x00)
				b = append(b, 0x01, 0x00)
				// extensions block of 4 bytes total: ext type+claimed len exceeding
				b = append(b, 0x00, 0x04)             // outer ext block length 4
				b = append(b, 0x00, 0x00, 0xff, 0xff) // ext_type=server_name, ext_len=65535
				return b
			}(),
		},
		{
			name: "server_name list overrun",
			body: func() []byte {
				b := []byte{0x01, 0x00, 0x00, 0x00}
				b = append(b, []byte{0x03, 0x03}...)
				b = append(b, make([]byte, 32)...)
				b = append(b, 0x00)
				b = append(b, 0x00, 0x00)
				b = append(b, 0x01, 0x00)
				// outer ext block: 4 byte hdr + 2 byte body
				b = append(b, 0x00, 0x06)
				b = append(b, 0x00, 0x00) // ext type
				b = append(b, 0x00, 0x02) // ext length = 2 (just listLen)
				b = append(b, 0xff, 0xff) // listLen 65535 — overrun
				return b
			}(),
		},
		{
			name: "server_name entry overrun",
			body: func() []byte {
				// listLen says 6 but the entry inside claims a 1024-byte name.
				b := []byte{0x01, 0x00, 0x00, 0x00}
				b = append(b, []byte{0x03, 0x03}...)
				b = append(b, make([]byte, 32)...)
				b = append(b, 0x00)
				b = append(b, 0x00, 0x00)
				b = append(b, 0x01, 0x00)
				b = append(b, 0x00, 0x0a) // outer ext block: 10 bytes
				b = append(b, 0x00, 0x00) // ext type = server_name
				b = append(b, 0x00, 0x06) // ext len = 6
				b = append(b, 0x00, 0x04) // listLen 4
				b = append(b, 0x00)       // name_type = host_name
				b = append(b, 0x04, 0x00) // name length 1024 — overrun
				b = append(b, 0x00)       // one byte of "name"
				return b
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSNIFromHandshake(tc.body)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

// awaitDeny polls the emitter for an egress_blocked event with the
// given reason. Replaces the busy-wait loops inline in the older tests
// so future readers see one canonical pattern.
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
	t.Fatalf("egress_blocked with reason %q not observed; events: %+v", reason, emitter.snapshot())
}

// readAllUntilEOF reads from conn until EOF or the existing read
// deadline trips. It exists so M2's "no 200 ever" assertion isn't
// fooled by partial reads of buffered bytes.
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

