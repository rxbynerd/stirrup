package tool

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

// lazyFixture registers two built-ins, the search tool, two MCP tools
// from server "github", one from "jira" and a control-plane tool.
func lazyFixture() *Registry {
	issue := fixedTool("mcp_github_create_issue", "Open a new issue in a repository")
	issue.InputSchema = json.RawMessage(`{"type":"object","properties":{"title":{"type":"string","description":"Issue headline"}}}`)
	return newRegistryWith(
		fixedTool("read_file", "Read a file"),
		fixedTool("run_command", "Run a shell command"),
		fixedTool(types.ToolSearchToolName, "Search for tools."),
		issue,
		fixedTool("mcp_github_list_pulls", "List pull requests"),
		fixedTool("mcp_jira_find_ticket", "Find a ticket by key"),
		fixedTool("lookup_order", "Look up a customer order"),
	)
}

var lazyFixtureOptions = LazyLoaderOptions{
	ControlPlaneTools: []string{"lookup_order"},
	MCPServers:        []string{"github", "jira"},
}

func defNames(defs []types.ToolDefinition) []string {
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Name
	}
	return names
}

func TestLazyLoader_DefaultEagerDefersMCPAndControlPlaneTools(t *testing.T) {
	must := require.New(t)
	l := NewLazyLoader(lazyFixture(), types.LazyToolLoadingConfig{Enabled: true}, lazyFixtureOptions)

	must.True(l.HasDeferred())
	for _, id := range []string{"mcp_github_create_issue", "mcp_github_list_pulls", "mcp_jira_find_ticket", "lookup_order"} {
		must.True(l.Deferred(id), id)
	}
	for _, id := range []string{"read_file", "run_command", types.ToolSearchToolName} {
		must.False(l.Deferred(id), id)
	}
	must.Empty(l.UnknownEager())
}

func TestLazyLoader_ExplicitEagerDefersEveryOtherTool(t *testing.T) {
	must := require.New(t)
	cfg := types.LazyToolLoadingConfig{Enabled: true, Eager: []string{"read_file", "mcp_jira_find_ticket", "no_such_tool"}}
	l := NewLazyLoader(lazyFixture(), cfg, lazyFixtureOptions)

	must.False(l.Deferred("read_file"))
	must.False(l.Deferred("mcp_jira_find_ticket"))
	must.False(l.Deferred(types.ToolSearchToolName), "the search tool is always eager")
	must.True(l.Deferred("run_command"))
	must.True(l.Deferred("lookup_order"))
	must.Equal([]string{"no_such_tool"}, l.UnknownEager())
}

func TestLazyLoader_VisibleHarnessModeSendsEagerLoadedAndSearchLast(t *testing.T) {
	must := require.New(t)
	l := NewLazyLoader(lazyFixture(), types.LazyToolLoadingConfig{Enabled: true}, lazyFixtureOptions)

	must.Equal([]string{"read_file", "run_command", types.ToolSearchToolName}, defNames(l.Visible(false)))

	must.Equal([]string{"mcp_jira_find_ticket"}, l.Load([]string{"mcp_jira_find_ticket"}))
	visible := l.Visible(false)
	must.Equal([]string{"read_file", "run_command", "mcp_jira_find_ticket", types.ToolSearchToolName}, defNames(visible))
	for _, d := range visible {
		must.False(d.Deferred, d.Name)
	}
	search := visible[len(visible)-1]
	must.Equal("Search for tools. Tools available to load: 2 tools from MCP server \"github\"; 1 tool from MCP server \"jira\"; 1 tool provided by the control plane.", search.Description)
}

func TestLazyLoader_VisibleNativeModeFlagsDeferredAndDropsHarnessSearch(t *testing.T) {
	must := require.New(t)
	l := NewLazyLoader(lazyFixture(), types.LazyToolLoadingConfig{Enabled: true}, lazyFixtureOptions)
	l.Load([]string{"mcp_github_list_pulls"})

	visible := l.Visible(true)
	must.Equal([]string{"read_file", "run_command", "mcp_github_create_issue", "mcp_github_list_pulls", "mcp_jira_find_ticket", "lookup_order"}, defNames(visible))
	deferred := map[string]bool{}
	for _, d := range visible {
		deferred[d.Name] = d.Deferred
	}
	must.Equal(map[string]bool{
		"read_file": false, "run_command": false,
		"mcp_github_create_issue": true, "mcp_github_list_pulls": true,
		"mcp_jira_find_ticket": true, "lookup_order": true,
	}, deferred, "a loaded tool stays flagged so the request is stable across turns")
}

