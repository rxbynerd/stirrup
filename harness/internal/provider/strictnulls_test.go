package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/harness/internal/security"
	"github.com/rxbynerd/stirrup/types"
)

// findFilesSchema has a required property, an optional array, and an
// optional property whose original type already admits null.
const findFilesSchema = `{
	"type": "object",
	"properties": {
		"pattern": {"type": "string"},
		"exclude": {"type": "array", "items": {"type": "string"}},
		"note":    {"type": ["string", "null"]}
	},
	"required": ["pattern"]
}`

// unliftableSchema fails the strict-mode lint, so the tool is sent with
// strict:false.
const unliftableSchema = `{
	"type": "object",
	"properties": {
		"pattern": {"type": "string"},
		"exclude": {"anyOf": [{"type": "array"}, {"type": "string"}]}
	},
	"required": ["pattern"]
}`

func strictNullTools() []types.ToolDefinition {
	return []types.ToolDefinition{
		{Name: "find_files", Description: "find", InputSchema: json.RawMessage(findFilesSchema)},
		{Name: "loose", Description: "loose", InputSchema: json.RawMessage(unliftableSchema)},
	}
}

// strictNullCalls are the arguments the model sends: an explicit null for
// every property, which strict mode demands of an omitted optional.
var strictNullCalls = map[string]string{
	"find_files": `{"pattern":null,"exclude":null,"note":null}`,
	"loose":      `{"pattern":"x","exclude":null}`,
}

func toolCallsByName(events []types.StreamEvent) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, ev := range events {
		if ev.Type == "tool_call" {
			out[ev.Name] = ev.Input
		}
	}
	return out
}

func chatToolCallSSE() string {
	var b strings.Builder
	idx := 0
	for _, name := range []string{"find_files", "loose"} {
		args, _ := json.Marshal(strictNullCalls[name])
		b.WriteString(makeOpenAIChunk(fmt.Sprintf(`{"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"id":"call_%s","type":"function","function":{"name":"%s","arguments":%s}}]},"finish_reason":null}]}`, idx, name, name, args)))
		idx++
	}
	b.WriteString(makeOpenAIChunk(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func streamChatNulls(t *testing.T, registry *quirks.Registry) map[string]map[string]any {
	t.Helper()
	must := require.New(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, chatToolCallSSE())
	}))
	t.Cleanup(srv.Close)
	adapter := NewOpenAICompatibleAdapter(staticBearer("k"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})
	adapter.Registry = registry
	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o-mini",
		MaxTokens: 256,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "go"}}}},
		Tools:     strictNullTools(),
	})
	must.NoError(err)
	return toolCallsByName(drainStream(t, ch))
}

func TestOpenAIAdapter_StrictNulls_DropsSyntheticNullOnOptionalProperty(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	calls := streamChatNulls(t, strictRegistryFor("gpt-4o-mini"))
	must.Contains(calls, "find_files")

	check.NotContains(calls["find_files"], "exclude", "optional array nulled by the normaliser must be dropped")
}

func TestOpenAIAdapter_StrictNulls_KeepsNullOnRequiredProperty(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	calls := streamChatNulls(t, strictRegistryFor("gpt-4o-mini"))
	must.Contains(calls, "find_files")

	check.Contains(calls["find_files"], "pattern")
	check.Nil(calls["find_files"]["pattern"])
	input, err := json.Marshal(calls["find_files"])
	must.NoError(err)
	check.Error(security.ValidateJSONSchema(input, json.RawMessage(findFilesSchema)), "a null required property must still fail original-schema validation")
}

func TestOpenAIAdapter_StrictNulls_KeepsNullOnPropertyNullableInOriginalSchema(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	calls := streamChatNulls(t, strictRegistryFor("gpt-4o-mini"))
	must.Contains(calls, "find_files")

	check.Contains(calls["find_files"], "note")
	check.Nil(calls["find_files"]["note"])
}

func TestOpenAIAdapter_StrictNulls_LeavesNonStrictToolsUntouched(t *testing.T) {
	t.Run("tool that fell back to strict:false", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		calls := streamChatNulls(t, strictRegistryFor("gpt-4o-mini"))
		must.Contains(calls, "loose")
		check.Contains(calls["loose"], "exclude")
	})

	t.Run("model without strict mode", func(t *testing.T) {
		must := require.New(t)
		check := assert.New(t)
		calls := streamChatNulls(t, quirks.NewRegistry(nil))
		must.Contains(calls, "find_files")
		check.Contains(calls["find_files"], "exclude")
	})
}

