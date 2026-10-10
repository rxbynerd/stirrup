package core

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace/noop"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/hook"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/router"
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
	// TokenBudget is the token budget the sub-agent was admitted with;
	// nil when the run has no token budget.
	TokenBudget *int `json:"tokenBudget,omitempty"`
}

// SpawnSubAgent creates and runs a sub-agent that reuses the parent loop's
// provider and executor but operates with its own message history, trace,
// and transport. The sub-agent runs synchronously in the caller's
// goroutine, blocking until it completes.
//
// The sub-agent is deliberately constrained: it uses a NullTransport (no
// streaming to the control plane), a NoneVerifier, a NoneGitStrategy, and
// a fresh sliding-window context strategy. It does NOT have access to the
// spawn_agent tool, preventing infinite recursion. A read-only child mode
// also removes every workspace-mutating tool and denies them in front of
// the inherited permission chain; a read-only parent cannot spawn an
// execution child. The child charges the parent run's token ledger, is
// admitted with the run's remaining token budget as its own, and a run
// admits at most maxSpawnsPerRun sub-agents.
func SpawnSubAgent(ctx context.Context, parent *AgenticLoop, parentConfig *types.RunConfig, subConfig SubAgentConfig) (*SubAgentResult, error) {
	run, err := prepareSubAgent(ctx, parent, parentConfig, subConfig)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	runTrace, err := run.execute(ctx)
	elapsed := time.Since(start)

	recordSpawnMetrics(ctx, parent, parentConfig, run.config.Mode, runTrace, elapsed, err == nil)

	if err != nil {
		return &SubAgentResult{
			Outcome:     "error",
			Output:      err.Error(),
			TokenBudget: run.config.MaxTokenBudget,
		}, nil
	}

	output := run.capture.lastText()
	if output == "" {
		output = fmt.Sprintf("Sub-agent completed with outcome: %s", runTrace.Outcome)
	}

	return &SubAgentResult{
		Outcome:     runTrace.Outcome,
		Output:      output,
		Turns:       runTrace.Turns,
		TokenBudget: run.config.MaxTokenBudget,
	}, nil
}

// subAgentRun is a fully assembled, not yet started sub-agent.
type subAgentRun struct {
	loop    *AgenticLoop
	config  *types.RunConfig
	capture *captureTransport
	// effectiveTools holds the internal IDs of every tool the child is
	// offered, in registration order.
	effectiveTools []string
}

// execute runs the child loop and stamps the effective tool list on the
// returned trace.
func (r *subAgentRun) execute(ctx context.Context) (*types.RunTrace, error) {
	runTrace, err := r.loop.Run(ctx, r.config)
	if runTrace != nil {
		runTrace.EffectiveTools = r.effectiveTools
	}
	return runTrace, err
}

