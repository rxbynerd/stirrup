package core

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

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

	// Determine max turns: default to 10, cap at the hard limit.
	maxTurns := subConfig.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultSubAgentMaxTurns
	}
	if maxTurns > maxSubAgentMaxTurns {
		maxTurns = maxSubAgentMaxTurns
	}
	// Also cap at the parent's remaining turns to prevent the child from
	// exceeding the parent's overall budget.
	if maxTurns > parentConfig.MaxTurns {
		maxTurns = parentConfig.MaxTurns
	}

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
		Trace:       trace.NewJSONLTraceEmitter(&bytes.Buffer{}),
		Tracer:      tracer,
		Metrics:     parent.Metrics,
		Logger:      parent.Logger,
		Security:    parent.Security,
	}

	// Run the child loop synchronously.
	runTrace, err := childLoop.Run(ctx, &childConfig)
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
