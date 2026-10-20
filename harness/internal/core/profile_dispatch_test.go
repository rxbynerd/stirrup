package core

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/guard"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// grepFilesTestTool is a minimal grep_files stand-in whose handler records
// that it ran, so a profile-alias dispatch test can prove the alias routed
// to this internal tool.
func grepFilesTestTool(ran *bool) *tool.Tool {
	return &tool.Tool{
		Name:        "grep_files",
		Description: "regex content search",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(context.Context, json.RawMessage) (string, error) {
			*ran = true
			return "grep_files ran", nil
		},
	}
}

// buildProfileDispatchLoop wires a loop whose Tools is a profile Presenter
// over the given registry, with a recording trace emitter so the test can
// assert the trace shape.
func buildProfileDispatchLoop(t *testing.T, reg *tool.Registry, profile *tool.Profile, rec *recordingTraceEmitter) *AgenticLoop {
	t.Helper()
	must := require.New(t)
	presenter, err := tool.NewPresenter(reg, profile)
	must.NoError(err, "NewPresenter")
	return &AgenticLoop{
		Router:       router.NewStaticRouter("anthropic", "claude-sonnet-4-6"),
		Prompt:       prompt.NewDefaultPromptBuilder(),
		Context:      contextpkg.NewSlidingWindowStrategy(),
		Tools:        presenter,
		ToolProfile:  profile,
		Edit:         edit.NewWholeFileStrategy(),
		Verifier:     verifier.NewNoneVerifier(),
		Permissions:  permission.NewAllowAll(),
		Git:          git.NewNoneGitStrategy(),
		Transport:    transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}),
		Trace:        rec,
		Tracer:       noop.NewTracerProvider().Tracer(""),
		TraceContext: context.Background(),
		Metrics:      observability.NewNoopMetrics(),
		Logger:       slog.Default(),
	}
}

// A profile presents an alias; the model calls by the alias; dispatch
// resolves the alias to the internal tool, executes it, and the trace
// records BOTH the model-facing alias and the internal tool identity.
func TestProfileDispatch_AliasResolvesAndTraceRecordsBothNames(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var ran bool
	reg := tool.NewRegistry()
	reg.Register(grepFilesTestTool(&ran))

	rec := &recordingTraceEmitter{}
	loop := buildProfileDispatchLoop(t, reg, mustProfile(t, "coding-classic"), rec)

	cfg := &types.RunConfig{Mode: "execution", RunID: "profile-dispatch"}
	stall := &stallDetector{}

	// The model calls "grep", the coding-classic alias for grep_files.
	results, records, outcome := loop.planAndDispatch(
		context.Background(), cfg,
		[]types.ToolCall{{ID: "tc1", Name: "grep", Input: json.RawMessage(`{}`)}},
		stall, "anthropic", "claude-sonnet-4-6",
	)
	must.Empty(outcome, "unexpected stall outcome")
	must.True(ran, "grep_files handler did not run; alias did not resolve to the internal tool")
	must.Len(results, 1)
	must.False(results[0].IsError, "expected a successful result, got %+v", results[0])
	check.Equal("grep_files ran", results[0].Content)

	// The trace carries the alias as Name and the internal ID as InternalName.
	_, calls := rec.snapshot()
	must.Len(calls, 1)
	check.Equal("grep", calls[0].Name)
	check.Equal("grep_files", calls[0].InternalName)

	must.Len(records, 1)
	check.Equal("grep", records[0].Name)
	check.Equal("grep_files", records[0].InternalName)
}

// Under the default profile, a call by internal tool name dispatches, and the
// trace omits InternalName because the alias and the internal name match.
func TestProfileDispatch_DefaultProfileAcceptsInternalName(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var ran bool
	reg := tool.NewRegistry()
	reg.Register(grepFilesTestTool(&ran))

	rec := &recordingTraceEmitter{}
	loop := buildProfileDispatchLoop(t, reg, mustProfile(t, "default"), rec)

	cfg := &types.RunConfig{Mode: "execution", RunID: "default-dispatch"}
	stall := &stallDetector{}

	results, _, outcome := loop.planAndDispatch(
		context.Background(), cfg,
		[]types.ToolCall{{ID: "tc1", Name: "grep_files", Input: json.RawMessage(`{}`)}},
		stall, "anthropic", "claude-sonnet-4-6",
	)
	must.Empty(outcome, "unexpected stall outcome")
	must.True(ran, "default profile should dispatch the internal tool name")
	must.Len(results, 1)
	must.False(results[0].IsError, "expected a successful result, got %+v", results[0])

	_, calls := rec.snapshot()
	must.Len(calls, 1)
	check.Equal("grep_files", calls[0].Name)
	check.Empty(calls[0].InternalName, "default profile must omit InternalName (alias == internal)")
}

func mustProfile(t *testing.T, name string) *tool.Profile {
	t.Helper()
	must := require.New(t)
	p, ok := tool.ProfileFor(name)
	must.True(ok, "ProfileFor(%q) not found", name)
	return p
}

// toolNameCapturingGuard records the ToolName it sees on each PhasePreTool
// check so a test can assert which name reaches the guardrail classifier.
type toolNameCapturingGuard struct {
	mu        sync.Mutex
	toolNames []string
}

func (g *toolNameCapturingGuard) Check(_ context.Context, in guard.Input) (*guard.Decision, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if in.Phase == guard.PhasePreTool {
		g.toolNames = append(g.toolNames, in.ToolName)
	}
	return &guard.Decision{Verdict: guard.VerdictAllow, GuardID: "capture"}, nil
}

// The PhasePreTool guardrail must see the internal tool ID, not the alias, so
// a guardrail rule written against an internal name fires under any toolset
// profile. The model calls "grep"; the guard must observe "grep_files".
func TestProfileDispatch_GuardrailSeesInternalToolName(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var ran bool
	reg := tool.NewRegistry()
	reg.Register(grepFilesTestTool(&ran))

	rec := &recordingTraceEmitter{}
	loop := buildProfileDispatchLoop(t, reg, mustProfile(t, "coding-classic"), rec)
	g := &toolNameCapturingGuard{}
	loop.GuardRail = g

	cfg := &types.RunConfig{Mode: "execution", RunID: "guard-name"}
	_, _, outcome := loop.planAndDispatch(
		context.Background(), cfg,
		[]types.ToolCall{{ID: "tc1", Name: "grep", Input: json.RawMessage(`{}`)}},
		&stallDetector{}, "anthropic", "claude-sonnet-4-6",
	)
	must.Empty(outcome, "unexpected stall outcome")

	g.mu.Lock()
	defer g.mu.Unlock()
	must.Len(g.toolNames, 1)
	check.Equal("grep_files", g.toolNames[0])
}
