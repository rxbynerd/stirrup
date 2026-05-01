package egressproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEmitter records every Emit call so tests can assert on the events
// the proxy fired. Concurrent-safe because the proxy serves on a goroutine.
type fakeEmitter struct {
	mu     sync.Mutex
	events []emittedEvent
}

type emittedEvent struct {
	Level string
	Event string
	Data  map[string]any
}

func (f *fakeEmitter) Emit(level, event string, data map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make(map[string]any, len(data))
	for k, v := range data {
		cp[k] = v
	}
	f.events = append(f.events, emittedEvent{Level: level, Event: event, Data: cp})
}

func (f *fakeEmitter) snapshot() []emittedEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]emittedEvent, len(f.events))
	copy(out, f.events)
	return out
}

func (f *fakeEmitter) hasEvent(name string) bool {
	for _, e := range f.snapshot() {
		if e.Event == name {
			return true
		}
	}
	return false
}

// startTestProxy boots a proxy listening on an ephemeral local port.
func startTestProxy(t *testing.T, allowlist []string, emitter *fakeEmitter) *Proxy {
	t.Helper()
	p, err := Start(context.Background(), Config{
		Allowlist:   allowlist,
		Security:    emitter,
		ReadTimeout: 5 * time.Second,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Stop(ctx)
	})
	return p
}

func TestProxy_PlainHTTP_Allowed(t *testing.T) {
	upstream := startUpstreamHTTP(t, "ok body")

	emitter := &fakeEmitter{}
	upstreamHost, upstreamPortInt := splitHostPort(t, upstream.Listener.Addr().String())
	allowEntry := fmt.Sprintf("%s:%d", upstreamHost, upstreamPortInt)
	p := startTestProxy(t, []string{allowEntry}, emitter)

	resp := doProxyHTTPGet(t, p.Addr(), upstream.URL)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp.Body)
	if body != "ok body" {
		t.Errorf("body: got %q, want %q", body, "ok body")
	}

	if !emitter.hasEvent("egress_allowed") {
		t.Error("expected egress_allowed event")
	}
	if emitter.hasEvent("egress_blocked") {
		t.Error("did not expect egress_blocked event for allowed request")
	}
}

func TestProxy_PlainHTTP_Denied(t *testing.T) {
	upstream := startUpstreamHTTP(t, "should-not-reach")

	emitter := &fakeEmitter{}
	p := startTestProxy(t, []string{"some-other-host.example:443"}, emitter)

	resp := doProxyHTTPGet(t, p.Addr(), upstream.URL)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status: got %d, want 403", resp.StatusCode)
	}
	if !emitter.hasEvent("egress_blocked") {
		t.Error("expected egress_blocked event")
	}

	for _, ev := range emitter.snapshot() {
		if ev.Event == "egress_blocked" {
			if ev.Data["reason"] != "not_allowlisted" {
				t.Errorf("reason: got %v, want not_allowlisted", ev.Data["reason"])
			}
		}
	}
}

func TestProxy_CONNECT_AllowedSplices(t *testing.T) {
	// Run a vanilla TCP echo "upstream" so we can verify the splice copies
	// bytes both ways without needing TLS negotiation. We do feed a TLS
	// ClientHello-shaped record to the proxy via the CONNECT path, but the
	// upstream just echoes whatever it sees.
	upstream := startEchoTCPServer(t)

	emitter := &fakeEmitter{}
	upstreamHost, upstreamPort := splitHostPort(t, upstream.Addr().String())
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

	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status: got %d, want 200", resp.StatusCode)
	}

	// Send a synthetic ClientHello whose SNI points at upstreamHost (well,
	// canonicalised — hostnames in SNI are RFC-mandated to be DNS names,
	// not IP literals; the proxy treats SNI mismatch as a drop, but if SNI
	// is absent it allows the splice). We use the "absent SNI" path for
	// simplicity here.
	hello := minimalClientHelloNoSNI()
	if _, err := conn.Write(hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	got := make([]byte, len(hello))
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatalf("read echoed bytes: %v", err)
	}
	if !bytes.Equal(got, hello) {
		t.Error("upstream echo mismatch")
	}

	// Allow the goroutines to deliver their security events.
	for i := 0; i < 50 && !emitter.hasEvent("egress_allowed"); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if !emitter.hasEvent("egress_allowed") {
		t.Error("expected egress_allowed event for allowed CONNECT")
	}
}

