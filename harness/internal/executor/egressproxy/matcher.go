// Package egressproxy implements an in-process HTTP forward proxy that
// terminates HTTPS CONNECT requests and forwards plain HTTP requests, gating
// every request against an FQDN allowlist. It is intended to run on the
// host network namespace next to a sandboxed container; the container reaches
// the proxy via HTTP_PROXY / HTTPS_PROXY environment variables.
//
// The matcher in this file enforces the allowlist. Wildcard syntax follows
// the cookie-style `*.example.com` rule: it matches any subdomain (one or
// more labels) of `example.com` but NOT the bare `example.com` itself, in
// keeping with RFC 6125 §6.4.3 guidance. Trailing dots are canonicalised
// and matching is case-insensitive throughout. Allowlist entries default
// to port 443 unless explicitly suffixed (e.g. `example.com:80`).
package egressproxy

import (
	"fmt"
	"strconv"
	"strings"
)

// defaultAllowPort is the implicit port for an allowlist entry with no
// explicit port suffix. The issue requires HTTPS-only egress by default;
// any other destination port must be opted in by suffixing the entry.
const defaultAllowPort = 443

// matcherEntry is a single parsed allowlist row.
type matcherEntry struct {
	host     string // canonical lower-case FQDN, no trailing dot, no leading "*."
	wildcard bool   // true if the entry was prefixed with "*."
	port     int    // canonical port (defaults to 443 when not specified)
}

// Matcher decides whether a (host, port) pair is permitted by the
// configured allowlist. Construct one with NewMatcher; the result is
// safe for concurrent reads but not for mutation after construction
// (we deliberately do not expose a hot-reload path — see CLAUDE.md).
type Matcher struct {
	entries []matcherEntry
}

// NewMatcher parses a slice of allowlist strings into a Matcher. Entries
// may take the forms:
//
//	example.com           // exact match on example.com:443
//	*.example.com         // any subdomain of example.com on :443
//	example.com:80        // exact match on example.com:80
//	*.example.com:8080    // any subdomain of example.com on :8080
//
// Empty/whitespace-only entries are ignored. Malformed entries (e.g.
// non-numeric port, IDN punycode that fails canonicalisation) return an
// error so configuration mistakes fail loudly at construction time.
func NewMatcher(allowlist []string) (*Matcher, error) {
	entries := make([]matcherEntry, 0, len(allowlist))
	for _, raw := range allowlist {
		entry, ok, err := parseAllowlistEntry(raw)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		entries = append(entries, entry)
	}
	return &Matcher{entries: entries}, nil
}

// Match reports whether `host` on `port` is permitted by the allowlist.
// The host argument may include a trailing dot and/or upper-case characters;
// it is canonicalised to lower-case without a trailing dot before matching.
//
// Match never resolves DNS itself: it operates purely on the requested
// hostname so a malicious DNS answer cannot widen the allowlist.
func (m *Matcher) Match(host string, port int) bool {
	if m == nil {
		return false
	}
	canonical := canonicaliseHost(host)
	if canonical == "" {
		return false
	}
	for _, entry := range m.entries {
		if entry.port != port {
			continue
		}
		if entry.wildcard {
			// "*.example.com" matches "a.example.com" and "a.b.example.com"
			// but not "example.com" itself. We require at least one
			// label between the canonical host and the entry host.
			suffix := "." + entry.host
			if strings.HasSuffix(canonical, suffix) && len(canonical) > len(suffix) {
				return true
			}
			continue
		}
		if canonical == entry.host {
			return true
		}
	}
	return false
}

// Entries returns a copy of the parsed allowlist. Useful for logging the
// effective configuration at startup; the slice is safe to mutate.
func (m *Matcher) Entries() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.entries))
	for _, e := range m.entries {
		host := e.host
		if e.wildcard {
			host = "*." + host
		}
		out = append(out, fmt.Sprintf("%s:%d", host, e.port))
	}
	return out
}

// canonicaliseHost lowercases a hostname and trims a trailing dot. The
// FQDN "example.com." and the FQDN "example.com" are equivalent for
// matching (RFC 1034 §3.1) so they must compare equal.
func canonicaliseHost(host string) string {
	host = strings.TrimSpace(host)
	host = strings.ToLower(host)
	host = strings.TrimSuffix(host, ".")
	return host
}

// parseAllowlistEntry parses one row of the allowlist. The boolean return
// is false when the row was empty (and therefore skipped without error).
func parseAllowlistEntry(raw string) (matcherEntry, bool, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return matcherEntry{}, false, nil
	}

	hostPart, portPart := trimmed, ""
	if idx := strings.LastIndex(trimmed, ":"); idx >= 0 {
		// IPv6 literals would contain multiple colons; we don't support
		// IP literals in the allowlist v1 (only FQDNs), so any colon is
		// treated as the host:port separator.
		hostPart = trimmed[:idx]
		portPart = trimmed[idx+1:]
	}

	wildcard := false
	if strings.HasPrefix(hostPart, "*.") {
		wildcard = true
		hostPart = hostPart[2:]
	}

	host := canonicaliseHost(hostPart)
	if host == "" {
		return matcherEntry{}, false, fmt.Errorf("egressproxy: empty host in allowlist entry %q", raw)
	}
	if strings.ContainsAny(host, " \t/") {
		return matcherEntry{}, false, fmt.Errorf("egressproxy: invalid characters in allowlist entry %q", raw)
	}

	port := defaultAllowPort
	if portPart != "" {
		p, err := strconv.Atoi(portPart)
		if err != nil {
			return matcherEntry{}, false, fmt.Errorf("egressproxy: invalid port in allowlist entry %q: %w", raw, err)
		}
		if p <= 0 || p > 65535 {
			return matcherEntry{}, false, fmt.Errorf("egressproxy: port out of range in allowlist entry %q", raw)
		}
		port = p
	}

	return matcherEntry{host: host, wildcard: wildcard, port: port}, true, nil
}
