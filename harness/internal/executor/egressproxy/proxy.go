package egressproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Default timeouts. The harness's own runs cap commands at five minutes
// (see executor.maxTimeout) so a 60s read deadline on a single proxy hop
// is generous; long-poll-style usage is not a target for v1.
const (
	defaultDialTimeout = 10 * time.Second
	defaultReadTimeout = 60 * time.Second
	defaultIdleTimeout = 120 * time.Second
)

// Config configures Start(). All fields except Allowlist have sensible
// zero-value defaults; the zero value of Allowlist is an empty allowlist
// which causes every request to be denied (fail closed).
type Config struct {
	// Allowlist is the slice of FQDN entries the proxy permits. Same
	// syntax as documented on Matcher.
	Allowlist []string

	// Listener, when non-nil, is used directly. When nil, Start() opens a
	// fresh tcp4 listener on a random port. The listener is closed by
	// Stop().
	Listener net.Listener

	// Security receives egress_allowed / egress_blocked events. nil disables
	// audit emission (acceptable in tests; production setups should always
	// wire one).
	Security SecurityEventEmitter

	// Logger receives debug-level access logs. nil uses slog.Default().
	// We never log full URLs (path/query may contain secrets); only
	// method, host:port, and the gating decision.
	Logger *slog.Logger

	// DialTimeout / ReadTimeout / IdleTimeout override the package
	// defaults. Zero means "use the default".
	DialTimeout time.Duration
	ReadTimeout time.Duration
	IdleTimeout time.Duration
}

// Proxy is a running egress proxy. It is safe to call Stop() exactly once.
type Proxy struct {
	matcher  *Matcher
	listener net.Listener
	server   *http.Server
	security SecurityEventEmitter
	logger   *slog.Logger

	dialTimeout time.Duration
	readTimeout time.Duration

	dialer *net.Dialer

	mu       sync.Mutex
	stopped  bool
	stopErr  error
	shutdown chan struct{} // closed by Stop to release the ctx-watcher goroutine
}

// Start parses the allowlist, opens (or adopts) a listener, and begins
// serving. It returns once the listener is bound and the goroutine is
// running; the returned Proxy can be queried for its address.
//
// Start fails if the allowlist contains malformed entries or the listener
// cannot be opened. The returned Proxy must be stopped via Stop() to release
// the listener — except when the supplied ctx is cancelled, in which case
// the proxy stops itself with a bounded grace period (5 seconds). This
// guarantees a leaked listener cannot outlive the caller scope even if the
// caller forgets to call Stop or panics before the defer fires (M4).
func Start(ctx context.Context, cfg Config) (*Proxy, error) {
	matcher, err := NewMatcher(cfg.Allowlist)
	if err != nil {
		return nil, err
	}

	listener := cfg.Listener
	if listener == nil {
		// tcp4 specifically: the container reaches the host gateway over
		// IPv4 and we have no need for IPv6 here.
		listener, err = net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("egressproxy: open listener: %w", err)
		}
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	dialTimeout := cfg.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = defaultDialTimeout
	}
	readTimeout := cfg.ReadTimeout
	if readTimeout <= 0 {
		readTimeout = defaultReadTimeout
	}
	idleTimeout := cfg.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = defaultIdleTimeout
	}

	p := &Proxy{
		matcher:     matcher,
		listener:    listener,
		security:    cfg.Security,
		logger:      logger.With(slog.String("component", "egressproxy")),
		dialTimeout: dialTimeout,
		readTimeout: readTimeout,
		dialer:      &net.Dialer{Timeout: dialTimeout},
		shutdown:    make(chan struct{}),
	}

	p.server = &http.Server{
		Handler:      http.HandlerFunc(p.handle),
		ReadTimeout:  readTimeout,
		WriteTimeout: readTimeout,
		IdleTimeout:  idleTimeout,
		// We do our own logging on errors; silence the default writer.
		ErrorLog: nil,
	}

	go func() {
		if err := p.server.Serve(p.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			p.logger.Error("proxy server stopped with error", slog.String("err", err.Error()))
		}
	}()

	// Tie the proxy lifetime to the caller's context. When ctx is
	// cancelled we trigger Stop with a bounded timeout so the listener
	// is always released; this defends against early-return paths in
	// the caller that skip a manual Stop call.
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = p.Stop(stopCtx)
			case <-p.shutdown:
			}
		}()
	}

	return p, nil
}

// Addr returns the bound address as host:port suitable for HTTP_PROXY.
func (p *Proxy) Addr() string {
	if p == nil || p.listener == nil {
		return ""
	}
	return p.listener.Addr().String()
}

// Stop closes the listener and shuts the underlying server down. It is
// idempotent and safe to call from multiple goroutines.
func (p *Proxy) Stop(ctx context.Context) error {
	p.mu.Lock()
	if p.stopped {
		err := p.stopErr
		p.mu.Unlock()
		return err
	}
	p.stopped = true
	// Release the ctx-watcher goroutine started by Start so it does not
	// linger after Stop returns. Closing under the lock ensures the
	// "already stopped" early return above never closes twice.
	if p.shutdown != nil {
		close(p.shutdown)
		p.shutdown = nil
	}
	p.mu.Unlock()

	err := p.server.Shutdown(ctx)
	p.mu.Lock()
	p.stopErr = err
	p.mu.Unlock()
	return err
}