func TestLazyLoader_NoDeferredToolsIsANoOp(t *testing.T) {
	must := require.New(t)
	reg := newRegistryWith(fixedTool("read_file", "Read a file"), fixedTool(types.ToolSearchToolName, "Search."))
	l := NewLazyLoader(reg, types.LazyToolLoadingConfig{Enabled: true}, LazyLoaderOptions{})

	must.False(l.HasDeferred())
	must.False(l.UsesNative(true))
	must.Equal([]string{"read_file"}, defNames(l.Visible(false)))
	must.Equal([]string{"read_file"}, defNames(l.Visible(true)))
}

func TestLazyLoader_UsesNative(t *testing.T) {
	cases := []struct {
		name      string
		cfg       types.LazyToolLoadingConfig
		supported bool
		want      bool
	}{
		{"auto with capability", types.LazyToolLoadingConfig{Enabled: true}, true, true},
		{"auto without capability", types.LazyToolLoadingConfig{Enabled: true}, false, false},
		{"harness strategy", types.LazyToolLoadingConfig{Enabled: true, Strategy: types.LazyToolStrategyHarness}, true, false},
		{"no eager tool besides search", types.LazyToolLoadingConfig{Enabled: true, Eager: []string{"no_such_tool"}}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			l := NewLazyLoader(lazyFixture(), tc.cfg, lazyFixtureOptions)
			must.Equal(tc.want, l.UsesNative(tc.supported))
		})
	}
}

func TestLazyLoader_LoadIsIdempotentAndIgnoresEagerAndUnknown(t *testing.T) {
	must := require.New(t)
	l := NewLazyLoader(lazyFixture(), types.LazyToolLoadingConfig{Enabled: true}, lazyFixtureOptions)

	must.Equal([]string{"lookup_order", "mcp_jira_find_ticket"}, l.Load([]string{"lookup_order", "read_file", "nope", "mcp_jira_find_ticket"}))
	must.Empty(l.Load([]string{"lookup_order", "mcp_jira_find_ticket"}))
	must.True(l.Loaded("lookup_order"))
	must.False(l.Unloaded("lookup_order"))
	must.True(l.Unloaded("mcp_github_list_pulls"))
	must.False(l.Unloaded("read_file"), "an eager tool is never unloaded")
	must.Equal(2, l.LoadedCount())

	n := l.LoadedCount()
	l.Load([]string{"mcp_github_list_pulls"})
	must.Equal([]string{"mcp_github_list_pulls"}, l.LoadedSince(n))
	must.Nil(l.LoadedSince(l.LoadedCount()))

	l.Reset()
	must.Zero(l.LoadedCount())
	must.True(l.Unloaded("lookup_order"))
}

func TestLazyLoader_LoadIsSafeForConcurrentUse(t *testing.T) {
	must := require.New(t)
	l := NewLazyLoader(lazyFixture(), types.LazyToolLoadingConfig{Enabled: true}, lazyFixtureOptions)
	var wg sync.WaitGroup
	added := make([][]string, 8)
	for i := range added {
		wg.Add(1)
		go func() {
			defer wg.Done()
			added[i] = l.Load([]string{"lookup_order", "mcp_jira_find_ticket"})
			_ = l.Visible(false)
		}()
	}
	wg.Wait()
	total := 0
	for _, a := range added {
		total += len(a)
	}
	must.Equal(2, total, "each tool is reported as newly loaded exactly once")
}

func TestLazyLoader_SearchRanksNameAboveDescriptionAboveSchema(t *testing.T) {
	must := require.New(t)
	l := NewLazyLoader(lazyFixture(), types.LazyToolLoadingConfig{Enabled: true}, lazyFixtureOptions)

	ids := func(matches []LazyMatch) []string {
		out := make([]string, len(matches))
		for i, m := range matches {
			out[i] = m.ID
		}
		return out
	}
	must.Equal([]string{"mcp_github_create_issue", "mcp_github_list_pulls"}, ids(l.Search("GitHub", 0)), "ties keep registration order")
	must.Equal([]string{"mcp_github_create_issue"}, ids(l.Search("issue headline", 0)))
	must.Equal([]string{"mcp_jira_find_ticket", "lookup_order"}, ids(l.Search("ticket order", 0)))
	must.Equal([]string{"mcp_github_list_pulls", "mcp_jira_find_ticket"}, ids(l.Search("pull find", 0)))
	must.Empty(l.Search("read file", 0), "eager tools are not searched")
	must.Empty(l.Search("  --  ", 0))
	must.Empty(l.Search("weather", 0))
}

