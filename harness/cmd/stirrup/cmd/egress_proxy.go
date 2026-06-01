package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/rxbynerd/stirrup/harness/internal/executor/egressproxy"
	"github.com/rxbynerd/stirrup/harness/internal/security"
)

var egressProxyCmd = &cobra.Command{
	Use:   "egress-proxy",
	Short: "Run the egress allowlist proxy in the foreground",
	Long: `Run the in-process egress allowlist proxy as a standalone process. The
proxy terminates HTTPS CONNECT requests and forwards plain HTTP requests,
gating every destination against an FQDN allowlist; anything not on the
allowlist is refused.

This is the same proxy the container and k8s executors start in-process for
allowlist network mode, exposed as a long-running Deployment so a sandbox Pod
(which cannot start its own host-side proxy) can route HTTP_PROXY / HTTPS_PROXY
through it. Deploy it from examples/k8s/egress-proxy/ alongside a sandbox Pod
configured with --k8s-egress-proxy-url.

The allowlist is supplied via repeatable --allowlist entries and/or an
--allowlist-file (one entry per line; blank lines and #-comments ignored). An
empty allowlist denies every destination (fail closed). Entry syntax matches
the executor allowlist: bare FQDN (port 443), *.example.com wildcard subdomain,
or host:port. The process serves until it receives SIGINT or SIGTERM.`,
	Args: cobra.NoArgs,
	RunE: runEgressProxy,
}

func init() {
	f := egressProxyCmd.Flags()
	f.String("listen", ":8080", "host:port to listen on. A bare \":8080\" binds all interfaces, which a Pod behind a Service needs.")
	f.StringArray("allowlist", nil, "Repeatable allowlist entry (e.g. --allowlist api.example.com --allowlist *.github.com:443). Combined with --allowlist-file.")
	f.String("allowlist-file", "", "Path to a file with one allowlist entry per line. Blank lines and lines starting with # are ignored. Combined with --allowlist.")
	f.String("log-level", "info", "Log level: debug, info, warn, error")
	rootCmd.AddCommand(egressProxyCmd)
}

// egressProxyOptions is the resolved configuration for the egress-proxy
// subcommand, parsed from flags by runEgressProxy and consumed by
// serveEgressProxy. Splitting parse from serve keeps serveEgressProxy
// driveable from a test with a cancelable context (the CLI path supplies a
// signal-cancelled one).
type egressProxyOptions struct {
	// listen is the host:port to bind when listener is nil. Ignored when a
	// pre-bound listener is supplied.
	listen string
	// listener, when non-nil, is used directly instead of binding listen.
	// The CLI leaves it nil and binds listen; tests pass a pre-bound listener
	// to avoid a free-port-then-rebind TOCTOU that can flake under parallel
	// runs. serveEgressProxy owns closing it on the Start-failure path.
	listener  net.Listener
	allowlist []string
	level     slog.Level
}

