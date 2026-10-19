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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// blockedReasons returns the reason of each egress_blocked event that
// carries a string reason, in emission order.
func (f *fakeEmitter) blockedReasons() []string {
	var reasons []string
	for _, ev := range f.snapshot() {
		if ev.Event != "egress_blocked" {
			continue
		}
		if reason, ok := ev.Data["reason"].(string); ok {
			reasons = append(reasons, reason)
		}
	}
	return reasons
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
	require.NoError(t, err, "Start")
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
	upstreamHost, upstreamPort := splitHostPort(t, upstream.Listener.Addr().String())
	p := startTestProxy(t, []string{fmt.Sprintf("%s:%d", upstreamHost, upstreamPort)}, emitter)

	resp := doProxyHTTPGet(t, p.Addr(), upstream.URL)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read body")
	require.Equal(t, "ok body", string(body))

	require.True(t, emitter.hasEvent("egress_allowed"), "expected egress_allowed event")
	require.False(t, emitter.hasEvent("egress_blocked"), "did not expect egress_blocked event for allowed request")
}

func TestProxy_PlainHTTP_Denied(t *testing.T) {
	upstream := startUpstreamHTTP(t, "should-not-reach")

	emitter := &fakeEmitter{}
	p := startTestProxy(t, []string{"some-other-host.example:443"}, emitter)

	resp := doProxyHTTPGet(t, p.Addr(), upstream.URL)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.True(t, emitter.hasEvent("egress_blocked"), "expected egress_blocked event")

	for _, ev := range emitter.snapshot() {
		if ev.Event == "egress_blocked" {
			require.Equal(t, "not_allowlisted", ev.Data["reason"])
		}
	}
}

func TestProxy_CONNECT_AllowedSplices(t *testing.T) {
	// The upstream echoes bytes verbatim, so the test checks the splice in
	// both directions without TLS. The client still sends a ClientHello-shaped
	// record through CONNECT so the proxy's SNI check passes.
	upstream := startEchoTCPServer(t)

	emitter := &fakeEmitter{}
	upstreamHost, upstreamPort := splitHostPort(t, upstream.Addr().String())
	target := fmt.Sprintf("%s:%d", upstreamHost, upstreamPort)
	p := startTestProxy(t, []string{target}, emitter)

	conn := dialConnect(t, p.Addr(), target)

	// The 200 is written before the SNI peek, so the hello can be sent
	// before the response is read.
	hello := clientHelloWithSNI(upstreamHost)
	_, err := conn.Write(hello)
	require.NoError(t, err, "write hello")

	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
	require.NoError(t, err, "read CONNECT response")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got := make([]byte, len(hello))
	_, err = io.ReadFull(r, got)
	require.NoError(t, err, "read echoed bytes")
	require.Equal(t, hello, got)

	require.Eventually(t, func() bool {
		return emitter.hasEvent("egress_allowed")
	}, time.Second, 20*time.Millisecond, "expected egress_allowed event for allowed CONNECT")
}

func TestProxy_CONNECT_Denied(t *testing.T) {
	emitter := &fakeEmitter{}
	p := startTestProxy(t, []string{"allowed.example:443"}, emitter)

	conn := dialConnect(t, p.Addr(), "denied.example:443")

	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	require.NoError(t, err, "read CONNECT response")
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	reasons := emitter.blockedReasons()
	require.NotEmpty(t, reasons, "expected egress_blocked event")
	require.Equal(t, "not_allowlisted", reasons[len(reasons)-1])
}

func TestProxy_CONNECT_SNIMismatch_DropsTunnel(t *testing.T) {
	upstream := startEchoTCPServer(t)
	upstreamHost, upstreamPort := splitHostPort(t, upstream.Addr().String())
	target := fmt.Sprintf("%s:%d", upstreamHost, upstreamPort)

	emitter := &fakeEmitter{}
	p := startTestProxy(t, []string{target}, emitter)

	conn := dialConnect(t, p.Addr(), target)

	// "evil.example" does not match the allowlisted CONNECT host. The proxy
	// may write the 200 before reading the SNI, but it must drop the tunnel
	// without dialing the upstream.
	hello := clientHelloWithSNI("evil.example")
	if _, err := conn.Write(hello); err != nil {
		t.Logf("write hello: %v", err)
	}

	// Draining yields the optional 200 but never the echoed ClientHello bytes.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)), "SetReadDeadline")
	got := readAllUntilEOF(t, conn)
	require.False(t, bytes.Contains(got, hello),
		"mismatched-SNI tunnel echoed client bytes back, so the upstream was reached: %q", string(got))

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Contains(c, emitter.blockedReasons(), "sni_mismatch")
	}, time.Second, 20*time.Millisecond)
}

