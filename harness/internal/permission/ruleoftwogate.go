package permission

import (
	"context"
	"encoding/json"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/types"
)

// RuleOfTwoDeniedReason is the reason string returned (and surfaced to
// the model and the permission_denied security event) when the
// Rule-of-Two gate revokes an external-communication tool. The
// "rule_of_two:" prefix is the stable grep key for operators and evals.
const RuleOfTwoDeniedReason = "rule_of_two: sensitive data observed in conversation; external communication revoked for this run"

// RuleOfTwoState is the narrow view of the Rule-of-Two runtime monitor
// the gate consults on every Check. Declared locally rather than
// importing harness/internal/ruleoftwo for the same hygiene reason
// askupstream.go declares its own Transport: permission stays a leaf
// package, and the gate's tests can substitute a three-method fake.
// ruleoftwo.Monitor satisfies this interface.
type RuleOfTwoState interface {
	// Tripped reports whether the run's sensitive-data latch is set.
	Tripped() bool
	// Enforcing reports whether detections may change run behaviour.
	Enforcing() bool
	// Action is the effective on-detect action ("warn" when not
	// enforcing).
	Action() string
}

// ruleOfTwoGate wraps a PermissionPolicy and revokes external-
// communication tools once the Rule-of-Two sensitive-data latch trips.
// Follows the metricRecorder composition pattern: Check-preserving
// wrapper with Unwrap for callers that need the concrete policy.
//
// Behaviour:
//
//   - latch not tripped, monitor not enforcing, or tool not in the
//     external-communication set → delegate to inner unchanged.
//   - tripped + action "block-external" → deny with
//     RuleOfTwoDeniedReason. The inner policy is not consulted: the
//     gate exists to restore the two-of-three invariant, and no inner
//     allow may override that.
//   - tripped + action "ask-upstream" → consult inner FIRST (a Cedar
//     forbid must still deny), then route the call through the
//     pre-built AskUpstreamPolicy so the operator adjudicates each
//     external call individually.
//
// redact, abort, and warn are loop-level actions with no permission-
// layer component; the gate delegates for those.
type ruleOfTwoGate struct {
	inner       PermissionPolicy
	state       RuleOfTwoState
	externalSet map[string]bool
	askUpstream *AskUpstreamPolicy
	metrics     *observability.Metrics
}

// NewRuleOfTwoGate wraps inner with the Rule-of-Two enforcement gate.
// externalTools keys are internal tool IDs (the Presenter never
// rewrites dispatch names, so toolset-profile aliases cannot bypass
// the set); any "mcp_"-prefixed tool name is treated as external even
// when absent from the set, covering tools registered after gate
// construction. askUpstream is consulted only for the "ask-upstream"
// action and may be nil otherwise; metrics may be nil (no action
// telemetry).
func NewRuleOfTwoGate(inner PermissionPolicy, state RuleOfTwoState, externalTools map[string]bool, askUpstream *AskUpstreamPolicy, metrics *observability.Metrics) PermissionPolicy {
	if inner == nil || state == nil {
		// Mirror NewMetricRecorder: surface the misuse at the next nil
		// dereference rather than hiding it behind a silent passthrough.
		return inner
	}
	return &ruleOfTwoGate{
		inner:       inner,
		state:       state,
		externalSet: externalTools,
		askUpstream: askUpstream,
		metrics:     metrics,
	}
}

// Check implements PermissionPolicy.
func (g *ruleOfTwoGate) Check(ctx context.Context, tool types.ToolDefinition, input json.RawMessage) (*PermissionResult, error) {
	if !g.state.Tripped() || !g.state.Enforcing() || !g.isExternal(tool.Name) {
		return g.inner.Check(ctx, tool, input)
	}
	switch g.state.Action() {
	case "ask-upstream":
		result, err := g.inner.Check(ctx, tool, input)
		if err != nil || result == nil || !result.Allowed {
			return result, err
		}
		g.recordAction(ctx, "ask-upstream")
		if g.askUpstream == nil {
			// The factory always pre-builds the ask policy for this
			// action; a nil here means a hand-assembled gate. Fail
			// closed — silently allowing would defeat the gate.
			return &PermissionResult{Allowed: false, Reason: RuleOfTwoDeniedReason}, nil
		}
		return g.askUpstream.Check(ctx, tool, input)
	case "block-external":
		g.recordAction(ctx, "block-external")
		return &PermissionResult{Allowed: false, Reason: RuleOfTwoDeniedReason}, nil
	default:
		return g.inner.Check(ctx, tool, input)
	}
}

func (g *ruleOfTwoGate) isExternal(name string) bool {
	return g.externalSet[name] || strings.HasPrefix(name, "mcp_")
}

func (g *ruleOfTwoGate) recordAction(ctx context.Context, action string) {
	if g.metrics == nil {
		return
	}
	g.metrics.RuleOfTwoActions.Add(ctx, 1, metric.WithAttributes(
		attribute.String("action", action),
	))
}

// Unwrap returns the wrapped PermissionPolicy, participating in the
// permission.Unwrap chain alongside metricRecorder.
func (g *ruleOfTwoGate) Unwrap() PermissionPolicy {
	return g.inner
}

// Rewrap reproduces the gate around a different inner policy. The
// monitor state, external set, ask policy, and metrics handle are
// shared — only the inner pointer changes — so a sub-agent's gate
// reads the same run-scoped latch as the parent's.
func (g *ruleOfTwoGate) Rewrap(inner PermissionPolicy) PermissionPolicy {
	clone := *g
	clone.inner = inner
	return &clone
}
