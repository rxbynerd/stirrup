package security

import (
	"net"
	"strings"
	"testing"
)

func TestValidatePublicHost(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		wantErr bool
	}{
		{"loopback_ipv4", "127.0.0.1", true},
		{"loopback_ipv6", "::1", true},
		{"localhost_name", "localhost", true},
		{"localhost_suffix", "svc.localhost", true},
		{"rfc1918", "10.0.0.1", true},
		{"link_local_metadata", "169.254.169.254", true},
		{"unspecified", "0.0.0.0", true},
		{"public_ip", "1.1.1.1", false},
		{"empty", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePublicHost(tc.host)
			if tc.wantErr != (err != nil) {
				t.Fatalf("ValidatePublicHost(%q) err = %v, wantErr = %v", tc.host, err, tc.wantErr)
			}
		})
	}
}

func TestIsPublicIP(t *testing.T) {
	if IsPublicIP(net.ParseIP("127.0.0.1")) {
		t.Error("loopback should not be public")
	}
	if IsPublicIP(net.ParseIP("169.254.169.254")) {
		t.Error("link-local metadata IP should not be public")
	}
	if !IsPublicIP(net.ParseIP("1.1.1.1")) {
		t.Error("1.1.1.1 should be public")
	}
}

func TestSafeDialContext_BlocksPrivate(t *testing.T) {
	dial := SafeDialContext(false, 0)
	_, err := dial(t.Context(), "tcp", "10.0.0.1:443")
	if err == nil {
		t.Fatal("SafeDialContext should refuse a private dial target")
	}
	if !strings.Contains(err.Error(), "private host") {
		t.Errorf("err = %v, want 'private host'", err)
	}
}

func TestLoopbackAwareDialContext_BlocksNonLoopbackPrivate(t *testing.T) {
	dial := LoopbackAwareDialContext(0)
	// A non-loopback private address (e.g. a rebind to the metadata endpoint)
	// must still be refused.
	_, err := dial(t.Context(), "tcp", "169.254.169.254:443")
	if err == nil {
		t.Fatal("LoopbackAwareDialContext should refuse a non-loopback private target")
	}
	if !strings.Contains(err.Error(), "private host") {
		t.Errorf("err = %v, want 'private host'", err)
	}
}
