package permission

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

// metricRecorder wraps a PermissionPolicy and records
// stirrup.permission.decisions on every Check() call. The policy type
// label ("allow-all" / "deny-side-effects" / "ask-upstream" /
// "policy-engine") is supplied at construction time because the
// wrapped policy itself does not carry one — the factory is the only
// place that knows the chosen policy class.
//
// Decision label mapping:
//
//   - inner returns error  → decision="error"
//   - PermissionResult.Allowed=true  → decision="allow"
//   - PermissionResult.Allowed=false → decision="deny"
//
// The "ask" decision is a property of the ask-upstream *policy class*
// rather than of any single Check call (a call always resolves to
// allow/deny once the operator responds), so the policy attribute alone
// already distinguishes ask flows on dashboards.
type metricRecorder struct {
	inner   PermissionPolicy
	metrics *observability.Metrics
	policy  string
}

// NewMetricRecorder wraps inner with metric recording. Returns inner
// unchanged when metrics is nil so the wrapper has zero overhead in
// no-metrics deployments. policy is the policy type label
// ("allow-all" / "deny-side-effects" / "ask-upstream" /
// "policy-engine") and is forwarded on every metric observation.
func NewMetricRecorder(inner PermissionPolicy, metrics *observability.Metrics, policy string) PermissionPolicy {
	if inner == nil {
		// See verifier.NewMetricRecorder: returning nil here would hide
		// the misuse until much later, so surface it at the next nil
		// dereference instead.
		return inner
	}
	if metrics == nil {
		return inner
	}
	return &metricRecorder{
		inner:   inner,
		metrics: metrics,
		policy:  policy,
	}
}

// Check delegates to the wrapped policy and records one
// stirrup.permission.decisions observation. Tool name, decision label,
// and policy type are forwarded as attributes.
func (r *metricRecorder) Check(ctx context.Context, tool types.ToolDefinition, input json.RawMessage) (*PermissionResult, error) {
	result, err := r.inner.Check(ctx, tool, input)

	var decision string
	switch {
	case err != nil:
		decision = "error"
	case result == nil:
		// Defensive: a nil result with no error is malformed but should
		// not panic the metric path. Treat as "error" so the caller
		// sees a non-success bucket on dashboards.
		decision = "error"
	case result.Allowed:
		decision = "allow"
	default:
		decision = "deny"
	}

	r.metrics.PermissionDecisions.Add(ctx, 1, metric.WithAttributes(
		attribute.String("tool", tool.Name),
		attribute.String("decision", decision),
		attribute.String("policy", r.policy),
	))

	return result, err
}

// Unwrap returns the wrapped PermissionPolicy. Exposed so callers that
// need to type-assert against the concrete policy type (tests, the
// factory's spawn_agent path) can reach through the metric wrapper. Use
// the standalone Unwrap helper below for a uniform API regardless of
// whether the policy is wrapped.
func (r *metricRecorder) Unwrap() PermissionPolicy {
	return r.inner
}

// Unwrap returns the underlying PermissionPolicy if pp is wrapped in a
// metric recorder, or pp itself otherwise. Use this whenever a caller
// needs to test the concrete policy class rather than dispatch a
// Check() — the wrapper preserves Check() semantics, but type
// assertions against *AllowAll / *DenySideEffects / *AskUpstreamPolicy /
// *PolicyEnginePolicy require unwrapping.
func Unwrap(pp PermissionPolicy) PermissionPolicy {
	for {
		w, ok := pp.(interface{ Unwrap() PermissionPolicy })
		if !ok {
			return pp
		}
		pp = w.Unwrap()
	}
}
