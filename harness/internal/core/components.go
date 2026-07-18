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

// builtComponents holds the probe-eligible components buildComponents
// constructs, shared by BuildLoopWithTransport and Preflight so a dry-run
// probes exactly what a real run builds. See docs/architecture.md for the
// parity guarantee this struct encodes.
type builtComponents struct {
	// defaultProvider is also keyed into providers by type; retained here
	// only because BuildLoop assembles the loop around it directly.
	defaultProvider provider.ProviderAdapter
	providers       map[string]provider.ProviderAdapter `probe:"provider-probe:"`

	// executor is supplied by the caller — its construction diverges
	// between BuildLoop (real container) and Preflight (read-only probe).
	executor executor.Executor `probe:"executor-probe"`

	permissionPolicy permission.PermissionPolicy `probe:"permission-policy-probe"`
	traceEmitter     trace.TraceEmitter          `probe:"trace-emitter-probe"`

	// resolvedHeaders are the dereferenced traceEmitter.Headers, reused
	// for the metrics exporter so traces and metrics authenticate alike.
	resolvedHeaders map[string]string
}

// componentStepSink lets buildComponents emit a per-component construction
// step (Preflight supplies one; BuildLoop passes nil). ok records success;
// failStep records failure with a remediation hint — buildComponents calls
// exactly one per component, then returns on failure.
type componentStepSink struct {
	ok       func(name, detail string)
	failStep func(name string, err error, hint string)
}

// executorBuildResult carries the caller-constructed executor (its
// construction diverges between BuildLoop and Preflight) so buildComponents
// can thread it onto builtComponents and emit its construction step.
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
// order (Preflight type-asserts each to Preflighter). Providers are sorted
// by name for reproducible --output=json ordering.
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
// BuildLoopWithTransport and Preflight: provider adapters, permission
// policy, and trace emitter. The executor is supplied by the caller via
// execResult (see builtComponents.executor).
//
// resolvedHeaders must already be secret://-dereferenced. When sink is
// non-nil, buildComponents emits a construction step per component
// (including execResult) so the dry-run report stays a property of this
// shared constructor rather than a hand-maintained mirror.
//
// A construction failure returns a nil struct and a wrapped error naming
// the failing component; owned closers (trace emitter) are NOT closed
// here — the caller owns the lifecycle.
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
) (*builtComponents, error) {

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

	te, err := buildTraceEmitter(ctx, config.TraceEmitter, resolvedHeaders, resourceOpts)
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