func runEgressProxy(cmd *cobra.Command, _ []string) error {
	f := cmd.Flags()
	listen, _ := f.GetString("listen")
	allowlistFlag, _ := f.GetStringArray("allowlist")
	allowlistFile, _ := f.GetString("allowlist-file")
	logLevelStr, _ := f.GetString("log-level")

	var level slog.Level
	if err := level.UnmarshalText([]byte(logLevelStr)); err != nil {
		return parseError(fmt.Errorf("invalid --log-level %q: %w", logLevelStr, err))
	}

	allowlist := append([]string{}, allowlistFlag...)
	if allowlistFile != "" {
		fileEntries, err := readAllowlistFile(allowlistFile)
		if err != nil {
			return ioError(err)
		}
		allowlist = append(allowlist, fileEntries...)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return serveEgressProxy(ctx, egressProxyOptions{
		listen:    listen,
		allowlist: allowlist,
		level:     level,
	}, os.Stderr)
}

// serveEgressProxy binds the listener, starts the egress proxy, and serves
// until ctx is cancelled (SIGINT/SIGTERM on the CLI path), then shuts down
// with a bounded grace period. Logs and audit events are written to logW.
//
// Errors are returned wrapped in the CLI exit-code classes: an I/O failure
// (listen / shutdown) is exit 3, a malformed allowlist is exit 1.
func serveEgressProxy(ctx context.Context, opts egressProxyOptions, logW io.Writer) error {
	logger := slog.New(slog.NewTextHandler(logW, &slog.HandlerOptions{Level: opts.level}))

	// A SecurityLogger writing JSON lines gives the proxy the same
	// egress_allowed / egress_blocked audit surface the in-process executor
	// path produces, so a Deployment's pod logs carry the gating decisions.
	// runID is empty: a standalone proxy is not scoped to a single run.
	audit := security.NewSecurityLogger(logW, "")

	// Bind the listener explicitly so listen-host overrides (default ":8080")
	// take effect; egressproxy.Start only opens its own loopback listener when
	// none is supplied, which would ignore the listen flag. A caller-supplied
	// listener (tests) is used as-is.
	listener := opts.listener
	if listener == nil {
		var err error
		listener, err = net.Listen("tcp", opts.listen)
		if err != nil {
			return ioError(fmt.Errorf("listen on %q: %w", opts.listen, err))
		}
	}

	proxy, err := egressproxy.Start(ctx, egressproxy.Config{
		Allowlist: opts.allowlist,
		Listener:  listener,
		Security:  audit,
		Logger:    logger,
	})
	if err != nil {
		_ = listener.Close()
		// A malformed allowlist entry surfaces here; it is a configuration
		// error, not an I/O one.
		return validationError(fmt.Errorf("start egress proxy: %w", err))
	}

	logger.Info("egress proxy listening",
		slog.String("addr", proxy.Addr()),
		slog.Int("allowlist_entries", len(opts.allowlist)),
	)

	<-ctx.Done()
	logger.Info("egress proxy shutting down")

	// Bounded shutdown so a wedged in-flight tunnel cannot hold the process
	// open past the orchestrator's SIGTERM→SIGKILL grace window. A fresh
	// context is used because ctx is already cancelled by the time we get here.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := proxy.Stop(shutdownCtx); err != nil {
		return ioError(fmt.Errorf("stop egress proxy: %w", err))
	}
	return nil
}

// maxAllowlistFileBytes caps the --allowlist-file read at 1 MiB — far more
// than thousands of FQDN entries need, and consistent with the project's
// other bounded file reads (RunConfig 1 MiB, prompt 10 MiB). The cap defends
// against a runaway or hostile file exhausting memory at startup.
const maxAllowlistFileBytes int64 = 1 << 20 // 1 MiB

// readAllowlistFile reads one allowlist entry per line, skipping blank lines
// and #-prefixed comments. Trailing inline comments are NOT stripped — an
// FQDN never contains '#', and stripping mid-line could silently truncate a
// malformed entry the matcher should reject loudly. The read is capped at
// maxAllowlistFileBytes.
func readAllowlistFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open allowlist file %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	// Bound the read with io.LimitReader (cap+1 so an exactly-at-cap file is
	// allowed while an over-cap file is detectable). The scanner draws from
	// the limited reader, so no more than cap+1 bytes ever reach memory.
	limited := &countingReader{r: io.LimitReader(file, maxAllowlistFileBytes+1)}

	var entries []string
	scanner := bufio.NewScanner(limited)
	// Raise the scanner's max token size to the cap so a single long line is
	// drawn through the LimitReader (and counted) rather than tripping the
	// default 64 KiB token limit with a confusing "token too long" before the
	// byte-cap check below can govern. The LimitReader still bounds total
	// memory at cap+1 bytes.
	scanner.Buffer(make([]byte, 0, 64*1024), int(maxAllowlistFileBytes)+1)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		entries = append(entries, line)
	}
	scanErr := scanner.Err()
	// The cap check takes precedence over any scanner error: a file past the
	// cap must surface as the byte-cap error.
	if limited.n > maxAllowlistFileBytes {
		return nil, fmt.Errorf("allowlist file %q exceeds %d byte cap", path, maxAllowlistFileBytes)
	}
	if scanErr != nil {
		return nil, fmt.Errorf("read allowlist file %q: %w", path, scanErr)
	}
	return entries, nil
}

// countingReader tallies bytes read so the caller can detect that an
// io.LimitReader hit its ceiling (read cap+1 bytes) versus stopped at EOF.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
