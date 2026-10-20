package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// boundLoopbackListener opens a listener on an ephemeral 127.0.0.1 port.
// Passing it straight into serveEgressProxy avoids the close-and-rebind
// TOCTOU a free-port-then-rebind helper would carry.
func boundLoopbackListener(t *testing.T) net.Listener {
	t.Helper()
	must := require.New(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must.NoError(err, "bind loopback listener")
	return l
}

// startTestEgressProxy runs serveEgressProxy in a goroutine against a
// pre-bound loopback listener, waits for it to accept connections, and
// registers cleanup that cancels the context and waits for a clean
// shutdown. The returned addr is the bound host:port.
func startTestEgressProxy(t *testing.T, allowlist []string) (addr string) {
	t.Helper()
	check := assert.New(t)
	must := require.New(t)
	listener := boundLoopbackListener(t)
	addr = listener.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveEgressProxy(ctx, egressProxyOptions{
			listener:  listener,
			allowlist: allowlist,
			level:     slog.LevelError,
		}, io.Discard)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			check.NoError(err, "serveEgressProxy returned an error")
		case <-time.After(10 * time.Second):
			check.Fail("serveEgressProxy did not shut down within 10s")
		}
	})

	// Wait for the listener to accept (or an early serve failure).
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-done:
			must.FailNow("serveEgressProxy exited before listening", "error: %v", err)
		default:
		}
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return addr
		}
		if time.Now().After(deadline) {
			must.FailNow(fmt.Sprintf("egress proxy did not start listening on %s within 5s", addr))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestEgressProxy_DeniesNonAllowlisted drives a CONNECT for a host that
// is not on the allowlist through the real serve path and asserts a 403.
func TestEgressProxy_DeniesNonAllowlisted(t *testing.T) {
	must := require.New(t)
	addr := startTestEgressProxy(t, []string{"allowed.example.com"})

	conn, err := net.Dial("tcp", addr)
	must.NoError(err, "dial proxy")
	defer func() { _ = conn.Close() }()

	connectLine := "CONNECT denied.example.com:443 HTTP/1.1\r\nHost: denied.example.com:443\r\n\r\n"
	_, err = conn.Write([]byte(connectLine))
	must.NoError(err, "write CONNECT")

	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	must.NoError(err, "read CONNECT response")
	must.Equal(http.StatusForbidden, resp.StatusCode)
}

// TestEgressProxy_TunnelsAllowlistedCONNECT drives a CONNECT for an allowlisted
// host through the real serve path and asserts the proxy returns 200 and
// splices a bidirectional tunnel to the upstream. A ClientHello carrying
// an SNI matching the CONNECT host is sent before reading the 200 (the
// proxy verifies SNI before establishing the tunnel); the upstream
// echoes the bytes back, confirming the splice.
func TestEgressProxy_TunnelsAllowlistedCONNECT(t *testing.T) {
	must := require.New(t)
	upstream := startEchoServer(t)
	upstreamHost, upstreamPort := hostPort(t, "http://"+upstream.Addr().String())
	allowEntry := fmt.Sprintf("%s:%s", upstreamHost, upstreamPort)
	addr := startTestEgressProxy(t, []string{allowEntry})

	conn, err := net.Dial("tcp", addr)
	must.NoError(err, "dial proxy")
	defer func() { _ = conn.Close() }()

	connectLine := fmt.Sprintf("CONNECT %s:%s HTTP/1.1\r\nHost: %s:%s\r\n\r\n", upstreamHost, upstreamPort, upstreamHost, upstreamPort)
	_, err = conn.Write([]byte(connectLine))
	must.NoError(err, "write CONNECT")

	// The proxy peeks the ClientHello SNI before writing the 200, so the
	// hello must be sent before reading the response. SNI matches the CONNECT
	// host (both the literal upstream host), which passes the cross-check.
	hello := clientHelloWithSNI(upstreamHost)
	_, err = conn.Write(hello)
	must.NoError(err, "write ClientHello")

	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
	must.NoError(err, "read CONNECT response")
	must.Equal(http.StatusOK, resp.StatusCode)

	// The upstream echoes the replayed ClientHello back through the tunnel.
	got := make([]byte, len(hello))
	_, err = io.ReadFull(r, got)
	must.NoError(err, "read echoed bytes through tunnel")
	must.Equal(hello, got, "spliced bytes should match the sent ClientHello")
}

// TestEgressProxy_ForwardsAllowlistedPlainHTTP drives a plain-HTTP proxy request
// for an allowlisted host through the real serve path and asserts it is
// forwarded to the upstream (200). Plain HTTP avoids the TLS
// ClientHello/SNI handshake; the allowlist gate is identical for both.
func TestEgressProxy_ForwardsAllowlistedPlainHTTP(t *testing.T) {
	check := assert.New(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("upstream-ok"))
	}))
	defer upstream.Close()

	upstreamHost, upstreamPort := hostPort(t, upstream.URL)
	allowEntry := fmt.Sprintf("%s:%s", upstreamHost, upstreamPort)
	addr := startTestEgressProxy(t, []string{allowEntry})

	status, body := proxiedGet(t, addr, upstream.URL)
	check.Equal(http.StatusOK, status)
	check.Equal("upstream-ok", body)
}