// handle dispatches an incoming proxy request. CONNECT requests open a
// raw TCP tunnel after SNI verification; all other methods are forwarded
// as plain HTTP. We never log full URLs.
func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleHTTP(w, r)
}

// handleConnect terminates a CONNECT request, verifies the destination
// against the allowlist, optionally peeks the TLS SNI to defeat a tampered
// HOST header, and splices a bidirectional tunnel to the upstream.
func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := parseHostPort(r.Host)
	if err != nil {
		p.deny(w, r, host, port, "invalid_host", http.StatusBadRequest)
		return
	}

	if !p.matcher.Match(host, port) {
		p.deny(w, r, host, port, "not_allowlisted", http.StatusForbidden)
		return
	}

	// Hijack the client connection so we can both write the 200 ourselves
	// and own the byte stream for splicing.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		p.logger.Error("response writer does not support hijack")
		http.Error(w, "proxy unavailable", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		p.logger.Error("hijack failed", slog.String("err", err.Error()))
		return
	}

	// Fail closed on any post-hijack error: we close clientConn before
	// returning. The upstream connection (if any) is closed in the splice
	// goroutines below.
	defer func() { _ = clientConn.Close() }()

	// Clear both deadlines that net/http inherited from
	// {Read,Write}Timeout. They were intended for the request/response
	// turn — once we hijack and splice we own the lifetime, and the
	// per-stream upstream timeouts handle health. Failing to clear the
	// write deadline here truncates long CONNECT tunnels at 60s (S1).
	if err := clientConn.SetDeadline(time.Time{}); err != nil {
		p.logger.Debug("clear deadlines failed", slog.String("err", err.Error()))
	}

	// Peek the ClientHello so we can verify SNI matches the CONNECT host
	// BEFORE writing the 200 response. Sending the 200 first would force
	// us to deny() on a hijacked connection that the client already saw
	// as "tunnel established" — confusing both audit logs and clients —
	// and would also count an "egress allowed" tunnel that we then tore
	// down. A short read deadline keeps a misbehaving client from
	// holding a hijacked socket forever. We restore the cleared deadline
	// after the peek succeeds.
	if err := clientConn.SetReadDeadline(time.Now().Add(p.readTimeout)); err != nil {
		p.logger.Debug("set read deadline failed", slog.String("err", err.Error()))
		return
	}

	// If the client buffered any bytes via the hijack we must consume them
	// from the bufio.Reader instead of the raw conn — reading the conn
	// would skip those bytes.
	var clientReader io.Reader = clientConn
	if clientBuf != nil && clientBuf.Reader != nil && clientBuf.Reader.Buffered() > 0 {
		clientReader = io.MultiReader(clientBuf.Reader, clientConn)
	}

	rawHello, sni, sniErr := peekTLSClientHello(clientReader)

	// Treat absent SNI as a deny: every modern TLS 1.2/1.3 client emits
	// SNI for HTTPS, so the absence is a tampering signal — most likely
	// a crafted client suppressing SNI to bypass the cross-check below.
	// Without this, a CONNECT to an allowlisted host could tunnel TLS
	// for a different hostname (M2).
	if errors.Is(sniErr, errSNINotPresent) {
		p.deny(w, r, host, port, "sni_absent", 0)
		return
	}
	if sniErr != nil {
		p.deny(w, r, host, port, "tls_parse_failed", 0)
		return
	}
	if canonicaliseHost(sni) != canonicaliseHost(host) {
		p.deny(w, r, host, port, "sni_mismatch", 0)
		return
	}

	// SNI checks out: tell the client the tunnel is open.
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		p.logger.Debug("write 200 to client failed", slog.String("err", err.Error()))
		return
	}

	// Reset the deadline before we splice — the upstream may legitimately
	// idle for a while during a long-running TLS connection.
	if err := clientConn.SetReadDeadline(time.Time{}); err != nil {
		p.logger.Debug("clear read deadline failed", slog.String("err", err.Error()))
	}

	upstream, err := p.dialer.DialContext(r.Context(), "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		p.deny(w, r, host, port, "upstream_dial_failed", 0)
		return
	}
	defer func() { _ = upstream.Close() }()

	p.allow(host, port, "CONNECT")

	// Replay the ClientHello to the upstream first.
	if _, err := upstream.Write(rawHello); err != nil {
		p.logger.Debug("replay ClientHello to upstream failed", slog.String("err", err.Error()))
		return
	}

	// Splice. Use io.Copy in both directions; close write half on EOF so
	// the peer sees half-close and exits its own copy.
	splice(clientConn, upstream)
}

