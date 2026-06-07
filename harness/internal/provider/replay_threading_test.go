package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/harness/internal/provider/quirks"
	"github.com/rxbynerd/stirrup/types"
)

// This file covers the outbound half of the quirks ReplayFields
// round-trip (design §9 risk 7): flattening the per-stream capture onto
// the message_complete event, attaching it to assistant wire messages,
// and the cross-provider leakage guards that keep the state from
// reaching a provider that does not own it.

// TestFlattenReplayCapture pins the flattening rule documented on
// types.StreamEvent.ReplayFields: all-string captures concatenate in
// arrival order; anything else snapshots the last value.
func TestFlattenReplayCapture(t *testing.T) {
	t.Run("string pieces concatenate in arrival order", func(t *testing.T) {
		got := flattenReplayCapture(map[string][]any{
			"reasoning_content": {"", "Let me think. ", "The answer is 4."},
		})
		want := `"Let me think. The answer is 4."`
		if string(got["reasoning_content"]) != want {
			t.Errorf("reasoning_content = %s, want %s", got["reasoning_content"], want)
		}
	})
	t.Run("non-string values snapshot the last capture", func(t *testing.T) {
		got := flattenReplayCapture(map[string][]any{
			"thinking": {
				map[string]any{"step": float64(1)},
				map[string]any{"step": float64(2)},
			},
		})
		want := `{"step":2}`
		if string(got["thinking"]) != want {
			t.Errorf("thinking = %s, want %s", got["thinking"], want)
		}
	})
	t.Run("mixed types snapshot the last capture", func(t *testing.T) {
		got := flattenReplayCapture(map[string][]any{
			"field": {map[string]any{"a": "b"}, "tail"},
		})
		want := `"tail"`
		if string(got["field"]) != want {
			t.Errorf("field = %s, want %s", got["field"], want)
		}
	})
	t.Run("empty capture returns nil", func(t *testing.T) {
		if got := flattenReplayCapture(nil); got != nil {
			t.Errorf("nil capture: got %v, want nil", got)
		}
		if got := flattenReplayCapture(map[string][]any{}); got != nil {
			t.Errorf("empty capture: got %v, want nil", got)
		}
		if got := flattenReplayCapture(map[string][]any{"p": {}}); got != nil {
			t.Errorf("empty value slice: got %v, want nil", got)
		}
	})
}

// TestOpenAIMessage_MarshalJSON_MergesReplayFields pins the wire shape
// of an assistant message carrying replay state: canonical fields in
// struct order, replay entries appended in sorted-key order, and the
// defence-in-depth error arms for collisions and invalid payloads.
func TestOpenAIMessage_MarshalJSON_MergesReplayFields(t *testing.T) {
	t.Run("appends replay entries after canonical fields", func(t *testing.T) {
		m := openaiMessage{
			Role:    "assistant",
			Content: "done",
			ReplayFields: map[string]json.RawMessage{
				"reasoning_content": json.RawMessage(`"thinking..."`),
			},
		}
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		want := `{"role":"assistant","content":"done","reasoning_content":"thinking..."}`
		if string(out) != want {
			t.Errorf("Marshal = %s, want %s", out, want)
		}
	})
	t.Run("multiple entries emit in sorted key order", func(t *testing.T) {
		m := openaiMessage{
			Role: "assistant",
			ReplayFields: map[string]json.RawMessage{
				"zeta_field":  json.RawMessage(`"z"`),
				"alpha_field": json.RawMessage(`"a"`),
			},
		}
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		want := `{"role":"assistant","alpha_field":"a","zeta_field":"z"}`
		if string(out) != want {
			t.Errorf("Marshal = %s, want %s", out, want)
		}
	})
	t.Run("no replay fields is byte-identical to the plain shape", func(t *testing.T) {
		m := openaiMessage{Role: "tool", Content: "ok", ToolCallID: "call_1"}
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		want := `{"role":"tool","content":"ok","tool_call_id":"call_1"}`
		if string(out) != want {
			t.Errorf("Marshal = %s, want %s", out, want)
		}
	})
	t.Run("canonical key collision errors", func(t *testing.T) {
		m := openaiMessage{
			Role:         "assistant",
			ReplayFields: map[string]json.RawMessage{"content": json.RawMessage(`"evil"`)},
		}
		_, err := json.Marshal(m)
		if err == nil {
			t.Fatal("expected collision error, got nil")
		}
		if !strings.Contains(err.Error(), "collides with canonical message field") {
			t.Errorf("error = %q, want collision message", err.Error())
		}
	})
	t.Run("invalid JSON payload errors", func(t *testing.T) {
		m := openaiMessage{
			Role:         "assistant",
			ReplayFields: map[string]json.RawMessage{"reasoning_content": json.RawMessage(`{not json`)},
		}
		_, err := json.Marshal(m)
		if err == nil {
			t.Fatal("expected invalid-JSON error, got nil")
		}
		if !strings.Contains(err.Error(), "invalid JSON") {
			t.Errorf("error = %q, want invalid-JSON message", err.Error())
		}
	})
}

