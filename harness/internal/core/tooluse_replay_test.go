package core

// Tool-use reliability regression suite, driven entirely by the in-process
// ReplayProvider + a real LocalExecutor over a synthetic temp workspace —
// no network, no provider credentials. See docs/eval.md for how this
// relates to eval/suites/tooluse.hcl.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"

	contextpkg "github.com/rxbynerd/stirrup/harness/internal/context"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/git"
	"github.com/rxbynerd/stirrup/harness/internal/observability"
	"github.com/rxbynerd/stirrup/harness/internal/permission"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/router"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/trace"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/harness/internal/verifier"
	"github.com/rxbynerd/stirrup/types"
)

// toolUseScenario is one replay-driven tool-use task. The provider replays
// turns verbatim; the loop dispatches the tool calls against the real
// built-in registry wired over workspaceFiles in a temp dir.
type toolUseScenario struct {
	// workspaceFiles seeds the synthetic repo before the run. Keys are
	// workspace-relative paths.
	workspaceFiles map[string]string
	// turns is the scripted model output. Tool inputs must match the
	// real built-in schemas — the loop validates them.
	turns []types.TurnRecord
	// editStrategy selects the edit tool. "multi" yields edit_file with the
	// operation enum; empty defaults to whole-file (write_file).
	editStrategy string
	// providerType, when set, wraps the replay provider in a
	// NormalizingAdapter under that provider's tool-name policy.
	providerType string
	// extraTools are registered alongside the built-ins (e.g. a synthetic
	// MCP-named tool) before the profile presenter is applied.
	extraTools []*tool.Tool
	// escalationRetries, when > 0, injects the default tool-choice
	// escalation policy with that cap. nil caps means the policy
	// always picks the prompt fallback.
	escalationRetries int
	// mode overrides the run mode (default "execution" from buildTestConfig).
	mode string
}

// runToolUseScenario builds an AgenticLoop around the scenario's replay turns
// and a real LocalExecutor, runs it to completion, and returns the workspace
// dir, the resulting RunTrace, and a recording transport that captured every
// emitted event (so tests can assert tool_result content, not just the
// summary counts in the trace). It fails the test on any setup error.
func runToolUseScenario(t *testing.T, sc toolUseScenario) (string, *types.RunTrace, *recordingTransport) {
	t.Helper()
	must := require.New(t)

	workspace := t.TempDir()
	for rel, content := range sc.workspaceFiles {
		abs := filepath.Join(workspace, rel)
		must.NoError(os.MkdirAll(filepath.Dir(abs), 0o755), "seed dir for %q", rel)
		must.NoError(os.WriteFile(abs, []byte(content), 0o644), "seed file %q", rel)
	}

	exec, err := executor.NewLocalExecutor(workspace)
	must.NoError(err)

	var es edit.EditStrategy
	switch sc.editStrategy {
	case "multi":
		es = edit.NewMultiStrategy(0.8)
	default:
		es = edit.NewWholeFileStrategy()
	}

	registry := buildToolRegistry(exec, es, types.ToolsConfig{}, nil)
	for _, et := range sc.extraTools {
		registry.Register(et)
	}

	var prov provider.ProviderAdapter = provider.NewReplayProvider(sc.turns)
	if sc.providerType != "" {
		prov = provider.NewNormalizingAdapter(prov, sc.providerType)
	}

	rec := &recordingTransport{}
	loop := &AgenticLoop{
		Provider:    prov,
		Router:      router.NewStaticRouter("replay", "replay-model"),
		Prompt:      prompt.NewDefaultPromptBuilder(),
		Context:     contextpkg.NewSlidingWindowStrategy(),
		Tools:       registry,
		Executor:    exec,
		Edit:        es,
		Verifier:    verifier.NewNoneVerifier(),
		Permissions: permission.NewAllowAll(),
		Git:         git.NewNoneGitStrategy(),
		Transport:   rec,
		Trace:       trace.NewJSONLTraceEmitter(&bytes.Buffer{}, false),
		Tracer:      noop.NewTracerProvider().Tracer(""),
		Metrics:     observability.NewNoopMetrics(),
		Logger:      slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	}
	if sc.escalationRetries > 0 {
		// nil capabilityResolver → the policy always falls back to the
		// prompt path, which is the no-credential-friendly branch.
		loop.Escalation = newDefaultEscalationPolicy(sc.escalationRetries, nil, keyAsProviderType)
	}

	config := buildTestConfig()
	config.Executor = types.ExecutorConfig{Type: "local", Workspace: workspace}
	config.MaxTurns = len(sc.turns) + 1
	if sc.mode != "" {
		config.Mode = sc.mode
	}

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.NotNil(runTrace)
	return workspace, runTrace, rec
}

