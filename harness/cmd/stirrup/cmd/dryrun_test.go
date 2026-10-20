package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/core"
	"github.com/rxbynerd/stirrup/types"
)

// stubPreflight swaps preflightFn for the duration of a test so runDryRun's
// dispatch/output/exit branches are exercised against a canned report
// without a live config or network. It restores the original on cleanup.
func stubPreflight(t *testing.T, report *core.PreflightReport) {
	t.Helper()
	orig := preflightFn
	preflightFn = func(_ context.Context, _ *types.RunConfig, _ core.PreflightOptions) (*core.PreflightReport, error) {
		return report, nil
	}
	t.Cleanup(func() { preflightFn = orig })
}

// dryRunTestCmd returns a harness command with stdout/stderr captured.
func dryRunTestCmd() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := newTestHarnessCommand()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	return cmd, &out, &errBuf
}

func okReport() *core.PreflightReport {
	return &core.PreflightReport{
		Steps: []core.PreflightStep{{Name: "config-validation", Status: core.PreflightOK, Detail: "ok"}},
		OK:    true,
	}
}

func failReport() *core.PreflightReport {
	return &core.PreflightReport{
		Steps: []core.PreflightStep{{Name: "credentials", Status: core.PreflightFail, Detail: "no key"}},
		OK:    false,
	}
}

func TestRunDryRun_TextToStderr(t *testing.T) {
	must := require.New(t)
	stubPreflight(t, okReport())
	cmd, out, errBuf := dryRunTestCmd()

	must.NoError(runDryRun(cmd, baseFileConfig(), core.PreflightOptions{}, "text", ""))
	must.Contains(errBuf.String(), "config-validation", "text report should go to stderr")
	must.Empty(out.String(), "stdout should be empty for text mode")
}

func TestRunDryRun_JSONToStdout(t *testing.T) {
	must := require.New(t)
	stubPreflight(t, okReport())
	cmd, out, _ := dryRunTestCmd()

	must.NoError(runDryRun(cmd, baseFileConfig(), core.PreflightOptions{}, "json", ""))
	var decoded core.PreflightReport
	must.NoError(json.Unmarshal(out.Bytes(), &decoded), "stdout should carry a PreflightReport JSON: %s", out.String())
	must.True(decoded.OK, "decoded report should preserve OK=true")
}

func TestRunDryRun_FailedReportReturnsExitCode1(t *testing.T) {
	must := require.New(t)
	stubPreflight(t, failReport())
	cmd, _, _ := dryRunTestCmd()

	err := runDryRun(cmd, baseFileConfig(), core.PreflightOptions{}, "text", "")
	must.Error(err, "a failing report must return a non-nil error")
	must.Equal(1, classifyExitCode(err), "probe failure is the untyped default exit code")
}

func TestRunDryRun_WithOutputRunConfig_RendersReportAndWritesConfig(t *testing.T) {
	must := require.New(t)
	stubPreflight(t, okReport())
	cmd, _, errBuf := dryRunTestCmd()
	outPath := filepath.Join(t.TempDir(), "captured.json")

	must.NoError(runDryRun(cmd, baseFileConfig(), core.PreflightOptions{}, "text", outPath))
	must.Contains(errBuf.String(), "Dry-run preflight", "report should be rendered to stderr")

	data, err := os.ReadFile(outPath)
	must.NoError(err, "captured config should be written")
	var captured types.RunConfig
	must.NoError(json.Unmarshal(data, &captured), "captured file should be valid RunConfig JSON: %s", data)
	must.Equal("from-file", captured.RunID, "captured config should be the one that was probed")
}

// TestUsageError_ClassifiesAsUsageAndPassesMessageThrough pins usageError to
// exit code 4 and to a transparent error message.
func TestUsageError_ClassifiesAsUsageAndPassesMessageThrough(t *testing.T) {
	must := require.New(t)
	must.NoError(usageError(nil), "usageError should pass nil through")

	err := usageError(errStub("bad combo"))
	must.Equal(exitUsage, classifyExitCode(err))
	must.EqualError(err, "bad combo")
}

