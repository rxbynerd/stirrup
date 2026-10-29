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
	cfg := &types.RunConfig{Provider: types.ProviderConfig{Type: "openai-responses"}}
	must.NoError(applyFixtureServer(cfg, types.FixtureServerConfig{Mode: types.FixtureModeSSE}, "http://127.0.0.1:9"))
	must.Equal("http://127.0.0.1:9", cfg.Provider.BaseURL)
}

func TestApplyFixtureServer_SSE_RejectsProviderThatWouldNotBeKeylessAndLocal(t *testing.T) {
	tests := []struct {
		name     string
		provider types.ProviderConfig
		want     string
	}{
		{"preset base url", types.ProviderConfig{Type: "openai-compatible", BaseURL: "https://example.com/v1"}, "must leave it unset"},
		{"non openai-wire provider", types.ProviderConfig{Type: "anthropic"}, "needs an openai-compatible or openai-responses provider"},
		{"api key ref", types.ProviderConfig{Type: "openai-compatible", APIKeyRef: "secret://KEY"}, "is keyless"},
		{"credential block", types.ProviderConfig{Type: "openai-compatible", Credential: &types.CredentialConfig{Type: "static"}}, "is keyless"},
		{"query params", types.ProviderConfig{Type: "openai-compatible", QueryParams: map[string]string{"k": "v"}}, "is keyless"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			cfg := &types.RunConfig{Provider: tc.provider}
			err := applyFixtureServer(cfg, types.FixtureServerConfig{Mode: types.FixtureModeSSE}, "http://127.0.0.1:9")
			must.ErrorContains(err, tc.want)
			must.Equal(tc.provider.BaseURL, cfg.Provider.BaseURL)
		})
	}
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
// and, per invocation, a copy of its --config document in configDir, then
// writes a trace to the config's trace path.
func writeConfigCapturingHarness(t *testing.T) (harnessPath, argvPath, configDir string) {
	t.Helper()
	dir := t.TempDir()
	harnessPath = filepath.Join(dir, "fake-harness")
	argvPath = filepath.Join(dir, "argv.txt")
	configDir = filepath.Join(dir, "configs")
	require.NoError(t, os.Mkdir(configDir, 0o700))
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
cp "$CONFIG" %q/config-$$.json
TRACE=$(sed -n 's/.*"filePath":"\([^"]*\)".*/\1/p' "$CONFIG")
echo '{"id":"run-1","turns":1,"outcome":"success"}' > "$TRACE"
`, argvPath, configDir)
	require.NoError(t, os.WriteFile(harnessPath, []byte(script), 0o755))
	return harnessPath, argvPath, configDir
}

// readCapturedConfigs decodes every config document the fake harness saw.
func readCapturedConfigs(t *testing.T, configDir string) []types.RunConfig {
	t.Helper()
	must := require.New(t)
	paths, err := filepath.Glob(filepath.Join(configDir, "config-*.json"))
	must.NoError(err)
	configs := make([]types.RunConfig, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		must.NoError(err)
		var cfg types.RunConfig
		must.NoError(json.Unmarshal(data, &cfg))
		configs = append(configs, cfg)
	}
	return configs
}

// TestRunSuite_FixtureTask_MCPKeepsProviderFlags pins that an mcp fixture
// task still receives the invocation's live-model flags: the stub stands in
// for a tool server, not for the model.
func TestRunSuite_FixtureTask_MCPKeepsProviderFlags(t *testing.T) {
	must := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness assumes POSIX sh")
	}
	harnessPath, argvPath, configDir := writeConfigCapturingHarness(t)

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

	configs := readCapturedConfigs(t, configDir)
	must.Len(configs, 1)
	must.Len(configs[0].Tools.MCPServers, 1)
	must.Regexp(`^http://127\.0\.0\.1:\d+$`, configs[0].Tools.MCPServers[0].URI)
}

// TestRunSuite_FixtureTask_MCPFillsBaselineServerWithoutURI pins that a
// uri-less mcp_server inherited from the suite baseline, with no task
// override, gets the fixture server's URL.
func TestRunSuite_FixtureTask_MCPFillsBaselineServerWithoutURI(t *testing.T) {
	must := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness assumes POSIX sh")
	}
	harnessPath, _, configDir := writeConfigCapturingHarness(t)

	suite := fixtureSuite(t)
	suite.Tasks[0].FixtureServer = &types.FixtureServerConfig{Mode: types.FixtureModeMCP}
	suite.Tasks[0].RunConfigOverrides = nil
	suite.RunConfig.Provider = types.ProviderConfig{Type: "anthropic", APIKeyRef: "secret://KEY"}
	suite.RunConfig.ModelRouter = types.ModelRouterConfig{Type: "static", Provider: "anthropic", Model: "claude-sonnet-5"}
	suite.RunConfig.Tools.MCPServers = []types.MCPServerConfig{{Name: "stub"}}

	_, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harnessPath})
	must.NoError(err)

	configs := readCapturedConfigs(t, configDir)
	must.Len(configs, 1)
	must.Regexp(`^http://127\.0\.0\.1:\d+$`, configs[0].Tools.MCPServers[0].URI)
}