// toolResultContent returns the Content of the tool_result event the loop
// emitted for the given tool-use ID. Fails the test if no such event was
// recorded — every dispatched tool call produces exactly one tool_result.
func toolResultContent(t *testing.T, rec *recordingTransport, toolUseID string) string {
	t.Helper()
	must := require.New(t)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, ev := range rec.events {
		if ev.Type == "tool_result" && ev.ToolUseID == toolUseID {
			return ev.Content
		}
	}
	must.Fail("no tool_result event recorded", "tool-use ID %q", toolUseID)
	return ""
}

// --- trace assertion helpers (the trace-side judge, inlined for the harness
// module which cannot import eval/judge across the module boundary) ---

func toolCallNames(tr *types.RunTrace) []string {
	names := make([]string, 0, len(tr.ToolCalls))
	for _, c := range tr.ToolCalls {
		n := c.Name
		if c.InternalName != "" {
			n = c.InternalName
		}
		names = append(names, n)
	}
	return names
}

// assertSequence checks each want name appears in the given relative order.
func assertSequence(t *testing.T, tr *types.RunTrace, want ...string) {
	t.Helper()
	must := require.New(t)
	got := toolCallNames(tr)
	idx := 0
	for _, n := range got {
		if idx < len(want) && n == want[idx] {
			idx++
		}
	}
	must.Equal(len(want), idx, "tool-call sequence %v should contain %v in order", got, want)
}

func countCalls(tr *types.RunTrace, internalName string) (total, failed int) {
	for _, c := range tr.ToolCalls {
		n := c.Name
		if c.InternalName != "" {
			n = c.InternalName
		}
		if n != internalName {
			continue
		}
		total++
		if !c.Success {
			failed++
		}
	}
	return total, failed
}

// requireToolCallCounts asserts how many calls to a tool the run dispatched
// and how many of those failed.
func requireToolCallCounts(t *testing.T, tr *types.RunTrace, internalName string, wantTotal, wantFailed int) {
	t.Helper()
	must := require.New(t)
	total, failed := countCalls(tr, internalName)
	must.Equal(wantTotal, total, "%s calls", internalName)
	must.Equal(wantFailed, failed, "%s failed calls", internalName)
}

func readWorkspaceFile(t *testing.T, workspace, rel string) string {
	t.Helper()
	must := require.New(t)
	data, err := os.ReadFile(filepath.Join(workspace, rel))
	must.NoError(err, "read %q", rel)
	return string(data)
}

// toolUse builds a single tool_use turn record.
func toolUse(id, name string, input string) types.TurnRecord {
	return types.TurnRecord{
		ModelOutput: []types.ContentBlock{
			{Type: "tool_use", ID: id, Name: name, Input: json.RawMessage(input)},
		},
	}
}

// finalText builds a terminal text turn ending the run.
func finalText(text string) types.TurnRecord {
	return types.TurnRecord{
		ModelOutput: []types.ContentBlock{{Type: "text", Text: text}},
	}
}

// TestToolUse_SearchReadEdit_AppliesEdit covers the core coding loop: the
// model searches for a symbol, reads the matching file, edits it, then gives
// a final answer.
func TestToolUse_SearchReadEdit_AppliesEdit(t *testing.T) {
	must := require.New(t)
	workspace, tr, _ := runToolUseScenario(t, toolUseScenario{
		editStrategy: "multi",
		workspaceFiles: map[string]string{
			"calc.go": "package calc\n\nfunc Double(n int) int {\n\treturn n + n\n}\n",
		},
		turns: []types.TurnRecord{
			toolUse("c1", "grep_files", `{"pattern":"func Double","include":["*.go"]}`),
			toolUse("c2", "read_file", `{"path":"calc.go"}`),
			toolUse("c3", "edit_file", `{"path":"calc.go","operation":"replace","old_string":"return n + n","new_string":"return n * 2"}`),
			finalText("Done."),
		},
	})

	assertSequence(t, tr, "grep_files", "read_file", "edit_file")
	must.Contains(readWorkspaceFile(t, workspace, "calc.go"), "return n * 2", "edit not applied to calc.go")
	requireToolCallCounts(t, tr, "edit_file", 1, 0)
}

