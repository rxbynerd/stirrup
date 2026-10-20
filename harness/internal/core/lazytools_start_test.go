package core

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/harness/internal/tool/builtins"
	"github.com/rxbynerd/stirrup/types"
)

const (
	lazyStartLogMessage      = "lazy tool loading enabled"
	lazyStartNoOpLogMessage  = "lazy tool loading enabled with nothing to defer; every tool is sent"
	unknownEagerLogMessage   = "tools.lazyLoading.eager names a tool this run does not have; it is ignored"
	unknownEagerWarningStart = "tools.lazyLoading.eager names "
)

// lazyLogRecords decodes the JSON log lines in buf whose message is msg.
func lazyLogRecords(t *testing.T, buf *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	must := require.New(t)
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		must.NoError(json.Unmarshal([]byte(line), &rec), "log line %q", line)
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func bufferLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func registerLazyMCPTool(registry *tool.Registry, name string) {
	registry.Register(&tool.Tool{
		Name:        name,
		Description: "stub " + name,
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler:     func(context.Context, json.RawMessage) (string, error) { return "ran " + name, nil },
	})
}

func TestRunFollowUpLoop_LazyTools_ReportsSetupOncePerSession(t *testing.T) {
	must := require.New(t)
	prov := &recordingScriptProvider{script: [][]types.StreamEvent{scriptEndTurn}}
	loop, tr := buildUserInputTestLoop(prov)
	var logs bytes.Buffer
	loop.Logger = bufferLogger(&logs)
	registry := loop.Tools.(*tool.Registry)
	registerLazyMCPTool(registry, lazyMCPTool)
	registry.Register(builtins.ToolSearchTool())
	loop.LazyTools = tool.NewLazyLoader(registry, types.LazyToolLoadingConfig{Enabled: true, Eager: []string{"test_tool", "no_such_tool"}}, tool.LazyLoaderOptions{})
	config := buildTestConfig()
	config.FollowUpHistory = types.FollowUpHistoryContinue

	followUpSession(t, loop, tr, config, "first follow-up", "second follow-up")

	tr.mu.Lock()
	var warnings []string
	for _, e := range tr.events {
		if e.Type == "warning" && strings.HasPrefix(e.Message, unknownEagerWarningStart) {
			warnings = append(warnings, e.Message)
		}
	}
	tr.mu.Unlock()
	must.Equal([]string{`tools.lazyLoading.eager names "no_such_tool", which is not a registered tool; it is ignored`}, warnings)
	must.Len(lazyLogRecords(t, &logs, unknownEagerLogMessage), 1)
	must.Len(lazyLogRecords(t, &logs, lazyStartLogMessage), 1)
}

func TestLoop_LazyTools_LogsWhatRunStartDefers(t *testing.T) {
	t.Run("deferred MCP and control-plane tools", func(t *testing.T) {
		must := require.New(t)
		loop := buildTestLoop(nil)
		loop.Provider = &scriptedParamsProvider{}
		var logs bytes.Buffer
		loop.Logger = bufferLogger(&logs)
		registry := loop.Tools.(*tool.Registry)
		registerLazyMCPTool(registry, lazyMCPTool)
		registerLazyMCPTool(registry, "mcp_github_add_comment")
		registry.Register(tool.ControlPlaneTool(types.ControlPlaneToolConfig{
			Name: "ticket_lookup", Description: "look up a ticket", InputSchema: json.RawMessage(`{"type":"object"}`),
		}))
		registry.Register(builtins.ToolSearchTool())
		loop.LazyTools = tool.NewLazyLoader(registry, types.LazyToolLoadingConfig{Enabled: true, Strategy: types.LazyToolStrategyHarness},
			tool.LazyLoaderOptions{ControlPlaneTools: []string{"ticket_lookup"}})

		_, err := loop.Run(context.Background(), buildTestConfig())
		must.NoError(err)
		records := lazyLogRecords(t, &logs, lazyStartLogMessage)
		must.Len(records, 1)
		must.Equal("info", strings.ToLower(records[0]["level"].(string)))
		must.Equal(map[string]any{
			"strategy": "harness", "eager": 1.0, "deferred": 3.0,
			"deferred.builtIn": 0.0, "deferred.mcp": 2.0, "deferred.controlPlane": 1.0,
		}, map[string]any{
			"strategy": records[0]["strategy"], "eager": records[0]["eager"], "deferred": records[0]["deferred"],
			"deferred.builtIn": records[0]["deferred.builtIn"], "deferred.mcp": records[0]["deferred.mcp"],
			"deferred.controlPlane": records[0]["deferred.controlPlane"],
		})
	})

	t.Run("nothing deferred", func(t *testing.T) {
		must := require.New(t)
		loop := buildTestLoop(nil)
		loop.Provider = &scriptedParamsProvider{}
		var logs bytes.Buffer
		loop.Logger = bufferLogger(&logs)
		registry := loop.Tools.(*tool.Registry)
		registry.Register(builtins.ToolSearchTool())
		loop.LazyTools = tool.NewLazyLoader(registry, types.LazyToolLoadingConfig{Enabled: true}, tool.LazyLoaderOptions{})

		_, err := loop.Run(context.Background(), buildTestConfig())
		must.NoError(err)
		must.Empty(lazyLogRecords(t, &logs, lazyStartLogMessage))
		records := lazyLogRecords(t, &logs, lazyStartNoOpLogMessage)
		must.Len(records, 1)
		must.InDelta(0.0, records[0]["deferred"], 0)
	})

	t.Run("lazy loading off", func(t *testing.T) {
		must := require.New(t)
		loop := buildTestLoop(nil)
		loop.Provider = &scriptedParamsProvider{}
		var logs bytes.Buffer
		loop.Logger = bufferLogger(&logs)

		_, err := loop.Run(context.Background(), buildTestConfig())
		must.NoError(err)
		must.Empty(lazyLogRecords(t, &logs, lazyStartLogMessage))
		must.Empty(lazyLogRecords(t, &logs, lazyStartNoOpLogMessage))
	})
}

func TestSpawnSubAgent_LazyTools_ChildReportsNoSetup(t *testing.T) {
	for _, mode := range []string{"execution", "research"} {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			registry, _ := spawnParentRegistry(t, true)
			registry.Register(builtins.ToolSearchTool())
			parent := buildSubAgentTestLoop(&mockProvider{})
			parent.Tools = registry
			var logs bytes.Buffer
			parent.Logger = bufferLogger(&logs)
			parent.LazyTools = tool.NewLazyLoader(registry,
				types.LazyToolLoadingConfig{Enabled: true, Eager: []string{"read_file", "run_command", "no_such_tool"}},
				tool.LazyLoaderOptions{ControlPlaneTools: []string{"ticket_lookup"}})
			parent.Provider = &scriptedProvider{turns: [][]types.StreamEvent{spawnTextTurn("done")}}

			_, err := SpawnSubAgent(context.Background(), parent, buildTestConfig(), SubAgentConfig{Prompt: "look around", Mode: mode})
			must.NoError(err)
			must.Empty(lazyLogRecords(t, &logs, unknownEagerLogMessage), "a sub-agent repeats no unknown-eager warning")
			must.Empty(lazyLogRecords(t, &logs, lazyStartLogMessage), "a sub-agent repeats no run-start line")
		})
	}
}

func TestLazyLoader_ForRegistryReportsNoUnknownEager(t *testing.T) {
	must := require.New(t)
	registry, _ := spawnParentRegistry(t, true)
	parentLoader := tool.NewLazyLoader(registry, types.LazyToolLoadingConfig{Enabled: true, Eager: []string{"read_file", "no_such_tool"}}, tool.LazyLoaderOptions{})
	must.Equal([]string{"no_such_tool"}, parentLoader.UnknownEager())
	must.Empty(parentLoader.ForRegistry(registry).UnknownEager())
}
