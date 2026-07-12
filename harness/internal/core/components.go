package core

import (
	"context"
	"fmt"
	"sort"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// builtComponents is the set of probe-eligible components constructed by
// buildComponents. It is the single source of truth shared by the two
// composition entry points: BuildLoopWithTransport consumes the typed
// fields to assemble the AgenticLoop, and Preflight iterates probeSteps()
// to run a dry-run reachability/auth probe against each.
//
// The parity guarantee, made structural (not by convention): Preflight and
// BuildLoopWithTransport BOTH construct the uniform components (providers,
// permission policy, trace emitter) by calling buildComponents — the same
// function — so a component cannot be added to the real run's construction
// without also being constructed, enumerated by probeSteps(), and probed in
// the dry-run. TestPreflightParity asserts every probeSteps() entry both
// appears in a representative report AND is not a "not constructed" skip
// (catching a field left nil), and TestProbeStepsCoversBuiltComponents
// asserts every probe-tagged field of builtComponents is named in
// probeSteps() and vice versa (catching a field added to the struct but
// omitted from the enumeration, or the reverse).
//
// The executor is the one documented exception: its construction semantics
// diverge (BuildLoop starts a real container; a dry-run must only probe the
// engine read-only), so it is supplied by the caller rather than built
// inside buildComponents.
//
// The `probe:"<step-prefix>"` struct tag marks a field as probe-eligible and
// names the probeSteps() step (or step prefix, for the providers map) it
// must be enumerated under. TestProbeStepsCoversBuiltComponents reflects over
// these tags. Untagged fields (defaultProvider, resolvedHeaders) are
// bookkeeping, not independently probed.
type builtComponents struct {
	// defaultProvider is the adapter for config.Provider; it is also keyed
	// into providers by its type, so it is probed via the providers map and
	// is retained here only because BuildLoop assembles the loop around it.
	defaultProvider provider.ProviderAdapter
	providers       map[string]provider.ProviderAdapter `probe:"provider-probe:"`

	// executor is the constructed executor. The two callers supply it via
	// divergent construction (BuildLoop starts a real container; Preflight
	// substitutes a read-only engine probe — see preflightExecutorConstruct),
	// so it is passed into buildComponents rather than constructed there.
	executor executor.Executor `probe:"executor-probe"`

	permissionPolicy permission.PermissionPolicy `probe:"permission-policy-probe"`
	traceEmitter     trace.TraceEmitter          `probe:"trace-emitter-probe"`

	// resolvedHeaders are the secret://-dereferenced traceEmitter.Headers.
	// BuildLoop reuses them for the metrics exporter so traces and metrics
	// authenticate identically; retained here to avoid a second resolve.
	resolvedHeaders map[string]string
}

// componentStepSink lets buildComponents emit a per-component construction
// step as it builds each component. Preflight supplies a sink so the dry-run
// report carries a row for every component buildComponents constructs —
// making the construction-step set a property of the shared constructor, not
// a hand-maintained mirror. BuildLoop passes a nil sink (it emits no
// per-component steps). The step names a sink receives
// ("credentials"/"executor"/"permission-policy"/"trace-emitter") match the
// report rows the operator sees.
//
// ok records a successful construction; failStep records a failed one with a
// remediation hint. buildComponents calls exactly one of them per component,
// then (on failure) returns so the dry-run stops at the first construction
// error.
type componentStepSink struct {
	ok       func(name, detail string)
	failStep func(name string, err error, hint string)
}

// executorBuildResult carries the executor a caller constructed (or chose
// not to) so buildComponents can thread it onto builtComponents and emit its
// construction step in the report's canonical position (after credentials,
// before the permission policy) without owning the executor's divergent
// construction. BuildLoop fills exec with the live executor it already built
// and leaves the report fields zero (nil sink ⇒ no step). Preflight fills the
// report fields from preflightExecutorConstruct: a container dry-run sets
// exec nil with an ok note that the engine is probed read-only; local/api set
// exec to the constructed executor; a construction failure sets err.
type executorBuildResult struct {
	exec   executor.Executor
	detail string // ok note when err is nil
	err    error  // non-nil ⇒ construction failed; detail is unused
	hint   string // remediation hint for a failed construction
}

// probeComponentStep names a probe-eligible component and the value to
// type-assert to Preflighter. Order is the report's display order.
type probeComponentStep struct {
	name      string
	component any
}

// probeSteps returns the probe-eligible components in deterministic report
// order. Preflight type-asserts each component to Preflighter and records
// ok/skip/fail. This is the structural parity seam: every field of
// builtComponents that represents a reachable component is enumerated here.
// TestProbeStepsCoversBuiltComponents asserts the converse — that no
// probe-tagged field is omitted from this list, and no step here lacks a
// backing tagged field.
//
// Providers are enumerated by sorted name so a multi-provider config yields
// a reproducible step order (matching --output=json determinism).
func (b *builtComponents) probeSteps() []probeComponentStep {
	nonProvider := []probeComponentStep{
		{name: "executor-probe", component: b.executor},
		{name: "permission-policy-probe", component: b.permissionPolicy},
		{name: "trace-emitter-probe", component: b.traceEmitter},
	}
	steps := make([]probeComponentStep, 0, len(b.providers)+len(nonProvider))

	names := make([]string, 0, len(b.providers))
	for name := range b.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		steps = append(steps, probeComponentStep{name: "provider-probe:" + name, component: b.providers[name]})
	}

	return append(steps, nonProvider...)
}

