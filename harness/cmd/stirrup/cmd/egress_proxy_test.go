package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
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
)

// boundLoopbackListener opens a listener on an ephemeral 127.0.0.1 port.
// Passing it straight into serveEgressProxy avoids the close-and-rebind
// TOCTOU a free-port-then-rebind helper would carry.
func boundLoopbackListener(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind loopback listener: %v", err)
	}
	return l
}

// startTestEgressProxy runs serveEgressProxy in a goroutine against a
// pre-bound loopback listener, waits for it to accept connections, and
// registers cleanup that cancels the context and waits for a clean
// shutdown. The returned addr is the bound host:port.
func startTestEgressProxy(t *testing.T, allowlist []string) (addr string) {
	t.Helper()
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
			if err != nil {
				t.Errorf("serveEgressProxy returned error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("serveEgressProxy did not shut down within 10s")
		}
	})

	// Wait for the listener to accept (or an early serve failure).
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("serveEgressProxy exited before listening: %v", err)
		default:
		}
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return addr
		}
		if time.Now().After(deadline) {
			t.Fatalf("egress proxy did not start listening on %s within 5s", addr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestEgressProxy_DeniesNonAllowlisted drives a CONNECT for a host that
// is not on the allowlist through the real serve path and asserts a 403.
func TestEgressProxy_DeniesNonAllowlisted(t *testing.T) {
	addr := startTestEgressProxy(t, []string{"allowed.example.com"})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()

	connectLine := "CONNECT denied.example.com:443 HTTP/1.1\r\nHost: denied.example.com:443\r\n\r\n"
	if _, err := conn.Write([]byte(connectLine)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("CONNECT for non-allowlisted host: status = %d, want 403", resp.StatusCode)
	}
}

// TestEgressProxy_AllowsCONNECT drives a CONNECT for an allowlisted host
// through the real serve path and asserts the proxy returns 200 and
// splices a bidirectional tunnel to the upstream. A ClientHello carrying
// an SNI matching the CONNECT host is sent before reading the 200 (the
// proxy verifies SNI before establishing the tunnel); the upstream
// echoes the bytes back, confirming the splice.
func TestEgressProxy_AllowsCONNECT(t *testing.T) {
	upstream := startEchoServer(t)
	upstreamHost, upstreamPort := hostPort(t, "http://"+upstream.Addr().String())
	allowEntry := fmt.Sprintf("%s:%s", upstreamHost, upstreamPort)
	addr := startTestEgressProxy(t, []string{allowEntry})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()

	connectLine := fmt.Sprintf("CONNECT %s:%s HTTP/1.1\r\nHost: %s:%s\r\n\r\n", upstreamHost, upstreamPort, upstreamHost, upstreamPort)
	if _, err := conn.Write([]byte(connectLine)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	// The proxy peeks the ClientHello SNI before writing the 200, so the
	// hello must be sent before reading the response. SNI matches the CONNECT
	// host (both the literal upstream host), which passes the cross-check.
	hello := clientHelloWithSNI(upstreamHost)
	if _, err := conn.Write(hello); err != nil {
		t.Fatalf("write ClientHello: %v", err)
	}

	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT to allowlisted host: status = %d, want 200", resp.StatusCode)
	}

	// The upstream echoes the replayed ClientHello back through the tunnel.
	got := make([]byte, len(hello))
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatalf("read echoed bytes through tunnel: %v", err)
	}
	if !bytes.Equal(got, hello) {
		t.Error("tunnel echo mismatch: spliced bytes differ from sent ClientHello")
	}
}

// TestEgressProxy_AllowsAllowlisted drives a plain-HTTP proxy request for
// an allowlisted host through the real serve path and asserts it is
// forwarded to the upstream (200). Plain HTTP avoids the TLS
// ClientHello/SNI handshake; the allowlist gate is identical for both.
func TestEgressProxy_AllowsAllowlisted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("upstream-ok"))
	}))
	defer upstream.Close()

	upstreamHost, upstreamPort := hostPort(t, upstream.URL)
	allowEntry := fmt.Sprintf("%s:%s", upstreamHost, upstreamPort)
	addr := startTestEgressProxy(t, []string{allowEntry})

	status, body := proxiedGet(t, addr, upstream.URL)
	if status != http.StatusOK {
		t.Errorf("proxied GET to allowlisted host: status = %d, want 200", status)
	}
	if body != "upstream-ok" {
		t.Errorf("proxied body = %q, want %q", body, "upstream-ok")
	}
}

