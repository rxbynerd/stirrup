package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/rxbynerd/stirrup/types"
)

func TestAnthropicAdapter_Stream_NoToolsOmitsToolFields(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	bodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		check.NoError(err)
		bodies <- raw
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, joinLines(
			makeSSE("content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`),
			makeSSE("content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"{\"label\":\"spam\"}"}}`),
			makeSSE("content_block_stop", `{"index":0}`),
			makeSSE("message_delta", `{"delta":{"stop_reason":"end_turn"}}`),
			makeSSE("message_stop", `{}`),
		))
	}))
	defer srv.Close()

	adapter := NewAnthropicAdapter(staticBearer("test-key"), AuthModeAPIKey)
	adapter.baseURL = srv.URL
	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:      "claude-haiku-5-5",
		System:     "Classify the submission.",
		Messages:   []types.Message{{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "Buy now!"}}}},
		MaxTokens:  1024,
		ToolChoice: types.ToolChoiceAuto,
	})
	must.NoError(err)
	collectEvents(t, ch)

	var body map[string]any
	must.NoError(json.Unmarshal(<-bodies, &body))
	check.NotContains(body, "tools")
	check.NotContains(body, "tool_choice")
}
