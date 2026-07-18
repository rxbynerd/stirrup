package egressproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// ProbeAllowlist performs a DNS-only dry-run reachability check of an
// egress allowlist: it resolves each entry's hostname (using resolver,
// defaulting to net.DefaultResolver when nil) so a typo'd or NXDOMAIN
// destination is caught before the run starts. It never opens a TCP
// connection — see docs/configuration.md for the DNS-only rationale. A
// wildcard entry resolves its base domain as a best-effort signal.
//
// The allowlist is parsed with NewMatcher's rules, so a malformed entry
// fails the probe identically to how it would fail the proxy at run
// time. An empty allowlist is a no-op.
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