func TestOpenAIResponsesAdapter_StrictNulls_StripsOnlySyntheticNulls(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	var items []string
	for _, name := range []string{"find_files", "loose"} {
		args, err := json.Marshal(strictNullCalls[name])
		must.NoError(err)
		items = append(items, fmt.Sprintf(`{"type":"function_call","id":"fc_%s","call_id":"call_%s","name":"%s","arguments":%s,"status":"completed"}`, name, name, name, args))
	}
	sse := strings.Join([]string{
		makeResponsesEvent("response.output_item.done", `{"output_index":0,"item":`+items[0]+`}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":1,"item":`+items[1]+`}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed"}}`),
	}, "")
	script := newResponsesScript(t, []byte(sse))
	ch, err := script.adapter(nil).Stream(context.Background(), types.StreamParams{
		Model:     responsesReplayModel,
		MaxTokens: 256,
		Messages:  []types.Message{replayUserPrompt},
		Tools:     strictNullTools(),
	})
	must.NoError(err)
	calls := toolCallsByName(drainStream(t, ch))
	must.Contains(calls, "find_files")
	must.Contains(calls, "loose")

	check.NotContains(calls["find_files"], "exclude", "optional array nulled by the normaliser must be dropped")
	check.Contains(calls["find_files"], "pattern", "a null required property is kept")
	check.Nil(calls["find_files"]["pattern"])
	check.Contains(calls["find_files"], "note", "a property nullable in the original schema keeps its null")
	check.Contains(calls["loose"], "exclude", "a tool sent with strict:false is untouched")
}

func TestBatchAdapter_StrictNulls_FabricatedToolCallsDropSyntheticNulls(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	a := NewBatchAdapter(nil, &fakeBatchClient{}, &types.BatchProviderConfig{Enabled: true}, "openai-compatible", "", "run-test")
	a.Registry = strictRegistryFor("gpt-4o-mini")
	_, nulls, err := a.marshalRequestBody(t.Context(), types.StreamParams{
		Model:     "gpt-4o-mini",
		MaxTokens: 256,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "go"}}}},
		Tools:     strictNullTools(),
	})
	must.NoError(err)

	args, err := json.Marshal(strictNullCalls["find_files"])
	must.NoError(err)
	chatResponse := fmt.Sprintf(`{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"find_files","arguments":%s}}]},"finish_reason":"tool_calls"}]}`, args)

	t.Run("chat completions", func(t *testing.T) {
		ch := make(chan types.StreamEvent, 8)
		fabricateStream(ch, []byte(chatResponse), "openai-compatible", nulls)
		close(ch)
		calls := toolCallsByName(drain(t, ch))
		check.NotContains(calls["find_files"], "exclude")
		check.Contains(calls["find_files"], "pattern")
		check.Contains(calls["find_files"], "note")
	})

	t.Run("responses", func(t *testing.T) {
		item := fmt.Sprintf(`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"find_files","arguments":%s,"status":"completed"}`, args)
		ch := make(chan types.StreamEvent, 8)
		fabricateStream(ch, responsesBatchResult(item), "openai-responses", nulls)
		close(ch)
		calls := toolCallsByName(drain(t, ch))
		check.NotContains(calls["find_files"], "exclude")
		check.Contains(calls["find_files"], "pattern")
		check.Contains(calls["find_files"], "note")
	})
}

func TestBatchAdapter_StrictNulls_NonStrictModelReportsNone(t *testing.T) {
	must := require.New(t)
	a := NewBatchAdapter(nil, &fakeBatchClient{}, &types.BatchProviderConfig{Enabled: true}, "openai-compatible", "", "run-test")
	a.Registry = quirks.NewRegistry(nil)
	_, nulls, err := a.marshalRequestBody(t.Context(), types.StreamParams{
		Model:     "gpt-4o-mini",
		MaxTokens: 256,
		Messages:  []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "go"}}}},
		Tools:     strictNullTools(),
	})
	must.NoError(err)
	must.Empty(nulls)
}

// namespacedStrictNullTools puts three deferred tools in one namespace
// ahead of an eager one, so the regrouped wire tools list is shorter than
// the tool definitions and its indexes no longer line up with them.
func namespacedStrictNullTools() []types.ToolDefinition {
	member := func(name string) types.ToolDefinition {
		return types.ToolDefinition{
			Name:                 name,
			Description:          name,
			InputSchema:          json.RawMessage(findFilesSchema),
			Deferred:             true,
			Namespace:            "mcp_crm",
			NamespaceDescription: "CRM tools",
		}
	}
	return []types.ToolDefinition{
		member("mcp_crm_list"),
		member("mcp_crm_get"),
		member("mcp_crm_update"),
		{Name: "loose", Description: "loose", InputSchema: json.RawMessage(unliftableSchema)},
	}
}

