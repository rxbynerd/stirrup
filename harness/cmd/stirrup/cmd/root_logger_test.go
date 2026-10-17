package cmd

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

const scrubTestKey = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// TestRootPersistentPreRun_ScrubsDefaultLogger drives the root command's
// persistent hook and checks that a package-level slog call afterwards is
// scrubbed, whichever value kind carries the key.
func TestRootPersistentPreRun_ScrubsDefaultLogger(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var errBuf safeBuf
	rootCmd.SetErr(&errBuf)
	t.Cleanup(func() { rootCmd.SetErr(nil) })

	if rootCmd.PersistentPreRun == nil {
		t.Fatal("rootCmd has no PersistentPreRun")
	}
	rootCmd.PersistentPreRun(rootCmd, nil)

	slog.Warn("upload failed: "+scrubTestKey,
		"error", &net.OpError{Op: "dial", Err: errString("rejected " + scrubTestKey)},
		"detail", map[string]string{"auth": scrubTestKey},
	)

	out := errBuf.String()
	if out == "" {
		t.Fatal("default logger wrote nothing to the command's stderr")
	}
	if strings.Contains(out, scrubTestKey) {
		t.Errorf("key leaked through the default logger: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in default logger output: %s", out)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// TestEgressProxy_LoggerScrubsSecrets sends a CONNECT whose host carries a
// key-shaped token; the proxy's "egress blocked" log line must not echo it.
func TestEgressProxy_LoggerScrubsSecrets(t *testing.T) {
	listener := boundLoopbackListener(t)
	addr := listener.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	var logBuf safeBuf
	done := make(chan error, 1)
	go func() {
		done <- serveEgressProxy(ctx, egressProxyOptions{
			listener:  listener,
			allowlist: []string{"allowed.example.com"},
			level:     slog.LevelInfo,
		}, &logBuf)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serveEgressProxy returned %v on shutdown, want nil", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("serveEgressProxy did not shut down within 10s")
		}
	})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()

	host := scrubTestKey + ".example.com"
	line := "CONNECT " + host + ":443 HTTP/1.1\r\nHost: " + host + ":443\r\n\r\n"
	if _, err := conn.Write([]byte(line)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}

	var blocked string
	deadline := time.Now().Add(5 * time.Second)
	for blocked == "" && time.Now().Before(deadline) {
		for _, l := range strings.Split(logBuf.String(), "\n") {
			if strings.Contains(l, `msg="egress blocked"`) {
				blocked = l
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if blocked == "" {
		t.Fatalf("no \"egress blocked\" log line within 5s: %s", logBuf.String())
	}
	if strings.Contains(blocked, scrubTestKey) {
		t.Errorf("key leaked through the egress proxy logger: %s", blocked)
	}
	if !strings.Contains(blocked, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in egress proxy log line: %s", blocked)
	}
}