func matchIDs(matches []LazyMatch) []string {
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m.ID
	}
	return out
}

func TestLazyLoader_SearchRanksByScoreAcrossRegistrationOrder(t *testing.T) {
	must := require.New(t)
	schemaHit := fixedTool("mcp_srv_beta", "Something else")
	schemaHit.InputSchema = json.RawMessage(`{"type":"object","properties":{"widget":{"type":"string"}}}`)
	l := NewLazyLoader(newRegistryWith(
		fixedTool("read_file", "Read a file"),
		schemaHit,
		fixedTool("mcp_srv_alpha", "Create a widget"),
		fixedTool("mcp_srv_widget", "Unrelated text"),
	), types.LazyToolLoadingConfig{Enabled: true}, LazyLoaderOptions{})

	must.Equal([]string{"mcp_srv_widget", "mcp_srv_alpha", "mcp_srv_beta"}, matchIDs(l.Search("widget", 0)),
		"a later name hit outranks an earlier description hit, which outranks a schema hit")
}

func TestLazyLoader_SearchAccumulatesTermScores(t *testing.T) {
	must := require.New(t)
	l := NewLazyLoader(newRegistryWith(
		fixedTool("read_file", "Read a file"),
		fixedTool("mcp_srv_issue", "Unrelated text"),
		fixedTool("mcp_srv_other", "Add a comment to an issue"),
	), types.LazyToolLoadingConfig{Enabled: true}, LazyLoaderOptions{})

	must.Equal([]string{"mcp_srv_other", "mcp_srv_issue"}, matchIDs(l.Search("issue comment", 0)),
		"two description hits outscore one name hit")
}

func TestLazyLoader_SearchScoresAtMost32Terms(t *testing.T) {
	must := require.New(t)
	l := NewLazyLoader(newRegistryWith(
		fixedTool("read_file", "Read a file"),
		fixedTool("mcp_srv_widget", "Make widgets"),
	), types.LazyToolLoadingConfig{Enabled: true}, LazyLoaderOptions{})
	query := ""
	for i := range maxSearchTerms {
		query += fmt.Sprintf("filler%02d ", i)
	}

	must.NotPanics(func() { l.Search(query+"widget", 0) })
	must.Empty(l.Search(query+"widget", 0), "the 33rd term is not scored")
	must.Equal([]string{"mcp_srv_widget"}, matchIDs(l.Search("widget "+query, 0)))
}

func TestLazyLoader_SearchFoldsNonASCIICase(t *testing.T) {
	must := require.New(t)
	l := NewLazyLoader(newRegistryWith(
		fixedTool("read_file", "Read a file"),
		fixedTool("mcp_srv_cv", "Build a résumé"),
	), types.LazyToolLoadingConfig{Enabled: true}, LazyLoaderOptions{})
	must.Equal([]string{"mcp_srv_cv"}, matchIDs(l.Search("RÉSUMÉ", 0)))
}

func TestLazyLoader_SearchExcludesLoadedTools(t *testing.T) {
	must := require.New(t)
	l := NewLazyLoader(lazyFixture(), types.LazyToolLoadingConfig{Enabled: true}, lazyFixtureOptions)
	l.Load([]string{"mcp_github_create_issue"})

	must.Equal([]string{"mcp_github_list_pulls"}, matchIDs(l.Search("github", 0)))
	must.Equal([]string{"mcp_github_create_issue"}, matchIDs(l.LoadedMatches("github")))
	must.Empty(l.LoadedMatches("ticket"), "a loaded tool that does not match is not reported")
}

func TestLazyLoader_InternalIDPassesUnknownNamesThrough(t *testing.T) {
	must := require.New(t)
	l := NewLazyLoader(lazyFixture(), types.LazyToolLoadingConfig{Enabled: true}, lazyFixtureOptions)
	must.Equal("mcp_nowhere_tool", l.InternalID("mcp_nowhere_tool"))
}

