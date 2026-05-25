package core

// Tool-use reliability regression suite (issue #233), driven entirely by the
// in-process ReplayProvider + a real LocalExecutor over a synthetic temp
// workspace. No network, no provider credentials: the ProviderAdapter is a
// scripted sequence of recorded turns, so `go test ./harness/...` exercises
// the full agentic-loop tool-dispatch path with zero external dependencies.
//
// Each test covers one tool-use behaviour from the redesign delivered across
// Waves 1-5 and asserts BOTH the resulting workspace state AND the tool-call
// trace the run produced (the same two surfaces the eval suite's file-state
// and tool-trace judges check). The declarative HCL form of this suite lives
// at eval/suites/tooluse.hcl for the opt-in live-provider path; this file is
// the no-credential executable regression that runs in CI.

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
	// operation enum (#225); empty defaults to whole-file (write_file).
	editStrategy string
	// providerType, when set, wraps the replay provider in a
	// NormalizingAdapter under that provider's tool-name policy (#223).
	providerType string
	// extraTools are registered alongside the built-ins (e.g. a synthetic
	// MCP-named tool) before the profile presenter is applied.
	extraTools []*tool.Tool
	// escalationRetries, when > 0, injects the default tool-choice
	// escalation policy with that cap (#230). nil caps means the policy
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

	workspace := t.TempDir()
	for rel, content := range sc.workspaceFiles {
		abs := filepath.Join(workspace, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("seed dir for %q: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("seed file %q: %v", rel, err)
		}
	}

	exec, err := executor.NewLocalExecutor(workspace)
	if err != nil {
		t.Fatalf("build local executor: %v", err)
	}

	var es edit.EditStrategy
	switch sc.editStrategy {
	case "multi":
		es = edit.NewMultiStrategy(0.8)
	default:
		es = edit.NewWholeFileStrategy()
	}

	registry := buildToolRegistry(exec, es, types.ToolsConfig{})
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
		Trace:       trace.NewJSONLTraceEmitter(&bytes.Buffer{}),
		Tracer:      noop.NewTracerProvider().Tracer(""),
		Metrics:     observability.NewNoopMetrics(),
		Logger:      slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	}
	if sc.escalationRetries > 0 {
		// nil capabilityResolver → the policy always falls back to the
		// prompt path, which is the no-credential-friendly branch.
		loop.Escalation = newDefaultEscalationPolicy(sc.escalationRetries, nil)
	}

	config := buildTestConfig()
	config.Executor = types.ExecutorConfig{Type: "local", Workspace: workspace}
	config.MaxTurns = len(sc.turns) + 1
	if sc.mode != "" {
		config.Mode = sc.mode
	}

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
	if runTrace == nil {
		t.Fatal("nil RunTrace")
	}
	return workspace, runTrace, rec
}

// toolResultContent returns the Content of the tool_result event the loop
// emitted for the given tool-use ID. Fails the test if no such event was
// recorded — every dispatched tool call produces exactly one tool_result.
func toolResultContent(t *testing.T, rec *recordingTransport, toolUseID string) string {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, ev := range rec.events {
		if ev.Type == "tool_result" && ev.ToolUseID == toolUseID {
			return ev.Content
		}
	}
	t.Fatalf("no tool_result event recorded for tool-use ID %q", toolUseID)
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
	got := toolCallNames(tr)
	idx := 0
	for _, n := range got {
		if idx < len(want) && n == want[idx] {
			idx++
		}
	}
	if idx != len(want) {
		t.Fatalf("tool-call sequence %v does not contain %v in order", got, want)
	}
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

func readWorkspaceFile(t *testing.T, workspace, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workspace, rel))
	if err != nil {
		t.Fatalf("read %q: %v", rel, err)
	}
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

