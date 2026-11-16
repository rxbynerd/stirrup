// Package httpsput uploads one object with a single HTTP PUT to a
// destination named by a types.HTTPSPutConfig: a presigned object-store
// URL or an endpoint a control plane serves.
//
// The URL and the header values are capabilities. Every error this
// package returns, and everything a Target prints, names at most the
// destination's origin (scheme and host); callers may log and record
// them without further redaction.
package httpsput

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

const (
	dialTimeout           = 30 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	responseHeaderTimeout = 60 * time.Second

	// maxErrorBodyBytes bounds how much of a non-2xx response is read to
	// find the store's error code.
	maxErrorBodyBytes = 4 * 1024
)

// Target is a resolved destination. Its String, GoString and LogValue
// forms are the origin, so a Target that reaches a log or an error by
// accident does not disclose the URL or the headers. The capability
// sits behind a pointer so that a Target printed as an unexported field
// of another struct, where fmt consults no method, shows an address.
type Target struct {
	dest *destination
}

type destination struct {
	url     *url.URL
	headers http.Header
}

// Resolve resolves cfg's references through secrets and checks the
// resulting URL: https://, or http:// to a loopback host, with a host and
// without userinfo or a fragment.
func Resolve(ctx context.Context, secrets security.SecretStore, cfg *types.HTTPSPutConfig) (Target, error) {
	if cfg == nil {
		return Target{}, errors.New("no HTTPS PUT destination is configured")
	}
	if secrets == nil {
		return Target{}, errors.New("no secret store is available to resolve the destination URL")
	}
	if !strings.HasPrefix(cfg.URLRef, "secret://") {
		return Target{}, errors.New("urlRef is not a secret reference")
	}
	raw, err := secrets.Resolve(ctx, cfg.URLRef)
	if err != nil {
		return Target{}, fmt.Errorf("resolve urlRef: %w", err)
	}
	u, err := parseDestination(raw)
	if err != nil {
		return Target{}, fmt.Errorf("urlRef resolved to an unusable URL: %w", err)
	}

	headers := http.Header{}
	for name, value := range cfg.Headers {
		if strings.HasPrefix(value, "secret://") {
			resolved, err := secrets.Resolve(ctx, value)
			if err != nil {
				return Target{}, fmt.Errorf("resolve header %q: %w", name, err)
			}
			value = strings.TrimSpace(resolved)
		}
		switch {
		case value == "" || !types.ValidHTTPSPutHeaderValue(value):
			return Target{}, fmt.Errorf("header %q resolved to an empty value or one with a control character", name)
		case len(value) > types.MaxHTTPSPutHeaderValueBytes:
			return Target{}, fmt.Errorf("header %q resolved to more than %d bytes", name, types.MaxHTTPSPutHeaderValueBytes)
		}
		headers.Set(name, value)
	}
	return Target{dest: &destination{url: u, headers: headers}}, nil
}

// parseDestination parses a resolved URL. Its errors describe the fault
// without quoting any part of the URL.
func parseDestination(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, errors.New("it does not parse as a URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, errors.New("its scheme is not https://")
	}
	if u.Hostname() == "" {
		return nil, errors.New("it has no host")
	}
	if u.User != nil {
		return nil, errors.New("it carries userinfo; put credentials in the query string or a header")
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return nil, errors.New("it carries a fragment")
	}
	if u.Scheme == "http" && !security.IsLoopbackHost(u.Hostname()) {
		return nil, errors.New("the http:// scheme is accepted only for a loopback host; use https://")
	}
	return u, nil
}

// Origin returns the destination's scheme and host, the only part of it
// that may be logged or recorded. Empty for an unresolved Target.
func (t Target) Origin() string {
	if t.dest == nil {
		return ""
	}
	return (&url.URL{Scheme: t.dest.url.Scheme, Host: t.dest.url.Host}).String()
}

func (t Target) String() string { return t.Origin() }

func (t Target) GoString() string { return fmt.Sprintf("httpsput.Target{%s}", t.Origin()) }

func (t Target) LogValue() slog.Value { return slog.StringValue(t.Origin()) }

type loopbackOnlyKey struct{}