// handleHTTP forwards a non-CONNECT proxy request to the upstream after
// allowlist verification.
func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	host, port, err := parseHostPort(hostHeader(r))
	if err != nil {
		p.deny(w, r, host, port, "invalid_host", http.StatusBadRequest)
		return
	}

	if !p.matcher.Match(host, port) {
		p.deny(w, r, host, port, "not_allowlisted", http.StatusForbidden)
		return
	}

	// Strip hop-by-hop headers per RFC 7230 §6.1 before forwarding.
	outReq := r.Clone(r.Context())
	stripHopByHopHeaders(outReq.Header)
	outReq.RequestURI = ""

	// Build a fresh transport per request to keep proxy lifetime tied to
	// the proxy instance rather than the global default. The destination
	// is unconditional: the URL is already absolute on a proxy request.
	transport := &http.Transport{
		DialContext: p.dialer.DialContext,
		// Disable connection reuse to keep the proxy stateless: if we
		// pooled connections, a second request for a different host
		// might get a connection from the wrong pool. The proxy is not
		// in the latency hot path.
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()

	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		p.deny(w, r, host, port, "upstream_request_failed", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	p.allow(host, port, r.Method)

	stripHopByHopHeaders(resp.Header)
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// deny writes a 403 (or other status) to the client and emits an
// egress_blocked security event. It is safe to call before or after a
// hijack: when statusCode == 0 the client connection is assumed to be
// already hijacked and is left to the caller's defer to close.
func (p *Proxy) deny(w http.ResponseWriter, r *http.Request, host string, port int, reason string, statusCode int) {
	if statusCode != 0 {
		// Set status before any body so net/http actually emits the code.
		http.Error(w, "egress denied", statusCode)
	}
	p.logger.Info("egress blocked",
		slog.String("method", methodOf(r)),
		slog.String("host", host),
		slog.Int("port", port),
		slog.String("reason", reason),
	)
	if p.security != nil {
		p.security.Emit("warn", "egress_blocked", map[string]any{
			"host":   host,
			"port":   port,
			"reason": reason,
			"method": methodOf(r),
		})
	}
}

// allow emits an egress_allowed security event. It is called after the
// upstream connection (CONNECT) or response (plain HTTP) is established.
func (p *Proxy) allow(host string, port int, method string) {
	p.logger.Debug("egress allowed",
		slog.String("method", method),
		slog.String("host", host),
		slog.Int("port", port),
	)
	if p.security != nil {
		p.security.Emit("info", "egress_allowed", map[string]any{
			"host":   host,
			"port":   port,
			"method": method,
		})
	}
}

// methodOf returns the request method or a placeholder when r is nil.
// We never log paths or queries — those may carry secrets.
func methodOf(r *http.Request) string {
	if r == nil {
		return ""
	}
	return r.Method
}

// hostHeader returns the host portion of r. For a proxy HTTP request the
// client sends the absolute URL; r.Host carries the URL host. For requests
// without an absolute URL we fall back to the Host header.
func hostHeader(r *http.Request) string {
	if r.URL != nil && r.URL.Host != "" {
		return r.URL.Host
	}
	return r.Host
}

// parseHostPort splits "host:port" into a canonical lower-case host (no
// trailing dot) and an int port. When the input has no port, port defaults
// to 443 to mirror the matcher's default.
func parseHostPort(hostPort string) (string, int, error) {
	if hostPort == "" {
		return "", 0, errors.New("empty host")
	}
	host, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		// Likely no port present. Default to 443 (matches matcher).
		return canonicaliseHost(hostPort), 443, nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, fmt.Errorf("parse port: %w", err)
	}
	return canonicaliseHost(host), port, nil
}

// hopByHopHeaders are forbidden in end-to-end forwarding per RFC 7230 §6.1.
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

func stripHopByHopHeaders(h http.Header) {
	// First read the Connection header — it lists tokens that are also
	// hop-by-hop for this connection only.
	if conn := h.Get("Connection"); conn != "" {
		for _, token := range splitConnectionHeader(conn) {
			h.Del(token)
		}
	}
	for _, key := range hopByHopHeaders {
		h.Del(key)
	}
}

// splitConnectionHeader is a small comma-split that trims whitespace.
func splitConnectionHeader(v string) []string {
	out := make([]string, 0, 4)
	start := 0
	for i := 0; i <= len(v); i++ {
		if i == len(v) || v[i] == ',' {
			tok := v[start:i]
			// Trim leading/trailing whitespace manually to keep this dep-free.
			for len(tok) > 0 && (tok[0] == ' ' || tok[0] == '\t') {
				tok = tok[1:]
			}
			for len(tok) > 0 && (tok[len(tok)-1] == ' ' || tok[len(tok)-1] == '\t') {
				tok = tok[:len(tok)-1]
			}
			if tok != "" {
				out = append(out, tok)
			}
			start = i + 1
		}
	}
	return out
}

// splice runs two io.Copy goroutines, closing the write half of each peer
// when the other peer's reader hits EOF so half-close semantics propagate
// correctly. Errors during copy are logged at debug level only.
func splice(a, b net.Conn) {
	closeWrite := func(c net.Conn) {
		// TCPConn supports CloseWrite; if not, we fall back to Close.
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = c.Close()
		}
	}

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(b, a)
		closeWrite(b)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(a, b)
		closeWrite(a)
		done <- struct{}{}
	}()
	<-done
	<-done
}