// TestToolUse_ReadSearchEditBuildLoop covers the core coding loop: the model
// searches for a symbol, reads the matching file, edits it, then a final
// answer. Regression-covers the #225 grep_files/read_file/edit_file split and
// the read→edit→answer arc the whole redesign serves.
func TestToolUse_ReadSearchEditBuildLoop(t *testing.T) {
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
	if got := readWorkspaceFile(t, workspace, "calc.go"); !strings.Contains(got, "return n * 2") {
		t.Fatalf("edit not applied; calc.go = %q", got)
	}
	if total, failed := countCalls(tr, "edit_file"); total != 1 || failed != 0 {
		t.Fatalf("edit_file calls: total=%d failed=%d, want 1/0", total, failed)
	}
}

// TestToolUse_ReadBeforeEdit asserts the trace records read_file before
// edit_file — the read-before-edit discipline #225's schema encourages.
func TestToolUse_ReadBeforeEdit(t *testing.T) {
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
	if got := readWorkspaceFile(t, workspace, "greeting.txt"); got != "hello new world\n" {
		t.Fatalf("greeting.txt = %q, want %q", got, "hello new world\n")
	}
}

// TestToolUse_LineRangeReading exercises read_file with start_line + limit
// (#225 line-range reading) and asserts the returned tool result carries the
// requested line window — lines 5-7 of the fixture and nothing outside it.
// Regression-covers the #231 structured result line arithmetic for read_file.
func TestToolUse_LineRangeReading(t *testing.T) {
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

	if total, failed := countCalls(tr, "read_file"); total != 1 || failed != 0 {
		t.Fatalf("read_file calls: total=%d failed=%d, want 1/0", total, failed)
	}

	// The window must be exactly lines 5-7; lines 4 and 8 must be absent.
	out := toolResultContent(t, rec, "c1")
	for _, want := range []string{"line-05", "line-06", "line-07"} {
		if !strings.Contains(out, want) {
			t.Errorf("read_file result missing %q in window; got:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"line-04", "line-08"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("read_file result leaked out-of-window line %q; got:\n%s", unwanted, out)
		}
	}
}

// TestToolUse_BoundedSearch asserts a broad grep with max_results=2 caps the
// returned matches end-to-end (#225 bounded results) over a workspace with
// twenty hits: the rendered tool result the model sees must carry exactly two
// match lines, not all twenty. The bounded count is the observable effect of
// the searchResult.Truncated flag; the structured flag itself is pinned at
// the builtin level in TestGrepFilesTool_StructuredTruncated (#231), and the
// grep text rendering carries no truncation marker, so this test asserts the
// behavioural cap rather than the flag.
func TestToolUse_BoundedSearch(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 10; i++ {
		files["f"+string(rune('0'+i))+".txt"] = "needle here\nneedle again\n"
	}
	_, tr, rec := runToolUseScenario(t, toolUseScenario{
		workspaceFiles: files,
		turns: []types.TurnRecord{
			toolUse("c1", "grep_files", `{"pattern":"needle","max_results":2}`),
			finalText("Searched."),
		},
	})

	if total, failed := countCalls(tr, "grep_files"); total != 1 || failed != 0 {
		t.Fatalf("grep_files calls: total=%d failed=%d, want 1/0", total, failed)
	}

	// The workspace has 20 matches (2 per file x 10 files); max_results=2
	// must cap the rendered output to 2 match lines.
	out := toolResultContent(t, rec, "c1")
	matchLines := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.Contains(line, "needle") {
			matchLines++
		}
	}
	if matchLines != 2 {
		t.Errorf("grep_files returned %d match lines, want 2 (max_results bound); got:\n%s", matchLines, out)
	}
}

// TestToolUse_InvalidArgRecovery drives an edit_file with a missing required
// field (the schema/operation contract from #225), then a corrected call.
// Asserts the first call failed, the second succeeded, and the file ended in
// the corrected state — the invalid-argument recovery loop.
func TestToolUse_InvalidArgRecovery(t *testing.T) {
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

	total, failed := countCalls(tr, "edit_file")
	if total != 2 || failed != 1 {
		t.Fatalf("edit_file calls: total=%d failed=%d, want 2/1", total, failed)
	}
	if got := readWorkspaceFile(t, workspace, "config.txt"); !strings.Contains(got, "mode=on") {
		t.Fatalf("config.txt = %q, want mode=on", got)
	}
}