func TestProxy_Stop_IsIdempotent(t *testing.T) {
	p := startTestProxy(t, []string{"example.com"}, &fakeEmitter{})

	require.NoError(t, p.Stop(context.Background()), "first Stop")
	require.NoError(t, p.Stop(context.Background()), "second Stop")
}

// TestProxy_StopsOnContextCancel: the proxy goroutine must shut down when the
// caller's ctx is cancelled, so a leaked listener cannot outlive the caller.
func TestProxy_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	p, err := Start(ctx, Config{
		Allowlist: []string{"example.com"},
		Security:  &fakeEmitter{},
	})
	require.NoError(t, err, "Start")

	addr := p.Addr()
	require.NotEmpty(t, addr, "expected listener addr")

	cancel()

	// A dial fails with "connection refused" once the proxy has stopped.
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return true
		}
		_ = c.Close()
		return false
	}, 3*time.Second, 50*time.Millisecond, "proxy listener at %s still accepting after ctx cancel", addr)
}

func TestProxy_PlainHTTP_RequiresAllowlistedPort(t *testing.T) {
	upstream := startUpstreamHTTP(t, "x")
	emitter := &fakeEmitter{}
	upstreamHost, upstreamPort := splitHostPort(t, upstream.Listener.Addr().String())

	// Allow on an unrelated default-port entry. The request should be denied
	// because the upstream's port is not 443 and the entry has no explicit
	// port suffix.
	p := startTestProxy(t, []string{upstreamHost}, emitter)
	resp := doProxyHTTPGet(t, p.Addr(), upstream.URL)
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "expected port-mismatch deny")

	emitter2 := &fakeEmitter{}
	p2 := startTestProxy(t, []string{fmt.Sprintf("%s:%d", upstreamHost, upstreamPort)}, emitter2)
	resp = doProxyHTTPGet(t, p2.Addr(), upstream.URL)
	require.Equal(t, http.StatusOK, resp.StatusCode, "expected allow")
	require.True(t, emitter2.hasEvent("egress_allowed"), "expected egress_allowed")
}

type httpUpstream struct {
	Listener net.Listener
	URL      string
	server   *http.Server
}

func startUpstreamHTTP(t *testing.T, body string) *httpUpstream {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err, "listen")
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
	require.NoError(t, err, "listen")
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

// dialConnect opens a connection to the proxy and sends a CONNECT request
// for target. The caller reads the response.
func dialConnect(t *testing.T, proxyAddr, target string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	require.NoError(t, err, "dial proxy")
	t.Cleanup(func() { _ = conn.Close() })

	_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	require.NoError(t, err, "write CONNECT")
	return conn
}

// doProxyHTTPGet performs an HTTP GET via the configured proxy and returns
// the response. It is the test equivalent of `curl -x` for plain HTTP.
func doProxyHTTPGet(t *testing.T, proxyAddr, target string) *http.Response {
	t.Helper()
	proxyURL, err := url.Parse("http://" + proxyAddr)
	require.NoError(t, err, "parse proxy URL")
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := client.Get(target)
	require.NoError(t, err, "client.Get")
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	require.NoError(t, err, "SplitHostPort %q", addr)
	port := 0
	_, err = fmt.Sscanf(portStr, "%d", &port)
	require.NoError(t, err, "parse port %q", portStr)
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

func TestParseSNIFromHandshake_ExtractsSNIOrReportsAbsent(t *testing.T) {
	t.Run("returns the server name when present", func(t *testing.T) {
		got, err := parseSNIFromHandshake(buildClientHelloBody("foo.example.com"))
		require.NoError(t, err)
		require.Equal(t, "foo.example.com", got)
	})

	t.Run("reports absent SNI as errSNINotPresent", func(t *testing.T) {
		_, err := parseSNIFromHandshake(buildClientHelloBody(""))
		require.ErrorIs(t, err, errSNINotPresent)
	})
}