// TestToolUse_ReadBeforeEdit asserts the trace records read_file before
// edit_file.
func TestToolUse_ReadBeforeEdit(t *testing.T) {
	must := require.New(t)
	workspace, tr, _ := runToolUseScenario(t, toolUseScenario{
		editStrategy: "multi",
		workspaceFiles: map[string]string{
			"greeting.txt": "hello old world\n",
		},
		turns: []types.TurnRecord{
			toolUse("c1", "read_file", `{"path":"greeting.txt"}`),
			toolUse("c2", "edit_file", `{"path":"greeting.txt","operation":"replace","old_string":"old","new_string":"new"}`),
			finalText("Updated."),
		},
	})

	assertSequence(t, tr, "read_file", "edit_file")
	must.Equal("hello new world\n", readWorkspaceFile(t, workspace, "greeting.txt"))
}

// TestToolUse_ReadFile_ReturnsRequestedLineWindow exercises read_file with
// start_line + limit and asserts the returned tool result carries the
// requested line window — lines 5-7 of the fixture and nothing outside it.
func TestToolUse_ReadFile_ReturnsRequestedLineWindow(t *testing.T) {
	// Distinct, greppable line contents so an off-by-one window leak is
	// detectable: "line-05", "line-06", ... "line-20".
	lines := make([]string, 0, 20)
	for i := 1; i <= 20; i++ {
		lines = append(lines, fmt.Sprintf("line-%02d", i))
	}
	_, tr, rec := runToolUseScenario(t, toolUseScenario{
		workspaceFiles: map[string]string{
			"doc.txt": strings.Join(lines, "\n") + "\n",
		},
		turns: []types.TurnRecord{
			toolUse("c1", "read_file", `{"path":"doc.txt","start_line":5,"limit":3}`),
			finalText("Read."),
		},
	})

	requireToolCallCounts(t, tr, "read_file", 1, 0)

	// The window must be exactly lines 5-7; lines 4 and 8 must be absent.
	out := toolResultContent(t, rec, "c1")
	for _, want := range []string{"line-05", "line-06", "line-07"} {
		t.Run("includes "+want, func(t *testing.T) {
			must := require.New(t)
			must.Contains(out, want)
		})
	}
	for _, unwanted := range []string{"line-04", "line-08"} {
		t.Run("excludes "+unwanted, func(t *testing.T) {
			must := require.New(t)
			must.NotContains(out, unwanted)
		})
	}
}

// TestToolUse_GrepFiles_CapsMatchesAtMaxResults asserts a broad grep with
// max_results=2 caps the returned matches end-to-end over a workspace with
// twenty hits: the rendered tool result the model sees must carry exactly two
// match lines, not all twenty. The grep text rendering carries no truncation
// marker (the structured flag is pinned separately in
// TestGrepFilesTool_StructuredTruncated), so this test asserts the behavioural
// cap rather than the flag.
func TestToolUse_GrepFiles_CapsMatchesAtMaxResults(t *testing.T) {
	must := require.New(t)
	files := map[string]string{}
	for i := 0; i < 10; i++ {
		files[fmt.Sprintf("f%d.txt", i)] = "needle here\nneedle again\n"
	}
	_, tr, rec := runToolUseScenario(t, toolUseScenario{
		workspaceFiles: files,
		turns: []types.TurnRecord{
			toolUse("c1", "grep_files", `{"pattern":"needle","max_results":2}`),
			finalText("Searched."),
		},
	})

	requireToolCallCounts(t, tr, "grep_files", 1, 0)

	// The workspace has 20 matches (2 per file x 10 files); max_results=2
	// must cap the rendered output to 2 match lines.
	out := toolResultContent(t, rec, "c1")
	matchLines := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(line, "needle") {
			matchLines++
		}
	}
	must.Equal(2, matchLines, "match lines in grep_files output (max_results bound):\n%s", out)
}

