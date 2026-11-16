package types

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func toollessClassificationConfig(mode string) *RunConfig {
	c := validConfig()
	c.Mode = mode
	c.PermissionPolicy = PermissionPolicyConfig{}
	c.Executor = ExecutorConfig{Type: "none"}
	c.Tools = ToolsConfig{NoBuiltIn: true}
	c.OutputSchema = json.RawMessage(`{"type":"object","properties":{"label":{"type":"string"}},"required":["label"]}`)
	return c
}

func TestValidateRunConfig_NoBuiltInAcceptsToollessReadOnlyRun(t *testing.T) {
	for _, mode := range []string{"planning", "review", "research", "toil"} {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			c := toollessClassificationConfig(mode)
			must.NoError(ValidateRunConfig(c))
			must.Equal("deny-side-effects", c.PermissionPolicy.Type, "the read-only policy default still applies")
		})
	}
}

func TestValidateRunConfig_NoBuiltInAcceptsDefaultExecutor(t *testing.T) {
	must := require.New(t)
	c := toollessClassificationConfig("review")
	c.Executor = ExecutorConfig{}
	must.NoError(ValidateRunConfig(c))
}

func TestValidateRunConfig_NoBuiltInRejectsBuiltInList(t *testing.T) {
	for _, mode := range []string{"execution", "review"} {
		t.Run(mode, func(t *testing.T) {
			must := require.New(t)
			c := toollessClassificationConfig(mode)
			c.PermissionPolicy = PermissionPolicyConfig{Type: "deny-side-effects"}
			c.Tools.BuiltIn = []string{"web_fetch"}
			err := ValidateRunConfig(c)
			must.ErrorContains(err, "tools.noBuiltIn registers no built-in tool, but tools.builtIn lists 1")
		})
	}
}

func TestValidateRunConfig_ReadOnlyModeWithoutBuiltInOrMarkerNamesTheMarker(t *testing.T) {
	must := require.New(t)
	c := toollessClassificationConfig("review")
	c.Tools = ToolsConfig{}
	err := ValidateRunConfig(c)
	must.ErrorContains(err, `read-only mode "review" requires an explicit tools.builtIn list`)
	must.ErrorContains(err, "tools.noBuiltIn: true")
}

func TestValidateRunConfig_NoBuiltInAllowsExplicitMCPAndControlPlaneTools(t *testing.T) {
	must := require.New(t)
	c := toollessClassificationConfig("review")
	c.Transport = TransportConfig{Type: "grpc", Address: "localhost:50051"}
	c.Tools.MCPServers = []MCPServerConfig{{Name: "kb", URI: "https://mcp.example.com/sse"}}
	c.Tools.ControlPlane = []ControlPlaneToolConfig{{
		Name:        "lookup_ticket",
		Description: "Look up a ticket.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}}
	must.NoError(ValidateRunConfig(c))
}

func TestToolsConfig_BuiltInEnabledSelection(t *testing.T) {
	tests := []struct {
		name  string
		tools ToolsConfig
		tool  string
		want  bool
	}{
		{name: "empty list enables all", tools: ToolsConfig{}, tool: "read_file", want: true},
		{name: "listed tool", tools: ToolsConfig{BuiltIn: []string{"read_file"}}, tool: "read_file", want: true},
		{name: "unlisted tool", tools: ToolsConfig{BuiltIn: []string{"read_file"}}, tool: "web_fetch", want: false},
		{name: "noBuiltIn disables an empty list", tools: ToolsConfig{NoBuiltIn: true}, tool: "web_fetch", want: false},
		{name: "noBuiltIn disables spawn_agent", tools: ToolsConfig{NoBuiltIn: true}, tool: "spawn_agent", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			must := require.New(t)
			must.Equal(tt.want, tt.tools.BuiltInEnabled(tt.tool))
		})
	}
}

func TestRuleOfTwoLegs_NoBuiltInDropsBuiltInToolLegs(t *testing.T) {
	check := assert.New(t)
	c := toollessClassificationConfig("execution")
	check.False(ruleOfTwoUntrustedInput(c), "no web_fetch means no untrusted-input leg")
	check.False(ruleOfTwoExternalComm(c), "no web_fetch or run_command means no external-comm leg")

	c.Tools = ToolsConfig{}
	check.True(ruleOfTwoUntrustedInput(c), "an empty builtIn list still enables web_fetch")
	check.True(ruleOfTwoExternalComm(c), "an empty builtIn list still enables run_command")
}

func TestToolsConfig_NoBuiltInSurvivesJSONRoundTrip(t *testing.T) {
	must := require.New(t)
	var tools ToolsConfig
	must.NoError(json.Unmarshal([]byte(`{"noBuiltIn":true}`), &tools))
	must.True(tools.NoBuiltIn)
	out, err := json.Marshal(tools)
	must.NoError(err)
	var decoded map[string]any
	must.NoError(json.Unmarshal(out, &decoded))
	must.Equal(true, decoded["noBuiltIn"])
}
