package security

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// ValidatePublicHost is the canonical SSRF guard shared by every harness
// component that dials an operator- or model-supplied host (web_fetch, the
// MCP client). It refuses "localhost" and any host that resolves to a
// loopback, private, link-local, multicast, or unspecified address, so a
// request cannot be steered at the harness's own loopback services, the
// cloud metadata endpoint, or other internal infrastructure (CWE-918).
//
// When host is a literal IP it is checked directly. When it is a name it is
// resolved and EVERY returned address must be public — a single private
// answer fails the host, which also blunts DNS-rebinding answers that mix a
// public and a private record. Callers that dial through a transport
// DialContext built with SafeDialContext re-run this check at connect time,
// closing the resolve→dial rebinding window; see docs/security.md.
func ValidatePublicHost(host string) error {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return fmt.Errorf("url must include a host")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return fmt.Errorf("refusing to reach private host %q", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !IsPublicIP(ip) {
			return fmt.Errorf("refusing to reach private host %q", host)
		}
		return nil
	}

	addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if err != nil {
		return fmt.Errorf("resolve host %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("resolve host %q: no addresses found", host)
	}
	for _, addr := range addrs {
		if !IsPublicIP(addr.IP) {
			return fmt.Errorf("refusing to reach private host %q", host)
		}
	}
	return nil
}

// cgnatNet is the RFC 6598 carrier-grade-NAT shared address space
// (100.64.0.0/10). Go's net.IP.IsPrivate covers only RFC 1918 and RFC 4193,
// not this range, so it is checked explicitly: on a deployment target such as
// GCP Cloud Run that routes 100.64/10 to internal subnets, a host in this
// range is a private target an SSRF should not reach.
var cgnatNet = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("100.64.0.0/10")
	return n
}()

// IsLoopbackHost reports whether host is a loopback target — the literal
// "localhost"/"*.localhost" names or a loopback IP literal. It is the single
// definition shared by every loopback-exemption site (the MCP connect-time
// gate and the MCP dial-time guard) so the two cannot drift; net.ParseIP does
// not resolve the "localhost" name, which is why a name check is required in
// addition to the IP check.
func IsLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// IsPublicIP reports whether ip is a globally routable unicast address —
// i.e. not loopback, private (incl. RFC 6598 CGNAT), multicast, link-local,
// or unspecified.
func IsPublicIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil && cgnatNet.Contains(v4) {
		return false
	}
	return !ip.IsLoopback() &&
		!ip.IsPrivate() &&
		!ip.IsMulticast() &&
		!ip.IsLinkLocalMulticast() &&
		!ip.IsLinkLocalUnicast() &&
		!ip.IsUnspecified()
}

// SafeDialContext returns a transport DialContext that re-validates the
// dialled host with ValidatePublicHost before connecting, closing the
// DNS-rebinding window between an initial resolve and the actual dial. When
// allowPrivateHosts is true the guard is skipped (local-dev / explicit
// opt-in paths).
func SafeDialContext(allowPrivateHosts bool, timeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if !allowPrivateHosts {
			if err := ValidatePublicHost(host); err != nil {
				return nil, err
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
	}
}

// LoopbackAwareDialContext returns a DialContext that admits loopback
// addresses (for local-dev servers reached over http://localhost) but
// re-runs the SSRF guard against every other dialled address. It is the
// MCP-client variant of SafeDialContext: a remote server URI that passed the
// connect-time host check but whose DNS later rebinds to a non-loopback
// private/reserved address (10.0.0.0/8, the 169.254.169.254 metadata
// endpoint, …) is refused at dial time, while a legitimately-local server is
// not. Loopback is the only relaxation because a remote http:// URI is
// already rejected upstream, so any non-loopback target here is a remote one
// that must be public.
func LoopbackAwareDialContext(timeout time.Duration) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if !IsLoopbackHost(host) {
			if err := ValidatePublicHost(host); err != nil {
				return nil, err
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
	}
}