// TestToolUse_EditFile_RecoversFromInvalidArgs drives an edit_file with a
// missing required field, then a corrected call. Asserts the first call
// failed, the second succeeded, and the file ended in the corrected state.
func TestToolUse_EditFile_RecoversFromInvalidArgs(t *testing.T) {
	must := require.New(t)
	workspace, tr, _ := runToolUseScenario(t, toolUseScenario{
		editStrategy: "multi",
		workspaceFiles: map[string]string{
			"config.txt": "mode=off\n",
		},
		turns: []types.TurnRecord{
			// 'replace' without new_string violates the per-operation
			// field contract; the tool returns a directional error.
			toolUse("c1", "edit_file", `{"path":"config.txt","operation":"replace","old_string":"off"}`),
			// Corrected call.
			toolUse("c2", "edit_file", `{"path":"config.txt","operation":"replace","old_string":"off","new_string":"on"}`),
			finalText("Fixed."),
		},
	})

	requireToolCallCounts(t, tr, "edit_file", 2, 1)
	must.Contains(readWorkspaceFile(t, workspace, "config.txt"), "mode=on")
}

// TestToolUse_EditFile_RecoversFromAmbiguousMatch covers the ambiguous-edit
// request behaviour: the first edit_file 'replace' uses an old_string that
// matches multiple locations, which the search-replace strategy rejects with
// an exactly-one-match error; the model recovers with a more specific
// old_string. This is a distinct failure mode from invalid-arg recovery
// (missing field) — here the arguments are well-formed but not unique.
// Asserts two attempts (first fails), final file in the corrected state, and
// that the failure message named the multi-location ambiguity.
func TestToolUse_EditFile_RecoversFromAmbiguousMatch(t *testing.T) {
	must := require.New(t)
	workspace, tr, rec := runToolUseScenario(t, toolUseScenario{
		editStrategy: "multi",
		workspaceFiles: map[string]string{
			// Two identical "value = 1" lines: a bare old_string of
			// "value = 1" matches both.
			"settings.ini": "[a]\nvalue = 1\n[b]\nvalue = 1\n",
		},
		turns: []types.TurnRecord{
			// Ambiguous: "value = 1" occurs twice.
			toolUse("c1", "edit_file", `{"path":"settings.ini","operation":"replace","old_string":"value = 1","new_string":"value = 2"}`),
			// Recovery: a section-qualified old_string matches exactly one.
			toolUse("c2", "edit_file", `{"path":"settings.ini","operation":"replace","old_string":"[b]\nvalue = 1","new_string":"[b]\nvalue = 2"}`),
			finalText("Disambiguated."),
		},
	})

	requireToolCallCounts(t, tr, "edit_file", 2, 1)
	// The first attempt's error must name the multi-location ambiguity so the
	// model has a signal to recover from.
	must.Contains(toolResultContent(t, rec, "c1"), "match", "first edit_file error should name the ambiguity")
	// Final state: section [b]'s value changed, section [a]'s did not.
	must.Equal("[a]\nvalue = 1\n[b]\nvalue = 2\n", readWorkspaceFile(t, workspace, "settings.ini"))
}

// TestToolUse_RetiredToolName_RecoversWithHint calls the retired search_files
// name, gets the directional renamed-tool hint, then recovers with grep_files.
// Asserts the failed call's error names the replacements and that the
// recovery call succeeded.
func TestToolUse_RetiredToolName_RecoversWithHint(t *testing.T) {
	must := require.New(t)
	_, tr, _ := runToolUseScenario(t, toolUseScenario{
		workspaceFiles: map[string]string{
			"src.go": "package src\n// TODO: implement\n",
		},
		turns: []types.TurnRecord{
			toolUse("c1", "search_files", `{"query":"TODO"}`),
			toolUse("c2", "grep_files", `{"pattern":"TODO"}`),
			finalText("Found it."),
		},
	})

	requireToolCallCounts(t, tr, "search_files", 1, 1)
	requireToolCallCounts(t, tr, "grep_files", 1, 0)
	// The renamed-tool hint must name both replacements so the model can
	// recover in-loop.
	var hint string
	for _, c := range tr.ToolCalls {
		if c.Name == "search_files" {
			hint = c.ErrorReason
		}
	}
	must.Contains(hint, "grep_files", "renamed-tool hint should name grep_files")
	must.Contains(hint, "find_files", "renamed-tool hint should name find_files")
}

