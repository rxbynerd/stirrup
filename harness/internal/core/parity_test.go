package core

import (
	"context"
	"io"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// reportStepNames collects the step names present in a preflight report.
func reportStepNames(report *PreflightReport) map[string]bool {
	names := make(map[string]bool, len(report.Steps))
	for _, s := range report.Steps {
		names[s.Name] = true
	}
	return names
}

// missingProbeSteps returns the probeSteps names absent from the report's
// step set — the parity violation the dry-run must never have: a
// probe-eligible component BuildLoop constructs that the dry-run does not
// surface.
func missingProbeSteps(steps []probeComponentStep, report *PreflightReport) []string {
	have := reportStepNames(report)
	var missing []string
	for _, s := range steps {
		if !have[s.name] {
			missing = append(missing, s.name)
		}
	}
	return missing
}

// buildComponentsForParity constructs the probe-eligible set through the
// SAME buildComponents path BuildLoop uses, so the test measures parity
// against the real construction rather than a re-listing. The executor is
// built read-only-safe (local) for the representative config.
func buildComponentsForParity(t *testing.T, config *types.RunConfig) *builtComponents {
	t.Helper()
	ctx := context.Background()
	secLogger := security.NewSecurityLogger(io.Discard, config.RunID)
	secrets, err := security.NewAutoSecretStore(ctx, config)
	if err != nil {
		t.Fatalf("secret store: %v", err)
	}
	exec, err := buildExecutor(ctx, config.Executor, secrets, secLogger)
	if err != nil {
		t.Fatalf("build executor: %v", err)
	}
	resolvedHeaders, err := observability.ResolveHeaders(ctx, secrets, config.TraceEmitter.Headers)
	if err != nil {
		t.Fatalf("resolve headers: %v", err)
	}
	tp := transport.NewStdioTransport(io.Discard, nil)
	bc, err := buildComponents(ctx, config, secrets, secLogger, tool.NewRegistry(), tp, exec, resolvedHeaders, resourceOptionsFromConfig(config))
	if err != nil {
		t.Fatalf("buildComponents: %v", err)
	}
	return bc
}

// TestPreflightParity is the #356 regression guard: every probe-eligible
// component BuildLoop constructs (enumerated by builtComponents.probeSteps,
// the set buildComponents produces) MUST surface as a step in a
// representative-config Preflight report. If a future change adds a
// component to buildComponents/probeSteps without teaching Preflight to
// build+probe it, this test fails — closing the false-clean-dry-run gap.
func TestPreflightParity(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	config := preflightTestConfig(t, srv.URL+"/v1")

	bc := buildComponentsForParity(t, config)
	steps := bc.probeSteps()
	if len(steps) == 0 {
		t.Fatal("probeSteps returned no components; the parity check would be vacuous")
	}

	report, err := Preflight(context.Background(), config, PreflightOptions{})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}

	if missing := missingProbeSteps(steps, report); len(missing) > 0 {
		t.Fatalf("probe-eligible components missing from the dry-run report: %v\n"+
			"every component in builtComponents.probeSteps must produce a Preflight step "+
			"(add construction + probe handling in Preflight)\nreport steps: %+v", missing, report.Steps)
	}
}

// TestPreflightParity_DetectsOmission proves the parity check actually
// catches an omission: a fabricated probe set carrying a component name the
// representative report does not produce must be flagged by
// missingProbeSteps. Without this, TestPreflightParity could pass trivially
// if the comparison logic were broken.
func TestPreflightParity_DetectsOmission(t *testing.T) {
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	config := preflightTestConfig(t, srv.URL+"/v1")
	report, err := Preflight(context.Background(), config, PreflightOptions{})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}

	// Simulate a component added to BuildLoop (probeSteps) but never built
	// or probed by Preflight.
	fabricated := []probeComponentStep{{name: "phantom-component-probe", component: nil}}
	if missing := missingProbeSteps(fabricated, report); len(missing) != 1 || missing[0] != "phantom-component-probe" {
		t.Fatalf("parity check failed to flag an omitted component; missing=%v", missing)
	}
}
