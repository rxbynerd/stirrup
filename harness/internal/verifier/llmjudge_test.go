package verifier

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rxbynerd/stirrup/types"
)

// mockProvider returns a canned sequence of StreamEvents for testing.
type mockProvider struct {
	events []types.StreamEvent
	err    error // returned by Stream itself (connection-level error)

	// Captured arguments from the last Stream call.
	lastParams types.StreamParams
}

func (m *mockProvider) Stream(_ context.Context, params types.StreamParams) (<-chan types.StreamEvent, error) {
	m.lastParams = params
	if m.err != nil {
		return nil, m.err
	}
	ch := make(chan types.StreamEvent, len(m.events))
	for _, e := range m.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

func TestLLMJudgeVerifier_Pass(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: `{"passed": true, `},
			{Type: "text_delta", Text: `"feedback": "meets all criteria"}`},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	v := NewLLMJudgeVerifier(prov, "claude-haiku-4-5-20251001", "code must compile")
	result, err := v.Verify(context.Background(), VerifyContext{
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "fix the bug"}}},
			{Role: "assistant", Content: []types.ContentBlock{{Type: "text", Text: "done"}}},
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Passed {
		t.Fatal("expected Passed to be true")
	}
	if result.Feedback != "meets all criteria" {
		t.Errorf("unexpected feedback: %q", result.Feedback)
	}

	// Verify the provider was called with correct parameters.
	if prov.lastParams.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("expected model %q, got %q", "claude-haiku-4-5-20251001", prov.lastParams.Model)
	}
	if prov.lastParams.MaxTokens != judgeMaxTokens {
		t.Errorf("expected maxTokens %d, got %d", judgeMaxTokens, prov.lastParams.MaxTokens)
	}
	if prov.lastParams.Temperature != judgeTemperature {
		t.Errorf("expected temperature %v, got %v", judgeTemperature, prov.lastParams.Temperature)
	}
	if len(prov.lastParams.Tools) != 0 {
		t.Errorf("expected no tools, got %d", len(prov.lastParams.Tools))
	}
	if prov.lastParams.System != judgeSystemPrompt {
		t.Errorf("expected judge system prompt, got %q", prov.lastParams.System)
	}
}

func TestLLMJudgeVerifier_Fail(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: `{"passed": false, "feedback": "code does not compile"}`},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	v := NewLLMJudgeVerifier(prov, "claude-haiku-4-5-20251001", "code must compile")
	result, err := v.Verify(context.Background(), VerifyContext{
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "write code"}}},
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Passed {
		t.Fatal("expected Passed to be false")
	}
	if result.Feedback != "code does not compile" {
		t.Errorf("unexpected feedback: %q", result.Feedback)
	}
}

func TestLLMJudgeVerifier_MalformedResponse(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: "I think the code looks good!"},
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	v := NewLLMJudgeVerifier(prov, "claude-haiku-4-5-20251001", "code must compile")
	result, err := v.Verify(context.Background(), VerifyContext{
		Messages: []types.Message{},
	})

	// Malformed response is not an error — it is a verification failure.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Passed {
		t.Fatal("expected Passed to be false for malformed response")
	}
	if !strings.Contains(result.Feedback, "malformed response") {
		t.Errorf("feedback should mention malformed response, got %q", result.Feedback)
	}
	if result.Details["rawResponse"] != "I think the code looks good!" {
		t.Errorf("details should include raw response, got %v", result.Details["rawResponse"])
	}
	if result.Details["parseError"] == nil {
		t.Error("details should include parse error")
	}
}

func TestLLMJudgeVerifier_StreamConnectionError(t *testing.T) {
	prov := &mockProvider{
		err: fmt.Errorf("connection refused"),
	}

	v := NewLLMJudgeVerifier(prov, "claude-haiku-4-5-20251001", "anything")
	_, err := v.Verify(context.Background(), VerifyContext{})

	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "stream request failed") {
		t.Errorf("error should mention stream request, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error should wrap underlying cause, got %q", err.Error())
	}
}

func TestLLMJudgeVerifier_StreamEventError(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: `{"pass`},
			{Type: "error", Error: fmt.Errorf("rate limited")},
		},
	}

	v := NewLLMJudgeVerifier(prov, "claude-haiku-4-5-20251001", "anything")
	_, err := v.Verify(context.Background(), VerifyContext{})

	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "stream error") {
		t.Errorf("error should mention stream error, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error should wrap underlying cause, got %q", err.Error())
	}
}

func TestLLMJudgeVerifier_UserMessageContainsCriteria(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "text_delta", Text: `{"passed": true, "feedback": "ok"}`},
		},
	}

	criteria := "output must include test results"
	v := NewLLMJudgeVerifier(prov, "test-model", criteria)
	v.Verify(context.Background(), VerifyContext{
		Messages: []types.Message{
			{Role: "user", Content: []types.ContentBlock{{Type: "text", Text: "hello"}}},
			{Role: "assistant", Content: []types.ContentBlock{
				{Type: "tool_use", Name: "read_file"},
				{Type: "text", Text: "here is the result"},
			}},
		},
	})

	// The user message sent to the judge should contain the criteria and conversation.
	if len(prov.lastParams.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(prov.lastParams.Messages))
	}
	userMsg := prov.lastParams.Messages[0]
	if userMsg.Role != "user" {
		t.Errorf("expected user role, got %q", userMsg.Role)
	}
	text := userMsg.Content[0].Text
	if !strings.Contains(text, criteria) {
		t.Error("user message should contain the criteria")
	}
	if !strings.Contains(text, "hello") {
		t.Error("user message should contain conversation content")
	}
	if !strings.Contains(text, "here is the result") {
		t.Error("user message should contain assistant response")
	}
	if !strings.Contains(text, "[tool_use: read_file]") {
		t.Error("user message should contain tool use references")
	}
}

func TestLLMJudgeVerifier_EmptyResponse(t *testing.T) {
	prov := &mockProvider{
		events: []types.StreamEvent{
			{Type: "message_complete", StopReason: "end_turn"},
		},
	}

	v := NewLLMJudgeVerifier(prov, "test-model", "anything")
	result, err := v.Verify(context.Background(), VerifyContext{})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Passed {
		t.Fatal("expected Passed to be false for empty response")
	}
	if !strings.Contains(result.Feedback, "malformed response") {
		t.Errorf("feedback should mention malformed response, got %q", result.Feedback)
	}
}

// Verify that LLMJudgeVerifier satisfies the Verifier interface.
var _ Verifier = (*LLMJudgeVerifier)(nil)

// Verify that mockProvider satisfies the ProviderAdapter interface.
var _ interface {
	Stream(context.Context, types.StreamParams) (<-chan types.StreamEvent, error)
} = (*mockProvider)(nil)
