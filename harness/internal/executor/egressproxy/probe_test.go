package egressproxy

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProbeAllowlist_EmptyIsNoop(t *testing.T) {
	err := ProbeAllowlist(context.Background(), nil, net.DefaultResolver)
	require.NoError(t, err, "empty allowlist should be a no-op")
}

func TestProbeAllowlist_MalformedEntry(t *testing.T) {
	err := ProbeAllowlist(context.Background(), []string{"bad host with spaces"}, net.DefaultResolver)
	require.Error(t, err, "expected parse error for malformed allowlist entry")
	require.ErrorContains(t, err, "parse")
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
	require.Error(t, err, "expected error when destinations do not resolve")
	require.ErrorContains(t, err, "unresolvable")
	// Both the exact host and the wildcard base zone should be reported.
	require.ErrorContains(t, err, "example.com")
}

func TestProbeHost_ExtractsHostname(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "exact host with port", in: "example.com:443", want: "example.com"},
		{name: "wildcard host with port", in: "*.api.example.com:8443", want: "api.example.com"},
		{name: "single label host", in: "host:80", want: "host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, probeHost(tt.in), "probeHost(%q)", tt.in)
		})
	}
}
