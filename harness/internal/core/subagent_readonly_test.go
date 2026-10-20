package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/tool/builtins"
	"github.com/rxbynerd/stirrup/types"
)

var (
	readOnlyModeNames = []string{"planning", "review", "research", "toil"}

	// mutatingSpawnTools are the parent's workspace-mutating tools; a
	// read-only child must be offered none of them.
	mutatingSpawnTools = []string{"write_file", "edit_file", "run_command", "search_replace", "apply_diff", "mcp_docs_update"}
)

// spawnToolCalls counts handler executions per internal tool ID.
type spawnToolCalls struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *spawnToolCalls) record(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[name]++
}

func (c *spawnToolCalls) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[name]
}

// spawnParentRegistry registers stand-ins for the parent's tools with
// production-accurate WorkspaceMutating/RequiresApproval flags. The
// mutating built-ins are left out when withMutatingBuiltIns is false, as
// for a read-only parent; the MCP tool is always mutating, as
// mcp.Client registers it.
func spawnParentRegistry(t *testing.T, withMutatingBuiltIns bool) (*tool.Registry, *spawnToolCalls) {
	t.Helper()
	calls := &spawnToolCalls{calls: map[string]int{}}
	registry := tool.NewRegistry()
	add := func(name string, mutating, approval bool) {
		registry.Register(&tool.Tool{
			Name:              name,
			Description:       "stub " + name,
			InputSchema:       json.RawMessage(`{"type":"object","properties":{}}`),
			WorkspaceMutating: mutating,
			RequiresApproval:  approval,
			Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
				calls.record(name)
				return "ran " + name, nil
			},
		})
	}
	add("read_file", false, false)
	add("grep_files", false, false)
	add("web_fetch", false, true)
	if withMutatingBuiltIns {
		for _, name := range []string{"write_file", "edit_file", "run_command", "search_replace", "apply_diff"} {
			add(name, true, true)
		}
	}
	add("mcp_docs_update", true, true)
	registry.Register(tool.ControlPlaneTool(types.ControlPlaneToolConfig{
		Name:        "ticket_lookup",
		Description: "look up a ticket",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	}))
	add("spawn_agent", false, true)
	return registry, calls
}

// permissionRequestTransport is a permission.Transport that records every
// permission_request it is asked to emit and never answers.
type permissionRequestTransport struct {
	mu       sync.Mutex
	requests []string
}

func (p *permissionRequestTransport) Emit(event types.HarnessEvent) error {
	if event.Type == "permission_request" {
		p.mu.Lock()
		p.requests = append(p.requests, event.ToolName)
		p.mu.Unlock()
	}
	return nil
}

func (p *permissionRequestTransport) OnControl(func(types.ControlEvent)) {}

func (p *permissionRequestTransport) requested() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests)
}

func spawnApprovalSet(registry *tool.Registry) map[string]bool {
	out := map[string]bool{}
	for _, def := range registry.List() {
		if registry.Resolve(def.Name).RequiresApproval {
			out[def.Name] = true
		}
	}
	return out
}

// offeredToolIDs returns the internal IDs of the tools a loop offers the
// model.
func offeredToolIDs(loop *AgenticLoop) []string {
	var ids []string
	for _, def := range loop.Tools.List() {
		if t := loop.Tools.Resolve(def.Name); t != nil {
			ids = append(ids, t.Name)
		}
	}
	return ids
}

// spawnCountingProvider scripts one response per call and counts the calls.
type spawnCountingProvider struct {
	scriptedProvider
	calls atomic.Int32
}

func (c *spawnCountingProvider) Stream(ctx context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	c.calls.Add(1)
	return c.scriptedProvider.Stream(ctx, params)
}

func spawnTextTurn(text string) []types.StreamEvent {
	return []types.StreamEvent{
		{Type: "text_delta", Text: text},
		{Type: "message_complete", StopReason: "end_turn"},
	}
}

func spawnToolCallTurn(names ...string) []types.StreamEvent {
	events := make([]types.StreamEvent, 0, len(names)+1)
	for _, name := range names {
		events = append(events, types.StreamEvent{Type: "tool_call", ID: "tc_" + name, Name: name, Input: map[string]any{}})
	}
	return append(events, types.StreamEvent{Type: "message_complete", StopReason: "tool_use"})
}

