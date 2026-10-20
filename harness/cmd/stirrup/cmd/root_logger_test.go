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

	"github.com/stretchr/testify/require"
)

const scrubTestKey = "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// TestRootPersistentPreRun_ScrubsDefaultLogger drives the root command's
// persistent hook and checks that a package-level slog call afterwards is
// scrubbed, whichever value kind carries the key.
func TestRootPersistentPreRun_ScrubsDefaultLogger(t *testing.T) {
	must := require.New(t)
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	var errBuf safeBuf
	rootCmd.SetErr(&errBuf)
	t.Cleanup(func() { rootCmd.SetErr(nil) })

	must.NotNil(rootCmd.PersistentPreRun, "rootCmd has no PersistentPreRun")
	rootCmd.PersistentPreRun(rootCmd, nil)

	slog.Warn("upload failed: "+scrubTestKey,
		"error", &net.OpError{Op: "dial", Err: errString("rejected " + scrubTestKey)},
		"detail", map[string]string{"auth": scrubTestKey},
	)

	out := errBuf.String()
	must.NotEmpty(out, "default logger wrote nothing to the command's stderr")
	t.Run("omits the key", func(t *testing.T) {
		must := require.New(t)
		must.NotContains(out, scrubTestKey, "key leaked through the default logger")
	})
	t.Run("contains the redaction marker", func(t *testing.T) {
		must := require.New(t)
		must.Contains(out, "[REDACTED]")
	})
}

type errString string

func (e errString) Error() string { return string(e) }

// TestEgressProxy_LoggerScrubsSecrets sends a CONNECT whose host carries a
// key-shaped token; the proxy's "egress blocked" log line must not echo it.
func TestEgressProxy_LoggerScrubsSecrets(t *testing.T) {
	must := require.New(t)
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
			must.NoError(err, "serveEgressProxy returned an error on shutdown")
		case <-time.After(10 * time.Second):
			must.Fail("serveEgressProxy did not shut down within 10s")
		}
	})

	conn, err := net.Dial("tcp", addr)
	must.NoError(err, "dial proxy")
	defer func() { _ = conn.Close() }()

	host := scrubTestKey + ".example.com"
	line := "CONNECT " + host + ":443 HTTP/1.1\r\nHost: " + host + ":443\r\n\r\n"
	_, err = conn.Write([]byte(line))
	must.NoError(err, "write CONNECT")
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	must.NoError(err, "read CONNECT response")
	must.Equal(http.StatusForbidden, resp.StatusCode)

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
	must.NotEmpty(blocked, "no \"egress blocked\" log line within 5s: %s", logBuf.String())

	t.Run("omits the key", func(t *testing.T) {
		must := require.New(t)
		must.NotContains(blocked, scrubTestKey, "key leaked through the egress proxy logger")
	})
	t.Run("contains the redaction marker", func(t *testing.T) {
		must := require.New(t)
		must.Contains(blocked, "[REDACTED]")
	})
}
