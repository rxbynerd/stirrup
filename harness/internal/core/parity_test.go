package core

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

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
	ctx := context.Background()
	secLogger := security.NewSecurityLogger(io.Discard, config.RunID)
	secrets, err := security.NewAutoSecretStore(ctx, config)
	if err != nil {
		t.Fatalf("secret store: %v", err)
	}
	execResult := preflightExecutorConstruct(ctx, config, secrets, secLogger)
	if execResult.err != nil {
		t.Fatalf("executor construct: %v", execResult.err)
	}
	resolvedHeaders, err := observability.ResolveHeaders(ctx, secrets, config.TraceEmitter.Headers)
	if err != nil {
		t.Fatalf("resolve headers: %v", err)
	}
	tp := transport.NewStdioTransport(io.Discard, nil)
	bc, err := buildComponents(ctx, config, secrets, secLogger, tool.NewRegistry(), tp, execResult, resolvedHeaders, resourceOptionsFromConfig(config), nil)
	if err != nil {
		t.Fatalf("buildComponents: %v", err)
	}
	return bc
}

// TestPreflightParity is the #356 regression guard. Every probe-eligible
// component BuildLoop constructs (enumerated by builtComponents.probeSteps,
// the set buildComponents produces) MUST surface as a step in a
// representative-config Preflight report AND that step must have actually
// run — not be a "not constructed" skip standing in for a component the
// dry-run forgot to build. The second assertion closes the hole where a
// probeSteps entry left nil would emit a present-but-vacuous skip and a naive
// name-only check would pass.
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

	for _, ps := range steps {
		step, found := reportStep(report, ps.name)
		if !found {
			t.Errorf("probe-eligible component %q produced no step in the dry-run report\n"+
				"every component in builtComponents.probeSteps must produce a Preflight step "+
				"(add construction + probe handling in Preflight)\nreport steps: %+v", ps.name, report.Steps)
			continue
		}
		// A "not constructed" skip means the probeSteps entry was nil — the
		// component was enumerated but Preflight never built it. That is the
		// false-clean-dry-run drift the parity guarantee exists to kill, so
		// it must fail the test rather than satisfy a name-only check.
		if step.Status == PreflightSkip && strings.Contains(step.Detail, "not constructed") {
			t.Errorf("probe step %q is a 'not constructed' skip: the component is enumerated "+
				"but Preflight did not build it (false-clean dry-run). detail=%q", ps.name, step.Detail)
		}
	}
}

// TestProbeStepsCoversBuiltComponents closes the converse hole: a
// probe-eligible field added to builtComponents (and built by buildComponents)
// but omitted from probeSteps() would never be probed, and nothing else would
// catch it. Every field tagged `probe:"..."` must be represented in
// probeSteps(), and every probeSteps() step must map back to a tagged field —
// so adding a probe-eligible field without enumerating it (or vice versa)
// fails here.
func TestProbeStepsCoversBuiltComponents(t *testing.T) {
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
	if len(tagged) == 0 {
		t.Fatal("no probe-tagged fields found; the coverage check would be vacuous")
	}

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
		found := false
		for name := range emitted {
			if matches(name, tag) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("probe-eligible field tagged %q is not enumerated by probeSteps(); "+
				"add it to probeSteps or remove the probe tag", tag)
		}
	}

	// Every emitted step must map back to a tagged field, so a step added to
	// probeSteps without a corresponding tagged field (drift in the other
	// direction) is also caught.
	for name := range emitted {
		found := false
		for tag := range tagged {
			if matches(name, tag) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("probeSteps emits %q but no builtComponents field is tagged for it; "+
				"tag the backing field or drop the step", name)
		}
	}
}

// TestPreflightParity_DetectsNotConstructedSkip proves the strengthened
// assertion in TestPreflightParity actually fires: a synthesized report whose
// probe step is a "not constructed" skip must be recognised as vacuous by the
// same check. Without this, a broken assertion could let the false-clean case
// through.
func TestPreflightParity_DetectsNotConstructedSkip(t *testing.T) {
	report := &PreflightReport{
		Steps: []PreflightStep{
			{Name: "trace-emitter-probe", Status: PreflightSkip, Detail: "component not constructed"},
		},
		OK: true,
	}
	step, found := reportStep(report, "trace-emitter-probe")
	if !found {
		t.Fatal("expected the fabricated step to be found")
	}
	isVacuous := step.Status == PreflightSkip && strings.Contains(step.Detail, "not constructed")
	if !isVacuous {
		t.Fatal("the parity assertion would not have flagged a 'not constructed' skip")
	}
}