// TestThreadableOpenAIReplayPath pins which path shapes qualify for
// outbound emission: single-segment, non-array, non-canonical only.
func TestThreadableOpenAIReplayPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"reasoning_content", true},
		{"thinking", true},
		{"delta.reasoning_content", false},                       // multi-segment
		{"candidates[].content.parts[].thoughtSignature", false}, // array iteration
		{"role", false},                                          // canonical message key
		{"content", false},                                       // canonical message key
		{"tool_calls", false},                                    // canonical message key
		{"tool_call_id", false},                                  // canonical message key
		{"name", false},                                          // canonical (documented optional key)
		{"a..b", false},                                          // malformed
		{"", false},                                              // malformed
	}
	for _, tc := range cases {
		if got := threadableOpenAIReplayPath(tc.path); got != tc.want {
			t.Errorf("threadableOpenAIReplayPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestTranslateMessages_ThreadsQualifyingReplayFields pins the selection
// rules at the translateMessages seam: only paths named by the resolved
// quirks for THIS stream are emitted, and non-threadable paths are
// silently skipped at runtime.
func TestTranslateMessages_ThreadsQualifyingReplayFields(t *testing.T) {
	assistant := types.Message{
		Role:    "assistant",
		Content: []types.ContentBlock{{Type: "text", Text: "done"}},
		ReplayFields: map[string]json.RawMessage{
			"reasoning_content": json.RawMessage(`"thinking"`),
			"stale_field":       json.RawMessage(`"from another model"`),
		},
	}

	t.Run("qualifying path is emitted", func(t *testing.T) {
		out := translateMessages("", []types.Message{assistant}, []string{"reasoning_content"})
		if len(out) != 1 {
			t.Fatalf("expected 1 message, got %d", len(out))
		}
		if string(out[0].ReplayFields["reasoning_content"]) != `"thinking"` {
			t.Errorf("ReplayFields = %v, want reasoning_content threaded", out[0].ReplayFields)
		}
		if _, leaked := out[0].ReplayFields["stale_field"]; leaked {
			t.Errorf("stale_field not in resolved paths must not leak: %v", out[0].ReplayFields)
		}
	})

	t.Run("no resolved paths emits nothing (registry is the source of truth)", func(t *testing.T) {
		out := translateMessages("", []types.Message{assistant}, nil)
		if out[0].ReplayFields != nil {
			t.Errorf("expected no replay fields without resolved paths, got %v", out[0].ReplayFields)
		}
		body, err := json.Marshal(out[0])
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if strings.Contains(string(body), "reasoning_content") {
			t.Errorf("wire message leaked reasoning_content with no resolved rule: %s", body)
		}
	})

	t.Run("multi-segment and colliding paths are skipped", func(t *testing.T) {
		msg := types.Message{
			Role:    "assistant",
			Content: []types.ContentBlock{{Type: "text", Text: "done"}},
			ReplayFields: map[string]json.RawMessage{
				"delta.reasoning_content": json.RawMessage(`"nested"`),
				"role":                    json.RawMessage(`"evil"`),
				"reasoning_content":       json.RawMessage(`"ok"`),
			},
		}
		paths := []string{"delta.reasoning_content", "role", "reasoning_content"}
		out := translateMessages("", []types.Message{msg}, paths)
		if len(out[0].ReplayFields) != 1 {
			t.Fatalf("ReplayFields = %v, want only reasoning_content", out[0].ReplayFields)
		}
		if string(out[0].ReplayFields["reasoning_content"]) != `"ok"` {
			t.Errorf("reasoning_content = %s, want \"ok\"", out[0].ReplayFields["reasoning_content"])
		}
	})

	t.Run("empty payload is skipped", func(t *testing.T) {
		msg := types.Message{
			Role:         "assistant",
			Content:      []types.ContentBlock{{Type: "text", Text: "done"}},
			ReplayFields: map[string]json.RawMessage{"reasoning_content": nil},
		}
		out := translateMessages("", []types.Message{msg}, []string{"reasoning_content"})
		if out[0].ReplayFields != nil {
			t.Errorf("expected empty payload skipped, got %v", out[0].ReplayFields)
		}
	})

	t.Run("user and tool messages never carry replay fields", func(t *testing.T) {
		msgs := []types.Message{
			{
				Role:    "user",
				Content: []types.ContentBlock{{Type: "text", Text: "hi"}},
				// A user message carrying replay state is malformed input;
				// the assistant-only emission point must ignore it.
				ReplayFields: map[string]json.RawMessage{"reasoning_content": json.RawMessage(`"x"`)},
			},
		}
		out := translateMessages("", msgs, []string{"reasoning_content"})
		for i, m := range out {
			if m.ReplayFields != nil {
				t.Errorf("message %d (%s) carries replay fields: %v", i, m.Role, m.ReplayFields)
			}
		}
	})
}

// TestReplayFields_DeepSeekV4_MessageCompleteCarriesFlattenedValue
// drives the synthetic deepseek-v4 SSE fixture through the production
// Stream path and asserts the message_complete event carries the
// concatenation of every reasoning_content piece — the parse-side half
// of the round-trip the loop persists onto the assistant Message.
func TestReplayFields_DeepSeekV4_MessageCompleteCarriesFlattenedValue(t *testing.T) {
	srv := sseStubServer(t, streamFixtureSSE(t, "testdata/quirks/openai-compatible/deepseek-v4/response.sse"))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "deepseek-v4",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drainStream(t, ch)

	var complete *types.StreamEvent
	for i := range events {
		if events[i].Type == "message_complete" {
			complete = &events[i]
		}
	}
	if complete == nil {
		t.Fatalf("no message_complete event in %v", events)
	}
	// The fixture streams reasoning_content as "" then "Considering the
	// request carefully." — the flattened value is the in-order
	// concatenation, marshalled as a JSON string.
	want := `"Considering the request carefully."`
	if got := string(complete.ReplayFields["reasoning_content"]); got != want {
		t.Errorf("message_complete ReplayFields[reasoning_content] = %s, want %s", got, want)
	}
}

// TestReplayFields_NoRule_MessageCompleteCarriesNothing pins the
// negative: a model whose resolved quirks register no ReplayFields
// produces a message_complete with a nil ReplayFields map, keeping the
// event byte-identical to the pre-threading shape.
func TestReplayFields_NoRule_MessageCompleteCarriesNothing(t *testing.T) {
	srv := sseStubServer(t, streamFixtureSSE(t, "testdata/quirks/openai-compatible/o1-mini/response.sse"))
	defer srv.Close()

	adapter := NewOpenAICompatibleAdapter(staticBearer("test-key"), srv.URL, OpenAIAuthConfig{}, RetryPolicy{})

	ch, err := adapter.Stream(context.Background(), types.StreamParams{
		Model:     "gpt-4o",
		MaxTokens: 1024,
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for _, ev := range drainStream(t, ch) {
		if ev.Type == "message_complete" && ev.ReplayFields != nil {
			t.Errorf("message_complete carries ReplayFields with no rule: %v", ev.ReplayFields)
		}
	}
}

// replayLeakageMessages is the shared history for the cross-provider
// leakage guards: an assistant message carrying ReplayFields state that
// only an openai-compatible adapter is allowed to serialise.
func replayLeakageMessages(withReplay bool) []types.Message {
	msgs := []types.Message{
		{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "list main.go"}}},
		{
			Role: "assistant",
			Content: []types.ContentBlock{
				{Type: "text", Text: "Reading."},
				{Type: "tool_use", ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"main.go"}`)},
			},
		},
		{Role: "user", Content: []types.ContentBlock{{Type: "tool_result", ToolUseID: "call_1", Content: "package main"}}},
	}
	if withReplay {
		msgs[1].ReplayFields = map[string]json.RawMessage{
			"reasoning_content": json.RawMessage(`"DeepSeek-private chain of thought"`),
		}
	}
	return msgs
}

// TestTranslateMessagesAnthropic_IgnoresMessageReplayFields is the
// cross-provider leakage guard for the message-level replay state: the
// Anthropic wire body must be byte-identical with and without
// Message.ReplayFields populated, the same invariant
// anthropicContentBlock already enforces for the block-level
// ThoughtSignature (#194).
func TestTranslateMessagesAnthropic_IgnoresMessageReplayFields(t *testing.T) {
	without, err := json.Marshal(translateMessagesAnthropic(replayLeakageMessages(false), quirks.StructuredToolResultCapability{}))
	if err != nil {
		t.Fatalf("marshal without: %v", err)
	}
	with, err := json.Marshal(translateMessagesAnthropic(replayLeakageMessages(true), quirks.StructuredToolResultCapability{}))
	if err != nil {
		t.Fatalf("marshal with: %v", err)
	}
	if !bytes.Equal(without, with) {
		t.Errorf("anthropic wire bytes diverge when Message.ReplayFields is set:\nwithout: %s\nwith:    %s", without, with)
	}
	if bytes.Contains(with, []byte("reasoning_content")) {
		t.Errorf("anthropic wire body leaked reasoning_content: %s", with)
	}
}

// TestTranslateMessagesGemini_IgnoresMessageReplayFields mirrors the
// Anthropic guard for the Gemini adapter: Gemini's round-trip state is
// the typed block-level ThoughtSignature, never the message-level
// ReplayFields map.
func TestTranslateMessagesGemini_IgnoresMessageReplayFields(t *testing.T) {
	contentsWithout, _, err := translateMessagesGemini("sys", replayLeakageMessages(false), quirks.StructuredToolResultCapability{})
	if err != nil {
		t.Fatalf("translate without: %v", err)
	}
	contentsWith, _, err := translateMessagesGemini("sys", replayLeakageMessages(true), quirks.StructuredToolResultCapability{})
	if err != nil {
		t.Fatalf("translate with: %v", err)
	}
	without, err := json.Marshal(contentsWithout)
	if err != nil {
		t.Fatalf("marshal without: %v", err)
	}
	with, err := json.Marshal(contentsWith)
	if err != nil {
		t.Fatalf("marshal with: %v", err)
	}
	if !bytes.Equal(without, with) {
		t.Errorf("gemini wire bytes diverge when Message.ReplayFields is set:\nwithout: %s\nwith:    %s", without, with)
	}
	if bytes.Contains(with, []byte("reasoning_content")) {
		t.Errorf("gemini wire body leaked reasoning_content: %s", with)
	}
}
