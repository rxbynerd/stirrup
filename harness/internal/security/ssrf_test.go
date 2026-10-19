package security

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidatePublicHost_ClassifiesHosts(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		wantErr bool
	}{
		{"loopback ipv4", "127.0.0.1", true},
		{"loopback ipv6", "::1", true},
		{"localhost name", "localhost", true},
		{"localhost suffix", "svc.localhost", true},
		{"rfc1918 address", "10.0.0.1", true},
		{"cgnat address", "100.64.0.1", true},
		{"link-local metadata address", "169.254.169.254", true},
		{"unspecified address", "0.0.0.0", true},
		{"public ip", "1.1.1.1", false},
		{"empty host", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePublicHost(tc.host)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestIsPublicIP_ClassifiesAddresses(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		want bool
	}{
		{"loopback is not public", "127.0.0.1", false},
		{"link-local metadata address is not public", "169.254.169.254", false},
		{"rfc 6598 cgnat address is not public", "100.64.0.1", false},
		{"address just below cgnat range is public", "100.63.255.255", true},
		{"public address is public", "1.1.1.1", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, IsPublicIP(net.ParseIP(tc.ip)))
		})
	}
}

func TestIsLoopbackHost_ClassifiesHosts(t *testing.T) {
	cases := []struct {
		name string
		host string
		want bool
	}{
		{"localhost name", "localhost", true},
		{"localhost suffix", "svc.localhost", true},
		{"loopback ipv4", "127.0.0.1", true},
		{"loopback ipv6", "::1", true},
		{"uppercase localhost", "LOCALHOST", true},
		{"public hostname", "mcp.example.com", false},
		{"rfc1918 address", "10.0.0.1", false},
		{"empty host", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, IsLoopbackHost(tc.host))
		})
	}
}

// TestLoopbackAwareDialContext_AdmitsLoopbackName pins that a dial to the
// "localhost" NAME (which net.ParseIP cannot resolve to a loopback IP) is
// admitted, not refused by the SSRF guard. The dial itself targets a closed
// port so a connection error is expected, but it must be a dial error, never
// the "private host" rejection.
func TestLoopbackAwareDialContext_AdmitsLoopbackName(t *testing.T) {
	dial := LoopbackAwareDialContext(0)
	_, err := dial(t.Context(), "tcp", "localhost:1")
	if err != nil {
		require.NotContains(t, err.Error(), "private host", "loopback name must not be SSRF-rejected")
	}
}

func TestSafeDialContext_BlocksPrivate(t *testing.T) {
	dial := SafeDialContext(false, 0)
	_, err := dial(t.Context(), "tcp", "10.0.0.1:443")
	require.ErrorContains(t, err, "private host")
}

func TestLoopbackAwareDialContext_BlocksNonLoopbackPrivate(t *testing.T) {
	dial := LoopbackAwareDialContext(0)
	// A non-loopback private address (e.g. a rebind to the metadata endpoint)
	// must still be refused.
	_, err := dial(t.Context(), "tcp", "169.254.169.254:443")
	require.ErrorContains(t, err, "private host")
}