// buildComponents constructs the probe-eligible components shared by
// BuildLoopWithTransport and Preflight: the provider adapters, the
// permission policy, and the trace emitter. The executor is supplied by the
// caller (its construction semantics diverge — see builtComponents.executor)
// via execResult and threaded onto the returned struct so probeSteps
// enumerates it.
//
// resolvedHeaders must be the already-dereferenced traceEmitter.Headers
// (the caller resolves them once and reuses them for the metrics exporter);
// resourceOpts is the OTel resource identity. registry and tp are inputs to
// the permission policy (mutating/approval tool sets, ask-upstream
// transport). secLogger receives security events from constructed
// components; pass an io.Discard-backed logger in a dry-run.
//
// When sink is non-nil, buildComponents emits a per-component construction
// step (ok on success, failStep on failure) as it builds each component,
// including the caller-supplied executor (execResult). This makes the
// dry-run's construction-step report a property of the shared constructor:
// adding a component here automatically adds its construction step (and, via
// probeSteps, its probe step) to the dry-run. BuildLoop passes a nil sink.
// The step names ("credentials", "executor", "permission-policy",
// "trace-emitter") match the dry-run report rows.
//
// A construction failure returns a nil struct and an error naming the
// failing component (so a non-dry-run caller still gets a wrapped error) and,
// when a sink is present, records the failed step before returning — the
// dry-run stops at the first construction error. Owned closers (trace
// emitter) are NOT closed here — the caller owns the lifecycle.
//
// debugRedactionDisabled threads the --debug bit (issue #219) into the
// constructed trace emitter. It is re-checked against
// debugbuild.DebugBuildEnabled() inside buildTraceEmitter, so passing
// true here has no effect in a release binary; Preflight always passes
// false (a dry-run never disables redaction).
func buildComponents(
	ctx context.Context,
	config *types.RunConfig,
	secrets security.SecretStore,
	secLogger *security.SecurityLogger,
	registry *tool.Registry,
	tp transport.Transport,
	execResult executorBuildResult,
	resolvedHeaders map[string]string,
	resourceOpts observability.ResourceOptions,
	sink *componentStepSink,
	debugRedactionDisabled bool,
) (*builtComponents, error) {
	// Error prefixes mirror BuildLoop's historical inline messages
	// ("build providers", etc.) so the composition root's operator-facing
	// error text is unchanged by the extraction.
	defaultProvider, providers, err := buildProviders(ctx, config, secrets)
	if err != nil {
		if sink != nil {
			sink.failStep("credentials", err, "verify the credential source resolves (env var set, federation rule valid, key present)")
		}
		return nil, fmt.Errorf("build providers: %w", err)
	}
	if sink != nil {
		sink.ok("credentials", "all provider credentials resolved")
	}

	// Executor construction step. The executor itself was built by the
	// caller (its semantics diverge), but its step is emitted here so the
	// dry-run report keeps its canonical row order and so a future
	// caller-supplied component cannot skip the construction step.
	if execResult.err != nil {
		if sink != nil {
			sink.failStep("executor", execResult.err, execResult.hint)
		}
		return nil, fmt.Errorf("build executor: %w", execResult.err)
	}
	if sink != nil {
		sink.ok("executor", execResult.detail)
	}

	pp, err := buildPermissionPolicy(config, registry, tp, secLogger)
	if err != nil {
		if sink != nil {
			sink.failStep("permission-policy", err, "check permissionPolicy.policyFile parses as Cedar")
		}
		return nil, fmt.Errorf("build permission policy: %w", err)
	}
	if sink != nil {
		sink.ok("permission-policy", fmt.Sprintf("%s policy constructed", config.PermissionPolicy.Type))
	}

	te, err := buildTraceEmitter(ctx, config.TraceEmitter, resolvedHeaders, resourceOpts, debugRedactionDisabled)
	if err != nil {
		if sink != nil {
			sink.failStep("trace-emitter", err, traceHint(config.TraceEmitter.Type))
		}
		return nil, fmt.Errorf("build trace emitter: %w", err)
	}
	if sink != nil {
		sink.ok("trace-emitter", fmt.Sprintf("%s trace emitter constructed", traceTypeName(config.TraceEmitter.Type)))
	}

	return &builtComponents{
		defaultProvider:  defaultProvider,
		providers:        providers,
		executor:         execResult.exec,
		permissionPolicy: pp,
		traceEmitter:     te,
		resolvedHeaders:  resolvedHeaders,
	}, nil
}
