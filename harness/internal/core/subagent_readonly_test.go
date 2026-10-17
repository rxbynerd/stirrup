package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
				if err != nil {
					t.Fatalf("prepareSubAgent: %v", err)
				}

				offered := offeredToolIDs(run.loop)
				wantOffered := []string{"read_file", "grep_files", "web_fetch", "ticket_lookup"}
				if !slices.Equal(offered, wantOffered) {
					t.Errorf("offered tools = %v, want %v", offered, wantOffered)
				}
				if !slices.Equal(run.effectiveTools, wantOffered) {
					t.Errorf("effectiveTools = %v, want %v", run.effectiveTools, wantOffered)
				}

				for _, name := range mutatingSpawnTools {
					result, err := run.loop.Permissions.Check(context.Background(), types.ToolDefinition{Name: name}, json.RawMessage(`{}`))
					if err != nil {
						t.Fatalf("Check(%s): %v", name, err)
					}
					if result.Allowed {
						t.Errorf("child permission chain allowed %s", name)
					}
				}
				if got := rt.requested(); len(got) != 0 {
					t.Errorf("mutating checks asked upstream for %v, want no requests", got)
				}

				if want := []string{"read_file", "grep_files", "web_fetch"}; !slices.Equal(run.config.Tools.BuiltIn, want) {
					t.Errorf("recorded tools.builtIn = %v, want %v", run.config.Tools.BuiltIn, want)
				}
				if run.config.PermissionPolicy.Type != parentPolicy.wantPolicy {
					t.Errorf("recorded permission policy = %q, want %q", run.config.PermissionPolicy.Type, parentPolicy.wantPolicy)
				}
				if run.config.Mode != mode {
					t.Errorf("recorded mode = %q, want %q", run.config.Mode, mode)
				}
			})
		}
	}
}

// TestSpawnSubAgent_ReadOnlyChildCannotDispatchMutatingTools drives a
// research child whose model calls every mutating tool by name: none of
// the handlers runs and every call is recorded as failed.
func TestSpawnSubAgent_ReadOnlyChildCannotDispatchMutatingTools(t *testing.T) {
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

	if _, err := SpawnSubAgent(context.Background(), parent, buildTestConfig(), SubAgentConfig{
		Prompt: "edit things",
		Mode:   "research",
	}); err != nil {
		t.Fatalf("SpawnSubAgent: %v", err)
	}

	for _, name := range mutatingSpawnTools {
		if n := calls.count(name); n != 0 {
			t.Errorf("%s handler ran %d times in a research child", name, n)
		}
	}
	if calls.count("read_file") != 1 {
		t.Errorf("read_file handler ran %d times, want 1", calls.count("read_file"))
	}
	_, toolCalls := parentTrace.snapshot()
	if len(toolCalls) != len(mutatingSpawnTools)+1 {
		t.Fatalf("recorded %d child tool calls, want %d", len(toolCalls), len(mutatingSpawnTools)+1)
	}
	for _, tc := range toolCalls {
		if wantSuccess := tc.Name == "read_file"; tc.Success != wantSuccess {
			t.Errorf("child call to %s recorded success=%v, want %v", tc.Name, tc.Success, wantSuccess)
		}
	}
}

// TestSpawnSubAgent_ReadOnlyChildOfAskUpstreamParentNeverAsks pins that a
// read-only child of an ask-upstream parent raises no approval request
// for mutating tools, while a non-mutating approval-required tool still
// asks upstream.
func TestSpawnSubAgent_ReadOnlyChildOfAskUpstreamParentNeverAsks(t *testing.T) {
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

	if _, err := SpawnSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{
		Prompt: "edit things",
		Mode:   "research",
	}); err != nil {
		t.Fatalf("SpawnSubAgent: %v", err)
	}

	if got := rt.requested(); !slices.Equal(got, []string{"web_fetch"}) {
		t.Errorf("permission requests = %v, want only web_fetch", got)
	}
	for _, name := range []string{"write_file", "run_command", "mcp_docs_update"} {
		if n := calls.count(name); n != 0 {
			t.Errorf("%s handler ran %d times", name, n)
		}
	}
}

// TestSpawnSubAgent_ReadOnlyParentCannotSpawnExecutionChild pins that a
// read-only parent's request for an execution child is rejected as a
// spawn_agent tool error before the child sends any provider request.
func TestSpawnSubAgent_ReadOnlyParentCannotSpawnExecutionChild(t *testing.T) {
	for _, parentMode := range readOnlyModeNames {
		t.Run(parentMode, func(t *testing.T) {
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
			if err == nil {
				t.Fatalf("spawn_agent returned %q, want a tool error", out)
			}
			if !strings.Contains(err.Error(), "cannot spawn") || !strings.Contains(err.Error(), parentMode) {
				t.Errorf("error %q does not explain the rejected transition", err)
			}
			if n := prov.calls.Load(); n != 0 {
				t.Errorf("provider received %d requests, want 0 (no child may start)", n)
			}
		})
	}
}