// TestPrepareSubAgent_ReadOnlyChildToolsAndPermissions covers every
// read-only mode under an allow-all and an ask-upstream execution
// parent: the child is offered no mutating tool, its permission chain
// denies each one without asking upstream, and its recorded config
// matches what it enforces.
func TestPrepareSubAgent_ReadOnlyChildToolsAndPermissions(t *testing.T) {
	parents := []struct {
		policy     string
		wantPolicy string
	}{
		{policy: "allow-all", wantPolicy: "deny-side-effects"},
		{policy: "ask-upstream", wantPolicy: "ask-upstream"},
	}
	for _, parentPolicy := range parents {
		for _, mode := range readOnlyModeNames {
			t.Run(parentPolicy.policy+"/"+mode, func(t *testing.T) {
				must := require.New(t)
				registry, _ := spawnParentRegistry(t, true)
				parent := buildSubAgentTestLoop(&mockProvider{})
				parent.Tools = registry
				rt := &permissionRequestTransport{}
				if parentPolicy.policy == "ask-upstream" {
					parent.Permissions = permission.NewAskUpstreamPolicy(rt, spawnApprovalSet(registry), 50*time.Millisecond)
				}
				parentConfig := buildTestConfig()
				parentConfig.PermissionPolicy = types.PermissionPolicyConfig{Type: parentPolicy.policy}

				run, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "investigate", Mode: mode})
				must.NoError(err, "prepareSubAgent")

				wantOffered := []string{"read_file", "grep_files", "web_fetch", "ticket_lookup"}
				must.Equal(wantOffered, offeredToolIDs(run.loop), "offered tools")
				must.Equal(wantOffered, run.effectiveTools, "effective tools")

				for _, name := range mutatingSpawnTools {
					result, err := run.loop.Permissions.Check(context.Background(), types.ToolDefinition{Name: name}, json.RawMessage(`{}`))
					must.NoError(err, "Check(%s)", name)
					must.False(result.Allowed, "child permission chain allowed %s", name)
				}
				must.Empty(rt.requested(), "mutating checks must not ask upstream")

				must.Equal([]string{"read_file", "grep_files", "web_fetch"}, run.config.Tools.BuiltIn, "recorded tools.builtIn")
				must.Equal(parentPolicy.wantPolicy, run.config.PermissionPolicy.Type, "recorded permission policy")
				must.Equal(mode, run.config.Mode, "recorded mode")
			})
		}
	}
}

// TestSpawnSubAgent_ReadOnlyChildCannotDispatchMutatingTools drives a
// research child whose model calls every mutating tool by name: none of
// the handlers runs and every call is recorded as failed.
func TestSpawnSubAgent_ReadOnlyChildCannotDispatchMutatingTools(t *testing.T) {
	must := require.New(t)
	registry, calls := spawnParentRegistry(t, true)
	parent := buildSubAgentTestLoop(&mockProvider{})
	parent.Tools = registry
	parent.Provider = &scriptedProvider{turns: [][]types.StreamEvent{
		// read_file breaks the consecutive-failure run so the stall
		// detector does not end the child before every name is tried.
		spawnToolCallTurn("write_file", "edit_file", "run_command", "read_file"),
		spawnToolCallTurn("search_replace", "apply_diff", "mcp_docs_update"),
		spawnTextTurn("done"),
	}}
	parentTrace := &recordingTraceEmitter{}
	parent.Trace = parentTrace

	_, err := SpawnSubAgent(context.Background(), parent, buildTestConfig(), SubAgentConfig{
		Prompt: "edit things",
		Mode:   "research",
	})
	must.NoError(err, "SpawnSubAgent")

	for _, name := range mutatingSpawnTools {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			must.Zero(calls.count(name), "handler must not run in a research child")
		})
	}
	must.Equal(1, calls.count("read_file"), "read_file handler runs")

	_, toolCalls := parentTrace.snapshot()
	must.Len(toolCalls, len(mutatingSpawnTools)+1)
	for _, tc := range toolCalls {
		t.Run("trace "+tc.Name, func(t *testing.T) {
			must := require.New(t)
			wantSuccess := tc.Name == "read_file"
			must.Equal(wantSuccess, tc.Success, "recorded success")
		})
	}
}

