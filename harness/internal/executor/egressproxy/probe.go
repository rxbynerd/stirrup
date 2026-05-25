package egressproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// ProbeAllowlist performs a dry-run preflight reachability check of an
// egress allowlist. For each entry it resolves the destination hostname
// via DNS (using the supplied resolver, defaulting to net.DefaultResolver
// when nil) so an operator catches a typo'd or NXDOMAIN destination before
// the run starts. A wildcard entry ("*.example.com") has no single
// resolvable host, so its base domain ("example.com") is resolved as a
// best-effort signal that the zone exists.
//
// The probe is DNS-only by design: it never opens a TCP connection to the
// destination. A TCP dial would be a stronger signal but turns a cheap
// dry-run into a fan-out of outbound connections to every allowlisted
// host, which is both slow and surprising for a "check my config" command.
// DNS resolution catches the common misconfiguration (a bad hostname)
// without that cost.
//
// The allowlist is parsed with the same NewMatcher rules the proxy
// enforces, so a malformed entry fails the probe identically to how it
// would fail the proxy at run time. An empty allowlist is a no-op (nil):
// allowlist network mode with no entries denies all egress, which is a
// valid — if restrictive — configuration the probe should not flag.
func ProbeAllowlist(ctx context.Context, allowlist []string, resolver *net.Resolver) error {
	matcher, err := NewMatcher(allowlist)
	if err != nil {
		return fmt.Errorf("egress allowlist parse: %w", err)
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}

	var failures []string
	for _, entry := range matcher.Entries() {
		host := probeHost(entry)
		if host == "" {
			continue
		}
		if _, err := resolver.LookupHost(ctx, host); err != nil {
			failures = append(failures, fmt.Sprintf("%s (%v)", entry, err))
		}
	}
	if len(failures) > 0 {
		return errors.New("egress allowlist destinations unresolvable: " + strings.Join(failures, "; "))
	}
	return nil
}

// probeHost extracts the resolvable hostname from a matcher entry of the
// form "host:port" or "*.host:port". The wildcard prefix is stripped so
// the base zone is resolved. Returns "" when the entry has no host
// component (which NewMatcher would already have rejected, so this is
// defence-in-depth).
func probeHost(entry string) string {
	host := entry
	if idx := strings.LastIndex(entry, ":"); idx >= 0 {
		host = entry[:idx]
	}
	host = strings.TrimPrefix(host, "*.")
	return host
}
