package core

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/transport"
	"github.com/rxbynerd/stirrup/types"
)

// TestAttachPostEditChecks_EveryEditToolRunsChecks pins that each
// workspace-writing edit tool name the strategies register runs the
// configured checks after a successful edit, and not after a failed one.
func TestAttachPostEditChecks_EveryEditToolRunsChecks(t *testing.T) {
	cases := []struct {
		name     string
		strategy edit.EditStrategy
		input    string
		bad      string
	}{
		{"write_file", edit.NewWholeFileStrategy(), `{"path":"f.go","content":"package p\n"}`, `{"path":"","content":"x"}`},
		{"search_replace", edit.NewSearchReplaceStrategy(), `{"path":"f.go","old_string":"package p","new_string":"package q"}`, `{"path":"f.go","old_string":"absent","new_string":"x"}`},
		{"apply_diff", edit.NewUdiffStrategy(0), `{"path":"f.go","diff":"--- a/f.go\n+++ b/f.go\n@@ -1 +1 @@\n-package p\n+package q\n"}`, `{"path":"f.go","diff":"not a diff"}`},
		{"edit_file", edit.NewMultiStrategy(0), `{"path":"f.go","operation":"replace","old_string":"package p","new_string":"package q"}`, `{"path":"f.go","operation":"replace","old_string":"absent","new_string":"x"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			if err := os.WriteFile(filepath.Join(workspace, "f.go"), []byte("package p\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			exec, err := executor.NewLocalExecutor(workspace)
			if err != nil {
				t.Fatal(err)
			}
			registry := buildToolRegistry(exec, tc.strategy, types.ToolsConfig{}, nil)
			checker := attachPostEditChecks(registry, tc.strategy.ToolDefinition().Name, exec, []types.PostEditCheckConfig{
				{Glob: "*.go", Command: "echo checked {path} >> checks.log; echo diagnostic-line; exit 3"},
			})
			if checker == nil {
				t.Fatal("attachPostEditChecks returned nil")
			}
			editTool := registry.Resolve(tc.name)
			if editTool == nil {
				t.Fatalf("%s is not registered", tc.name)
			}

			out, err := editTool.Handler(context.Background(), json.RawMessage(tc.input))
			if err != nil {
				t.Fatalf("edit failed: %v", err)
			}
			if !strings.Contains(out, "exited with code 3") || !strings.Contains(out, "diagnostic-line") {
				t.Fatalf("tool result lacks check diagnostics:\n%s", out)
			}

			if _, err := editTool.Handler(context.Background(), json.RawMessage(tc.bad)); err == nil {
				t.Fatal("failing edit input unexpectedly succeeded")
			}
			log, err := os.ReadFile(filepath.Join(workspace, "checks.log"))
			if err != nil {
				t.Fatal(err)
			}
			if string(log) != "checked f.go\n" {
				t.Errorf("checks.log = %q, want exactly one run for the successful edit", log)
			}
		})
	}
}

func TestAttachPostEditChecks_NoChecksLeavesHandlerUntouched(t *testing.T) {
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	es := edit.NewWholeFileStrategy()
	registry := buildToolRegistry(exec, es, types.ToolsConfig{}, nil)
	if checker := attachPostEditChecks(registry, es.ToolDefinition().Name, exec, nil); checker != nil {
		t.Fatal("expected nil checker without checks")
	}
	out, err := registry.Resolve("write_file").Handler(context.Background(), json.RawMessage(`{"path":"a.go","content":"x"}`))
	if err != nil || out != "Successfully edited a.go" {
		t.Fatalf("handler = %q, %v", out, err)
	}

	noEdit := buildToolRegistry(exec, es, types.ToolsConfig{BuiltIn: []string{"read_file"}}, nil)
	if checker := attachPostEditChecks(noEdit, es.ToolDefinition().Name, exec, []types.PostEditCheckConfig{{Glob: "*", Command: "true"}}); checker != nil {
		t.Fatal("expected nil checker when no edit tool is registered")
	}
}

// TestBuildLoop_PostEditDiagnosticsReachModelAndTrace drives a full run:
// the model writes a file, the failing check's fenced diagnostics are
// sent back to the model in the next request, and the run trace records
// the execution.
func TestBuildLoop_PostEditDiagnosticsReachModelAndTrace(t *testing.T) {
	workspace := t.TempDir()
	var requestBodies []string
	server := newOpenAIServer(t, nil, []string{
		openAIChunk(`{"id":"pe-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"main.go\",\"content\":\"package main\\n\"}"}}]},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"pe-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
			"data: [DONE]\n\n",
		openAIChunk(`{"id":"pe-2","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":null}]}`) +
			openAIChunk(`{"id":"pe-2","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n",
	}, &requestBodies)
	defer server.Close()

	config := buildOpenAIConfig(t, server.URL)
	config.Executor.Workspace = workspace
	config.MaxTurns = 4
	config.Tools = types.ToolsConfig{
		BuiltIn:  []string{"write_file"},
		PostEdit: []types.PostEditCheckConfig{{Glob: "*.go", Command: "echo undefined: frobnicate in {path} AKIAABCDEFGHIJKLMNOP; exit 1"}},
	}

	t.Setenv("TEST_OPENAI_KEY", "test-key")
	loop, err := BuildLoopWithTransport(context.Background(), config, transport.NewStdioTransport(&bytes.Buffer{}, &bytes.Buffer{}))
	if err != nil {
		t.Fatalf("BuildLoopWithTransport() error: %v", err)
	}
	defer func() { _ = loop.Close() }()

	runTrace, err := loop.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("Run() error: %v", err)
	}
	if runTrace.Outcome != "success" {
		t.Fatalf("outcome = %q, want success", runTrace.Outcome)
	}
	if _, err := os.Stat(filepath.Join(workspace, "main.go")); err != nil {
		t.Fatalf("edit was not applied: %v", err)
	}
	if len(requestBodies) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(requestBodies))
	}
	second := requestBodies[1]
	for _, want := range []string{"undefined: frobnicate in main.go", "POST_EDIT_DIAGNOSTICS_", "exited with code 1"} {
		if !strings.Contains(second, want) {
			t.Errorf("follow-up request lacks %q:\n%s", want, second)
		}
	}
	if strings.Contains(second, "AKIAABCDEFGHIJKLMNOP") {
		t.Error("unscrubbed secret reached the model")
	}

	if len(runTrace.PostEditChecks) != 1 {
		t.Fatalf("PostEditChecks = %+v", runTrace.PostEditChecks)
	}
	rec := runTrace.PostEditChecks[0]
	if rec.Tool != "write_file" || rec.Path != "main.go" || rec.ExitCode != 1 || rec.ToolUseID != "call_1" {
		t.Errorf("record = %+v", rec)
	}
}