// TestToolUse_AmbiguousEdit covers the "ambiguous edit request" behaviour
// named in #233's goal list: the first edit_file 'replace' uses an old_string
// that matches multiple locations, which the search-replace strategy rejects
// with an exactly-one-match error; the model recovers with a more specific
// old_string. This is a distinct failure mode from invalid-arg recovery
// (missing field) — here the arguments are well-formed but not unique.
// Asserts two attempts (first fails), final file in the corrected state, and
// that the failure message named the multi-location ambiguity.
func TestToolUse_AmbiguousEdit(t *testing.T) {
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

	total, failed := countCalls(tr, "edit_file")
	if total != 2 || failed != 1 {
		t.Fatalf("edit_file calls: total=%d failed=%d, want 2/1", total, failed)
	}
	// The first attempt's error must name the multi-location ambiguity so the
	// model has a signal to recover from.
	firstErr := toolResultContent(t, rec, "c1")
	if !strings.Contains(firstErr, "match") {
		t.Errorf("first edit_file error should name the ambiguity; got %q", firstErr)
	}
	// Final state: section [b]'s value changed, section [a]'s did not.
	got := readWorkspaceFile(t, workspace, "settings.ini")
	if got != "[a]\nvalue = 1\n[b]\nvalue = 2\n" {
		t.Fatalf("settings.ini = %q, want only the [b] value changed", got)
	}
}

// TestToolUse_UnknownToolRecovery calls the retired search_files name, gets
// the directional renamed-tool hint (#225), then recovers with grep_files.
// Asserts the failed call's error names the replacements and that the
// recovery call succeeded.
func TestToolUse_UnknownToolRecovery(t *testing.T) {
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

	if total, failed := countCalls(tr, "search_files"); total != 1 || failed != 1 {
		t.Fatalf("search_files calls: total=%d failed=%d, want 1/1", total, failed)
	}
	if total, failed := countCalls(tr, "grep_files"); total != 1 || failed != 0 {
		t.Fatalf("grep_files calls: total=%d failed=%d, want 1/0", total, failed)
	}
	// The renamed-tool hint must name both replacements so the model can
	// recover in-loop.
	var hint string
	for _, c := range tr.ToolCalls {
		if c.Name == "search_files" {
			hint = c.ErrorReason
		}
	}
	if !strings.Contains(hint, "grep_files") || !strings.Contains(hint, "find_files") {
		t.Fatalf("renamed-tool hint %q does not name both replacements", hint)
	}
}

// TestToolUse_MultiToolTurn drives a single assistant turn that emits two
// tool calls (read + find) before any tool result, exercising the
// fan-out/ordered-recording path in planAndDispatch. Asserts both calls were
// recorded in order.
func TestToolUse_MultiToolTurn(t *testing.T) {
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
	if total, failed := countCalls(tr, "read_file"); total != 1 || failed != 0 {
		t.Fatalf("read_file: total=%d failed=%d, want 1/0", total, failed)
	}
	if total, failed := countCalls(tr, "find_files"); total != 1 || failed != 0 {
		t.Fatalf("find_files: total=%d failed=%d, want 1/0", total, failed)
	}
}

// TestToolUse_NoToolAnswerEscalates exercises tool-choice escalation (#230):
// in execution mode the model first answers with text only, having called no
// tool. With the escalation policy enabled the loop forces a retry; the
// retried turn calls read_file and the run then completes. Asserts the
// recovery actually happened — read_file was called and the run reached the
// "success" outcome (not max_turns or a crash), so a regression that fires
// escalation but never recovers is caught.
func TestToolUse_NoToolAnswerEscalates(t *testing.T) {
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

	if total, failed := countCalls(tr, "read_file"); total != 1 || failed != 0 {
		t.Fatalf("expected exactly one successful read_file after escalation; got total=%d failed=%d names %v", total, failed, toolCallNames(tr))
	}
	if tr.Outcome != "success" {
		t.Fatalf("expected run to complete with outcome 'success' after escalation recovery, got %q", tr.Outcome)
	}
}