// TestEgressProxy_ForwardsHostFromAllowlistFile asserts entries from an allowlist
// file are honoured by the parse+serve path: a host listed in the file is
// forwarded. Blank lines and #-comments in the file must be ignored.
func TestEgressProxy_ForwardsHostFromAllowlistFile(t *testing.T) {
	must := require.New(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	upstreamHost, upstreamPort := hostPort(t, upstream.URL)

	dir := t.TempDir()
	file := filepath.Join(dir, "allowlist.txt")
	content := fmt.Sprintf("# comment line\n\n%s:%s\n", upstreamHost, upstreamPort)
	err := os.WriteFile(file, []byte(content), 0o600)
	must.NoError(err, "write allowlist file")

	// Exercise the file reader the subcommand uses, then serve with the parsed
	// entries — the same composition runEgressProxy performs.
	entries, err := readAllowlistFile(file)
	must.NoError(err, "readAllowlistFile")
	must.Len(entries, 1, "comment and blank lines should be ignored")

	addr := startTestEgressProxy(t, entries)
	status, _ := proxiedGet(t, addr, upstream.URL)
	must.Equal(http.StatusOK, status)
}

// TestReadAllowlistFile_SizeCap asserts the 1 MiB read cap fires on an
// oversized allowlist file rather than reading it all into memory. A file at
// exactly the cap is accepted; one byte over is rejected.
func TestReadAllowlistFile_SizeCap(t *testing.T) {
	dir := t.TempDir()

	t.Run("over cap rejected", func(t *testing.T) {
		must := require.New(t)
		file := filepath.Join(dir, "over.txt")
		// One byte past the cap. Content shape is irrelevant — the read is
		// bounded before any parsing.
		big := bytes.Repeat([]byte("a"), int(maxAllowlistFileBytes)+1)
		err := os.WriteFile(file, big, 0o600)
		must.NoError(err, "write oversize file")

		_, err = readAllowlistFile(file)
		must.ErrorContains(err, "cap", "error should mention the byte cap")
	})

	t.Run("at cap accepted", func(t *testing.T) {
		must := require.New(t)
		file := filepath.Join(dir, "atcap.txt")
		// Fill exactly to the cap with a single valid entry followed by
		// padding comment bytes so the parse yields one entry.
		entry := "example.com\n"
		pad := bytes.Repeat([]byte("#x\n"), (int(maxAllowlistFileBytes)-len(entry))/3)
		content := append([]byte(entry), pad...)
		if int64(len(content)) > maxAllowlistFileBytes {
			content = content[:maxAllowlistFileBytes]
		}
		err := os.WriteFile(file, content, 0o600)
		must.NoError(err, "write at-cap file")

		entries, err := readAllowlistFile(file)
		must.NoError(err, "at-cap file should be accepted")
		must.NotEmpty(entries)
		must.Equal("example.com", entries[0])
	})
}

// TestEgressProxy_RejectsMalformedAllowlistEntry asserts a malformed allowlist
// entry makes serveEgressProxy return an error rather than serving.
// serveEgressProxy closes the supplied listener on the Start-failure path, so no leak.
func TestEgressProxy_RejectsMalformedAllowlistEntry(t *testing.T) {
	must := require.New(t)
	err := serveEgressProxy(context.Background(), egressProxyOptions{
		listener: boundLoopbackListener(t),
		// A non-numeric port is rejected by the matcher at Start.
		allowlist: []string{"bad.example.com:notaport"},
		level:     slog.LevelError,
	}, io.Discard)

	var ee *exitError
	must.ErrorAs(err, &ee, "want an exitValidation (exit 1) exitError")
	must.Equal(exitValidation, ee.code)
}

// proxiedGet issues a plain-HTTP proxied GET for targetURL through the proxy
// at proxyAddr and returns the status code and body. The request line carries
// the absolute URL, which is what an HTTP_PROXY client sends and what the
// proxy gates on the allowlist before forwarding.
func proxiedGet(t *testing.T, proxyAddr, targetURL string) (int, string) {
	t.Helper()
	must := require.New(t)
	conn, err := net.Dial("tcp", proxyAddr)
	must.NoError(err, "dial proxy")
	defer func() { _ = conn.Close() }()

	hostOnly := strings.TrimPrefix(targetURL, "http://")
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", targetURL, hostOnly)
	_, err = conn.Write([]byte(req))
	must.NoError(err, "write request")

	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	must.NoError(err, "read response")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// hostPort splits a http://host:port URL into its host and port components.
func hostPort(t *testing.T, rawURL string) (host, port string) {
	t.Helper()
	must := require.New(t)
	hostPort := strings.TrimPrefix(rawURL, "http://")
	h, p, err := net.SplitHostPort(hostPort)
	must.NoError(err, "split %q", hostPort)
	return h, p
}

// startEchoServer runs a TCP echo upstream on loopback. The CONNECT test
// points the proxy at it and confirms bytes splice through both ways.
func startEchoServer(t *testing.T) net.Listener {
	t.Helper()
	must := require.New(t)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	must.NoError(err, "listen echo upstream")
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

// clientHelloWithSNI returns a minimal TLS handshake record carrying the
// given SNI. It mirrors the egressproxy package's own test helper: the proxy
// peeks this record to verify SNI matches the CONNECT host before opening the
// tunnel. A real tls.Client cannot be used here because Go omits SNI for an
// IP-literal ServerName, which the proxy treats as a tampering signal.
func clientHelloWithSNI(sni string) []byte {
	body := buildClientHelloBody(sni)
	rec := make([]byte, 5+len(body))
	rec[0] = 0x16 // handshake
	rec[1] = 0x03 // TLS 1.0 in record layer
	rec[2] = 0x01
	binary.BigEndian.PutUint16(rec[3:5], uint16(len(body)))
	copy(rec[5:], body)
	return rec
}

// buildClientHelloBody constructs a ClientHello message body carrying a
// server_name extension for sni.
func buildClientHelloBody(sni string) []byte {
	var inner bytes.Buffer
	inner.Write([]byte{0x03, 0x03})             // client_version TLS 1.2
	inner.Write(make([]byte, 32))               // random
	inner.WriteByte(0x00)                       // session_id (empty)
	inner.Write([]byte{0x00, 0x02, 0x00, 0x2f}) // cipher_suites: one suite
	inner.Write([]byte{0x01, 0x00})             // compression_methods: null

	sniBytes := []byte(sni)
	var hostName bytes.Buffer
	hostName.WriteByte(0x00) // host_name type
	_ = binary.Write(&hostName, binary.BigEndian, uint16(len(sniBytes)))
	hostName.Write(sniBytes)
	var serverNameList bytes.Buffer
	_ = binary.Write(&serverNameList, binary.BigEndian, uint16(hostName.Len()))
	serverNameList.Write(hostName.Bytes())
	var ext bytes.Buffer
	ext.Write([]byte{0x00, 0x00}) // extension_type = server_name
	_ = binary.Write(&ext, binary.BigEndian, uint16(serverNameList.Len()))
	ext.Write(serverNameList.Bytes())
	_ = binary.Write(&inner, binary.BigEndian, uint16(ext.Len()))
	inner.Write(ext.Bytes())

	body := make([]byte, 4+inner.Len())
	body[0] = 0x01 // ClientHello
	body[1] = byte(inner.Len() >> 16)
	body[2] = byte(inner.Len() >> 8)
	body[3] = byte(inner.Len())
	copy(body[4:], inner.Bytes())
	return body
}
