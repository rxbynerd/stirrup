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
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
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
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
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

// TestToolResultBudget_EveryBuiltinAndMCP drives every built-in tool and
// the MCP bridge through planAndDispatch with inputs that make the tool
// produce as much as it can, and checks no model-visible result exceeds
// the default budget and no cut result keeps a structured envelope.
func TestToolResultBudget_EveryBuiltinAndMCP(t *testing.T) {
	dir, haveGit := budgetWorkspace(t)
	localExec, err := executor.NewLocalExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := commandoutput.New(commandoutput.Options{RunID: "run", ArchivePath: filepath.Join(t.TempDir(), "archive.tar.gz")})
	if err != nil {
		t.Fatal(err)
	}
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
	if err := mcp.NewClient(registry, srv.Client()).Connect(context.Background(), types.MCPServerConfig{Name: "big", URI: srv.URL}, &stubSecretStore{}); err != nil {
		t.Fatalf("connect MCP server: %v", err)
	}

	tr := newAsyncTestTransport()
	loop := buildAsyncTestLoop(t, tr, plainEchoTool())
	loop.Tools = registry
	ctx := tool.WithCallContext(context.Background(), tool.CallContext{RunID: "run"})
	config := &types.RunConfig{Mode: "execution"}
	budget := types.DefaultToolMaxResultBytes

	dispatch := func(t *testing.T, id, name string, input any) types.ToolResult {
		t.Helper()
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		results, _, stall := loop.planAndDispatch(ctx, config, []types.ToolCall{{ID: id, Name: name, Input: raw}}, &stallDetector{}, "anthropic", "claude-sonnet-4-6")
		if stall != "" || len(results) != 1 {
			t.Fatalf("unexpected dispatch outcome: stall %q, %d results", stall, len(results))
		}
		result := results[0]
		if len(result.Content) > budget {
			t.Fatalf("%s result is %d bytes, want <= %d", name, len(result.Content), budget)
		}
		if strings.Contains(result.Content, "[tool result truncated: ") && (result.Structured != nil || result.Kind != "") {
			t.Fatalf("%s: truncated result kept its structured envelope", name)
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
			if !registered[tc.name] {
				t.Fatalf("tool %q is not registered", tc.name)
			}
			result := dispatch(t, "tc_"+tc.name, tc.name, tc.input)
			if tc.wantTrunc && !strings.Contains(result.Content, "[tool result truncated: ") {
				t.Fatalf("expected %s to hit the budget, got %d bytes", tc.name, len(result.Content))
			}
		})
	}

	t.Run("read_command_output", func(t *testing.T) {
		run := dispatch(t, "tc_spill", "run_command", map[string]any{"command": "head -c 3000000 /dev/zero | tr '\\0' 'r'"})
		var spilled struct {
			StdoutRef string `json:"stdout_ref"`
		}
		if err := json.Unmarshal(run.Structured, &spilled); err != nil || spilled.StdoutRef == "" {
			t.Fatalf("run_command did not spill to a reference: %v", err)
		}
		dispatch(t, "tc_read", "read_command_output", map[string]any{"ref": spilled.StdoutRef, "limit": commandoutput.ReadMaxBytes})
	})

	t.Run("read_file single long line", func(t *testing.T) {
		result := dispatch(t, "tc_minified", "read_file", map[string]any{"path": "bundle.min.js"})
		if !strings.Contains(result.Content, " [line truncated: ") || !strings.Contains(result.Content, " of 3145728 bytes shown]") {
			t.Fatalf("expected a per-line marker naming the 3 MiB line, got %d bytes", len(result.Content))
		}
	})

	t.Run("run_command without capture", func(t *testing.T) {
		loop.Tools = buildToolRegistry(localExec, edit.NewMultiStrategy(0.8), types.ToolsConfig{}, nil)
		t.Cleanup(func() { loop.Tools = registry })
		result := dispatch(t, "tc_uncaptured", "run_command", map[string]any{"command": "head -c 3000000 /dev/zero | tr '\\0' 'r'; echo build failed >&2; exit 1"})
		if !strings.Contains(result.Content, "[run_command stdout truncated: ") {
			t.Fatalf("expected a truncation marker in %d bytes", len(result.Content))
		}
		if !strings.HasSuffix(result.Content, "\nSTDERR:\nbuild failed\n\n[exit code: 1]") {
			t.Fatalf("stderr or exit code lost; result ends %q", result.Content[len(result.Content)-80:])
		}
	})

	for name := range registered {
		if !covered[name] {
			t.Errorf("registered tool %q has no budget case", name)
		}
	}
}