// TestSpawnSubAgent_ReadOnlyChildOfAskUpstreamParentNeverAsks pins that a
// read-only child of an ask-upstream parent raises no approval request
// for mutating tools, while a non-mutating approval-required tool still
// asks upstream.
func TestSpawnSubAgent_ReadOnlyChildOfAskUpstreamParentNeverAsks(t *testing.T) {
	check := assert.New(t)
	must := require.New(t)
	registry, calls := spawnParentRegistry(t, true)
	parent := buildSubAgentTestLoop(&mockProvider{})
	parent.Tools = registry
	rt := &permissionRequestTransport{}
	parent.Permissions = permission.NewAskUpstreamPolicy(rt, spawnApprovalSet(registry), 50*time.Millisecond)
	parent.Provider = &scriptedProvider{turns: [][]types.StreamEvent{
		spawnToolCallTurn("write_file", "run_command", "mcp_docs_update"),
		spawnToolCallTurn("web_fetch"),
		spawnTextTurn("done"),
	}}
	parentConfig := buildTestConfig()
	parentConfig.PermissionPolicy = types.PermissionPolicyConfig{Type: "ask-upstream"}

	_, err := SpawnSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{
		Prompt: "edit things",
		Mode:   "research",
	})
	must.NoError(err, "SpawnSubAgent")

	check.Equal([]string{"web_fetch"}, rt.requested(), "permission requests")
	for _, name := range []string{"write_file", "run_command", "mcp_docs_update"} {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			must.Zero(calls.count(name), "handler must not run")
		})
	}
}

// TestSpawnSubAgent_ReadOnlyParentCannotSpawnExecutionChild pins that a
// read-only parent's request for an execution child is rejected as a
// spawn_agent tool error before the child sends any provider request.
func TestSpawnSubAgent_ReadOnlyParentCannotSpawnExecutionChild(t *testing.T) {
	for _, parentMode := range readOnlyModeNames {
		t.Run(parentMode, func(t *testing.T) {
			must := require.New(t)
			registry, _ := spawnParentRegistry(t, false)
			parent := buildSubAgentTestLoop(&mockProvider{})
			parent.Tools = registry
			prov := &spawnCountingProvider{scriptedProvider: scriptedProvider{turns: [][]types.StreamEvent{spawnTextTurn("child ran")}}}
			parent.Provider = prov
			parentConfig := buildTestConfig()
			parentConfig.Mode = parentMode
			parentConfig.PermissionPolicy = types.PermissionPolicyConfig{Type: "deny-side-effects"}

			spawn := builtins.SpawnAgentTool(func(ctx context.Context, prompt, mode string, maxTurns int) (json.RawMessage, error) {
				result, err := SpawnSubAgent(ctx, parent, parentConfig, SubAgentConfig{Prompt: prompt, Mode: mode, MaxTurns: maxTurns})
				if err != nil {
					return nil, err
				}
				return json.Marshal(result)
			})
			out, err := spawn.Handler(context.Background(), json.RawMessage(`{"prompt":"write the fix","mode":"execution"}`))
			must.Error(err, "spawn_agent returned %q, want a tool error", out)
			must.ErrorContains(err, "cannot spawn")
			must.ErrorContains(err, parentMode, "rejection names the parent mode")
			must.Zero(prov.calls.Load(), "no child may start")
		})
	}
}

// TestCheckSubAgentMode_ExecutionChildNeedsExecutionParent enumerates the
// mode-transition rule.
func TestCheckSubAgentMode_ExecutionChildNeedsExecutionParent(t *testing.T) {
	must := require.New(t)
	allModes := append([]string{"execution"}, readOnlyModeNames...)
	for _, parentMode := range allModes {
		for _, childMode := range allModes {
			t.Run(parentMode+"/"+childMode, func(t *testing.T) {
				must := require.New(t)
				err := checkSubAgentMode(parentMode, childMode)
				if parentMode != "execution" && childMode == "execution" {
					must.Error(err)
				} else {
					must.NoError(err)
				}
			})
		}
	}
	must.Error(checkSubAgentMode("research", "unknown"), "a read-only parent must not spawn a child in an unrecognised mode")
}

