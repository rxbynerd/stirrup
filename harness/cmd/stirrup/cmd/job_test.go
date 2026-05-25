package cmd

import (
	"strings"
	"testing"
)

// TestRunJob_MissingControlPlaneAddrIsPlain pins issue #249's job
// contract: the "CONTROL_PLANE_ADDR environment variable is required"
// error stays plain text with no ANSI escapes. Log aggregators ingest
// the job's stderr verbatim, so a future help-system refactor that
// reached for colour here would corrupt every captured line. There is
// deliberately no behaviour change for the job entry point — this test
// is the regression guard.
func TestRunJob_MissingControlPlaneAddrIsPlain(t *testing.T) {
	// Unset rather than save/restore: t.Setenv("", "") cannot clear a
	// var, and the test must observe the empty-addr branch. t.Setenv
	// registers cleanup so a value set by the surrounding environment is
	// restored for later tests.
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
