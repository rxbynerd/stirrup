package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

const guardRejectionText = "Tool call rejected by security guard"

// toolCallChunk streams one openai-compatible tool call followed by a
// tool_calls finish.
func toolCallChunk(t *testing.T, id, name string, args map[string]string) string {
	t.Helper()
	argJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	quoted, err := json.Marshal(string(argJSON))
	if err != nil {
		t.Fatal(err)
	}
	return openAIChunk(`{"id":"`+id+`","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"`+name+`","arguments":`+string(quoted)+`}}]},"finish_reason":null}]}`) +
		openAIChunk(`{"id":"`+id+`","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
		"data: [DONE]\n\n"
}

func TestBuildLoop_ProtectedWriteTargets(t *testing.T) {
	const original = "# original\n"
	const rewritten = "# rewritten\n"

	type testCase struct {
		name         string
		configure    func(*types.RunConfig)
		tool         string
		args         func(workspace string) map[string]string
		file         string
		wantRejected bool
	}
	writeArgs := func(p string) func(string) map[string]string {
		return func(string) map[string]string { return map[string]string{"path": p, "content": rewritten} }
	}
	rewriteArgs := func(p string) func(string) map[string]string {
		return func(string) map[string]string {
			return map[string]string{"path": p, "operation": "rewrite", "content": rewritten}
		}
	}
	guidance := func(files ...string) func(*types.RunConfig) {
		return func(c *types.RunConfig) {
			c.RepositoryGuidance = &types.RepositoryGuidanceConfig{Files: files}
		}
	}
	operator := func(entries ...string) func(*types.RunConfig) {
		return func(c *types.RunConfig) { c.Tools.ProtectedWriteTargets = entries }
	}

	cases := []testCase{
		{name: "default allows root AGENTS.md", tool: "write_file", args: writeArgs("AGENTS.md"), file: "AGENTS.md"},
		{name: "default allows root CLAUDE.md via edit_file", tool: "edit_file", args: rewriteArgs("./CLAUDE.md"), file: "CLAUDE.md"},
		{name: "default allows .agents tree", tool: "write_file", args: writeArgs(".agents/skills/x.md"), file: ".agents/skills/x.md"},
		{name: "loaded guidance rejects write_file", configure: guidance("AGENTS.md"), tool: "write_file", args: writeArgs("AGENTS.md"), file: "AGENTS.md", wantRejected: true},
		{name: "loaded guidance rejects edit_file", configure: guidance("AGENTS.md"), tool: "edit_file", args: rewriteArgs("./AGENTS.md"), file: "AGENTS.md", wantRejected: true},
		{
			name: "loaded guidance rejects the absolute workspace path", configure: guidance("AGENTS.md"), tool: "write_file",
			args: func(ws string) map[string]string {
				return map[string]string{"path": filepath.Join(ws, "AGENTS.md"), "content": rewritten}
			},
			file: "AGENTS.md", wantRejected: true,
		},
		{name: "loaded guidance leaves nested copies writable", configure: guidance("AGENTS.md"), tool: "write_file", args: writeArgs("docs/AGENTS.md"), file: "docs/AGENTS.md"},
		{name: "missing advisory guidance is not protected", configure: guidance("GUIDE.md"), tool: "write_file", args: writeArgs("GUIDE.md"), file: "GUIDE.md"},
		{name: "operator path rejects plain form", configure: operator("docs/AGENTS.md"), tool: "write_file", args: writeArgs("docs/AGENTS.md"), file: "docs/AGENTS.md", wantRejected: true},
		{name: "operator path rejects ./ form", configure: operator("docs/AGENTS.md"), tool: "write_file", args: writeArgs("./docs/AGENTS.md"), file: "docs/AGENTS.md", wantRejected: true},
		{name: "operator path rejects backslash form", configure: operator("docs/AGENTS.md"), tool: "edit_file", args: rewriteArgs(`docs\AGENTS.md`), file: "docs/AGENTS.md", wantRejected: true},
		{name: "operator exact path leaves siblings writable", configure: operator("docs/AGENTS.md"), tool: "write_file", args: writeArgs("docs/guide.md"), file: "docs/guide.md"},
		{name: "operator directory rejects descendants", configure: operator("docs/"), tool: "write_file", args: writeArgs("docs/guide.md"), file: "docs/guide.md", wantRejected: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"AGENTS.md", "CLAUDE.md", "docs/AGENTS.md", "docs/guide.md"} {
				p := filepath.Join(workspace, name)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(original), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			var bodies []string
			server := newOpenAIServer(t, nil, []string{
				toolCallChunk(t, "pw-1", tc.tool, tc.args(workspace)),
				finalAnswerChunk("pw-2"),
			}, &bodies)
			defer server.Close()

			config := buildOpenAIConfig(t, server.URL)
			config.Executor.Workspace = workspace
			if tc.tool == "edit_file" {
				config.EditStrategy = types.EditStrategyConfig{Type: "multi"}
			}
			if tc.configure != nil {
				tc.configure(config)
			}
			if err := types.ValidateRunConfig(config); err != nil {
				t.Fatalf("ValidateRunConfig: %v", err)
			}

			loop, _ := buildGuidanceLoop(t, config)
			if _, err := loop.Run(context.Background(), config); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(bodies) != 2 {
				t.Fatalf("provider requests = %d, want 2", len(bodies))
			}
			rejected := strings.Contains(bodies[1], guardRejectionText)
			if rejected != tc.wantRejected {
				t.Errorf("guard rejection = %v, want %v; tool result request: %s", rejected, tc.wantRejected, bodies[1])
			}
			got, err := os.ReadFile(filepath.Join(workspace, tc.file))
			if err != nil {
				t.Fatalf("read %s: %v", tc.file, err)
			}
			wantContent := rewritten
			if tc.wantRejected {
				wantContent = original
			}
			if string(got) != wantContent {
				t.Errorf("%s content = %q, want %q", tc.file, got, wantContent)
			}
		})
	}
}

func newProtectedGuidanceLoop(t *testing.T, prov provider.ProviderAdapter) (*AgenticLoop, *[]string) {
	t.Helper()
	remote := &remoteGuidanceExecutor{files: map[string]string{"AGENTS.md": "conventions"}}
	loop := buildTestLoop(simpleSuccessProvider())
	loop.Provider = prov
	loop.Executor = remote
	loop.ProtectedWriteTargets = []string{"config/"}
	loop.Guidance = prompt.NewRepositoryGuidanceLoader(types.RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}}, remote)

	var writes []string
	loop.Tools.(*tool.Registry).Register(&tool.Tool{
		Name:              "write_file",
		Description:       "records the path it is asked to write",
		InputSchema:       json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
		WorkspaceMutating: true,
		Handler: func(_ context.Context, input json.RawMessage) (string, error) {
			var in struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return "", err
			}
			writes = append(writes, in.Path)
			return "ok", nil
		},
	})
	return loop, &writes
}

func TestLoop_ProtectedWriteTargets_ResetBetweenRuns(t *testing.T) {
	loop, _ := newProtectedGuidanceLoop(t, simpleSuccessProvider())
	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, p := range []string{"AGENTS.md", "config/app.yaml"} {
		if !loop.writeProtection.Matches(p) {
			t.Errorf("after a guidance run, %q must be protected", p)
		}
	}

	loop.Guidance = nil
	if _, err := loop.Run(context.Background(), buildTestConfig()); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if loop.writeProtection.Matches("AGENTS.md") {
		t.Error("a run without guidance must not keep the previous run's guidance protection")
	}
	if !loop.writeProtection.Matches("config/app.yaml") {
		t.Error("operator targets must stay protected on every run")
	}
}

func TestSpawnSubAgent_InheritsProtectedWriteTargets(t *testing.T) {
	parent, writes := newProtectedGuidanceLoop(t, simpleSuccessProvider())
	config := buildTestConfig()
	config.RepositoryGuidance = &types.RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}}
	if _, err := parent.Run(context.Background(), config); err != nil {
		t.Fatalf("parent Run: %v", err)
	}

	parent.Provider = &multiCallProvider{calls: [][]types.StreamEvent{
		{
			{Type: "tool_call", ID: "w1", Name: "write_file", Input: map[string]any{"path": "AGENTS.md", "content": "x"}},
			{Type: "tool_call", ID: "w2", Name: "write_file", Input: map[string]any{"path": "config/app.yaml", "content": "x"}},
			{Type: "tool_call", ID: "w3", Name: "write_file", Input: map[string]any{"path": "notes.md", "content": "x"}},
			{Type: "message_complete", StopReason: "tool_use"},
		},
		{
			{Type: "text_delta", Text: "done"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}}
	if _, err := SpawnSubAgent(context.Background(), parent, config, SubAgentConfig{Prompt: "child task"}); err != nil {
		t.Fatalf("SpawnSubAgent: %v", err)
	}
	if len(*writes) != 1 || (*writes)[0] != "notes.md" {
		t.Errorf("sub-agent writes = %v, want only notes.md", *writes)
	}
}