// TestPrepareSubAgent_AllowedTransitionsValidate runs every allowed
// parent-to-child transition through ValidateRunConfig, reusing the
// top-level read-only invariant as a structural check on the recorded
// child config.
func TestPrepareSubAgent_AllowedTransitionsValidate(t *testing.T) {
	must := require.New(t)
	policyPath := filepath.Join(t.TempDir(), "policy.cedar")
	err := os.WriteFile(policyPath, []byte(`permit (principal, action, resource);`), 0o644)
	must.NoError(err, "write policy")

	readOnlyBuiltIns := []string{"read_file", "grep_files", "web_fetch", "spawn_agent"}
	parents := []struct {
		name     string
		mode     string
		builtIn  []string
		policy   types.PermissionPolicyConfig
		mutating bool
	}{
		{name: "execution allow-all", mode: "execution", policy: types.PermissionPolicyConfig{Type: "allow-all"}, mutating: true},
		{name: "execution ask-upstream", mode: "execution", policy: types.PermissionPolicyConfig{Type: "ask-upstream"}, mutating: true},
		{name: "execution policy-engine allow-all fallback", mode: "execution", policy: types.PermissionPolicyConfig{Type: "policy-engine", PolicyFile: policyPath, Fallback: "allow-all"}, mutating: true},
		{name: "research deny-side-effects", mode: "research", builtIn: readOnlyBuiltIns, policy: types.PermissionPolicyConfig{Type: "deny-side-effects"}},
		{name: "planning ask-upstream", mode: "planning", builtIn: readOnlyBuiltIns, policy: types.PermissionPolicyConfig{Type: "ask-upstream"}},
		{name: "toil policy-engine", mode: "toil", builtIn: readOnlyBuiltIns, policy: types.PermissionPolicyConfig{Type: "policy-engine", PolicyFile: policyPath, Fallback: "deny-side-effects"}},
	}
	for _, p := range parents {
		parentConfig := buildTestConfig()
		parentConfig.Mode = p.mode
		parentConfig.Tools.BuiltIn = p.builtIn
		parentConfig.PermissionPolicy = p.policy
		must.NoError(types.ValidateRunConfig(parentConfig), "%s: parent config must be valid", p.name)

		childModes := readOnlyModeNames
		if p.mode == "execution" {
			childModes = append([]string{"execution", ""}, readOnlyModeNames...)
		}
		for _, childMode := range childModes {
			t.Run(p.name+"/"+childMode, func(t *testing.T) {
				must := require.New(t)
				registry, _ := spawnParentRegistry(t, p.mutating)
				parent := buildSubAgentTestLoop(&mockProvider{})
				parent.Tools = registry

				run, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "subtask", Mode: childMode})
				must.NoError(err, "prepareSubAgent")
				// The nested run ID ("parent/sub-N") is deliberately outside
				// the operator-facing run ID pattern; validate the rest.
				validated := *run.config
				validated.RunID = "child-run"
				must.NoError(types.ValidateRunConfig(&validated), "child config fails validation")
			})
		}
	}
}

// TestSpawnSubAgent_ChildTraceRecordsEnforcedConfig pins that the child's
// own RunTrace records the tools, permission policy and context strategy
// it enforced, and that building it leaves the parent's config intact.
func TestSpawnSubAgent_ChildTraceRecordsEnforcedConfig(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	registry, _ := spawnParentRegistry(t, true)
	parent := buildSubAgentTestLoop(&mockProvider{})
	parent.Tools = registry
	parent.Provider = &scriptedProvider{turns: [][]types.StreamEvent{spawnTextTurn("findings")}}
	parentConfig := buildTestConfig()
	keep := 3
	parentConfig.ContextStrategy = types.ContextStrategyConfig{
		Type: "clear-tool-results", MaxTokens: 100000, Keep: &keep, ExcludeTools: []string{"read_file"},
	}

	run, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "investigate", Mode: "research"})
	must.NoError(err, "prepareSubAgent")
	runTrace, err := run.execute(context.Background())
	must.NoError(err, "execute")

	got := runTrace.Config
	check.Equal("research", got.Mode, "recorded mode")
	check.Equal([]string{"read_file", "grep_files", "web_fetch"}, got.Tools.BuiltIn, "recorded tools.builtIn")
	check.Equal("deny-side-effects", got.PermissionPolicy.Type, "recorded permission policy")
	check.Equal(types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 100000}, got.ContextStrategy, "recorded context strategy")
	check.Equal([]string{"read_file", "grep_files", "web_fetch", "ticket_lookup"}, runTrace.EffectiveTools, "trace effectiveTools")

	check.Nil(parentConfig.Tools.BuiltIn, "parent builtIn tools mutated")
	check.Equal("allow-all", parentConfig.PermissionPolicy.Type, "parent permission policy mutated")
	check.Equal("clear-tool-results", parentConfig.ContextStrategy.Type, "parent context strategy mutated")
}

