package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/commandoutput"
	"github.com/rxbynerd/stirrup/harness/internal/edit"
	"github.com/rxbynerd/stirrup/harness/internal/executor"
	"github.com/rxbynerd/stirrup/harness/internal/mcp"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/tool/builtins"
	"github.com/rxbynerd/stirrup/types"
)

// budgetWorkspace builds a workspace whose ordinary contents make each
// built-in tool produce a result far above the default budget: a 3 MiB
// single-line file, a 5000-line file, thousands of long file names, long
// matching lines, and (when git is available) a large diff and commit.
func budgetWorkspace(t *testing.T) (string, bool) {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	write("bundle.min.js", strings.Repeat("x", 3<<20))
	write("lines.txt", strings.Repeat(strings.Repeat("l", 300)+"\n", 5000))
	write("matches.txt", strings.Repeat("needle"+strings.Repeat("y", 5000)+"\n", 1000))
	for i := range 3000 {
		write(fmt.Sprintf("many/%04d_%s.txt", i, strings.Repeat("n", 180)), "")
	}

	if _, err := exec.LookPath("git"); err != nil {
		return dir, false
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v\n%s", args, out)
	}
	git("init", "-q")
	write("tracked.txt", "small\n")
	git("add", "tracked.txt")
	git("commit", "-q", "-m", "one")
	write("committed.txt", strings.Repeat(strings.Repeat("c", 200)+"\n", 5000))
	git("add", "committed.txt")
	git("commit", "-q", "-m", "two")
	write("tracked.txt", strings.Repeat(strings.Repeat("d", 200)+"\n", 5000))
	return dir, true
}

