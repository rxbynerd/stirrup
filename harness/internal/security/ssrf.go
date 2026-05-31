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

// IsPublicIP reports whether ip is a globally routable unicast address —
// i.e. not loopback, private, multicast, link-local, or unspecified.
func IsPublicIP(ip net.IP) bool {
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