// TestSpawnSubAgent_CedarModeRuleFiresForResearchChild pins that a Cedar
// rule on principal.mode == "research" decides a research child's calls
// through the parent's wrapped policy-engine chain. web_fetch is
// non-mutating, so the denial can only come from Cedar.
func TestSpawnSubAgent_CedarModeRuleFiresForResearchChild(t *testing.T) {
	must := require.New(t)
	policyPath := filepath.Join(t.TempDir(), "policy.cedar")
	policy := `forbid (
		principal,
		action == Action::"tool:web_fetch",
		resource == Tool::"web_fetch"
	) when {
		principal.mode == "research"
	};`
	err := os.WriteFile(policyPath, []byte(policy), 0o644)
	must.NoError(err, "write policy")

	engine, err := permission.New(
		types.PermissionPolicyConfig{Type: "policy-engine", PolicyFile: policyPath, Fallback: "allow-all"},
		permission.PolicyEngineEnv{RunID: "run-parent-1", Mode: "execution"},
		func(string) (permission.PermissionPolicy, error) { return permission.NewAllowAll(), nil },
	)
	must.NoError(err, "permission.New")

	registry, _ := spawnParentRegistry(t, true)
	parent := buildSubAgentTestLoop(&mockProvider{})
	parent.Tools = registry
	parent.Permissions = permission.NewMetricRecorder(engine, observability.NewNoopMetrics(), "policy-engine")
	parentConfig := buildTestConfig()

	webFetch := types.ToolDefinition{Name: "web_fetch"}
	input := json.RawMessage(`{"url":"https://example.com"}`)
	decide := func(t *testing.T, pp permission.PermissionPolicy) *permission.PermissionResult {
		t.Helper()
		must := require.New(t)
		result, err := pp.Check(context.Background(), webFetch, input)
		must.NoError(err, "Check")
		return result
	}

	must.True(decide(t, parent.Permissions).Allowed, "execution parent: web_fetch should fall through to the allow-all fallback")

	research, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "look it up", Mode: "research"})
	must.NoError(err, "prepareSubAgent(research)")
	must.False(decide(t, research.loop.Permissions).Allowed, "research child: web_fetch should be denied by the principal.mode rule")

	execution, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "look it up", Mode: "execution"})
	must.NoError(err, "prepareSubAgent(execution)")
	must.True(decide(t, execution.loop.Permissions).Allowed, "execution child: the research-only rule must not fire")
}

// TestPrepareSubAgent_ExecutionChildKeepsParentAuthority pins that an
// execution child of an execution parent keeps every parent tool except
// spawn_agent and the parent's permission chain, with no extra gate.
func TestPrepareSubAgent_ExecutionChildKeepsParentAuthority(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	registry, _ := spawnParentRegistry(t, true)
	parent := buildSubAgentTestLoop(&mockProvider{})
	parent.Tools = registry
	parentConfig := buildTestConfig()

	run, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "fix it", Mode: "execution"})
	must.NoError(err, "prepareSubAgent")

	offered := offeredToolIDs(run.loop)
	check.NotContains(offered, "spawn_agent", "execution child offered spawn_agent")
	for _, name := range mutatingSpawnTools {
		t.Run(name, func(t *testing.T) {
			must := require.New(t)
			must.Contains(offered, name, "execution child must keep the parent tool")
		})
	}
	_, gated := run.loop.Permissions.(*permission.SideEffectGate)
	check.False(gated, "execution child permission chain wrapped in a SideEffectGate")
	check.Equal("allow-all", run.config.PermissionPolicy.Type, "recorded permission policy")
	check.NotContains(run.config.Tools.BuiltIn, "spawn_agent", "recorded tools.builtIn")
}

// TestPrepareSubAgent_ReadOnlyChildWithoutBuiltInsIsRejected pins that a
// read-only child left with no built-in tool is refused: its recorded
// tools.builtIn could not express an empty set, since an empty list means
// every built-in.
func TestPrepareSubAgent_ReadOnlyChildWithoutBuiltInsIsRejected(t *testing.T) {
	must := require.New(t)
	registry := tool.NewRegistry()
	for _, name := range []string{"write_file", "mcp_docs_update", "spawn_agent"} {
		registry.Register(&tool.Tool{
			Name:              name,
			InputSchema:       json.RawMessage(`{"type":"object"}`),
			WorkspaceMutating: name != "spawn_agent",
			Handler:           func(context.Context, json.RawMessage) (string, error) { return "", nil },
		})
	}
	parent := buildSubAgentTestLoop(&mockProvider{})
	parent.Tools = registry

	_, err := prepareSubAgent(context.Background(), parent, buildTestConfig(), SubAgentConfig{Prompt: "look", Mode: "review"})
	must.Error(err)
	must.ErrorContains(err, "no read-only built-in tools")
}

var _ permission.Transport = (*permissionRequestTransport)(nil)