// TestRunSuite_FixtureTask_EachTrialGetsItsOwnServer pins that concurrent
// trials of one fixture task are handed distinct servers, since an sse
// server counts its requests and would refuse a second trial's first turn.
func TestRunSuite_FixtureTask_EachTrialGetsItsOwnServer(t *testing.T) {
	must := require.New(t)
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness assumes POSIX sh")
	}
	harnessPath, _, configDir := writeConfigCapturingHarness(t)

	result, err := RunSuite(context.Background(), fixtureSuite(t), RunConfig{
		HarnessPath: harnessPath,
		Trials:      3,
		Concurrency: 3,
	})
	must.NoError(err)
	must.Len(result.Tasks, 1)

	urls := map[string]bool{}
	for _, cfg := range readCapturedConfigs(t, configDir) {
		urls[cfg.Provider.BaseURL] = true
	}
	must.Len(urls, 3)
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
	harnessPath, argvPath, configDir := writeConfigCapturingHarness(t)

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

	configs := readCapturedConfigs(t, configDir)
	must.Len(configs, 1)
	cfg := configs[0]
	must.Equal("openai-compatible", cfg.Provider.Type)
	must.Regexp(`^http://127\.0\.0\.1:\d+$`, cfg.Provider.BaseURL)

	host := cfg.Provider.BaseURL[len("http://"):]
	conn, dialErr := net.Dial("tcp", host)
	if dialErr == nil {
		_ = conn.Close()
	}
	must.Error(dialErr, "fixture server still listening after the task ended")
}

// TestRunSuite_FixtureTask_MisconfigurationIsAnError pins that a fixture
// the merged config cannot use fails the task, in a dry run and in a real
// run alike, without launching the harness.
func TestRunSuite_FixtureTask_MisconfigurationIsAnError(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*types.EvalSuite)
		want   string
	}{
		{
			name: "sse with a preset base url",
			mutate: func(s *types.EvalSuite) {
				s.Tasks[0].RunConfigOverrides.Provider.BaseURL = "https://example.com/v1"
			},
			want: "must leave it unset",
		},
		{
			name: "mcp without a uri-less server",
			mutate: func(s *types.EvalSuite) {
				s.Tasks[0].FixtureServer = &types.FixtureServerConfig{Mode: types.FixtureModeMCP}
			},
			want: "mcp_server with no uri",
		},
		{
			name: "missing stream file",
			mutate: func(s *types.EvalSuite) {
				s.Tasks[0].FixtureServer.StreamFile = filepath.Join(os.TempDir(), "absent-stream-file.sse")
			},
			want: "stream file",
		},
		{
			name: "unknown mode",
			mutate: func(s *types.EvalSuite) {
				s.Tasks[0].FixtureServer.Mode = "grpc"
			},
			want: `unknown fixture`,
		},
	}
	for _, tc := range tests {
		for _, dryRun := range []bool{true, false} {
			name := tc.name + " real run"
			if dryRun {
				name = tc.name + " dry run"
			}
			t.Run(name, func(t *testing.T) {
				must := require.New(t)
				suite := fixtureSuite(t)
				tc.mutate(&suite)
				harnessPath, argvPath, _ := writeConfigCapturingHarness(t)
				result, err := RunSuite(context.Background(), suite, RunConfig{DryRun: dryRun, HarnessPath: harnessPath})
				must.NoError(err)
				must.Equal("error", result.Tasks[0].Outcome)
				must.Contains(result.Tasks[0].Error, tc.want)
				_, statErr := os.Stat(argvPath)
				must.True(os.IsNotExist(statErr), "harness ran despite the error")
			})
		}
	}
}

func TestRunSuite_FixtureTask_WithoutBaselineIsAnErrorInARealRun(t *testing.T) {
	must := require.New(t)
	suite := fixtureSuite(t)
	suite.RunConfig = nil
	harnessPath, argvPath, _ := writeConfigCapturingHarness(t)
	result, err := RunSuite(context.Background(), suite, RunConfig{HarnessPath: harnessPath})
	must.NoError(err)
	must.Equal("error", result.Tasks[0].Outcome)
	must.Contains(result.Tasks[0].Error, "needs a suite run_config")
	_, statErr := os.Stat(argvPath)
	must.True(os.IsNotExist(statErr), "harness ran despite the error")
}

func TestWithoutProviderFlags_ClearsModelSelectionAndKeepsTheRest(t *testing.T) {
	must := require.New(t)
	in := RunConfig{
		HarnessPath:  "/bin/stirrup",
		OutputDir:    "out",
		Concurrency:  4,
		Trials:       2,
		Model:        "m",
		PromptModel:  "pm",
		Provider:     "anthropic",
		BaseURL:      "https://example.com",
		APIKeyRef:    "secret://K",
		AnthropicWIF: AnthropicWIFConfig{FederationRuleID: "fdrl_1", FromGitHubActions: true},
	}
	got := withoutProviderFlags(in)
	must.Equal(RunConfig{HarnessPath: "/bin/stirrup", OutputDir: "out", Concurrency: 4, Trials: 2}, got)
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
