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
		{"NAT64 form of the metadata address", "64:ff9b::a9fe:a9fe", true},
		{"public ip", "1.1.1.1", false},
		{"empty host", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			err := ValidatePublicHost(tc.host)
			if tc.wantErr {
				must.Error(err)
			} else {
				must.NoError(err)
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
		{"AWS IPv6 metadata address is not public", "fd00:ec2::254", false},
		{"Alibaba Cloud metadata address is not public", "100.100.100.200", false},
		{"NAT64 form of the metadata address is not public", "64:ff9b::a9fe:a9fe", false},
		{"NAT64 form of a private address is not public", "64:ff9b::a00:1", false},
		{"NAT64 form of a public address is public", "64:ff9b::101:101", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, IsPublicIP(net.ParseIP(tc.ip)))
		})
	}
}

func TestIsCloudMetadataIP_ClassifiesAddresses(t *testing.T) {
	cases := []struct {
		name string
		ip   string
		want bool
	}{
		{"IPv4 metadata endpoint", "169.254.169.254", true},
		{"IPv4-mapped metadata endpoint", "::ffff:169.254.169.254", true},
		{"IPv6 link-local address", "fe80::1", true},
		{"AWS IPv6 metadata endpoint", "fd00:ec2::254", true},
		{"EKS Pod Identity agent over IPv6", "fd00:ec2::23", true},
		{"NAT64 form of the metadata endpoint", "64:ff9b::a9fe:a9fe", true},
		{"Alibaba Cloud metadata endpoint", "100.100.100.200", true},
		{"other unique-local address", "fd00:ec3::254", false},
		{"other CGNAT address", "100.100.100.201", false},
		{"NAT64 form of a private address", "64:ff9b::a00:1", false},
		{"private address", "10.96.0.12", false},
		{"public address", "1.1.1.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tc.want, IsCloudMetadataIP(net.ParseIP(tc.ip)))
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
			must := require.New(t)
			must.Equal(tc.want, IsLoopbackHost(tc.host))
		})
	}
}

// TestLoopbackAwareDialContext_AdmitsLoopbackName pins that a dial to the
// "localhost" NAME (which net.ParseIP cannot resolve to a loopback IP) is
// admitted, not refused by the SSRF guard. The dial itself targets a closed
// port so a connection error is expected, but it must be a dial error, never
// the "private host" rejection.
func TestLoopbackAwareDialContext_AdmitsLoopbackName(t *testing.T) {
	must := require.New(t)
	dial := LoopbackAwareDialContext(0)
	_, err := dial(t.Context(), "tcp", "localhost:1")
	if err != nil {
		must.NotContains(err.Error(), "private host", "loopback name must not be SSRF-rejected")
	}
}

func TestSafeDialContext_BlocksPrivate(t *testing.T) {
	must := require.New(t)
	dial := SafeDialContext(false, 0)
	_, err := dial(t.Context(), "tcp", "10.0.0.1:443")
	must.ErrorContains(err, "private host")
}

func TestLoopbackAwareDialContext_BlocksNonLoopbackPrivate(t *testing.T) {
	must := require.New(t)
	dial := LoopbackAwareDialContext(0)
	// A non-loopback private address (e.g. a rebind to the metadata endpoint)
	// must still be refused.
	_, err := dial(t.Context(), "tcp", "169.254.169.254:443")
	must.ErrorContains(err, "private host")
}