// prepareSubAgent validates the requested child mode against the parent's
// and assembles the child loop and the RunConfig it records. A rejected
// request returns an error before any child component is built.
func prepareSubAgent(ctx context.Context, parent *AgenticLoop, parentConfig *types.RunConfig, subConfig SubAgentConfig) (*subAgentRun, error) {
	if subConfig.Prompt == "" {
		return nil, fmt.Errorf("sub-agent prompt must not be empty")
	}

	mode := subConfig.Mode
	if mode == "" {
		mode = parentConfig.Mode
	}
	if err := checkSubAgentMode(parentConfig.Mode, mode); err != nil {
		return nil, err
	}
	readOnly := types.IsReadOnlyMode(mode)

	maxTurns := capSubAgentMaxTurns(subConfig.MaxTurns, parentConfig.MaxTurns)

	// filterToolRegistry keys on internal tool IDs, not the parent's
	// presented aliases, so the exclusions hold regardless of toolset
	// profile.
	childRegistry := filterToolRegistry(parent.Tools, "spawn_agent")
	var mutating map[string]bool
	if readOnly {
		mutating = mutatingToolSet(childRegistry)
		childRegistry = filterToolRegistry(childRegistry, slices.Collect(maps.Keys(mutating))...)
	}
	effectiveTools, builtIns := subAgentToolNames(childRegistry)
	if readOnly && len(builtIns) == 0 {
		return nil, fmt.Errorf("cannot spawn a %q sub-agent: no read-only built-in tools remain once workspace-mutating tools are removed", mode)
	}

	// Admission is the last refusal, so only spawns that go on to run
	// count toward the per-run cap.
	tokenBudget := parentConfig.MaxTokenBudget
	if ledger := parent.activeLedger(); ledger != nil {
		admitted, err := ledger.AdmitSpawn()
		if err != nil {
			return nil, err
		}
		tokenBudget = admitted
	}

	// Re-wrap under the parent's profile so the sub-agent sees the same
	// aliases as the parent.
	var childTools tool.ToolRegistry = childRegistry
	if presenter, err := tool.NewPresenter(childRegistry, parent.ToolProfile); err == nil {
		childTools = presenter
	} else {
		// Degrade to the unaliased registry rather than aborting the spawn,
		// but log it: the child would otherwise use internal tool names
		// while prior turns used aliases, breaking tool-name continuity.
		profileName := ""
		if parent.ToolProfile != nil {
			profileName = parent.ToolProfile.Name
		}
		parent.Logger.WarnContext(ctx, "child presenter build failed; sub-agent will use internal tool names",
			"err", err, "profile", profileName)
	}

	captureTp := newCaptureTransport()

	tracer := parent.Tracer
	if tracer == nil {
		tracer = noop.NewTracerProvider().Tracer("")
	}

	// Build the child RunConfig first so we have the child run ID to
	// thread through the Cedar policy clone below. The recorded tools,
	// permission policy and context strategy describe what the child
	// enforces, not what the parent configured.
	childConfig := *parentConfig
	childConfig.RunID = subAgentRunID(parentConfig.RunID, parent.subAgentSpawns.Add(1))
	childConfig.Prompt = subConfig.Prompt
	childConfig.Mode = mode
	childConfig.MaxTurns = maxTurns
	childConfig.MaxTokenBudget = tokenBudget
	childConfig.GitStrategy = types.GitStrategyConfig{Type: "none"}
	childConfig.OutputSchema = nil
	childConfig.Tools.BuiltIn = builtIns
	childConfig.ContextStrategy = types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: parentConfig.ContextStrategy.MaxTokens}
	if readOnly {
		childConfig.PermissionPolicy = readOnlyChildPermissionPolicy(parentConfig.PermissionPolicy)
	}
	// Lifecycle hooks are parent-run-only: the child always gets hook.Noop
	// (below), so clear the field here too, or a persisted child trace
	// would claim hooks the sub-agent never ran.
	childConfig.Hooks = nil

	// A Cedar policy-engine policy gets a per-child clone with parentRunId
	// and the child's mode populated, which is what lets the
	// subagent-capability-cap.cedar starter policy and principal.mode
	// rules match. permission.Unwrap/RewrapChain find and replace the
	// policy through any wrapper chain (Rule-of-Two gate, metric
	// recorder) so the clone doesn't shed those wrappers.
	childPermissions := parent.Permissions
	if parentPolicyEngine, ok := permission.Unwrap(parent.Permissions).(*permission.PolicyEnginePolicy); ok {
		clone := parentPolicyEngine.ForChildRun(childConfig.RunID, mode)
		if rewrapped, ok := permission.RewrapChain(parent.Permissions, clone); ok {
			childPermissions = rewrapped
		} else {
			// Not rewrappable: keep the parent's full chain (fail toward
			// more enforcement) rather than the per-child Cedar identity.
			parent.Logger.WarnContext(ctx, "permission chain not rewrappable; sub-agent keeps parent policy without per-child Cedar identity")
		}
	}
	if readOnly {
		childPermissions = permission.NewSideEffectGate(mutating, childPermissions)
	}

	// Forwards every Turn/ToolCall the child records to the parent's
	// TraceEmitter, tagged with the child's and parent's runIDs.
	childTrace := trace.NewNestedJSONLEmitter(parent.Trace, parentConfig.RunID, ctx)

	var childGuidance prompt.GuidanceLoader
	if len(parent.repositoryGuidance) > 0 {
		childGuidance = guidanceSnapshot(parent.repositoryGuidance)
	}

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
		Hooks:             hook.NewNoop(),
		Guidance:          childGuidance,
		Transport:         captureTp,
		Trace:             childTrace,
		Tracer:            tracer,
		Metrics:           parent.Metrics,
		Logger:            parent.Logger,
		Security:          parent.Security,
		CommandOutput:     parent.CommandOutput,
		OwnsCommandOutput: false,
		ParentRunID:       parentConfig.RunID,
		// Inherited so spawn_agent can't bypass the configured guards.
		GuardRail: parent.GuardRail,
		// Shared (not copied): the latch is run-scoped, so sensitive
		// content observed by either side must tighten the whole run.
		RuleOfTwo: parent.RuleOfTwo,
		// Shared so the child's turns and auxiliary calls charge the
		// parent run's ledger.
		ledger: parent.ledger,
		// Tags child-emitted metrics so dashboards can decompose a run
		// into parent vs sub-agent contributions.
		MetricAttrs: []attribute.KeyValue{
			attribute.Bool("run.subagent", true),
			attribute.String("run.parent_id", parentConfig.RunID),
		},
	}

	childLoop.StructuredResults = parent.StructuredResults
	childLoop.ContextWindows = parent.ContextWindows
	// Copied so a sub-agent cannot write a path the parent run protects;
	// the child's Run adds the inherited guidance snapshot's files.
	childLoop.ProtectedWriteTargets = parent.ProtectedWriteTargets

	return &subAgentRun{
		loop:           childLoop,
		config:         &childConfig,
		capture:        captureTp,
		effectiveTools: effectiveTools,
	}, nil
}

