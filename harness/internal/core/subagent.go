package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace/noop"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/hook"
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

	maxTurns := capSubAgentMaxTurns(subConfig.MaxTurns, parentConfig.MaxTurns)

	mode := subConfig.Mode
	if mode == "" {
		mode = parentConfig.Mode
	}

	// filterToolRegistry keys on internal tool IDs, not the parent's
	// presented aliases, so the exclusion holds regardless of toolset
	// profile. Re-wrap under the parent's profile so the sub-agent sees
	// the same aliases as the parent.
	var childTools tool.ToolRegistry = filterToolRegistry(parent.Tools, "spawn_agent")
	if presenter, err := tool.NewPresenter(childTools, parent.ToolProfile); err == nil {
		childTools = presenter
	} else {
		// Degrade to the unaliased registry rather than aborting the spawn,
		// but log it: the child would otherwise use internal tool names
		// while prior turns used aliases, breaking tool-name continuity.
		profileName := ""
		if parent.ToolProfile != nil {
			profileName = parent.ToolProfile.Name
		}
		parent.Logger.Warn("child presenter build failed; sub-agent will use internal tool names",
			"err", err, "profile", profileName)
	}

	captureTp := newCaptureTransport()

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
	// Lifecycle hooks are parent-run-only: the child always gets hook.Noop
	// (below), so clear the field here too, or a persisted child trace
	// would claim hooks the sub-agent never ran.
	childConfig.Hooks = nil

	// A Cedar policy-engine policy gets a per-child clone with parentRunId
	// populated, which is what lets the subagent-capability-cap.cedar
	// starter policy match. permission.Unwrap/RewrapChain find and replace
	// the policy through any wrapper chain (Rule-of-Two gate, metric
	// recorder) so the clone doesn't shed those wrappers.
	childPermissions := parent.Permissions
	if parentPolicyEngine, ok := permission.Unwrap(parent.Permissions).(*permission.PolicyEnginePolicy); ok {
		clone := parentPolicyEngine.ForChildRun(childConfig.RunID)
		if rewrapped, ok := permission.RewrapChain(parent.Permissions, clone); ok {
			childPermissions = rewrapped
		} else {
			// Not rewrappable: keep the parent's full chain (fail toward
			// more enforcement) rather than the per-child Cedar identity.
			parent.Logger.Warn("permission chain not rewrappable; sub-agent keeps parent policy without per-child Cedar identity")
		}
	}

	// Forwards every Turn/ToolCall the child records to the parent's
	// TraceEmitter, tagged with the child's and parent's runIDs.
	childTrace := trace.NewNestedJSONLEmitter(parent.Trace, parentConfig.RunID)

	childLoop := &AgenticLoop{
		Provider:    parent.Provider,
		Providers:   parent.Providers,
		Router:      parent.Router,
		Prompt:      parent.Prompt,
		Context:     contextpkg.NewSlidingWindowStrategy(),
		Tools:       childTools,
		ToolProfile: parent.ToolProfile,
		Executor:    parent.Executor,
		Edit:        parent.Edit,
		Verifier:    verifier.NewNoneVerifier(),
		Permissions: childPermissions,
		Git:         git.NewNoneGitStrategy(),
		// A sub-agent never runs lifecycle hooks, regardless of the
		// parent's RunConfig.
		Hooks:     hook.NewNoop(),
		Transport: captureTp,
		Trace:     childTrace,
		Tracer:    tracer,
		Metrics:   parent.Metrics,
		Logger:    parent.Logger,
		Security:  parent.Security,
		// Inherited so spawn_agent can't bypass the configured guards.
		GuardRail: parent.GuardRail,
		// Shared (not copied): the latch is run-scoped, so sensitive
		// content observed by either side must tighten the whole run.
		RuleOfTwo: parent.RuleOfTwo,
		// Tags child-emitted metrics so dashboards can decompose a run
		// into parent vs sub-agent contributions.
		MetricAttrs: []attribute.KeyValue{
			attribute.Bool("run.subagent", true),
			attribute.String("run.parent_id", parentConfig.RunID),
		},
	}

	// Nests every child span under the parent's tool.spawn_agent span;
	// Run() preserves a pre-set TraceContext rather than overwriting it.
	childLoop.TraceContext = ctx

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
	// parent.metricAttrs prepends the loop's own MetricAttrs (e.g.
	// run.parent_id when the parent is itself a sub-agent); bypassing it
	// would break attribution on multi-level spawn trees.
	parent.Metrics.SubagentSpawns.Add(ctx, 1, parent.metricAttrs(
		attribute.String("parent.mode", parentMode),
		attribute.Bool("success", success),
	))
	parent.Metrics.SubagentDuration.Record(ctx, float64(elapsed.Milliseconds()), parent.metricAttrs(
		attribute.String("parent.mode", parentMode),
	))
	if runTrace == nil {
		return
	}
	parent.Metrics.SubagentTokensInput.Add(ctx, int64(runTrace.TokenUsage.Input), parent.metricAttrs(
		attribute.String("parent.mode", parentMode),
	))
	parent.Metrics.SubagentTokensOutput.Add(ctx, int64(runTrace.TokenUsage.Output), parent.metricAttrs(
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
// directly without standing up a full sub-agent loop.
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
// source except those whose internal tool IDs match any of the excluded
// names.
//
// Exclusion is checked against the resolved tool's internal Name, not the
// name from source.List(): when source is a *Presenter (the production
// case after factory wiring), List() yields the model-facing alias while
// excludedNames are internal IDs (e.g. "spawn_agent"). Keying the check on
// def.Name would let a profile that aliases an excluded tool slip it past
// the filter — defeating the spawn_agent recursion guard. Resolve returns
// the underlying tool whose Name is always the internal identity, so the
// check is profile-independent.
func filterToolRegistry(source tool.ToolRegistry, excludedNames ...string) *tool.Registry {
	excluded := make(map[string]bool, len(excludedNames))
	for _, name := range excludedNames {
		excluded[name] = true
	}

	filtered := tool.NewRegistry()
	for _, def := range source.List() {
		t := source.Resolve(def.Name)
		if t == nil || excluded[t.Name] {
			continue
		}
		filtered.Register(t)
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
