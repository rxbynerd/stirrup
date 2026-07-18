package cmd

import (
	"strings"
	"testing"
)

// TestRunJob_MissingControlPlaneAddrIsPlain checks that the
// "CONTROL_PLANE_ADDR environment variable is required" error stays
// plain text with no ANSI escapes, since log aggregators ingest the
// job's stderr verbatim.
func TestRunJob_MissingControlPlaneAddrIsPlain(t *testing.T) {
	t.Setenv("CONTROL_PLANE_ADDR", "")

	err := runJob(jobCmd, nil)
	if err == nil {
		t.Fatal("runJob returned nil, want an error when CONTROL_PLANE_ADDR is unset")
	}
	msg := err.Error()
	if !strings.Contains(msg, "CONTROL_PLANE_ADDR environment variable is required") {
		t.Errorf("error = %q, want the CONTROL_PLANE_ADDR-required message", msg)
	}
	if strings.Contains(msg, "\x1b[") {
		t.Errorf("job error must not contain ANSI escapes (log aggregators ingest it verbatim): %q", msg)
	}
}
