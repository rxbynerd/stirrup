package provider

import (
	"encoding/json"
	"testing"

	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// foreignSearchBlocks is a recorded native search pair, as either native
// adapter persists it.
func foreignSearchBlocks() []types.ContentBlock {
	return []types.ContentBlock{
		{Type: "tool_search", ID: "srvtoolu_1", Name: "tool_search_tool_bm25", Input: json.RawMessage(`{"query":"jira"}`)},
		{Type: "tool_search_result", ToolUseID: "srvtoolu_1", ToolReferences: []string{"mcp_jira_create_issue"}},
	}
}

// foreignSearchHistory returns a history whose second message holds a
// search pair: alone when searchOnly, otherwise followed by a tool_use
// that the final message answers.
func foreignSearchHistory(searchOnly bool) []types.Message {
	textUser := func(text string) types.Message {
		return types.Message{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: text}}}
	}
	if searchOnly {
		return []types.Message{
			textUser("file a ticket"),
			{Role: "assistant", Content: foreignSearchBlocks()},
			textUser("continue"),
		}
	}
	return []types.Message{
		textUser("file a ticket"),
		{Role: "assistant", Content: append(foreignSearchBlocks(),
			types.ContentBlock{Type: "tool_use", ID: "call_1", Name: "mcp_jira_create_issue", Input: json.RawMessage(`{"title":"Broken build"}`)})},
		{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: "call_1", Content: "PROJ-1"}}},
	}
}

func TestTranslateMessages_ForeignToolSearchHistory(t *testing.T) {
	t.Run("search-only assistant message is omitted", func(t *testing.T) {
		must := require.New(t)
		got := translateMessages("", foreignSearchHistory(true), nil)
		must.Len(got, 2)
		for _, m := range got {
			must.Equal("user", m.Role)
		}
	})

	t.Run("search blocks beside a tool_use are ignored", func(t *testing.T) {
		must := require.New(t)
		got := translateMessages("", foreignSearchHistory(false), nil)
		must.Len(got, 3)
		must.Equal("assistant", got[1].Role)
		must.Len(got[1].ToolCalls, 1)
		must.Equal("call_1", got[1].ToolCalls[0].ID)
		must.Equal("tool", got[2].Role)
	})
}

func TestTranslateMessagesGemini_ForeignToolSearchHistory(t *testing.T) {
	t.Run("search-only assistant message is omitted", func(t *testing.T) {
		must := require.New(t)
		got, _, err := translateMessagesGemini("", foreignSearchHistory(true), quirks.StructuredToolResultCapability{}, "user")
		must.NoError(err)
		must.Len(got, 2)
		for _, c := range got {
			must.Equal("user", c.Role)
			must.NotEmpty(c.Parts)
		}
	})

	t.Run("search blocks beside a tool_use are ignored", func(t *testing.T) {
		must := require.New(t)
		got, names, err := translateMessagesGemini("", foreignSearchHistory(false), quirks.StructuredToolResultCapability{}, "user")
		must.NoError(err)
		must.Len(got, 3)
		must.Equal("model", got[1].Role)
		must.Len(got[1].Parts, 1)
		must.NotNil(got[1].Parts[0].FunctionCall)
		must.Equal("mcp_jira_create_issue", got[1].Parts[0].FunctionCall.Name)
		must.Equal("mcp_jira_create_issue", names["call_1"])
	})
}

func TestBedrockTranslateMessages_ForeignToolSearchHistory(t *testing.T) {
	t.Run("search-only assistant message is omitted and its neighbours joined", func(t *testing.T) {
		must := require.New(t)
		got, err := bedrockTranslateMessages(foreignSearchHistory(true))
		must.NoError(err)
		must.Len(got, 1)
		must.Equal(brtypes.ConversationRoleUser, got[0].Role)
		must.Len(got[0].Content, 2)
		first, ok := got[0].Content[0].(*brtypes.ContentBlockMemberText)
		must.True(ok)
		must.Equal("file a ticket", first.Value)
		second, ok := got[0].Content[1].(*brtypes.ContentBlockMemberText)
		must.True(ok)
		must.Equal("continue", second.Value)
	})

	t.Run("search blocks beside a tool_use are ignored", func(t *testing.T) {
		must := require.New(t)
		got, err := bedrockTranslateMessages(foreignSearchHistory(false))
		must.NoError(err)
		must.Len(got, 3)
		must.Equal(brtypes.ConversationRoleAssistant, got[1].Role)
		must.Len(got[1].Content, 1)
		toolUse, ok := got[1].Content[0].(*brtypes.ContentBlockMemberToolUse)
		must.True(ok)
		must.Equal("call_1", *toolUse.Value.ToolUseId)
	})

	t.Run("an empty message without search blocks is kept", func(t *testing.T) {
		must := require.New(t)
		got, err := bedrockTranslateMessages([]types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
			{Role: "assistant"},
		})
		must.NoError(err)
		must.Len(got, 2)
	})
}