// TestToolUse_MultiToolTurn_DispatchesInOrder drives a single assistant turn
// that emits two tool calls (read + find) before any tool result, exercising
// the fan-out/ordered-recording path in planAndDispatch. Asserts both calls
// were recorded in order.
func TestToolUse_MultiToolTurn_DispatchesInOrder(t *testing.T) {
	_, tr, _ := runToolUseScenario(t, toolUseScenario{
		workspaceFiles: map[string]string{
			"a.go": "package a\n",
			"b.go": "package b\n",
		},
		turns: []types.TurnRecord{
			{
				ModelOutput: []types.ContentBlock{
					{Type: "tool_use", ID: "c1", Name: "read_file", Input: json.RawMessage(`{"path":"a.go"}`)},
					{Type: "tool_use", ID: "c2", Name: "find_files", Input: json.RawMessage(`{"name":"*.go"}`)},
				},
			},
			finalText("Inspected."),
		},
	})

	assertSequence(t, tr, "read_file", "find_files")
	requireToolCallCounts(t, tr, "read_file", 1, 0)
	requireToolCallCounts(t, tr, "find_files", 1, 0)
}

// TestToolUse_NoToolAnswer_EscalatesToToolCall exercises tool-choice
// escalation: in execution mode the model first answers with text only,
// having called no tool. With the escalation policy enabled the loop forces a
// retry; the retried turn calls read_file and the run then completes with
// outcome "success", not max_turns or a crash.
func TestToolUse_NoToolAnswer_EscalatesToToolCall(t *testing.T) {
	must := require.New(t)
	_, tr, _ := runToolUseScenario(t, toolUseScenario{
		escalationRetries: 1,
		mode:              "execution",
		workspaceFiles: map[string]string{
			"main.go": "package main\n",
		},
		turns: []types.TurnRecord{
			// Turn 0: no-tool answer — the missed-tool failure.
			finalText("The file looks fine."),
			// Turn 1: forced retry — the model now reads the file.
			toolUse("c1", "read_file", `{"path":"main.go"}`),
			// Turn 2: final answer after using the tool.
			finalText("Confirmed package main."),
		},
	})

	total, failed := countCalls(tr, "read_file")
	must.Equal(1, total, "read_file calls after escalation; tool names %v", toolCallNames(tr))
	must.Zero(failed, "failed read_file calls after escalation")
	must.Equal("success", tr.Outcome, "run should complete after escalation recovery")
}

// TestToolUse_NoToolAnswer_AcceptedWhenEscalationOff is the control: with the
// escalation policy disabled (the OFF-by-default posture) a no-tool answer is
// accepted unchanged and no tool is called.
func TestToolUse_NoToolAnswer_AcceptedWhenEscalationOff(t *testing.T) {
	must := require.New(t)
	_, tr, _ := runToolUseScenario(t, toolUseScenario{
		mode: "execution",
		workspaceFiles: map[string]string{
			"main.go": "package main\n",
		},
		turns: []types.TurnRecord{
			finalText("The file looks fine."),
		},
	})

	must.Empty(tr.ToolCalls, "no tool calls expected with escalation off; tool names %v", toolCallNames(tr))
}

// TestToolUse_MCPName_NormalizedToInternalName registers a tool under an
// MCP-style internal name containing a hyphen and wraps the replay provider
// in a NormalizingAdapter under the Gemini policy (which forbids hyphens). The
// replay emits a tool_call under the EXTERNAL (sanitized) name; the normalizer
// must reverse it to the internal name so dispatch resolves and the tool runs.
// Asserts the trace records the internal MCP name and success.
func TestToolUse_MCPName_NormalizedToInternalName(t *testing.T) {
	must := require.New(t)
	const internalName = "mcp__docs__fetch-page"

	ran := false
	mcpTool := &tool.Tool{
		Name:              internalName,
		Description:       "Synthetic MCP tool for normalization regression.",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		WorkspaceMutating: false,
		RequiresApproval:  false,
		Handler: func(_ context.Context, _ json.RawMessage) (string, error) {
			ran = true
			return "page body", nil
		},
	}

	// The Gemini policy rewrites the hyphen to an underscore. The replay
	// must emit the external name the model would have seen on the wire.
	externalName := strings.ReplaceAll(internalName, "-", "_")

	_, tr, _ := runToolUseScenario(t, toolUseScenario{
		providerType: "gemini",
		extraTools:   []*tool.Tool{mcpTool},
		turns: []types.TurnRecord{
			toolUse("c1", externalName, `{}`),
			finalText("Fetched."),
		},
	})

	must.True(ran, "MCP tool handler did not run; reverse name mapping failed")
	requireToolCallCounts(t, tr, internalName, 1, 0)
}