func TestSchemaSearchText_IgnoresUnparseableSchemas(t *testing.T) {
	must := require.New(t)
	must.Empty(schemaSearchText(json.RawMessage(`{"properties":`)))
	must.Empty(schemaSearchText(nil))
	must.Equal("path\nfile to read\n", schemaSearchText(json.RawMessage(`{"properties":{"path":{"description":"File to read"}}}`)))
}

func TestLazyLoader_SearchClampsLimit(t *testing.T) {
	must := require.New(t)
	reg := NewRegistry()
	reg.Register(fixedTool("read_file", "Read a file"))
	for i := range 30 {
		reg.Register(fixedTool(fmt.Sprintf("mcp_srv_tool_%02d", i), "widget"))
	}
	l := NewLazyLoader(reg, types.LazyToolLoadingConfig{Enabled: true}, LazyLoaderOptions{})
	must.Len(l.Search("widget", 0), types.DefaultLazyToolSearchResults)
	must.Len(l.Search("widget", 2), 2)
	must.Len(l.Search("widget", 99), types.DefaultLazyToolSearchResults)

	l = NewLazyLoader(reg, types.LazyToolLoadingConfig{Enabled: true, MaxResults: types.MaxLazyToolSearchResults}, LazyLoaderOptions{})
	must.Len(l.Search("widget", 99), types.MaxLazyToolSearchResults)
}

func TestLazyLoader_KeysOnInternalIDsUnderAProfile(t *testing.T) {
	must := require.New(t)
	presenter, err := NewPresenter(lazyFixture(), codingClassicProfile)
	must.NoError(err)
	l := NewLazyLoader(presenter, types.LazyToolLoadingConfig{Enabled: true, Eager: []string{"read_file"}}, lazyFixtureOptions)

	must.True(l.Deferred("run_command"))
	must.Equal("run_command", l.InternalID("bash"))

	matches := l.Search("shell", 0)
	must.Len(matches, 1)
	must.Equal("run_command", matches[0].ID)
	must.Equal("bash", matches[0].Definition.Name)

	must.Equal([]string{"run_command"}, l.Load([]string{l.InternalID("bash")}))
	must.Contains(defNames(l.Visible(false)), "bash", "a loaded tool is sent under its presented name")

	messages := []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		{Role: "assistant", Content: []types.ContentBlock{
			{Type: "tool_search", ID: "srv_1", Name: "tool_search_tool_bm25"},
			{Type: "tool_search_result", ToolUseID: "srv_1", ToolReferences: []string{"run_command", "mcp_jira_find_ticket"}},
		}},
	}
	presented := l.PresentReferences(messages)
	must.Equal([]string{"bash", "mcp_jira_find_ticket"}, presented[1].Content[1].ToolReferences)
	must.Equal([]string{"run_command", "mcp_jira_find_ticket"}, messages[1].Content[1].ToolReferences, "the history is not mutated")
	must.Same(&messages[0].Content[0], &presented[0].Content[0], "messages without a renamed reference are shared")
}

func TestLazyLoader_PresentReferencesReturnsHistoryUnchangedWithoutAliases(t *testing.T) {
	must := require.New(t)
	l := NewLazyLoader(lazyFixture(), types.LazyToolLoadingConfig{Enabled: true}, lazyFixtureOptions)
	messages := []types.Message{{Role: "assistant", Content: []types.ContentBlock{
		{Type: "tool_search_result", ToolUseID: "srv_1", ToolReferences: []string{"mcp_jira_find_ticket"}},
	}}}
	must.Same(&messages[0], &l.PresentReferences(messages)[0])
}

func TestLazyLoader_ForRegistryStartsFresh(t *testing.T) {
	must := require.New(t)
	parent := NewLazyLoader(lazyFixture(), types.LazyToolLoadingConfig{Enabled: true}, lazyFixtureOptions)
	parent.Load([]string{"lookup_order"})

	child := parent.ForRegistry(newRegistryWith(fixedTool("read_file", "Read a file"), fixedTool("lookup_order", "Look up an order")))
	must.True(child.Deferred("lookup_order"))
	must.False(child.Loaded("lookup_order"))
	must.False(child.Deferred("mcp_github_list_pulls"), "the child knows only its own registry")
}
