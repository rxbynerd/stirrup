package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

const fixtureStream = "data: {\"choices\":[]}\n\ndata: [DONE]\n"

// fixtureSuite is a suite whose single task declares an sse fixture server
// and a keyless openai-compatible provider with no base URL.
func fixtureSuite(t *testing.T) types.EvalSuite {
	t.Helper()
	streamFile := filepath.Join(t.TempDir(), "turns.sse")
	require.NoError(t, os.WriteFile(streamFile, []byte(fixtureStream), 0o600))
	return types.EvalSuite{
		ID: "fixture-suite",
		RunConfig: &types.RunConfig{
			Mode:             "execution",
			MaxTurns:         4,
			PromptBuilder:    types.PromptBuilderConfig{Type: "default"},
			ContextStrategy:  types.ContextStrategyConfig{Type: "sliding-window", MaxTokens: 200000},
			Executor:         types.ExecutorConfig{Type: "local"},
			EditStrategy:     types.EditStrategyConfig{Type: "search-replace"},
			Verifier:         types.VerifierConfig{Type: "none"},
			PermissionPolicy: types.PermissionPolicyConfig{Type: "allow-all"},
			GitStrategy:      types.GitStrategyConfig{Type: "none"},
			TraceEmitter:     types.TraceEmitterConfig{Type: "jsonl"},
		},
		Tasks: []types.EvalTask{{
			ID:     "t1",
			Prompt: "p",
			Mode:   "execution",
			Judge:  validJudge,
			RunConfigOverrides: &types.RunConfigOverrides{
				Provider:    &types.ProviderConfig{Type: "openai-compatible"},
				ModelRouter: &types.ModelRouterConfig{Type: "static", Provider: "openai-compatible", Model: "fixture-model"},
			},
			FixtureServer: &types.FixtureServerConfig{Mode: types.FixtureModeSSE, StreamFile: streamFile},
		}},
	}
}

func TestApplyFixtureServer_SSE_SetsProviderBaseURL(t *testing.T) {
	must := require.New(t)
	cfg := &types.RunConfig{}
	must.NoError(applyFixtureServer(cfg, types.FixtureServerConfig{Mode: types.FixtureModeSSE}, "http://127.0.0.1:9"))
	must.Equal("http://127.0.0.1:9", cfg.Provider.BaseURL)
}

func TestApplyFixtureServer_SSE_RejectsPresetBaseURL(t *testing.T) {
	must := require.New(t)
	cfg := &types.RunConfig{Provider: types.ProviderConfig{BaseURL: "https://example.com/v1"}}
	err := applyFixtureServer(cfg, types.FixtureServerConfig{Mode: types.FixtureModeSSE}, "http://127.0.0.1:9")
	must.ErrorContains(err, "must leave it unset")
	must.Equal("https://example.com/v1", cfg.Provider.BaseURL)
}

func TestApplyFixtureServer_MCP_FillsServersWithoutURI(t *testing.T) {
	must := require.New(t)
	cfg := &types.RunConfig{Tools: types.ToolsConfig{MCPServers: []types.MCPServerConfig{
		{Name: "stub"},
		{Name: "remote", URI: "https://mcp.example.com"},
	}}}
	must.NoError(applyFixtureServer(cfg, types.FixtureServerConfig{Mode: types.FixtureModeMCP}, "http://127.0.0.1:9"))
	must.Equal("http://127.0.0.1:9", cfg.Tools.MCPServers[0].URI)
	must.Equal("https://mcp.example.com", cfg.Tools.MCPServers[1].URI)
}

func TestApplyFixtureServer_MCP_RequiresServerWithoutURI(t *testing.T) {
	must := require.New(t)
	cfg := &types.RunConfig{Tools: types.ToolsConfig{MCPServers: []types.MCPServerConfig{{Name: "remote", URI: "https://mcp.example.com"}}}}
	err := applyFixtureServer(cfg, types.FixtureServerConfig{Mode: types.FixtureModeMCP}, "http://127.0.0.1:9")
	must.ErrorContains(err, "mcp_server with no uri")
}

func TestApplyFixtureServer_UnknownMode_ReturnsError(t *testing.T) {
	must := require.New(t)
	err := applyFixtureServer(&types.RunConfig{}, types.FixtureServerConfig{Mode: "grpc"}, "http://127.0.0.1:9")
	must.ErrorContains(err, `unknown fixture_server mode "grpc"`)
}

func TestRunSuite_FixtureTask_DryRunValidatesWithoutBaseURL(t *testing.T) {
	must := require.New(t)
	result, err := RunSuite(context.Background(), fixtureSuite(t), RunConfig{DryRun: true})
	must.NoError(err)
	must.Equal("pass", result.Tasks[0].Outcome, "task error %q", result.Tasks[0].Error)
}

func TestRunSuite_FixtureTask_WithoutBaselineIsAnError(t *testing.T) {
	must := require.New(t)
	suite := fixtureSuite(t)
	suite.RunConfig = nil
	result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: true})
	must.NoError(err)
	must.Equal("error", result.Tasks[0].Outcome)
	must.Contains(result.Tasks[0].Error, "needs a suite run_config")
}