func TestOpenAIResponsesAdapter_StrictNulls_NamespacedDeferredToolsUnderToolSearch(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	args, err := json.Marshal(strictNullCalls["find_files"])
	must.NoError(err)
	looseArgs, err := json.Marshal(strictNullCalls["loose"])
	must.NoError(err)
	sse := strings.Join([]string{
		makeResponsesEvent("response.output_item.done", `{"output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"mcp_crm_update","namespace":"mcp_crm","arguments":`+string(args)+`,"status":"completed"}}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"loose","arguments":`+string(looseArgs)+`,"status":"completed"}}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed"}}`),
	}, "")
	script := newResponsesScript(t, []byte(sse))
	ch, err := script.adapter(nil).Stream(context.Background(), types.StreamParams{
		Model:      "gpt-5.4-mini",
		MaxTokens:  256,
		ToolSearch: true,
		Messages:   []types.Message{replayUserPrompt},
		Tools:      namespacedStrictNullTools(),
	})
	must.NoError(err)
	calls := toolCallsByName(drainStream(t, ch))
	must.Contains(string(script.lastBody(t)), `"type":"namespace"`, "the request must group the deferred tools")
	must.Contains(calls, "mcp_crm_update")
	must.Contains(calls, "loose")

	check.NotContains(calls["mcp_crm_update"], "exclude", "a namespaced strict tool's synthetic null must be dropped")
	check.Contains(calls["mcp_crm_update"], "note")
	check.Contains(calls["loose"], "exclude", "a tool sent with strict:false is untouched")
}

func TestTranslateToolsResponses_StrictNulls_KeyedByToolNameAcrossNamespaces(t *testing.T) {
	must := require.New(t)
	_, _, nulls, err := translateToolsResponses(namespacedStrictNullTools(), true, false, true, "gpt-5.4-mini", nil)
	must.NoError(err)

	want := map[string]struct{}{"exclude": {}}
	must.Equal(strictNullables{"mcp_crm_list": want, "mcp_crm_get": want, "mcp_crm_update": want}, nulls)
}

func TestTranslateTools_StrictNulls_KeyedByToolName(t *testing.T) {
	must := require.New(t)
	_, _, nulls, err := translateTools(strictNullTools(), true, false, "gpt-4o-mini", nil)
	must.NoError(err)

	must.Equal(strictNullables{"find_files": {"exclude": {}}}, nulls)
}

func TestOpenAIResponsesAdapter_StrictNulls_ReplayKeepsStoredItemsVerbatim(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	args, err := json.Marshal(strictNullCalls["find_files"])
	must.NoError(err)
	reasoning := `{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAAAAB-placeholder"}`
	call := `{"type":"function_call","id":"fc_1","call_id":"call_1","name":"find_files","arguments":` + string(args) + `,"status":"completed"}`
	sse := strings.Join([]string{
		makeResponsesEvent("response.output_item.done", `{"output_index":0,"item":`+reasoning+`}`),
		makeResponsesEvent("response.output_item.done", `{"output_index":1,"item":`+call+`}`),
		makeResponsesEvent("response.completed", `{"response":{"status":"completed","output":[`+reasoning+`,`+call+`]}}`),
	}, "")
	script := newResponsesScript(t, []byte(sse))
	adapter := script.adapter(nil)
	stream := func(messages ...types.Message) []types.StreamEvent {
		ch, err := adapter.Stream(context.Background(), types.StreamParams{
			Model:     responsesReplayModel,
			MaxTokens: 256,
			Messages:  messages,
			Tools:     strictNullTools(),
		})
		must.NoError(err)
		return drainStream(t, ch)
	}

	events := stream(replayUserPrompt)
	assistant := assistantFromEvents(events)
	must.Len(assistant.Content, 1)
	must.NotContains(string(assistant.Content[0].Input), "exclude", "the persisted tool_use input lost the synthetic null")
	stored := storedReplayItems(t, messageComplete(t, events))

	results := types.Message{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: "call_1", Content: "a.txt"}}}
	stream(replayUserPrompt, assistant, results)
	input := requestInput(t, script.lastBody(t))

	must.Len(input, 1+len(stored)+1)
	for i, item := range stored {
		check.JSONEq(string(item), string(input[1+i]), "input[%d] should replay the stored item", 1+i)
	}
	check.Contains(string(input[1]), "encrypted_content", "the encrypted reasoning item is replayed")
}

func TestTranslateTools_StrictNulls_ComputedOncePerCachedTool(t *testing.T) {
	must := require.New(t)
	cache := newStrictSchemaCache()
	_, _, first, err := translateTools(strictNullTools(), true, false, "gpt-4o-mini", cache)
	must.NoError(err)
	misses := cache.Misses.Load()

	_, _, second, err := translateTools(strictNullTools(), true, false, "gpt-4o-mini", cache)
	must.NoError(err)
	must.Equal(misses, cache.Misses.Load(), "a repeat request must not recompute any cache entry")
	must.Equal(first, second)
	must.Equal(strictNullables{"find_files": {"exclude": {}}}, second)
}