// TestToolUse_NoToolAnswerAcceptedWhenEscalationOff is the control: with the
// escalation policy disabled (the OFF-by-default posture) a no-tool answer is
// accepted unchanged and no tool is called.
func TestToolUse_NoToolAnswerAcceptedWhenEscalationOff(t *testing.T) {
	_, tr, _ := runToolUseScenario(t, toolUseScenario{
		mode: "execution",
		workspaceFiles: map[string]string{
			"main.go": "package main\n",
		},
		turns: []types.TurnRecord{
			finalText("The file looks fine."),
		},
	})

	if len(tr.ToolCalls) != 0 {
		t.Fatalf("expected no tool calls with escalation off; got %v", toolCallNames(tr))
	}
}

// TestToolUse_MCPNameNormalization registers a tool under an MCP-style
// internal name containing a hyphen and wraps the replay provider in a
// NormalizingAdapter under the Gemini policy (which forbids hyphens, #223).
// The replay emits a tool_call under the EXTERNAL (sanitized) name; the
// normalizer must reverse it to the internal name so dispatch resolves and
// the tool runs. Asserts the trace records the internal MCP name and success.
func TestToolUse_MCPNameNormalization(t *testing.T) {
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

	if !ran {
		t.Fatal("MCP tool handler did not run; reverse name mapping failed")
	}
	if total, failed := countCalls(tr, internalName); total != 1 || failed != 0 {
		t.Fatalf("%s calls: total=%d failed=%d, want 1/0", internalName, total, failed)
	}
}

// TestToolUse_StreamingToolCallParsing exercises provider-specific streaming
// tool-call parsing (#233) through the real openai-compatible adapter's SSE
// parser, driven by a loopback httptest server — no network, no live key
// (the credential is a dummy resolved from the test env). The server streams
// an OpenAI-style tool_calls delta whose arguments arrive split across two
// chunks; the adapter must reassemble the JSON, the loop must dispatch the
// edit_file tool, and the workspace must reflect the edit. This is the
// fake-provider counterpart to the ReplayProvider tasks: ReplayProvider
// bypasses wire parsing, whereas this task proves the streamed-delta parser
// itself produces a dispatchable tool call.
func TestToolUse_StreamingToolCallParsing(t *testing.T) {
	t.Setenv("TEST_OPENAI_KEY", "test-key")

	workspace := t.TempDir()
	target := filepath.Join(workspace, "target.txt")
	if err := os.WriteFile(target, []byte("hello old world\n"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	// Turn 1 streams a tool_calls delta with the function name in the first
	// chunk and the arguments split across two subsequent chunks, so the
	// adapter's accumulator is exercised. Turn 2 is the final answer.
	server := newOpenAIServer(t, nil, []string{
		openAIChunk(`{"id":"t1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search_replace","arguments":"{\"path\":\"target.txt\",\"old"}}]},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"t1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"_string\":\"old\",\"new_string\":\"new\"}"}}]},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"t1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
			"data: [DONE]\n\n",
		openAIChunk(`{"id":"t2","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"t2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n",
	}, nil)
	defer server.Close()

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
	if err != nil {
		t.Fatalf("BuildLoopWithTransport: %v", err)
	}
	defer func() { _ = loop.Close() }()

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("expected success, got %q", runTrace.Outcome)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "hello new world\n" {
		t.Fatalf("target.txt = %q, want %q", string(got), "hello new world\n")
	}
	if total, failed := countCalls(runTrace, "search_replace"); total != 1 || failed != 0 {
		t.Fatalf("search_replace calls: total=%d failed=%d, want 1/0", total, failed)
	}
}
