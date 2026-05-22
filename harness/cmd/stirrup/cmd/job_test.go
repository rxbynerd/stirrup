package cmd

import (
	"strings"
	"testing"
)

// TestRunJob_MissingControlPlaneAddrPlainText pins the #249-C
// behaviour: when CONTROL_PLANE_ADDR is unset, runJob must return
// an error whose surface text is plain — no ANSI escapes, no
// formatting helpers, no bold heading — so a Kubernetes restart
// loop or log aggregator surfaces the failure cleanly. This is the
// counter-pin to the harness bare-invocation formatting: a future
// refactor that hoists the format helpers into a shared "every
// error gets dressed up" path would silently break the log-grep
// workflows that the job entrypoint is built for, and this test
// would catch it.
//
// t.Setenv is used so a workstation that happens to have
// CONTROL_PLANE_ADDR exported (rare, but possible during local
// gRPC bring-up) does not skew the assertion.
func TestRunJob_MissingControlPlaneAddrPlainText(t *testing.T) {
	t.Setenv("CONTROL_PLANE_ADDR", "")

	err := runJob(jobCmd, nil)
	if err == nil {
		t.Fatal("runJob without CONTROL_PLANE_ADDR must return an error; got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "CONTROL_PLANE_ADDR") {
		t.Errorf("error must reference the missing env var, got: %q", msg)
	}
	if strings.Contains(msg, "\x1b[") {
		t.Errorf("job entrypoint must surface plain text; got ANSI escape in: %q", msg)
	}
}