// TestCheckSubAgentMode enumerates the mode-transition rule.
func TestCheckSubAgentMode(t *testing.T) {
	allModes := append([]string{"execution"}, readOnlyModeNames...)
	for _, parentMode := range allModes {
		for _, childMode := range allModes {
			err := checkSubAgentMode(parentMode, childMode)
			wantErr := parentMode != "execution" && childMode == "execution"
			if (err != nil) != wantErr {
				t.Errorf("checkSubAgentMode(%q, %q) error = %v, want error %v", parentMode, childMode, err, wantErr)
			}
		}
	}
	if err := checkSubAgentMode("research", "unknown"); err == nil {
		t.Error("a read-only parent must not spawn a child in an unrecognised mode")
	}
}

// TestPrepareSubAgent_AllowedTransitionsValidate runs every allowed
// parent-to-child transition through ValidateRunConfig, reusing the
// top-level read-only invariant as a structural check on the recorded
// child config.
func TestPrepareSubAgent_AllowedTransitionsValidate(t *testing.T) {
	policyPath := filepath.Join(t.TempDir(), "policy.cedar")
	if err := os.WriteFile(policyPath, []byte(`permit (principal, action, resource);`), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
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
		if err := types.ValidateRunConfig(parentConfig); err != nil {
			t.Fatalf("%s: parent config must be valid: %v", p.name, err)
		}
		childModes := readOnlyModeNames
		if p.mode == "execution" {
			childModes = append([]string{"execution", ""}, readOnlyModeNames...)
		}
		for _, childMode := range childModes {
			t.Run(p.name+"/"+childMode, func(t *testing.T) {
				registry, _ := spawnParentRegistry(t, p.mutating)
				parent := buildSubAgentTestLoop(&mockProvider{})
				parent.Tools = registry

				run, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "subtask", Mode: childMode})
				if err != nil {
					t.Fatalf("prepareSubAgent: %v", err)
				}
				// The nested run ID ("parent/sub-N") is deliberately outside
				// the operator-facing run ID pattern; validate the rest.
				validated := *run.config
				validated.RunID = "child-run"
				if err := types.ValidateRunConfig(&validated); err != nil {
					t.Errorf("child config fails validation: %v", err)
				}
			})
		}
	}
}

// TestSpawnSubAgent_ChildTraceRecordsEnforcedConfig pins that the child's
// own RunTrace records the tools, permission policy and context strategy
// it enforced, and that building it leaves the parent's config intact.
func TestSpawnSubAgent_ChildTraceRecordsEnforcedConfig(t *testing.T) {
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
	if err != nil {
		t.Fatalf("prepareSubAgent: %v", err)
	}
	runTrace, err := run.execute(context.Background())
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	got := runTrace.Config
	if got.Mode != "research" {
		t.Errorf("recorded mode = %q, want research", got.Mode)
	}
	if want := []string{"read_file", "grep_files", "web_fetch"}; !slices.Equal(got.Tools.BuiltIn, want) {
		t.Errorf("recorded tools.builtIn = %v, want %v", got.Tools.BuiltIn, want)
	}
	if got.PermissionPolicy.Type != "deny-side-effects" {
		t.Errorf("recorded permission policy = %q, want deny-side-effects", got.PermissionPolicy.Type)
	}
	if want := (types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 100000}); !reflect.DeepEqual(got.ContextStrategy, want) {
		t.Errorf("recorded context strategy = %+v, want %+v", got.ContextStrategy, want)
	}
	if want := []string{"read_file", "grep_files", "web_fetch", "ticket_lookup"}; !slices.Equal(runTrace.EffectiveTools, want) {
		t.Errorf("trace effectiveTools = %v, want %v", runTrace.EffectiveTools, want)
	}

	if parentConfig.Tools.BuiltIn != nil || parentConfig.PermissionPolicy.Type != "allow-all" || parentConfig.ContextStrategy.Type != "clear-tool-results" {
		t.Errorf("parent config mutated: builtIn=%v policy=%q context=%q",
			parentConfig.Tools.BuiltIn, parentConfig.PermissionPolicy.Type, parentConfig.ContextStrategy.Type)
	}
}

