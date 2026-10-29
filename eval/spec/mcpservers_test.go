package spec

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// TestLoadSuiteHCL_DecodesMCPServers pins mcp_server blocks on the suite
// baseline's tools block and on a task's run_config_overrides tools block,
// including a server with no uri (supplied by an mcp fixture_server) and a
// task without the block, whose override stays nil.
func TestLoadSuiteHCL_DecodesMCPServers(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	src := "suite \"mcp\" {\n" +
		"  run_config {\n" +
		"    tools {\n" +
		"      built_in = [\"read_file\"]\n" +
		"      mcp_server \"docs\" {\n" +
		"        uri               = \"https://docs.example.com/mcp\"\n" +
		"        api_key_ref       = \"secret://DOCS_KEY\"\n" +
		"        allowed_tools     = [\"fetch-page\"]\n" +
		"        allowed_mcp_hosts = [\"docs.example.com\"]\n" +
		"        optional          = true\n" +
		"      }\n" +
		"    }\n" +
		"  }\n" +
		"  task \"override\" {\n" +
		"    mode   = \"execution\"\n" +
		"    prompt = \"p\"\n" +
		"    run_config_overrides {\n" +
		"      tools {\n" +
		"        mcp_server \"stub\" {}\n" +
		"      }\n" +
		"    }\n" +
		"    judge {\n" +
		"      type    = \"test-command\"\n" +
		"      command = \"true\"\n" +
		"    }\n" +
		"  }\n" +
		"  task \"inherit\" {\n" +
		"    mode   = \"execution\"\n" +
		"    prompt = \"p\"\n" +
		"    run_config_overrides {\n" +
		"      max_turns = 3\n" +
		"    }\n" +
		"    judge {\n" +
		"      type    = \"test-command\"\n" +
		"      command = \"true\"\n" +
		"    }\n" +
		"  }\n" +
		"}\n"
	got, err := LoadSuiteHCL(writeTemp(t, "mcp.hcl", src))
	must.NoError(err)
	must.NotNil(got.RunConfig)
	check.Equal([]string{"read_file"}, got.RunConfig.Tools.BuiltIn)
	check.Equal([]types.MCPServerConfig{{
		Name:            "docs",
		URI:             "https://docs.example.com/mcp",
		APIKeyRef:       "secret://DOCS_KEY",
		AllowedTools:    []string{"fetch-page"},
		AllowedMCPHosts: []string{"docs.example.com"},
		Optional:        true,
	}}, got.RunConfig.Tools.MCPServers)
	must.Len(got.Tasks, 2)
	check.Equal([]types.MCPServerConfig{{Name: "stub"}}, got.Tasks[0].RunConfigOverrides.MCPServers)
	check.Nil(got.Tasks[1].RunConfigOverrides.MCPServers)
}

func TestLoadSuiteHCL_RejectsRawMCPServerAPIKey(t *testing.T) {
	tests := []struct {
		name string
		hcl  string
		want string
	}{
		{
			name: "baseline",
			hcl: "suite \"s\" {\n  run_config {\n    tools {\n      mcp_server \"docs\" {\n        uri = \"https://d.example.com\"\n        api_key_ref = \"literal-key\"\n      }\n    }\n  }\n" +
				"  task \"t\" {\n    prompt = \"p\"\n    judge {\n      type = \"test-command\"\n      command = \"true\"\n    }\n  }\n}\n",
			want: `tools.mcp_server "docs" api_key_ref`,
		},
		{
			name: "override",
			hcl: "suite \"s\" {\n  task \"t\" {\n    prompt = \"p\"\n    run_config_overrides {\n      tools {\n        mcp_server \"docs\" {\n          uri = \"https://d.example.com\"\n          api_key_ref = \"literal-key\"\n        }\n      }\n    }\n" +
				"    judge {\n      type = \"test-command\"\n      command = \"true\"\n    }\n  }\n}\n",
			want: `run_config_overrides.tools.mcp_server "docs" api_key_ref`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			_, err := LoadSuiteHCL(writeTemp(t, "raw.hcl", tc.hcl))
			must.ErrorContains(err, tc.want)
			must.ErrorContains(err, "raw credentials are not permitted")
		})
	}
}
