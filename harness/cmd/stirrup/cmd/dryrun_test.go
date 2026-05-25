package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/core"
)

// TestUsageErrorClass pins usageError to exit code 4 and the nil
// passthrough / transparency contract the other wrappers also honour.
func TestUsageErrorClass(t *testing.T) {
	if got := usageError(nil); got != nil {
		t.Errorf("usageError(nil) = %v, want nil", got)
	}
	err := usageError(errStub("bad combo"))
	if got := classifyExitCode(err); got != exitUsage {
		t.Errorf("classifyExitCode(usageError) = %d, want %d", got, exitUsage)
	}
	if err.Error() != "bad combo" {
		t.Errorf("usageError Error() = %q, want transparent passthrough", err.Error())
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }

// TestValidateDryRunFlags pins that a probe gate or --dry-run-timeout
// without --dry-run classifies as usage (exit 4), while the same flags
// alongside --dry-run are accepted.
func TestValidateDryRunFlags(t *testing.T) {
	for _, gate := range dryRunProbeGates {
		t.Run(gate+" without dry-run is exit 4", func(t *testing.T) {
			cmd := newTestHarnessCommand()
			if err := cmd.Flags().Set(gate, gateSetValue(gate)); err != nil {
				t.Fatalf("set %s: %v", gate, err)
			}
			err := validateDryRunFlags(cmd.Flags(), false)
			if err == nil {
				t.Fatalf("expected usage error for %s without --dry-run", gate)
			}
			if got := classifyExitCode(err); got != exitUsage {
				t.Errorf("classifyExitCode = %d, want %d (usage)", got, exitUsage)
			}
			if !strings.Contains(err.Error(), gate) {
				t.Errorf("error should name the offending flag %q, got: %v", gate, err)
			}
		})
		t.Run(gate+" with dry-run is accepted", func(t *testing.T) {
			cmd := newTestHarnessCommand()
			if err := cmd.Flags().Set(gate, gateSetValue(gate)); err != nil {
				t.Fatalf("set %s: %v", gate, err)
			}
			if err := validateDryRunFlags(cmd.Flags(), true); err != nil {
				t.Errorf("expected no error with --dry-run, got: %v", err)
			}
		})
	}
}

// gateSetValue returns a valid pflag string for the probe gate (bools take
// "true", the duration takes a parseable value).
func gateSetValue(gate string) string {
	if gate == "dry-run-timeout" {
		return "10s"
	}
	return "true"
}

func TestDryRunOptionsFromFlags(t *testing.T) {
	cmd := newTestHarnessCommand()
	f := cmd.Flags()
	_ = f.Set("no-probe-provider", "true")
	_ = f.Set("no-probe-egress", "true")
	_ = f.Set("dry-run-timeout", "12s")

	opts := dryRunOptionsFromFlags(f)
	if !opts.SkipProvider {
		t.Error("SkipProvider should be true")
	}
	if opts.SkipMCP {
		t.Error("SkipMCP should default false")
	}
	if !opts.SkipEgress {
		t.Error("SkipEgress should be true")
	}
	if opts.Timeout.String() != "12s" {
		t.Errorf("Timeout = %v, want 12s", opts.Timeout)
	}
}

func TestWritePreflightJSON(t *testing.T) {
	report := &core.PreflightReport{
		Steps: []core.PreflightStep{
			{Name: "config-validation", Status: core.PreflightOK, Detail: "ok"},
			{Name: "credentials", Status: core.PreflightFail, Detail: "no key", Hint: "set the env var"},
		},
		OK: false,
	}
	var buf bytes.Buffer
	if err := writePreflightJSON(&buf, report); err != nil {
		t.Fatalf("writePreflightJSON: %v", err)
	}
	var decoded core.PreflightReport
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("emitted JSON does not round-trip: %v\n%s", err, buf.String())
	}
	if decoded.OK {
		t.Error("decoded report should preserve OK=false")
	}
	if len(decoded.Steps) != 2 {
		t.Fatalf("decoded %d steps, want 2", len(decoded.Steps))
	}
	if decoded.Steps[1].Hint != "set the env var" {
		t.Errorf("hint not preserved through JSON: %+v", decoded.Steps[1])
	}
}

func TestWritePreflightText(t *testing.T) {
	report := &core.PreflightReport{
		Steps: []core.PreflightStep{
			{Name: "credentials", Status: core.PreflightFail, Detail: "no key", Hint: "set the env var"},
			{Name: "mcp", Status: core.PreflightSkip, Detail: "none configured"},
		},
		OK: false,
	}
	var buf bytes.Buffer
	writePreflightText(&buf, report)
	out := buf.String()
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "credentials") {
		t.Errorf("text output should flag the failing step, got:\n%s", out)
	}
	if !strings.Contains(out, "hint: set the env var") {
		t.Errorf("text output should render the hint, got:\n%s", out)
	}
	if !strings.Contains(out, "SKIP") {
		t.Errorf("text output should render the skipped step, got:\n%s", out)
	}
}