// TestSpawnSubAgent_CedarModeRuleFiresForResearchChild pins that a Cedar
// rule on principal.mode == "research" decides a research child's calls
// through the parent's wrapped policy-engine chain. web_fetch is
// non-mutating, so the denial can only come from Cedar.
func TestSpawnSubAgent_CedarModeRuleFiresForResearchChild(t *testing.T) {
	policyPath := filepath.Join(t.TempDir(), "policy.cedar")
	policy := `forbid (
		principal,
		action == Action::"tool:web_fetch",
		resource == Tool::"web_fetch"
	) when {
		principal.mode == "research"
	};`
	if err := os.WriteFile(policyPath, []byte(policy), 0o644); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	engine, err := permission.New(
		types.PermissionPolicyConfig{Type: "policy-engine", PolicyFile: policyPath, Fallback: "allow-all"},
		permission.PolicyEngineEnv{RunID: "run-parent-1", Mode: "execution"},
		func(string) (permission.PermissionPolicy, error) { return permission.NewAllowAll(), nil },
	)
	if err != nil {
		t.Fatalf("permission.New: %v", err)
	}

	registry, _ := spawnParentRegistry(t, true)
	parent := buildSubAgentTestLoop(&mockProvider{})
	parent.Tools = registry
	parent.Permissions = permission.NewMetricRecorder(engine, observability.NewNoopMetrics(), "policy-engine")
	parentConfig := buildTestConfig()

	webFetch := types.ToolDefinition{Name: "web_fetch"}
	input := json.RawMessage(`{"url":"https://example.com"}`)
	decide := func(t *testing.T, pp permission.PermissionPolicy) *permission.PermissionResult {
		t.Helper()
		result, err := pp.Check(context.Background(), webFetch, input)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		return result
	}

	if !decide(t, parent.Permissions).Allowed {
		t.Fatal("execution parent: web_fetch should fall through to the allow-all fallback")
	}

	research, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "look it up", Mode: "research"})
	if err != nil {
		t.Fatalf("prepareSubAgent(research): %v", err)
	}
	if decide(t, research.loop.Permissions).Allowed {
		t.Error("research child: web_fetch should be denied by the principal.mode rule")
	}

	execution, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "look it up", Mode: "execution"})
	if err != nil {
		t.Fatalf("prepareSubAgent(execution): %v", err)
	}
	if !decide(t, execution.loop.Permissions).Allowed {
		t.Error("execution child: the research-only rule must not fire")
	}
}

// TestPrepareSubAgent_ExecutionChildKeepsParentAuthority pins that an
// execution child of an execution parent keeps every parent tool except
// spawn_agent and the parent's permission chain, with no extra gate.
func TestPrepareSubAgent_ExecutionChildKeepsParentAuthority(t *testing.T) {
	registry, _ := spawnParentRegistry(t, true)
	parent := buildSubAgentTestLoop(&mockProvider{})
	parent.Tools = registry
	parentConfig := buildTestConfig()

	run, err := prepareSubAgent(context.Background(), parent, parentConfig, SubAgentConfig{Prompt: "fix it", Mode: "execution"})
	if err != nil {
		t.Fatalf("prepareSubAgent: %v", err)
	}
	offered := offeredToolIDs(run.loop)
	if slices.Contains(offered, "spawn_agent") {
		t.Error("execution child offered spawn_agent")
	}
	for _, name := range mutatingSpawnTools {
		if !slices.Contains(offered, name) {
			t.Errorf("execution child lost %s", name)
		}
	}
	if _, gated := run.loop.Permissions.(*permission.SideEffectGate); gated {
		t.Error("execution child permission chain wrapped in a SideEffectGate")
	}
	if run.config.PermissionPolicy.Type != "allow-all" {
		t.Errorf("recorded permission policy = %q, want the parent's allow-all", run.config.PermissionPolicy.Type)
	}
	if slices.Contains(run.config.Tools.BuiltIn, "spawn_agent") {
		t.Errorf("recorded tools.builtIn %v includes spawn_agent", run.config.Tools.BuiltIn)
	}
}

// TestPrepareSubAgent_ReadOnlyChildWithoutBuiltInsIsRejected pins that a
// read-only child left with no built-in tool is refused: its recorded
// tools.builtIn could not express an empty set, since an empty list means
// every built-in.
func TestPrepareSubAgent_ReadOnlyChildWithoutBuiltInsIsRejected(t *testing.T) {
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
	if err == nil {
		t.Fatal("expected an error for a read-only child with no built-in tools")
	}
	if !strings.Contains(err.Error(), "no read-only built-in tools") {
		t.Errorf("error %q does not name the missing tools", err)
	}
}

var _ permission.Transport = (*permissionRequestTransport)(nil)