func TestProxy_CONNECT_Denied(t *testing.T) {
	emitter := &fakeEmitter{}
	p := startTestProxy(t, []string{"allowed.example:443"}, emitter)

	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()

	connectLine := "CONNECT denied.example:443 HTTP/1.1\r\nHost: denied.example:443\r\n\r\n"
	if _, err := conn.Write([]byte(connectLine)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("CONNECT status: got %d, want 403", resp.StatusCode)
	}

	if !emitter.hasEvent("egress_blocked") {
		t.Error("expected egress_blocked event")
	}
	var foundReason string
	for _, ev := range emitter.snapshot() {
		if ev.Event == "egress_blocked" {
			if reason, ok := ev.Data["reason"].(string); ok {
				foundReason = reason
			}
		}
	}
	if foundReason != "not_allowlisted" {
		t.Errorf("reason: got %q, want not_allowlisted", foundReason)
	}
}

func TestProxy_CONNECT_SNIMismatchIsDropped(t *testing.T) {
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

	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status: got %d, want 200", resp.StatusCode)
	}

	// Send a ClientHello whose SNI is "evil.example", which does NOT match
	// the allowlisted CONNECT host. The proxy should drop the connection.
	hello := clientHelloWithSNI("evil.example")
	if _, err := conn.Write(hello); err != nil {
		// Write may succeed because the proxy hasn't closed yet, but the
		// follow-up read should hit EOF.
		t.Logf("write hello: %v", err)
	}

	// Read should fail with EOF (or a fixed-byte echo if the splice
	// happened — we assert the latter does NOT happen).
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err == nil && n > 0 {
		t.Errorf("expected EOF after SNI mismatch, got %d echoed bytes", n)
	}

	// Wait for the egress_blocked event to land.
	for i := 0; i < 50; i++ {
		for _, ev := range emitter.snapshot() {
			if ev.Event == "egress_blocked" {
				if reason, ok := ev.Data["reason"].(string); ok && reason == "sni_mismatch" {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("expected egress_blocked with reason sni_mismatch, events: %+v", emitter.snapshot())
}

func TestProxy_Stop_IsIdempotent(t *testing.T) {
	p := startTestProxy(t, []string{"example.com"}, &fakeEmitter{})

	if err := p.Stop(context.Background()); err != nil {
		t.Errorf("first Stop: %v", err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Errorf("second Stop: %v", err)
	}
}

func TestProxy_ParsesHostHeaderForPlainHTTP(t *testing.T) {
	upstream := startUpstreamHTTP(t, "x")
	emitter := &fakeEmitter{}
	upstreamHost, upstreamPort := splitHostPort(t, upstream.Listener.Addr().String())

	// Allow on an unrelated default-port entry. The request should be denied
	// because the upstream's port is not 443 and the entry has no explicit
	// port suffix.
	p := startTestProxy(t, []string{upstreamHost}, emitter)
	resp := doProxyHTTPGet(t, p.Addr(), upstream.URL)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected port-mismatch deny, got %d", resp.StatusCode)
	}

	// And now allow it explicitly.
	emitter2 := &fakeEmitter{}
	p2 := startTestProxy(t, []string{fmt.Sprintf("%s:%d", upstreamHost, upstreamPort)}, emitter2)
	resp = doProxyHTTPGet(t, p2.Addr(), upstream.URL)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected allow, got %d", resp.StatusCode)
	}
	if !emitter2.hasEvent("egress_allowed") {
		t.Error("expected egress_allowed")
	}
}

// --- Helpers ---

type httpUpstream struct {
	Listener net.Listener
	URL      string
	server   *http.Server
}

func startUpstreamHTTP(t *testing.T, body string) *httpUpstream {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return &httpUpstream{
		Listener: ln,
		URL:      "http://" + ln.Addr().String() + "/",
		server:   srv,
	}
}

func startEchoTCPServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// doProxyHTTPGet performs an HTTP GET via the configured proxy and returns
// the response. It is the test equivalent of `curl -x` for plain HTTP.
func doProxyHTTPGet(t *testing.T, proxyAddr, target string) *http.Response {
	t.Helper()
	proxyURL, err := url.Parse("http://" + proxyAddr)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Get(target)
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func readBody(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort %q: %v", addr, err)
	}
	port := 0
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return host, port
}

// minimalClientHelloNoSNI returns a TLS handshake record that parses as a
// valid ClientHello but carries no extensions (and therefore no SNI).
func minimalClientHelloNoSNI() []byte {
	body := buildClientHelloBody("")
	rec := make([]byte, 5+len(body))
	rec[0] = 0x16 // handshake
	rec[1] = 0x03 // TLS 1.0 in record
	rec[2] = 0x01
	binary.BigEndian.PutUint16(rec[3:5], uint16(len(body)))
	copy(rec[5:], body)
	return rec
}

// clientHelloWithSNI returns a TLS handshake record carrying the given SNI.
func clientHelloWithSNI(sni string) []byte {
	body := buildClientHelloBody(sni)
	rec := make([]byte, 5+len(body))
	rec[0] = 0x16
	rec[1] = 0x03
	rec[2] = 0x01
	binary.BigEndian.PutUint16(rec[3:5], uint16(len(body)))
	copy(rec[5:], body)
	return rec
}

// buildClientHelloBody constructs a ClientHello message body. If sni is
// non-empty, a server_name extension is appended.
func buildClientHelloBody(sni string) []byte {
	// Inner contents: ClientHello fields without msg_type/length.
	var inner bytes.Buffer
	// client_version (TLS 1.2 wire value to be permissive)
	inner.Write([]byte{0x03, 0x03})
	// random (32 bytes of zeros — fine for testing the parser)
	inner.Write(make([]byte, 32))
	// session_id (empty)
	inner.WriteByte(0x00)
	// cipher_suites: a single suite
	inner.Write([]byte{0x00, 0x02, 0x00, 0x2f}) // TLS_RSA_WITH_AES_128_CBC_SHA
	// compression_methods: null
	inner.Write([]byte{0x01, 0x00})

	if sni != "" {
		// Build server_name extension.
		sniBytes := []byte(sni)
		// HostName: type(1) + length(2) + name
		var hostName bytes.Buffer
		hostName.WriteByte(0x00) // host_name type
		_ = binary.Write(&hostName, binary.BigEndian, uint16(len(sniBytes)))
		hostName.Write(sniBytes)
		// ServerNameList: length(2) + entries
		var serverNameList bytes.Buffer
		_ = binary.Write(&serverNameList, binary.BigEndian, uint16(hostName.Len()))
		serverNameList.Write(hostName.Bytes())
		// Extension: type(2) + length(2) + body
		var ext bytes.Buffer
		ext.Write([]byte{0x00, 0x00}) // extension_type = server_name
		_ = binary.Write(&ext, binary.BigEndian, uint16(serverNameList.Len()))
		ext.Write(serverNameList.Bytes())

		// Extensions container: length(2) + bytes
		_ = binary.Write(&inner, binary.BigEndian, uint16(ext.Len()))
		inner.Write(ext.Bytes())
	} else {
		// Even with no SNI we emit an empty extensions block to simulate
		// a TLS 1.3-ish client. The parser tolerates either.
		_ = binary.Write(&inner, binary.BigEndian, uint16(0))
	}

	// Wrap inner in handshake header: msg_type(1) + length(3).
	body := make([]byte, 4+inner.Len())
	body[0] = 0x01 // ClientHello
	body[1] = byte(inner.Len() >> 16)
	body[2] = byte(inner.Len() >> 8)
	body[3] = byte(inner.Len())
	copy(body[4:], inner.Bytes())
	return body
}

// --- direct unit test against the SNI parser ---

func TestParseSNIFromHandshake_PresentAndAbsent(t *testing.T) {
	body := buildClientHelloBody("foo.example.com")
	got, err := parseSNIFromHandshake(body)
	if err != nil {
		t.Fatalf("parse SNI present: %v", err)
	}
	if got != "foo.example.com" {
		t.Errorf("SNI: got %q, want foo.example.com", got)
	}

	body = buildClientHelloBody("")
	if _, err := parseSNIFromHandshake(body); err == nil || !errIsNotPresent(err) {
		t.Errorf("expected errSNINotPresent for empty SNI, got %v", err)
	}
}

func errIsNotPresent(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no SNI extension")
}