// TestEgressProxy_AllowlistFile asserts entries from an allowlist file are
// honoured by the parse+serve path: a host listed in the file is forwarded.
// Blank lines and #-comments in the file must be ignored.
func TestEgressProxy_AllowlistFile(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	upstreamHost, upstreamPort := hostPort(t, upstream.URL)

	dir := t.TempDir()
	file := filepath.Join(dir, "allowlist.txt")
	content := fmt.Sprintf("# comment line\n\n%s:%s\n", upstreamHost, upstreamPort)
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatalf("write allowlist file: %v", err)
	}

	// Exercise the file reader the subcommand uses, then serve with the parsed
	// entries — the same composition runEgressProxy performs.
	entries, err := readAllowlistFile(file)
	if err != nil {
		t.Fatalf("readAllowlistFile: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("readAllowlistFile entries = %v, want exactly one (comment/blank ignored)", entries)
	}

	addr := startTestEgressProxy(t, entries)
	status, _ := proxiedGet(t, addr, upstream.URL)
	if status != http.StatusOK {
		t.Errorf("file-allowlisted host: status = %d, want 200", status)
	}
}

// TestReadAllowlistFile_SizeCap asserts the 1 MiB read cap fires on an
// oversized allowlist file rather than reading it all into memory. A file at
// exactly the cap is accepted; one byte over is rejected.
func TestReadAllowlistFile_SizeCap(t *testing.T) {
	dir := t.TempDir()

	t.Run("over cap rejected", func(t *testing.T) {
		file := filepath.Join(dir, "over.txt")
		// One byte past the cap. Content shape is irrelevant — the read is
		// bounded before any parsing.
		big := bytes.Repeat([]byte("a"), int(maxAllowlistFileBytes)+1)
		if err := os.WriteFile(file, big, 0o600); err != nil {
			t.Fatalf("write oversize file: %v", err)
		}
		_, err := readAllowlistFile(file)
		if err == nil {
			t.Fatal("expected error for an over-cap allowlist file")
		}
		if !strings.Contains(err.Error(), "cap") {
			t.Errorf("error %q should mention the byte cap", err)
		}
	})

	t.Run("at cap accepted", func(t *testing.T) {
		file := filepath.Join(dir, "atcap.txt")
		// Fill exactly to the cap with a single valid entry followed by
		// padding comment bytes so the parse yields one entry.
		entry := "example.com\n"
		pad := bytes.Repeat([]byte("#x\n"), (int(maxAllowlistFileBytes)-len(entry))/3)
		content := append([]byte(entry), pad...)
		// Trim to be at most the cap.
		if int64(len(content)) > maxAllowlistFileBytes {
			content = content[:maxAllowlistFileBytes]
		}
		if err := os.WriteFile(file, content, 0o600); err != nil {
			t.Fatalf("write at-cap file: %v", err)
		}
		entries, err := readAllowlistFile(file)
		if err != nil {
			t.Fatalf("at-cap file should be accepted, got: %v", err)
		}
		if len(entries) == 0 || entries[0] != "example.com" {
			t.Errorf("entries = %v, want it to contain example.com", entries)
		}
	})
}

// TestEgressProxy_BadAllowlistFails asserts a malformed allowlist entry makes
// serveEgressProxy return an error rather than serving. serveEgressProxy
// closes the supplied listener on the Start-failure path, so no leak.
func TestEgressProxy_BadAllowlistFails(t *testing.T) {
	err := serveEgressProxy(context.Background(), egressProxyOptions{
		listener: boundLoopbackListener(t),
		// A non-numeric port is rejected by the matcher at Start.
		allowlist: []string{"bad.example.com:notaport"},
		level:     slog.LevelError,
	}, io.Discard)
	if err == nil {
		t.Fatal("expected error for malformed allowlist entry")
	}
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != exitValidation {
		t.Errorf("error = %v, want an exitValidation (exit 1) exitError", err)
	}
}

// proxiedGet issues a plain-HTTP proxied GET for targetURL through the proxy
// at proxyAddr and returns the status code and body. The request line carries
// the absolute URL, which is what an HTTP_PROXY client sends and what the
// proxy gates on the allowlist before forwarding.
func proxiedGet(t *testing.T, proxyAddr, targetURL string) (int, string) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()

	hostOnly := strings.TrimPrefix(targetURL, "http://")
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", targetURL, hostOnly)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// hostPort splits a http://host:port URL into its host and port components.
func hostPort(t *testing.T, rawURL string) (host, port string) {
	t.Helper()
	hostPort := strings.TrimPrefix(rawURL, "http://")
	h, p, err := net.SplitHostPort(hostPort)
	if err != nil {
		t.Fatalf("split %q: %v", hostPort, err)
	}
	return h, p
}

// startEchoServer runs a TCP echo upstream on loopback. The CONNECT test
// points the proxy at it and confirms bytes splice through both ways.
func startEchoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo upstream: %v", err)
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
