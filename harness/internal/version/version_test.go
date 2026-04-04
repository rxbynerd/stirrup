package version

import "testing"

func TestDefaultVersion(t *testing.T) {
	if v := Version(); v != "dev" {
		t.Fatalf("expected default version %q, got %q", "dev", v)
	}
}
