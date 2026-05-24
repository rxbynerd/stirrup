package core

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace/noop"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/git"
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
	presenter, err := tool.NewPresenter(reg, profile)
	if err != nil {
		t.Fatalf("NewPresenter: %v", err)
	}
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
	var ran bool
	reg := tool.NewRegistry()
	reg.Register(grepFilesTestTool(&ran))

	rec := &recordingTraceEmitter{}
	loop := buildProfileDispatchLoop(t, reg, mustProfile(t, "coding-classic"), rec)

	cfg := &types.RunConfig{Mode: "execution", RunID: "profile-dispatch"}
	stall := &stallDetector{}

	// The model calls "grep" — the coding-classic alias for grep_files.
	results, records, outcome := loop.planAndDispatch(
		context.Background(), cfg,
		[]types.ToolCall{{ID: "tc1", Name: "grep", Input: json.RawMessage(`{}`)}},
		stall, "anthropic", "claude-sonnet-4-6",
	)
	if outcome != "" {
		t.Fatalf("unexpected stall outcome: %q", outcome)
	}
	if !ran {
		t.Fatal("grep_files handler did not run — alias did not resolve to the internal tool")
	}
	if len(results) != 1 || results[0].IsError {
		t.Fatalf("expected one successful result, got %+v", results)
	}
	if results[0].Content != "grep_files ran" {
		t.Errorf("result content %q, want %q", results[0].Content, "grep_files ran")
	}

	// Trace: ToolCallTrace must carry the alias as Name and the internal ID
	// as InternalName.
	_, calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 recorded tool call, got %d", len(calls))
	}
	if calls[0].Name != "grep" {
		t.Errorf("trace Name = %q, want alias %q", calls[0].Name, "grep")
	}
	if calls[0].InternalName != "grep_files" {
		t.Errorf("trace InternalName = %q, want %q", calls[0].InternalName, "grep_files")
	}

	// The full record carries both names too.
	if len(records) != 1 {
		t.Fatalf("expected 1 tool record, got %d", len(records))
	}
	if records[0].Name != "grep" || records[0].InternalName != "grep_files" {
		t.Errorf("record names: Name=%q InternalName=%q, want grep/grep_files",
			records[0].Name, records[0].InternalName)
	}
}

// Existing configs that name tools by their internal IDs keep working:
// under the default profile, calling grep_files by its internal name
// dispatches and the trace omits InternalName (alias == internal), so the
// trace wire shape is byte-identical to the pre-profile behaviour.
func TestProfileDispatch_DefaultProfileInternalNameStillWorks(t *testing.T) {
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
	if outcome != "" {
		t.Fatalf("unexpected stall outcome: %q", outcome)
	}
	if !ran || len(results) != 1 || results[0].IsError {
		t.Fatalf("default profile + internal name should dispatch: ran=%v results=%+v", ran, results)
	}

	_, calls := rec.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 recorded tool call, got %d", len(calls))
	}
	if calls[0].Name != "grep_files" {
		t.Errorf("trace Name = %q, want %q", calls[0].Name, "grep_files")
	}
	if calls[0].InternalName != "" {
		t.Errorf("default profile must omit InternalName (alias == internal), got %q", calls[0].InternalName)
	}
}

func mustProfile(t *testing.T, name string) *tool.Profile {
	t.Helper()
	p, ok := tool.ProfileFor(name)
	if !ok {
		t.Fatalf("ProfileFor(%q) not found", name)
	}
	return p
}
