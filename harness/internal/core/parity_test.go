package core

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// reportStep returns the step with the given name, or false.
func reportStep(report *PreflightReport, name string) (PreflightStep, bool) {
	for _, s := range report.Steps {
		if s.Name == name {
			return s, true
		}
	}
	return PreflightStep{}, false
}

// buildComponentsForParity constructs the probe-eligible set through the SAME
// buildComponents path BuildLoop uses, so the test measures parity against the
// real construction rather than a re-listing. The executor is built
// read-only-safe (local) for the representative config.
func buildComponentsForParity(t *testing.T, config *types.RunConfig) *builtComponents {
	t.Helper()
	must := require.New(t)
	ctx := context.Background()
	secLogger := security.NewSecurityLogger(io.Discard, config.RunID)
	secrets, err := security.NewAutoSecretStore(ctx, config)
	must.NoError(err, "secret store")
	execResult := preflightExecutorConstruct(ctx, config, secrets, secLogger)
	must.NoError(execResult.err, "executor construct")
	resolvedHeaders, err := observability.ResolveHeaders(ctx, secrets, config.TraceEmitter.Headers)
	must.NoError(err, "resolve headers")
	tp := transport.NewStdioTransport(io.Discard, nil)
	exporters, resourceOpts, err := resolveTelemetry(ctx, config, resolvedHeaders)
	must.NoError(err, "resolve telemetry")
	bc, err := buildComponents(ctx, config, secrets, secLogger, tool.NewRegistry(), tp, execResult, exporters, resourceOpts, nil, false)
	must.NoError(err, "buildComponents")
	return bc
}

// TestPreflight_DryRunProbesMatchRealRunComponents is the preflight-parity regression guard. Every
// probe-eligible component BuildLoop constructs (enumerated by
// builtComponents.probeSteps) MUST surface as a step in a
// representative-config Preflight report AND that step must have actually
// run — not be a "not constructed" skip standing in for a component the
// dry-run forgot to build. The second assertion closes the hole where a
// probeSteps entry left nil would emit a present-but-vacuous skip and a naive
// name-only check would pass.
func TestPreflight_DryRunProbesMatchRealRunComponents(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	config := preflightTestConfig(t, srv.URL+"/v1")

	bc := buildComponentsForParity(t, config)
	steps := bc.probeSteps()
	must.NotEmpty(steps, "probeSteps returned no components; the parity check would be vacuous")

	report, err := Preflight(context.Background(), config, PreflightOptions{})
	must.NoError(err)

	for _, ps := range steps {
		t.Run(ps.name, func(t *testing.T) {
			must := require.New(t)
			step, found := reportStep(report, ps.name)
			must.True(found,
				"probe-eligible component produced no step in the dry-run report; "+
					"every component in builtComponents.probeSteps must produce a Preflight step "+
					"(add construction + probe handling in Preflight); report steps: %+v", report.Steps)

			// A "not constructed" skip means the probeSteps entry was nil — the
			// component was enumerated but Preflight never built it. That is the
			// false-clean-dry-run drift the parity guarantee exists to kill, so
			// it must fail rather than satisfy a name-only check.
			notConstructed := step.Status == PreflightSkip && strings.Contains(step.Detail, "not constructed")
			must.False(notConstructed,
				"the component is enumerated but Preflight did not build it (false-clean dry-run), detail=%q", step.Detail)
		})
	}
}

// TestProbeSteps_CoversBuiltComponents closes the converse hole: a
// probe-eligible field added to builtComponents (and built by buildComponents)
// but omitted from probeSteps() would never be probed, and nothing else would
// catch it. Every field tagged `probe:"..."` must be represented in
// probeSteps(), and every probeSteps() step must map back to a tagged field —
// so adding a probe-eligible field without enumerating it (or vice versa)
// fails here.
func TestProbeSteps_CoversBuiltComponents(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_PREFLIGHT_KEY", "sk-test")
	srv, _ := metadataOnlyServer(t)
	defer srv.Close()

	config := preflightTestConfig(t, srv.URL+"/v1")
	bc := buildComponentsForParity(t, config)

	emitted := make(map[string]bool)
	for _, ps := range bc.probeSteps() {
		emitted[ps.name] = true
	}

	// Collect the step names (or prefixes) declared by the probe struct tags.
	tagged := make(map[string]bool)
	rt := reflect.TypeOf(*bc)
	for i := 0; i < rt.NumField(); i++ {
		tag, ok := rt.Field(i).Tag.Lookup("probe")
		if !ok {
			continue
		}
		tagged[tag] = true
	}
	must.NotEmpty(tagged, "no probe-tagged fields found; the coverage check would be vacuous")

	matches := func(stepName, tag string) bool {
		if stepName == tag {
			return true
		}
		// A tag ending in ":" is a prefix for a map field that expands to one
		// step per entry (the providers map → "provider-probe:<name>").
		return strings.HasSuffix(tag, ":") && strings.HasPrefix(stepName, tag)
	}

	// Every tagged field must be represented in probeSteps. The representative
	// config has at least one provider, so the prefix tag matches at least one
	// emitted step.
	for tag := range tagged {
		t.Run("tagged field "+tag, func(t *testing.T) {
			must := require.New(t)
			found := false
			for name := range emitted {
				if matches(name, tag) {
					found = true
					break
				}
			}
			must.True(found,
				"probe-eligible field is not enumerated by probeSteps(); add it to probeSteps or remove the probe tag")
		})
	}

	// Every emitted step must map back to a tagged field, so a step added to
	// probeSteps without a corresponding tagged field (drift in the other
	// direction) is also caught.
	for name := range emitted {
		t.Run("emitted step "+name, func(t *testing.T) {
			must := require.New(t)
			found := false
			for tag := range tagged {
				if matches(name, tag) {
					found = true
					break
				}
			}
			must.True(found,
				"no builtComponents field is tagged for this step; tag the backing field or drop the step")
		})
	}
}

// TestPreflightParity_DetectsNotConstructedSkip checks that the not-constructed
// test applied by TestPreflight_DryRunProbesMatchRealRunComponents flags a "not constructed" skip as vacuous.
// Without this, a broken check could let the false-clean case through.
func TestPreflightParity_DetectsNotConstructedSkip(t *testing.T) {
	must := require.New(t)
	report := &PreflightReport{
		Steps: []PreflightStep{
			{Name: "trace-emitter-probe", Status: PreflightSkip, Detail: "component not constructed"},
		},
		OK: true,
	}
	step, found := reportStep(report, "trace-emitter-probe")
	must.True(found, "expected the fabricated step to be found")

	isVacuous := step.Status == PreflightSkip && strings.Contains(step.Detail, "not constructed")
	must.True(isVacuous, "a 'not constructed' skip must be flagged as vacuous")
}