// checkSubAgentMode rejects a child mode that would widen a read-only
// parent's authority. Any child mode may be requested from a parent that
// is not read-only; a read-only parent may request only read-only modes.
func checkSubAgentMode(parentMode, childMode string) error {
	if types.IsReadOnlyMode(parentMode) && !types.IsReadOnlyMode(childMode) {
		return fmt.Errorf("a %q run cannot spawn a %q sub-agent: a read-only run may only spawn planning, review, research or toil sub-agents", parentMode, childMode)
	}
	return nil
}

// subAgentToolNames returns the internal IDs of every tool in registry
// and the subset that are built-in tools, both in registration order.
func subAgentToolNames(registry *tool.Registry) (all, builtIns []string) {
	for _, def := range registry.List() {
		all = append(all, def.Name)
		if types.IsBuiltInTool(def.Name) {
			builtIns = append(builtIns, def.Name)
		}
	}
	return all, builtIns
}

// readOnlyChildPermissionPolicy returns the permission policy a read-only
// child records. The child denies workspace-mutating tools in front of
// the parent's policy, so an allow-all parent (or an allow-all
// policy-engine fallback) is enforced as deny-side-effects.
func readOnlyChildPermissionPolicy(parent types.PermissionPolicyConfig) types.PermissionPolicyConfig {
	child := parent
	switch {
	case child.Type == "allow-all":
		child = types.PermissionPolicyConfig{Type: "deny-side-effects"}
	case child.Type == "policy-engine" && child.Fallback == "allow-all":
		child.Fallback = "deny-side-effects"
	}
	return child
}

// recordSpawnMetrics emits stirrup.subagent.{spawns,duration,
// tokens.input,tokens.output,tokens.cache_read,tokens.cache_write} for
// one sub-agent run; the cache counters only when non-zero. parent.mode is
// the *parent loop's* mode (not the sub-agent's), so dashboards can
// attribute sub-agent activity to the calling run mode (e.g. an
// execution-mode parent spawning a research-mode child still appears
// under parent.mode=execution). The token counters also carry the provider
// and model the router selects for the sub-agent's mode on its first turn;
// a router that switches models mid-run still attributes the sub-agent's
// whole total to that first selection. childMode is the sub-agent's mode.
// A nil parent.Metrics short-circuits.
//
// runTrace may be nil when the child loop returned an error before any
// trace was assembled; in that case the spawn counter still fires
// (with success=false) but token counters are skipped.
func recordSpawnMetrics(ctx context.Context, parent *AgenticLoop, parentConfig *types.RunConfig, childMode string, runTrace *types.RunTrace, elapsed time.Duration, success bool) {
	if parent == nil || parent.Metrics == nil {
		return
	}
	parentMode := parentConfig.Mode
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
	selection := parent.Router.Select(ctx, router.RouterContext{Mode: childMode})
	tokenAttrs := parent.metricAttrs(append(
		observability.ProviderModelAttributes(
			providerTypeFor(parentConfig, selection.Provider),
			selection.Model,
		),
		attribute.String("parent.mode", parentMode),
	)...)
	parent.Metrics.SubagentTokensInput.Add(ctx, int64(runTrace.TokenUsage.Input), tokenAttrs)
	parent.Metrics.SubagentTokensOutput.Add(ctx, int64(runTrace.TokenUsage.Output), tokenAttrs)
	if runTrace.TokenUsage.CacheRead > 0 {
		parent.Metrics.SubagentTokensCacheRead.Add(ctx, int64(runTrace.TokenUsage.CacheRead), tokenAttrs)
	}
	if runTrace.TokenUsage.CacheWrite > 0 {
		parent.Metrics.SubagentTokensCacheWrite.Add(ctx, int64(runTrace.TokenUsage.CacheWrite), tokenAttrs)
	}
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

// subAgentRunID names the n-th sub-agent of a run as
// "<parentRunID>/sub-<n>".
func subAgentRunID(parentRunID string, n int64) string {
	if parentRunID == "" {
		return fmt.Sprintf("sub-%d", n)
	}
	return fmt.Sprintf("%s/sub-%d", parentRunID, n)
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

// guidanceSnapshot replays guidance a parent run already loaded and
// verified, so a sub-agent's prompt matches the parent's.
type guidanceSnapshot []prompt.GuidanceDocument

func (s guidanceSnapshot) Load(context.Context) ([]prompt.GuidanceDocument, error) {
	return s, nil
}