// dialControl is the dialer's ControlContext; tests wrap it to observe
// what reaches the dial.
var dialControl = checkDialAddress

// NewClient returns the client Put sends with. timeout bounds one whole
// exchange, body included, so callers size it to the object: there is no
// streaming response to keep open. The client follows no redirects, since
// a redirect would carry the configured headers to a host the operator
// did not name, uses no proxy, and keeps no connection alive: an upload
// is one request.
func NewClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: dialTimeout,
		ControlContext: func(ctx context.Context, network, address string, c syscall.RawConn) error {
			return dialControl(ctx, network, address, c)
		},
	}
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   tlsHandshakeTimeout,
			ResponseHeaderTimeout: responseHeaderTimeout,
			DisableKeepAlives:     true,
		},
	}
}

// checkDialAddress runs on the resolved address of every connection. It
// refuses cloud metadata addresses (security.IsCloudMetadataIP, which
// includes every link-local address), multicast and the unspecified
// address, and holds an http:// upload to loopback
// whatever its host name resolved to. Private addresses are allowed: a
// control plane's own endpoint is commonly in-cluster.
func checkDialAddress(ctx context.Context, _, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("refusing to connect: unparseable dial address")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("refusing to connect: dial address is not an IP")
	}
	if security.IsCloudMetadataIP(ip) || ip.IsMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("refusing to connect to %s: cloud metadata, link-local, multicast and unspecified addresses are not upload destinations", ip)
	}
	if loopbackOnly, _ := ctx.Value(loopbackOnlyKey{}).(bool); loopbackOnly && !ip.IsLoopback() {
		return fmt.Errorf("refusing to connect to %s: an http:// destination must be loopback", ip)
	}
	return nil
}

// StatusError is Put's error for a response outside 2xx.
type StatusError struct {
	StatusCode int
	// Code is the object store's own error code (the <Code> element of
	// an S3, GCS or Azure Blob error document), when the response
	// carried one.
	Code string
}

func (e *StatusError) Error() string {
	text := http.StatusText(e.StatusCode)
	switch {
	case e.Code != "" && text != "":
		return fmt.Sprintf("destination returned HTTP %d %s (%s)", e.StatusCode, text, e.Code)
	case e.Code != "":
		return fmt.Sprintf("destination returned HTTP %d (%s)", e.StatusCode, e.Code)
	case text != "":
		return fmt.Sprintf("destination returned HTTP %d %s", e.StatusCode, text)
	}
	return fmt.Sprintf("destination returned HTTP %d", e.StatusCode)
}

// Transient reports whether another attempt could succeed.
func (e *StatusError) Transient() bool {
	return e.StatusCode == http.StatusRequestTimeout || e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// storeErrorCode matches the error code of an XML error document. The
// rest of the document is discarded: S3 echoes the request's signature
// and canonical query string in it.
var storeErrorCode = regexp.MustCompile(`<Code>([A-Za-z0-9._-]{1,64})</Code>`)

// Put uploads body to t with one PUT. contentType is sent unless the
// target's headers carry their own Content-Type. A response outside 2xx,
// redirects included, is a *StatusError.
func Put(ctx context.Context, client *http.Client, t Target, body []byte, contentType string) error {
	if client == nil {
		return errors.New("https put: http client is required")
	}
	if t.dest == nil {
		return errors.New("https put: destination is not resolved")
	}
	if t.dest.url.Scheme == "http" {
		ctx = context.WithValue(ctx, loopbackOnlyKey{}, true)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, t.dest.url.String(), bytes.NewReader(body))
	if err != nil {
		return errors.New("https put: build request")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for name, values := range t.dest.headers {
		req.Header[name] = values
	}
	req.ContentLength = int64(len(body))

	resp, err := client.Do(req)
	if err != nil {
		// A *url.Error's text embeds the request URL.
		return fmt.Errorf("put to %s: %w", t.Origin(), security.UnwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	status := &StatusError{StatusCode: resp.StatusCode}
	if m := storeErrorCode.FindSubmatch(msg); m != nil {
		status.Code = string(m[1])
	}
	return fmt.Errorf("put to %s: %w", t.Origin(), status)
}