// writeConfigCapturingHarness writes a fake harness that records its argv
// and a copy of its --config document, then writes a trace to the config's
// trace path.
func writeConfigCapturingHarness(t *testing.T) (harnessPath, argvPath, configCopy string) {
	t.Helper()
	dir := t.TempDir()
	harnessPath = filepath.Join(dir, "fake-harness")
	argvPath = filepath.Join(dir, "argv.txt")
	configCopy = filepath.Join(dir, "config.json")
	script := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do printf '%%s\n' "$a"; done > %q
shift
CONFIG=""
while [ $# -gt 0 ]; do
  case "$1" in
    --config) CONFIG="$2"; shift 2 ;;
    *) shift ;;
  esac
done
cp "$CONFIG" %q
TRACE=$(sed -n 's/.*"filePath":"\([^"]*\)".*/\1/p' "$CONFIG")
echo '{"id":"run-1","turns":1,"outcome":"success"}' > "$TRACE"
`, argvPath, configCopy)
	require.NoError(t, os.WriteFile(harnessPath, []byte(script), 0o755))
	return harnessPath, argvPath, configCopy
}

// TestRunSuite_FixtureTask_MCPKeepsProviderFlags pins that an mcp fixture
// task still receives the invocation's live-model flags: the stub stands in
// for a tool server, not for the model.
func TestRunSuite_FixtureTask_MCPKeepsProviderFlags(t *testing.T) {
	must := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness assumes POSIX sh")
	}
	harnessPath, argvPath, configCopy := writeConfigCapturingHarness(t)

	suite := fixtureSuite(t)
	suite.Tasks[0].FixtureServer = &types.FixtureServerConfig{Mode: types.FixtureModeMCP}
	suite.Tasks[0].RunConfigOverrides = &types.RunConfigOverrides{
		Provider:    &types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://KEY"},
		ModelRouter: &types.ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "claude-sonnet-5"},
		MCPServers:  []types.MCPServerConfig{{Name: "stub"}},
	}

	_, err := RunSuite(context.Background(), suite, RunConfig{
		HarnessPath: harnessPath,
		Provider:    "openai-compatible",
		Model:       "gpt-test",
	})
	must.NoError(err)

	requireArgvHas(t, readArgvFile(t, argvPath), "--provider", "openai-compatible", "--model", "gpt-test")

	data, err := os.ReadFile(configCopy)
	must.NoError(err)
	var cfg types.RunConfig
	must.NoError(json.Unmarshal(data, &cfg))
	must.Len(cfg.Tools.MCPServers, 1)
	must.Regexp(`^http://127\.0\.0\.1:\d+$`, cfg.Tools.MCPServers[0].URI)
}

// TestRunSuite_FixtureTask_StartsServerAndDropsProviderFlags pins the
// per-task lifecycle: the harness is handed a config whose base URL is the
// live fixture server, none of the invocation's live-model flags reach an
// sse task, and the server is closed once the task ends.
func TestRunSuite_FixtureTask_StartsServerAndDropsProviderFlags(t *testing.T) {
	must := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness assumes POSIX sh")
	}
	harnessPath, argvPath, configCopy := writeConfigCapturingHarness(t)

	_, err := RunSuite(context.Background(), fixtureSuite(t), RunConfig{
		HarnessPath: harnessPath,
		Provider:    "anthropic",
		Model:       "claude-sonnet-5",
		BaseURL:     "https://example.com/v1",
		APIKeyRef:   "secret://KEY",
	})
	must.NoError(err)

	argv := readArgvFile(t, argvPath)
	for _, flag := range []string{"--provider", "--model", "--base-url", "--api-key-ref"} {
		must.NotContains(argv, flag+"\n", "fixture task received %s", flag)
	}

	data, err := os.ReadFile(configCopy)
	must.NoError(err)
	var cfg types.RunConfig
	must.NoError(json.Unmarshal(data, &cfg))
	must.Equal("openai-compatible", cfg.Provider.Type)
	must.Regexp(`^http://127\.0\.0\.1:\d+$`, cfg.Provider.BaseURL)

	host := cfg.Provider.BaseURL[len("http://"):]
	conn, dialErr := net.Dial("tcp", host)
	if dialErr == nil {
		_ = conn.Close()
	}
	must.Error(dialErr, "fixture server still listening after the task ended")
}

func TestRunSuite_FixtureTask_MissingStreamFileIsAnError(t *testing.T) {
	must := require.New(t)
	suite := fixtureSuite(t)
	suite.Tasks[0].FixtureServer.StreamFile = filepath.Join(t.TempDir(), "absent.sse")
	harnessPath, _ := writeArgvRecordingHarness(t)
	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harnessPath})
	must.NoError(err)
	must.Equal("error", result.Tasks[0].Outcome)
	must.Contains(result.Tasks[0].Error, "stream file")
}
