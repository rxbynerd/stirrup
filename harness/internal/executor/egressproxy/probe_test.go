package egressproxy

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestProbeAllowlist_EmptyIsNoop(t *testing.T) {
	if err := ProbeAllowlist(context.Background(), nil, net.DefaultResolver); err != nil {
		t.Fatalf("empty allowlist should be a no-op, got: %v", err)
	}
}

func TestProbeAllowlist_MalformedEntry(t *testing.T) {
	err := ProbeAllowlist(context.Background(), []string{"bad host with spaces"}, net.DefaultResolver)
	if err == nil {
		t.Fatal("expected parse error for malformed allowlist entry")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse failure, got: %v", err)
	}
}

func TestProbeAllowlist_Unresolvable(t *testing.T) {
	// A custom resolver whose Dial always errors makes every lookup fail,
	// standing in for an NXDOMAIN/unreachable destination without depending
	// on a hostname that is guaranteed not to resolve in CI.
	failing := &net.Resolver{
		PreferGo: true,
		Dial: func(_ context.Context, _, _ string) (net.Conn, error) {
			return nil, fmt.Errorf("no DNS")
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := ProbeAllowlist(ctx, []string{"example.com", "*.api.example.com:8443"}, failing)
	if err == nil {
		t.Fatal("expected error when destinations do not resolve")
	}
	if !strings.Contains(err.Error(), "unresolvable") {
		t.Errorf("error should report unresolvable destinations, got: %v", err)
	}
	// Both the exact host and the wildcard base zone should be reported.
	if !strings.Contains(err.Error(), "example.com") {
		t.Errorf("error should name the failing destinations, got: %v", err)
	}
}

func TestProbeHost(t *testing.T) {
	cases := map[string]string{
		"example.com:443":        "example.com",
		"*.api.example.com:8443": "api.example.com",
		"host:80":                "host",
	}
	for in, want := range cases {
		if got := probeHost(in); got != want {
			t.Errorf("probeHost(%q) = %q, want %q", in, got, want)
		}
	}
}
