package builtins

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/harness/internal/tool"
	"github.com/rxbynerd/stirrup/types"
)

func searchFixtureLoader() *tool.LazyLoader {
	reg := tool.NewRegistry()
	noop := func(context.Context, json.RawMessage) (string, error) { return "", nil }
	for _, t := range []*tool.Tool{
		{Name: "read_file", Description: "Read a file", InputSchema: json.RawMessage(`{"type":"object"}`), Handler: noop},
		ToolSearchTool(),
		{Name: "mcp_github_create_issue", Description: "Open a new issue.\nSecond line.", InputSchema: json.RawMessage(`{"type":"object"}`), Handler: noop},
		{Name: "mcp_github_list_pulls", Description: strings.Repeat("é", 150), InputSchema: json.RawMessage(`{"type":"object"}`), Handler: noop},
	} {
		reg.Register(t)
	}
	return tool.NewLazyLoader(reg, types.LazyToolLoadingConfig{Enabled: true}, tool.LazyLoaderOptions{MCPServers: []string{"github"}})
}

func TestToolSearchTool_Flags(t *testing.T) {
	must := require.New(t)
	ts := ToolSearchTool()
	must.Equal(types.ToolSearchToolName, ts.Name)
	must.False(ts.WorkspaceMutating)
	must.False(ts.RequiresApproval)
	must.True(ts.SelfBoundedResult)
}

func TestToolSearchTool_SchemaBoundsQueryAndLimit(t *testing.T) {
	schema := ToolSearchTool().InputSchema
	cases := []struct {
		name  string
		input string
		ok    bool
	}{
		{"query only", `{"query":"github"}`, true},
		{"query and limit", `{"query":"github","limit":20}`, true},
		{"missing query", `{"limit":3}`, false},
		{"empty query", `{"query":""}`, false},
		{"query too long", `{"query":"` + strings.Repeat("a", 501) + `"}`, false},
		{"limit zero", `{"query":"x","limit":0}`, false},
		{"limit above maximum", `{"query":"x","limit":21}`, false},
		{"unknown field", `{"query":"x","extra":1}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			err := security.ValidateJSONSchema(json.RawMessage(tc.input), schema)
			if tc.ok {
				must.NoError(err)
			} else {
				must.Error(err)
			}
		})
	}
}

func TestToolSearchTool_LoadsMatchesAndListsThem(t *testing.T) {
	must := require.New(t)
	loader := searchFixtureLoader()
	ctx := tool.WithCallContext(context.Background(), tool.CallContext{Lazy: loader})

	out, err := ToolSearchTool().Handler(ctx, json.RawMessage(`{"query":"github","limit":5}`))
	must.NoError(err)
	lines := strings.Split(out, "\n")
	must.Equal("Loaded 2 tools:", lines[0])
	must.Equal("- mcp_github_create_issue: Open a new issue.…", lines[1])
	must.True(strings.HasPrefix(lines[2], "- mcp_github_list_pulls: "))
	must.True(strings.HasSuffix(lines[2], "…"))
	must.LessOrEqual(len(strings.TrimPrefix(lines[2], "- mcp_github_list_pulls: ")), toolSearchDescriptionBytes+len("…"))
	must.True(loader.Loaded("mcp_github_create_issue"))
	must.True(loader.Loaded("mcp_github_list_pulls"))
}

func TestToolSearchTool_ReportsNoMatch(t *testing.T) {
	must := require.New(t)
	loader := searchFixtureLoader()
	ctx := tool.WithCallContext(context.Background(), tool.CallContext{Lazy: loader})

	out, err := ToolSearchTool().Handler(ctx, json.RawMessage(`{"query":"weather"}`))
	must.NoError(err)
	must.Equal(`No tools matched "weather"; try different terms.`, out)
	must.Zero(loader.LoadedCount())
}

func TestToolSearchTool_FailsWithoutALoader(t *testing.T) {
	must := require.New(t)
	_, err := ToolSearchTool().Handler(context.Background(), json.RawMessage(`{"query":"github"}`))
	must.EqualError(err, "lazy tool loading is not active")
}
