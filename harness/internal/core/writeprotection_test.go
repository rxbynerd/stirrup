package core

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/prompt"
	"github.com/rxbynerd/stirrup/harness/internal/provider"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/tool/builtins"
	"github.com/rxbynerd/stirrup/types"
)

const guardRejectionText = "Tool call rejected by security guard"

// toolCallChunk streams one openai-compatible tool call followed by a
// tool_calls finish.
func toolCallChunk(t *testing.T, id, name string, args map[string]string) string {
	t.Helper()
	must := require.New(t)
	argJSON, err := json.Marshal(args)
	must.NoError(err)
	quoted, err := json.Marshal(string(argJSON))
	must.NoError(err)
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
		{name: "loaded guidance rejects a case variant", configure: guidance("AGENTS.md"), tool: "write_file", args: writeArgs("agents.md"), file: "AGENTS.md", wantRejected: true},
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
			must := require.New(t)
			workspace, err := filepath.EvalSymlinks(t.TempDir())
			must.NoError(err)
			for _, name := range []string{"AGENTS.md", "CLAUDE.md", "docs/AGENTS.md", "docs/guide.md"} {
				p := filepath.Join(workspace, name)
				must.NoError(os.MkdirAll(filepath.Dir(p), 0o755))
				must.NoError(os.WriteFile(p, []byte(original), 0o644))
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
			must.NoError(types.ValidateRunConfig(config), "ValidateRunConfig")

			loop, _ := buildGuidanceLoop(t, config)
			_, err = loop.Run(context.Background(), config)
			must.NoError(err, "Run")
			must.Len(bodies, 2)
			must.Equal(tc.wantRejected, strings.Contains(bodies[1], guardRejectionText),
				"tool result request: %s", bodies[1])

			got, err := os.ReadFile(filepath.Join(workspace, tc.file))
			must.NoError(err)
			wantContent := rewritten
			if tc.wantRejected {
				wantContent = original
			}
			must.Equal(wantContent, string(got))
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
	check := assert.New(t)
	must := require.New(t)
	loop, _ := newProtectedGuidanceLoop(t, simpleSuccessProvider())
	_, err := loop.Run(context.Background(), buildTestConfig())
	must.NoError(err, "Run")
	for _, p := range []string{"AGENTS.md", "config/app.yaml"} {
		check.True(loop.writeProtection.Matches(p), "after a guidance run, %q must be protected", p)
	}

	loop.Guidance = nil
	_, err = loop.Run(context.Background(), buildTestConfig())
	must.NoError(err, "second Run")
	check.False(loop.writeProtection.Matches("AGENTS.md"),
		"a run without guidance must not keep the previous run's guidance protection")
	check.True(loop.writeProtection.Matches("config/app.yaml"),
		"operator targets must stay protected on every run")
}

func TestSpawnSubAgent_InheritsProtectedWriteTargets(t *testing.T) {
	must := require.New(t)
	parent, writes := newProtectedGuidanceLoop(t, simpleSuccessProvider())
	config := buildTestConfig()
	config.RepositoryGuidance = &types.RepositoryGuidanceConfig{Files: []string{"AGENTS.md"}}
	_, err := parent.Run(context.Background(), config)
	must.NoError(err, "parent Run")

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
	_, err = SpawnSubAgent(context.Background(), parent, config, SubAgentConfig{Prompt: "child task"})
	must.NoError(err, "SpawnSubAgent")
	must.Equal([]string{"notes.md"}, *writes)
}

func TestLoop_ProtectedWriteTargets_EscapingSymlinkReportsTraversalOnce(t *testing.T) {
	must := require.New(t)
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	must.NoError(err)
	must.NoError(os.Symlink(t.TempDir(), filepath.Join(workspace, "escape_link")))
	local, err := executor.NewLocalExecutor(workspace)
	must.NoError(err)

	var secBuf bytes.Buffer
	loop := buildTestLoopWithSecurity(nil, &secBuf)
	local.Security = loop.Security
	loop.Executor = local
	loop.ProtectedWriteTargets = []string{"AGENTS.md"}
	loop.Tools.(*tool.Registry).Register(builtins.WriteFileTool(local))
	loop.Provider = &multiCallProvider{calls: [][]types.StreamEvent{
		{
			{Type: "tool_call", ID: "w1", Name: "write_file", Input: map[string]any{"path": "escape_link/notes.md", "content": "x"}},
			{Type: "message_complete", StopReason: "tool_use"},
		},
		{
			{Type: "text_delta", Text: "done"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}}

	config := buildTestConfig()
	config.Executor.Workspace = workspace
	_, err = loop.Run(context.Background(), config)
	must.NoError(err, "Run")
	must.Equal(1, strings.Count(secBuf.String(), `"event":"path_traversal_blocked"`),
		"path_traversal_blocked events for one escaping write:\n%s", secBuf.String())
}
