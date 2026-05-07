package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace/noop"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

const (
	// defaultSubAgentMaxTurns is the default max turns for a sub-agent when
	// the caller does not specify one.
	defaultSubAgentMaxTurns = 10

	// maxSubAgentMaxTurns is the hard upper bound on sub-agent turns,
	// regardless of what the caller requests.
	maxSubAgentMaxTurns = 20
)

// SubAgentConfig describes how to spawn a sub-agent.
type SubAgentConfig struct {
	Prompt   string `json:"prompt"`
	Mode     string `json:"mode"`
	MaxTurns int    `json:"maxTurns"`
}

// SubAgentResult holds the outcome of a sub-agent run.
type SubAgentResult struct {
	Outcome string `json:"outcome"`
	Output  string `json:"output"`
	Turns   int    `json:"turns"`
}

// SpawnSubAgent creates and runs a sub-agent that reuses the parent loop's
// provider, executor, and tools but operates with its own message history,
// trace, and transport. The sub-agent runs synchronously in the caller's
// goroutine, blocking until it completes.
//
// The sub-agent is deliberately constrained: it uses a NullTransport (no
// streaming to the control plane), a NoneVerifier, a NoneGitStrategy, and
// a fresh sliding-window context strategy. It does NOT have access to the
// spawn_agent tool, preventing infinite recursion.
func SpawnSubAgent(ctx context.Context, parent *AgenticLoop, parentConfig *types.RunConfig, subConfig SubAgentConfig) (*SubAgentResult, error) {
	if subConfig.Prompt == "" {
		return nil, fmt.Errorf("sub-agent prompt must not be empty")
	}

	// Determine max turns via the dedicated helper so tests can exercise
	// the capping branches without driving an entire SpawnSubAgent path
	// (#55, B5).
	maxTurns := capSubAgentMaxTurns(subConfig.MaxTurns, parentConfig.MaxTurns)

	// Determine mode.
	mode := subConfig.Mode
	if mode == "" {
		mode = parentConfig.Mode
	}

	// Build a child tool registry that excludes spawn_agent to prevent recursion.
	childTools := filterToolRegistry(parent.Tools, "spawn_agent")

	// Use a capture transport to collect the sub-agent's text output.
	// The capture transport wraps a NullTransport, recording text_delta
	// events so we can extract the final assistant response.
	captureTp := newCaptureTransport()

	// Use the parent's tracer if available, otherwise noop.
	tracer := parent.Tracer
	if tracer == nil {
		tracer = noop.NewTracerProvider().Tracer("")
	}

	// Build the child RunConfig first so we have the child run ID to
	// thread through the Cedar policy clone below.
	childConfig := *parentConfig
	childConfig.RunID = fmt.Sprintf("sub-%d", time.Now().UnixNano())
	childConfig.Prompt = subConfig.Prompt
	childConfig.Mode = mode
	childConfig.MaxTurns = maxTurns
	childConfig.GitStrategy = types.GitStrategyConfig{Type: "none"}

	// Permissions: when the parent is a Cedar policy-engine policy, the
	// sub-agent gets its own clone with parentRunId populated. This is
	// the only path that activates the subagent-capability-cap.cedar
	// starter policy — without it, principal.parentRunId is absent and
	// `has parentRunId` evaluates to false for every sub-agent run,
	// silently negating the policy (M3).
	childPermissions := parent.Permissions
	if parentPolicyEngine, ok := parent.Permissions.(*permission.PolicyEnginePolicy); ok {
		childPermissions = parentPolicyEngine.ForChildRun(childConfig.RunID)
	}

	// Forwarding trace emitter: every Turn / ToolCall the child records
	// is forwarded live to the parent's TraceEmitter, tagged with the
	// child's runID and the parent's runID. Replaces the previous
	// bytes.Buffer{} sink, which discarded every sub-agent trace event.
	// See harness/internal/trace/nested_jsonl.go.
	childTrace := trace.NewNestedJSONLEmitter(parent.Trace, parentConfig.RunID)

	// Build the child loop, reusing parent components where safe.
	childLoop := &AgenticLoop{
		Provider:    parent.Provider,
		Providers:   parent.Providers,
		Router:      parent.Router,
		Prompt:      parent.Prompt,
		Context:     contextpkg.NewSlidingWindowStrategy(),
		Tools:       childTools,
		Executor:    parent.Executor,
		Edit:        parent.Edit,
		Verifier:    verifier.NewNoneVerifier(),
		Permissions: childPermissions,
		Git:         git.NewNoneGitStrategy(),
		Transport:   captureTp,
		Trace:       childTrace,
		Tracer:      tracer,
		Metrics:     parent.Metrics,
		Logger:      parent.Logger,
		Security:    parent.Security,
		// Inherit the parent's GuardRail so spawned sub-agents are
		// not a silent escape hatch around the configured guards.
		// Without this, an indirect-injection payload could route
		// harmful work through spawn_agent and bypass all phases.
		GuardRail: parent.GuardRail,
		// Tag every metric observation emitted from the child so
		// dashboards can decompose a run into parent vs sub-agent
		// contributions. The parent's run id is preserved as
		// run.parent_id so correlated traces and metrics line up.
		// Attribute keys follow the run.* namespace convention used by
		// every other run-scoped attribute (run.mode, run.id, etc.).
		MetricAttrs: []attribute.KeyValue{
			attribute.Bool("run.subagent", true),
			attribute.String("run.parent_id", parentConfig.RunID),
		},
	}

	// Inherit the parent's tool-span ctx as the child's TraceContext so
	// every span the child loop creates (turn[N], tool.<name>, etc.)
	// nests under the parent's tool.spawn_agent span. The Run() method
	// preserves a pre-set TraceContext rather than overwriting it.
	childLoop.TraceContext = ctx

	// Run the child loop synchronously while timing the spawn for
	// stirrup.subagent.duration_ms / stirrup.subagent.spawns. Token
	// observations come from the child's RunTrace (TokenUsage) so the
	// counts align exactly with what was billed to the run.
	start := time.Now()
	runTrace, err := childLoop.Run(ctx, &childConfig)
	elapsed := time.Since(start)

	parentMode := parentConfig.Mode
	recordSpawnMetrics(ctx, parent, parentMode, runTrace, elapsed, err == nil)

	if err != nil {
		return &SubAgentResult{
			Outcome: "error",
			Output:  err.Error(),
		}, nil
	}

	// Extract the output: prefer the captured text from the transport,
	// falling back to a summary string.
	output := captureTp.lastText()
	if output == "" {
		output = fmt.Sprintf("Sub-agent completed with outcome: %s", runTrace.Outcome)
	}

	return &SubAgentResult{
		Outcome: runTrace.Outcome,
		Output:  output,
		Turns:   runTrace.Turns,
	}, nil
}