type errStub string

func (e errStub) Error() string { return string(e) }

// TestValidateDryRunFlags_ProbeGatesNeedDryRun pins that a probe gate or
// --dry-run-timeout without --dry-run classifies as usage (exit 4), while the
// same flags alongside --dry-run are accepted.
func TestValidateDryRunFlags_ProbeGatesNeedDryRun(t *testing.T) {
	for _, gate := range dryRunProbeGates {
		t.Run(gate+" without dry-run is a usage error", func(t *testing.T) {
			must := require.New(t)
			cmd := newTestHarnessCommand()
			must.NoError(cmd.Flags().Set(gate, gateSetValue(gate)))

			err := validateDryRunFlags(cmd.Flags(), false)
			must.Error(err)
			must.Equal(exitUsage, classifyExitCode(err))
			must.ErrorContains(err, gate, "error should name the offending flag")
		})
		t.Run(gate+" with dry-run is accepted", func(t *testing.T) {
			must := require.New(t)
			cmd := newTestHarnessCommand()
			must.NoError(cmd.Flags().Set(gate, gateSetValue(gate)))

			must.NoError(validateDryRunFlags(cmd.Flags(), true))
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

func TestDryRunOptionsFromFlags_MapsSkipFlagsAndTimeout(t *testing.T) {
	check := assert.New(t)
	cmd := newTestHarnessCommand()
	f := cmd.Flags()
	_ = f.Set("no-probe-provider", "true")
	_ = f.Set("no-probe-egress", "true")
	_ = f.Set("no-probe-executor", "true")
	_ = f.Set("dry-run-timeout", "12s")

	opts := dryRunOptionsFromFlags(f)
	check.True(opts.SkipProvider, "SkipProvider should be true")
	check.False(opts.SkipMCP, "SkipMCP should default to false")
	check.True(opts.SkipEgress, "SkipEgress should be true")
	check.True(opts.SkipExecutor, "SkipExecutor should be true")
	check.Equal("12s", opts.Timeout.String())
}

func TestWritePreflightJSON_RoundTripsReport(t *testing.T) {
	must := require.New(t)
	report := &core.PreflightReport{
		Steps: []core.PreflightStep{
			{Name: "config-validation", Status: core.PreflightOK, Detail: "ok"},
			{Name: "credentials", Status: core.PreflightFail, Detail: "no key", Hint: "set the env var"},
		},
		OK: false,
	}
	var buf bytes.Buffer
	must.NoError(writePreflightJSON(&buf, report))

	var decoded core.PreflightReport
	must.NoError(json.Unmarshal(buf.Bytes(), &decoded), "emitted JSON should round-trip: %s", buf.String())
	must.False(decoded.OK, "decoded report should preserve OK=false")
	must.Len(decoded.Steps, 2)
	must.Equal("set the env var", decoded.Steps[1].Hint, "hint should survive the JSON round trip")
}

func TestWritePreflightText_RendersEveryStatus(t *testing.T) {
	report := &core.PreflightReport{
		Steps: []core.PreflightStep{
			{Name: "credentials", Status: core.PreflightFail, Detail: "no key", Hint: "set the env var"},
			{Name: "mcp", Status: core.PreflightSkip, Detail: "none configured"},
			{Name: "mcp:docs", Status: core.PreflightWarn, Detail: "optional server down"},
		},
		OK: false,
	}
	var buf bytes.Buffer
	writePreflightText(&buf, report)
	out := buf.String()

	t.Run("warned step", func(t *testing.T) {
		must := require.New(t)
		must.Contains(out, "WARN")
		must.Contains(out, "optional server down")
	})
	t.Run("failed step", func(t *testing.T) {
		must := require.New(t)
		must.Contains(out, "FAIL")
		must.Contains(out, "credentials")
	})
	t.Run("hint", func(t *testing.T) {
		must := require.New(t)
		must.Contains(out, "hint: set the env var")
	})
	t.Run("skipped step", func(t *testing.T) {
		must := require.New(t)
		must.Contains(out, "SKIP")
	})
}