// TestToolUse_StreamingToolCall_ReassemblesSplitArgs exercises provider-specific
// streaming tool-call parsing through the real openai-compatible adapter's SSE
// parser, driven by a loopback httptest server — no network, no live key
// (the credential is a dummy resolved from the test env). The server streams
// an OpenAI-style tool_calls delta whose arguments arrive split across two
// chunks; the adapter must reassemble the JSON, the loop must dispatch the
// edit_file tool, and the workspace must reflect the edit. This is the
// fake-provider counterpart to the ReplayProvider tasks: ReplayProvider
// bypasses wire parsing, whereas this task proves the streamed-delta parser
// itself produces a dispatchable tool call.
func TestToolUse_StreamingToolCall_ReassemblesSplitArgs(t *testing.T) {
	must := require.New(t)
	t.Setenv("TEST_OPENAI_KEY", "test-key")

	workspace := t.TempDir()
	target := filepath.Join(workspace, "target.txt")
	must.NoError(os.WriteFile(target, []byte("hello old world\n"), 0o644), "seed target")

	// Turn 1 streams a tool_calls delta with the function name in the first
	// chunk and the arguments split across two subsequent chunks, so the
	// adapter's accumulator is exercised. Turn 2 is the final answer.
	var requestBodies []string
	server := newOpenAIServer(t, nil, []string{
		openAIChunk(`{"id":"t1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search_replace","arguments":"{\"path\":\"target.txt\",\"old"}}]},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"t1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"_string\":\"old\",\"new_string\":\"new\"}"}}]},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"t1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
			"data: [DONE]\n\n",
		openAIChunk(`{"id":"t2","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"t2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n",
	}, &requestBodies)

	timeout := 30
	enforce := false
	config := &types.RunConfig{
		RunID:            "tooluse-streaming",
		Mode:             "execution",
		Prompt:           "Update the file.",
		Provider:         types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://TEST_OPENAI_KEY", BaseURL: server.URL},
		ModelRouter:      types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "gpt-4o-mini"},
		PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
		ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
		Executor:         types.ExecutorConfig{Type: "local", Workspace: workspace},
		EditStrategy:     types.EditStrategyConfig{Type: "search-replace"},
		Verifier:         types.VerifierConfig{Type: "none"},
		PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
		GitStrategy:      types.GitStrategyConfig{Type: "none"},
		TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		Tools:            types.ToolsConfig{BuiltIn: []string{"search_replace"}},
		RuleOfTwo:        &types.RuleOfTwoConfig{Enforce: &enforce},
		MaxTurns:         4,
		Timeout:          &timeout,
	}

	loop, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
	must.NoError(err)
	// Order matters: defers run LIFO, so server.Close() is registered first
	// and loop.Close() second, ensuring the loop tears down its provider
	// transport before the httptest server stops listening. Closing the
	// server first would race the in-flight HTTP teardown and emit spurious
	// connection-error logs.
	defer server.Close()
	defer func() { _ = loop.Close() }()

	runTrace, err := loop.Run(context.Background(), config)
	must.NoError(err)
	must.Equal("success", runTrace.Outcome)

	got, err := os.ReadFile(target)
	must.NoError(err, "read target")
	must.Equal("hello new world\n", string(got))
	requireToolCallCounts(t, runTrace, "search_replace", 1, 0)
	// Assert the tool reached the wire: the first request must advertise
	// search_replace in its tools payload. Without this, a regression that
	// dropped the tool from the request body could still pass on the
	// streamed-response parse alone (the canned response names the tool
	// regardless of what was sent).
	must.NotEmpty(requestBodies, "no provider requests captured")
	must.Contains(requestBodies[0], "search_replace", "first request should advertise the search_replace tool")
}
