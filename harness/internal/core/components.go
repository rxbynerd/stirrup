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
// The parity invariant the type enforces is: a probe-eligible component
// cannot be added to BuildLoop without also surfacing in the dry-run.
// Because both paths construct the set through buildComponents, and
// Preflight derives its step list from probeSteps(), a new component added
// to buildComponents is automatically probed by Preflight. The matching
// parity test (TestPreflightParity) fails if a component in probeSteps()
// produces no step in a representative-config Preflight report — catching
// the inverse drift where a step name diverges from the probe set.
type builtComponents struct {
	// defaultProvider is the adapter for config.Provider; it is also keyed
	// into providers by its type, so it is probed via the providers map and
	// is retained here only because BuildLoop assembles the loop around it.
	defaultProvider provider.ProviderAdapter
	providers       map[string]provider.ProviderAdapter

	// executor is the constructed executor. The two callers supply it via
	// divergent construction (BuildLoop starts a real container; Preflight
	// substitutes a read-only engine probe — see preflightExecutor), so it
	// is passed into buildComponents rather than constructed here.
	executor executor.Executor

	permissionPolicy permission.PermissionPolicy
	traceEmitter     trace.TraceEmitter

	// resolvedHeaders are the secret://-dereferenced traceEmitter.Headers.
	// BuildLoop reuses them for the metrics exporter so traces and metrics
	// authenticate identically; retained here to avoid a second resolve.
	resolvedHeaders map[string]string
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
// builtComponents that represents a reachable component is enumerated here,
// so adding a component to buildComponents without adding it to probeSteps
// is the omission TestPreflightParity catches.
//
// Providers are enumerated by sorted name so a multi-provider config yields
// a reproducible step order (matching --output=json determinism).
func (b *builtComponents) probeSteps() []probeComponentStep {
	steps := make([]probeComponentStep, 0, len(b.providers)+3)

	names := make([]string, 0, len(b.providers))
	for name := range b.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		steps = append(steps, probeComponentStep{name: "provider-probe:" + name, component: b.providers[name]})
	}

	steps = append(steps,
		probeComponentStep{name: "executor-probe", component: b.executor},
		probeComponentStep{name: "permission-policy-probe", component: b.permissionPolicy},
		probeComponentStep{name: "trace-emitter-probe", component: b.traceEmitter},
	)
	return steps
}

// buildComponents constructs the probe-eligible components shared by
// BuildLoopWithTransport and Preflight: the provider adapters, the
// permission policy, and the trace emitter. The executor is supplied by the
// caller (its construction semantics diverge — see builtComponents.executor)
// and threaded onto the returned struct so probeSteps enumerates it.
//
// resolvedHeaders must be the already-dereferenced traceEmitter.Headers
// (the caller resolves them once and reuses them for the metrics exporter);
// resourceOpts is the OTel resource identity. registry and tp are inputs to
// the permission policy (mutating/approval tool sets, ask-upstream
// transport). secLogger receives security events from constructed
// components; pass an io.Discard-backed logger in a dry-run.
//
// A construction failure returns a nil struct and an error naming the
// failing component so the caller can map it to a failed preflight step or a
// build error. Owned closers (trace emitter) are NOT closed here — the
// caller owns the lifecycle.
func buildComponents(
	ctx context.Context,
	config *types.RunConfig,
	secrets security.SecretStore,
	secLogger *security.SecurityLogger,
	registry *tool.Registry,
	tp transport.Transport,
	exec executor.Executor,
	resolvedHeaders map[string]string,
	resourceOpts observability.ResourceOptions,
) (*builtComponents, error) {
	// Error prefixes mirror BuildLoop's historical inline messages
	// ("build providers", etc.) so the composition root's operator-facing
	// error text is unchanged by the extraction.
	defaultProvider, providers, err := buildProviders(ctx, config, secrets)
	if err != nil {
		return nil, fmt.Errorf("build providers: %w", err)
	}

	pp, err := buildPermissionPolicy(config, registry, tp, secLogger)
	if err != nil {
		return nil, fmt.Errorf("build permission policy: %w", err)
	}

	te, err := buildTraceEmitter(ctx, config.TraceEmitter, resolvedHeaders, resourceOpts)
	if err != nil {
		return nil, fmt.Errorf("build trace emitter: %w", err)
	}

	return &builtComponents{
		defaultProvider:  defaultProvider,
		providers:        providers,
		executor:         exec,
		permissionPolicy: pp,
		traceEmitter:     te,
		resolvedHeaders:  resolvedHeaders,
	}, nil
}