// recordSpawnMetrics emits stirrup.subagent.{spawns,duration_ms,
// tokens.input,tokens.output} for one sub-agent run. parent.mode is
// the *parent loop's* mode (not the sub-agent's), so dashboards can
// attribute sub-agent activity to the calling run mode (e.g. an
// execution-mode parent spawning a research-mode child still appears
// under parent.mode=execution). A nil parent.Metrics short-circuits.
//
// runTrace may be nil when the child loop returned an error before any
// trace was assembled; in that case the spawn counter still fires
// (with success=false) but token counters are skipped.
func recordSpawnMetrics(ctx context.Context, parent *AgenticLoop, parentMode string, runTrace *types.RunTrace, elapsed time.Duration, success bool) {
	if parent == nil || parent.Metrics == nil {
		return
	}
	parent.Metrics.SubagentSpawns.Add(ctx, 1, metric.WithAttributes(
		attribute.String("parent.mode", parentMode),
		attribute.Bool("success", success),
	))
	parent.Metrics.SubagentDuration.Record(ctx, float64(elapsed.Milliseconds()), metric.WithAttributes(
		attribute.String("parent.mode", parentMode),
	))
	if runTrace == nil {
		return
	}
	parent.Metrics.SubagentTokensInput.Add(ctx, int64(runTrace.TokenUsage.Input), metric.WithAttributes(
		attribute.String("parent.mode", parentMode),
	))
	parent.Metrics.SubagentTokensOutput.Add(ctx, int64(runTrace.TokenUsage.Output), metric.WithAttributes(
		attribute.String("parent.mode", parentMode),
	))
}

// capSubAgentMaxTurns returns the effective MaxTurns a sub-agent should
// run with, given the caller-requested value and the parent run's own
// MaxTurns budget. The capping rules are, in order:
//
//  1. A non-positive request (zero) defaults to defaultSubAgentMaxTurns.
//  2. Cap at maxSubAgentMaxTurns regardless of the request.
//  3. Cap at the parent's MaxTurns so the child cannot exceed the
//     parent's overall budget.
//
// Pulled out of SpawnSubAgent so tests can exercise the branches
// directly without standing up a full sub-agent loop (#55, B5).
func capSubAgentMaxTurns(requested, parentMaxTurns int) int {
	maxTurns := requested
	if maxTurns <= 0 {
		maxTurns = defaultSubAgentMaxTurns
	}
	if maxTurns > maxSubAgentMaxTurns {
		maxTurns = maxSubAgentMaxTurns
	}
	if maxTurns > parentMaxTurns {
		maxTurns = parentMaxTurns
	}
	return maxTurns
}

// filterToolRegistry creates a new Registry containing all tools from the
// source except those whose names match any of the excluded names.
func filterToolRegistry(source tool.ToolRegistry, excludedNames ...string) *tool.Registry {
	excluded := make(map[string]bool, len(excludedNames))
	for _, name := range excludedNames {
		excluded[name] = true
	}

	filtered := tool.NewRegistry()
	for _, def := range source.List() {
		if excluded[def.Name] {
			continue
		}
		t := source.Resolve(def.Name)
		if t != nil {
			filtered.Register(t)
		}
	}
	return filtered
}

// captureTransport wraps a NullTransport but records all text_delta events
// emitted during the sub-agent run, allowing extraction of the sub-agent's
// accumulated response text. Text is never reset, so lastText() returns the
// concatenated output from the entire run.
type captureTransport struct {
	transport.NullTransport
	segments []string
}

func newCaptureTransport() *captureTransport {
	return &captureTransport{}
}

func (t *captureTransport) Emit(event types.HarnessEvent) error {
	switch event.Type {
	case "text_delta":
		t.segments = append(t.segments, event.Text)
	case "done":
		// Don't reset — we want the accumulated text from the entire run.
	}
	return nil
}

// lastText returns the concatenated text from the last assistant response.
// If the sub-agent made multiple turns with text output, this returns all
// text content from the run.
func (t *captureTransport) lastText() string {
	if len(t.segments) == 0 {
		return ""
	}
	return strings.Join(t.segments, "")
}