// bigMCPServer serves two MCP tools: one returning 5 MB of text with a
// structured payload, one reporting a 5 MB tool error.
func bigMCPServer(t *testing.T) *httptest.Server {
	t.Helper()
	big := strings.Repeat("m", 5_000_000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var result any
		switch req.Method {
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{
				{"name": "dump", "inputSchema": map[string]any{"type": "object"}},
				{"name": "fail", "inputSchema": map[string]any{"type": "object"}},
			}}
		case "tools/call":
			var params struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(req.Params, &params)
			result = map[string]any{
				"content":           []map[string]string{{"type": "text", "text": big}},
				"structuredContent": map[string]string{"summary": "large"},
				"isError":           params.Name == "fail",
			}
		default:
			http.Error(w, "unknown method", http.StatusBadRequest)
			return
		}
		raw, _ := json.Marshal(result)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": json.RawMessage(raw)})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestToolResultBudget_BuiltinAndMCPResultsStayWithinBudget drives every
// built-in tool and the MCP bridge through planAndDispatch with inputs that
// make the tool produce as much as it can, and checks no model-visible result
// exceeds the default budget and no cut result keeps a structured envelope.
func TestToolResultBudget_BuiltinAndMCPResultsStayWithinBudget(t *testing.T) {
	dir, haveGit := budgetWorkspace(t)
	localExec, err := executor.NewLocalExecutor(dir)
	require.NoError(t, err)
	store, err := commandoutput.New(commandoutput.Options{RunID: "run", ArchivePath: filepath.Join(t.TempDir(), "archive.tar.gz")})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	registry := buildToolRegistry(localExec, edit.NewMultiStrategy(0.8), types.ToolsConfig{}, store)
	for _, es := range []edit.EditStrategy{edit.NewWholeFileStrategy(), edit.NewSearchReplaceStrategy(), edit.NewUdiffStrategy(0.8)} {
		registry.Register(editStrategyTool(es, localExec))
	}
	childResult, _ := json.Marshal(map[string]string{"output": strings.Repeat("s", 3<<20)})
	registry.Register(builtins.SpawnAgentTool(func(context.Context, string, string, int) (json.RawMessage, error) {
		return childResult, nil
	}))
	srv := bigMCPServer(t)
	err = mcp.NewClient(registry, srv.Client()).Connect(context.Background(), types.MCPServerConfig{Name: "big", URI: srv.URL}, &stubSecretStore{})
	require.NoError(t, err, "connect MCP server")

	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, plainEchoTool())
	loop.Tools = registry
	ctx := tool.WithCallContext(context.Background(), tool.CallContext{RunID: "run"})
	config := &types.RunConfig{Mode: "execution"}
	budget := types.DefaultToolMaxResultBytes

	dispatch := func(t *testing.T, id, name string, input any) types.ToolResult {
		t.Helper()
		raw, err := json.Marshal(input)
		require.NoError(t, err)
		results, _, stall := loop.planAndDispatch(ctx, config, []types.ToolCall{{ID: id, Name: name, Input: raw}}, &stallDetector{}, "anthropic", "claude-sonnet-4-6")
		require.Empty(t, stall, "unexpected stall")
		require.Len(t, results, 1)
		result := results[0]
		require.LessOrEqual(t, len(result.Content), budget, "%s result size in bytes", name)
		if strings.Contains(result.Content, "[tool result truncated: ") {
			require.Nil(t, result.Structured, "%s: truncated result kept its structured envelope", name)
			require.Empty(t, result.Kind, "%s: truncated result kept its kind", name)
		}
		return result
	}

	content := strings.Repeat("w\n", 16<<10)
	cases := []struct {
		name      string
		input     any
		needsGit  bool
		wantTrunc bool
	}{
		{name: "read_file", input: map[string]any{"path": "lines.txt", "limit": 5000}, wantTrunc: true},
		{name: "list_directory", input: map[string]any{"path": "many", "max_entries": 10000}, wantTrunc: true},
		{name: "grep_files", input: map[string]any{"pattern": "needle", "path": "matches.txt", "max_results": 1000}, wantTrunc: true},
		{name: "find_files", input: map[string]any{"name": "*.txt", "path": "many", "max_results": 1000}, wantTrunc: true},
		{name: "run_command", input: map[string]any{"command": "head -c 3000000 /dev/zero | tr '\\0' 'r'"}},
		// The SSRF guard refuses the loopback test server, so web_fetch
		// covers its refusal path here; its success path is capped at
		// 100 KB by the tool itself.
		{name: "web_fetch", input: map[string]any{"url": srv.URL}},
		{name: "write_file", input: map[string]any{"path": "written.txt", "content": content}},
		{name: "search_replace", input: map[string]any{"path": "written.txt", "old_string": "absent", "new_string": "short"}},
		{name: "apply_diff", input: map[string]any{"path": "written.txt", "diff": "--- a/written.txt\n+++ b/written.txt\n@@ -1,2 +1,2 @@\n-w\n+x\n w\n"}},
		{name: "edit_file", input: map[string]any{"path": "written.txt", "operation": "replace", "old_string": "x\nw\n", "new_string": "y\nw\n"}},
		{name: "spawn_agent", input: map[string]any{"prompt": "summarise"}, wantTrunc: true},
		{name: "mcp_big_dump", input: map[string]any{}, wantTrunc: true},
		{name: "mcp_big_fail", input: map[string]any{}, wantTrunc: true},
		{name: "git_status", input: map[string]any{}, needsGit: true},
		{name: "git_changed_files", input: map[string]any{}, needsGit: true},
		{name: "git_diff", input: map[string]any{}, needsGit: true},
		{name: "git_show", input: map[string]any{"ref": "HEAD"}, needsGit: true},
	}
	registered := map[string]bool{}
	for _, def := range registry.List() {
		registered[def.Name] = true
	}
	covered := map[string]bool{"read_command_output": true}
	for _, tc := range cases {
		covered[tc.name] = true
		t.Run(tc.name, func(t *testing.T) {
			if tc.needsGit && !haveGit {
				t.Skip("git is not installed")
			}
			require.True(t, registered[tc.name], "tool %q is not registered", tc.name)
			result := dispatch(t, "tc_"+tc.name, tc.name, tc.input)
			if tc.wantTrunc {
				require.Contains(t, result.Content, "[tool result truncated: ", "expected the result to hit the budget, got %d bytes", len(result.Content))
			}
		})
	}

	t.Run("read_command_output", func(t *testing.T) {
		run := dispatch(t, "tc_spill", "run_command", map[string]any{"command": "head -c 3000000 /dev/zero | tr '\\0' 'r'"})
		var spilled struct {
			StdoutRef string `json:"stdout_ref"`
		}
		require.NoError(t, json.Unmarshal(run.Structured, &spilled), "run_command structured result")
		require.NotEmpty(t, spilled.StdoutRef, "run_command did not spill to a reference")
		dispatch(t, "tc_read", "read_command_output", map[string]any{"ref": spilled.StdoutRef, "limit": commandoutput.ReadMaxBytes})
	})

	t.Run("read_file single long line", func(t *testing.T) {
		result := dispatch(t, "tc_minified", "read_file", map[string]any{"path": "bundle.min.js"})
		require.Contains(t, result.Content, " [line truncated: ", "expected a per-line marker, got %d bytes", len(result.Content))
		require.Contains(t, result.Content, " of 3145728 bytes shown]", "expected the marker to name the 3 MiB line")
	})

	t.Run("edit_file mismatch hint", func(t *testing.T) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "tabs.go"), []byte("func a() {\n\tx := 1\n\ty := 2\n}\n"), 0o644))
		result := dispatch(t, "tc_hint", "edit_file", map[string]any{"path": "tabs.go", "operation": "replace", "old_string": "    x := 1\n    y := 2", "new_string": "z := 3"})
		require.True(t, result.IsError, "expected an error result, got %q", result.Content)
		require.Contains(t, result.Content, "lines 2-3")
		require.Contains(t, result.Content, "the file indents with tabs")
	})

	t.Run("run_command without capture", func(t *testing.T) {
		loop.Tools = buildToolRegistry(localExec, edit.NewMultiStrategy(0.8), types.ToolsConfig{}, nil)
		t.Cleanup(func() { loop.Tools = registry })
		result := dispatch(t, "tc_uncaptured", "run_command", map[string]any{"command": "head -c 3000000 /dev/zero | tr '\\0' 'r'; echo build failed >&2; exit 1"})
		require.Contains(t, result.Content, "[run_command stdout truncated: ", "expected a truncation marker, got %d bytes", len(result.Content))
		require.True(t, strings.HasSuffix(result.Content, "\nSTDERR:\nbuild failed\n\n[exit code: 1]"), "stderr or exit code lost from the result tail")
	})

	for name := range registered {
		assert.True(t, covered[name], "registered tool %q has no budget case", name)
	}
}
